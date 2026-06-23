package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// siteNameRe is a DNS label: site names become hostnames, so they are
// stricter than the CLI's [a-z0-9-]+ (no leading/trailing hyphen).
var siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const (
	maxDeploySize  = 100 << 20
	maxDeployFiles = 2000
	// maxRawDeployParts caps multipart parts before junk is filtered, as a
	// DoS guard. The user-facing maxDeployFiles limit is applied to the
	// published file count instead, so it reflects what actually ships.
	maxRawDeployParts = maxDeployFiles * 4
)

type SiteStorage interface {
	Put(ctx context.Context, site, path, contentType string, data []byte) error
	List(ctx context.Context, site string) ([]string, error)
	Open(ctx context.Context, site, path string) (io.ReadCloser, SiteFileInfo, error)
	Remove(ctx context.Context, site, path string) error
}

type SiteFileInfo struct {
	LastModified time.Time
}

// SiteStore writes deployed sites into an S3-compatible bucket. Deploys
// and reads go through the server so browsers never see storage
// credentials.
type SiteStore struct {
	client *minio.Client
	bucket string
}

func NewSiteStore(endpoint, accessKey, secretKey, bucket string) (*SiteStore, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(accessKey, secretKey, ""),
	})
	if err != nil {
		return nil, fmt.Errorf("site store client: %w", err)
	}
	if err := ensureS3Bucket(context.Background(), client, bucket); err != nil {
		return nil, fmt.Errorf("site store bucket %s: %w", bucket, err)
	}
	return &SiteStore{client: client, bucket: bucket}, nil
}

func (s *SiteStore) Put(ctx context.Context, site, path, contentType string, data []byte) error {
	key := site + "/" + path
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("store site file %s: %w", key, err)
	}
	return nil
}

// List returns the site-relative paths of every file the site currently
// serves.
func (s *SiteStore) List(ctx context.Context, site string) ([]string, error) {
	prefix := site + "/"
	var paths []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list site %s: %w", site, obj.Err)
		}
		paths = append(paths, strings.TrimPrefix(obj.Key, prefix))
	}
	return paths, nil
}

func (s *SiteStore) Open(ctx context.Context, site, path string) (io.ReadCloser, SiteFileInfo, error) {
	key := site + "/" + path
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, SiteFileInfo{}, fmt.Errorf("open site file %s: %w", key, err)
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		var resp minio.ErrorResponse
		if errors.As(err, &resp) && (resp.StatusCode == 404 || resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket") {
			return nil, SiteFileInfo{}, ErrNotFound
		}
		return nil, SiteFileInfo{}, fmt.Errorf("stat site file %s: %w", key, err)
	}
	return obj, SiteFileInfo{LastModified: info.LastModified}, nil
}

func (s *SiteStore) Remove(ctx context.Context, site, path string) error {
	key := site + "/" + path
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("remove site file %s: %w", key, err)
	}
	return nil
}

// deployFile is one file of a site deploy, held in memory so the whole
// deploy validates before anything is written — a rejected deploy never
// partially overwrites a live site.
type deployFile struct {
	path string
	data []byte
}

// handleDeploy publishes a site from the browser: a multipart form with
// a "site" name field and one "files" part per file, each part's
// filename carrying the file's site-relative path. Semantics match the
// CLI's sync semantics: the uploaded set replaces the site and stale
// files are removed.
//
// The endpoint only answers on the apex domain. Combined with the
// same-origin check, that means a deployed site's JavaScript cannot
// quietly redeploy other sites with a visitor's ambient mesh identity —
// deploying stays a deliberate act on the platform page.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if s.sites == nil {
		httpError(w, http.StatusServiceUnavailable,
			"site store not configured: set SPOT_S3_ENDPOINT and credentials")
		return
	}
	if siteFromHost(s.requestHost(r), s.spotDomain) != "" {
		httpError(w, http.StatusBadRequest,
			"the deploy API is served on the platform root, not on site subdomains")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDeploySize)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, http.StatusBadRequest, "request must be multipart/form-data")
		return
	}

	var site string
	var files []deployFile
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			deployReadError(w, err)
			return
		}
		switch part.FormName() {
		case "site":
			raw, err := io.ReadAll(io.LimitReader(part, 256))
			if err != nil {
				deployReadError(w, err)
				return
			}
			site = strings.TrimSpace(string(raw))
		case "files":
			if len(files) >= maxRawDeployParts {
				httpError(w, http.StatusBadRequest,
					fmt.Sprintf("too many files in the deploy (max %d)", maxDeployFiles))
				return
			}
			data, err := io.ReadAll(part)
			if err != nil {
				deployReadError(w, err)
				return
			}
			files = append(files, deployFile{path: partFilename(part), data: data})
		}
	}

	if !siteNameRe.MatchString(site) {
		httpError(w, http.StatusBadRequest,
			"site name must be 1-63 lowercase letters, digits or hyphens, starting and ending with a letter or digit")
		return
	}
	files, err = normalizeDeploy(files)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(files) > maxDeployFiles {
		httpError(w, http.StatusBadRequest,
			fmt.Sprintf("too many files in the deploy (max %d)", maxDeployFiles))
		return
	}
	if err := validateDeployPolicy(site, files); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	metadata, err := metadataForDeploy(site, files)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	incomingPolicy, hasIncomingPolicy, incomingPolicyErr := deployAccessPolicy(site, files)
	restricted := policyRestrictsAccess(incomingPolicy, hasIncomingPolicy, incomingPolicyErr)
	if s.deployAuth == nil {
		httpError(w, http.StatusServiceUnavailable, "deploy registry not configured")
		return
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return
	}
	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	defer siteLock.Unlock()

	authz, err := s.deployAuth.AuthorizeDeploy(r.Context(), site, actor)
	if errors.Is(err, ErrDeployForbidden) {
		s.recordDeployAudit(r, DeployAuditEvent{
			Site:       site,
			Actor:      actor,
			Action:     "deploy",
			Status:     "denied",
			Message:    "actor is not the site owner or a platform admin",
			FileCount:  len(files),
			TotalBytes: totalDeployBytes(files),
		})
		httpError(w, http.StatusForbidden, "only the site owner or a platform admin can deploy this site")
		return
	}
	if err != nil {
		log.Printf("deploy %s: authorize: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize deploy")
		return
	}
	var policyOnFailure *failurePolicyCache
	if authz.Action == "create" {
		s.cacheIncomingPolicyForCreate(site, incomingPolicy, hasIncomingPolicy, incomingPolicyErr)
		if hasIncomingPolicy {
			policyOnFailure = &failurePolicyCache{
				policy: immediatePolicyForCreate(incomingPolicy),
				err:    incomingPolicyErr,
			}
		}
	} else {
		previousPolicy, previousPolicyErr := s.policyForSite(r.Context(), site)
		if preservePolicyOnFailure(previousPolicy, previousPolicyErr, incomingPolicy, hasIncomingPolicy, incomingPolicyErr) {
			policyOnFailure = &failurePolicyCache{policy: previousPolicy, err: previousPolicyErr}
		}
	}
	writeAccessFirst := authz.Action == "create" && restricted && s.policies == nil && hasIncomingPolicy
	deferAccessChange := policyOnFailure != nil && !writeAccessFirst
	updateBeforePolicyBroadening := deferAccessChange && !restricted

	existing, err := s.sites.List(r.Context(), site)
	if err != nil {
		log.Printf("deploy %s: %v", site, err)
		s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not read current files")
		httpError(w, http.StatusInternalServerError, "could not read the site's current files")
		return
	}
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[f.path] = true
	}
	// Remove stale paths that collide in shape with an incoming path (a file
	// where a new directory now lives, or vice versa) before writing: the
	// shapes cannot coexist, so the new content cannot be stored until the
	// conflicting old path is gone.
	removed := make(map[string]bool)
	removeAccessLast := false
	for _, old := range conflictingStalePaths(existing, files, keep) {
		if deferAccessChange && old == accessFileName {
			removeAccessLast = true
			continue
		}
		if err := s.sites.Remove(r.Context(), site, old); err != nil {
			log.Printf("deploy %s: %v", site, err)
			s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not remove stale file "+old)
			httpError(w, http.StatusInternalServerError, "could not remove stale file "+old)
			return
		}
		removed[old] = true
	}
	// Write every new file before removing the remaining stale files: a
	// storage failure mid-write then leaves those previous (non-conflicting)
	// files intact rather than punching holes in the live site.
	if writeAccessFirst {
		accessData := incomingAccessData(files)
		if err := s.sites.Put(r.Context(), site, accessFileName, contentTypeFor(accessFileName, accessData), accessData); err != nil {
			log.Printf("deploy %s: %v", site, err)
			s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not store "+accessFileName)
			httpError(w, http.StatusInternalServerError, "could not store "+accessFileName)
			return
		}
	}
	var deferredAccessPut deployFile
	putAccessLast := false
	for _, f := range files {
		if writeAccessFirst && f.path == accessFileName {
			continue
		}
		if deferAccessChange && f.path == accessFileName {
			deferredAccessPut = f
			putAccessLast = true
			continue
		}
		if err := s.sites.Put(r.Context(), site, f.path, contentTypeFor(f.path, f.data), f.data); err != nil {
			log.Printf("deploy %s: %v", site, err)
			s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not store "+f.path)
			httpError(w, http.StatusInternalServerError, "could not store "+f.path)
			return
		}
	}

	metadataUpdated := false
	updateMetadata := func(metadataRestricted bool) error {
		if updater, ok := s.deployAuth.(siteMetadataUpdater); ok {
			completed := s.completeSiteMetadata(r.Context(), site, files, metadata, metadataRestricted)
			if err := updater.UpdateSiteMetadata(r.Context(), site, completed); err != nil {
				return err
			}
		}
		metadataUpdated = true
		return nil
	}
	for _, old := range existing {
		if keep[old] || removed[old] {
			continue
		}
		if deferAccessChange && old == accessFileName {
			removeAccessLast = true
			continue
		}
		if err := s.sites.Remove(r.Context(), site, old); err != nil {
			log.Printf("deploy %s: %v", site, err)
			s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not remove stale file "+old)
			httpError(w, http.StatusInternalServerError, "could not remove stale file "+old)
			return
		}
	}
	rollbackMetadata := func() {}
	if updateBeforePolicyBroadening {
		if reader, ok := s.deployAuth.(siteMetadataReader); ok {
			previousMetadata, err := reader.SiteMetadata(r.Context(), site)
			if err != nil {
				log.Printf("deploy %s: read previous site metadata: %v", site, err)
				s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not read previous site metadata")
				httpError(w, http.StatusInternalServerError, "could not read previous site metadata")
				return
			}
			rollbackMetadata = func() {
				updater, ok := s.deployAuth.(siteMetadataUpdater)
				if !ok {
					return
				}
				if err := updater.UpdateSiteMetadata(r.Context(), site, previousMetadata); err != nil {
					log.Printf("deploy %s: rollback site metadata: %v", site, err)
				}
			}
		}
		if err := updateMetadata(true); err != nil {
			log.Printf("deploy %s: update site metadata: %v", site, err)
			s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not update site metadata")
			httpError(w, http.StatusInternalServerError, "could not update site metadata")
			return
		}
	}
	if putAccessLast {
		if err := s.sites.Put(r.Context(), site, deferredAccessPut.path, contentTypeFor(deferredAccessPut.path, deferredAccessPut.data), deferredAccessPut.data); err != nil {
			log.Printf("deploy %s: %v", site, err)
			rollbackMetadata()
			s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not store "+deferredAccessPut.path)
			httpError(w, http.StatusInternalServerError, "could not store "+deferredAccessPut.path)
			return
		}
	}
	if removeAccessLast {
		if err := s.sites.Remove(r.Context(), site, accessFileName); err != nil {
			log.Printf("deploy %s: %v", site, err)
			rollbackMetadata()
			s.failDeployStorage(r, site, actor, authz.Action, files, policyOnFailure, "could not remove stale file "+accessFileName)
			httpError(w, http.StatusInternalServerError, "could not remove stale file "+accessFileName)
			return
		}
	}
	if updateBeforePolicyBroadening {
		metadataUpdated = false
	}
	s.updatePolicyCacheFromDeploy(site, files)
	s.recordDeployAudit(r, DeployAuditEvent{
		Site:       site,
		Actor:      actor,
		Action:     authz.Action,
		Status:     "success",
		FileCount:  len(files),
		TotalBytes: totalDeployBytes(files),
	})
	if !metadataUpdated {
		if err := updateMetadata(restricted); err != nil {
			log.Printf("deploy %s: update site metadata: %v", site, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site":  site,
		"url":   s.siteURL(r, site),
		"files": len(files),
	})
}

func conflictingStalePaths(existing []string, files []deployFile, keep map[string]bool) []string {
	var out []string
	for _, old := range existing {
		if keep[old] {
			continue
		}
		for _, f := range files {
			if pathShapeConflict(old, f.path) {
				out = append(out, old)
				break
			}
		}
	}
	return out
}

func pathShapeConflict(old, next string) bool {
	return strings.HasPrefix(old, next+"/") || strings.HasPrefix(next, old+"/")
}

// validateDeployPolicy rejects a deploy whose _access.json cannot be
// parsed, so a malformed allowlist fails at deploy time with a clear
// message instead of silently shipping a site that fails closed to every
// visitor. Serving-time behavior stays fail-closed unchanged.
func validateDeployPolicy(site string, files []deployFile) error {
	for _, f := range files {
		if strings.HasPrefix(f.path, accessFileName+"/") {
			return fmt.Errorf("%s must be a file at the site root", accessFileName)
		}
		if f.path != accessFileName {
			continue
		}
		if _, err := parseAccessPolicy(site, f.data); err != nil {
			return fmt.Errorf("invalid %s: %w", accessFileName, err)
		}
		break
	}
	return nil
}

func deployAccessPolicy(site string, files []deployFile) (*AccessPolicy, bool, error) {
	for _, f := range files {
		if f.path != accessFileName {
			continue
		}
		policy, err := parseAccessPolicy(site, f.data)
		return policy, true, err
	}
	return nil, false, nil
}

func incomingAccessData(files []deployFile) []byte {
	for _, f := range files {
		if f.path == accessFileName {
			return f.data
		}
	}
	return nil
}

func policyRestrictsAccess(policy *AccessPolicy, hasPolicy bool, err error) bool {
	return err != nil || (hasPolicy && policy != nil && policy.RestrictsAccess())
}

func (s *Server) cacheIncomingPolicyForCreate(site string, policy *AccessPolicy, hasPolicy bool, err error) {
	if s.policies == nil || !hasPolicy {
		return
	}
	if err != nil {
		s.policies.Set(site, nil, err)
		return
	}
	s.policies.Set(site, immediatePolicyForCreate(policy), nil)
}

func immediatePolicyForCreate(policy *AccessPolicy) *AccessPolicy {
	if policy == nil {
		return nil
	}
	immediate := cloneAccessPolicy(policy)
	immediate.AI = ""
	immediate.Slack = ""
	return immediate
}

func preservePolicyOnFailure(current *AccessPolicy, currentErr error, next *AccessPolicy, hasNext bool, nextErr error) bool {
	if currentErr != nil {
		return true
	}
	if nextErr != nil {
		return false
	}
	if !hasNext {
		return current != nil && policyRemovalBroadens(current)
	}
	return policyBroadens(current, next)
}

func policyRemovalBroadens(current *AccessPolicy) bool {
	return current.RestrictsAccess() || !current.AllowsDownload() ||
		current.AllowsAIVisitors() || current.AllowsSlackVisitors()
}

func policyBroadens(current, next *AccessPolicy) bool {
	if accessBroadens(current, next) {
		return true
	}
	if next == nil {
		return false
	}
	if current == nil {
		return next.AllowsAIVisitors() || next.AllowsSlackVisitors()
	}
	return (!current.AllowsDownload() && next.AllowsDownload()) ||
		(!current.AllowsAIVisitors() && next.AllowsAIVisitors()) ||
		(!current.AllowsSlackVisitors() && next.AllowsSlackVisitors())
}

func (s *Server) updatePolicyCacheFromDeploy(site string, files []deployFile) {
	if s.policies == nil {
		return
	}
	var next *AccessPolicy
	hasAccessFile := false
	for _, f := range files {
		if f.path != accessFileName {
			continue
		}
		hasAccessFile = true
		policy, err := parseAccessPolicy(site, f.data)
		if err != nil {
			s.policies.Set(site, nil, err)
			return
		}
		next = policy
		break
	}

	current, currentErr := s.policies.For(site)
	if currentErr != nil {
		s.policies.Invalidate(site)
		return
	}
	if !hasAccessFile {
		if current == nil {
			s.policies.Set(site, nil, nil)
			return
		}
		s.policies.Invalidate(site)
		return
	}
	if immediate, ok := immediatePolicyCache(current, next); ok {
		s.policies.Set(site, immediate, nil)
		return
	}
	s.policies.Invalidate(site)
}

func immediatePolicyCache(current, next *AccessPolicy) (*AccessPolicy, bool) {
	if current == nil {
		immediate := cloneAccessPolicy(next)
		immediate.AI = ""
		immediate.Slack = ""
		return immediate, true
	}
	if accessBroadens(current, next) {
		return nil, false
	}
	immediate := cloneAccessPolicy(next)
	if !current.AllowsAIVisitors() {
		immediate.AI = ""
	}
	if !current.AllowsSlackVisitors() {
		immediate.Slack = ""
	}
	return immediate, true
}

func accessBroadens(current, next *AccessPolicy) bool {
	currentRestricts := current != nil && current.RestrictsAccess()
	nextRestricts := next != nil && next.RestrictsAccess()
	if currentRestricts && !nextRestricts {
		return true
	}
	if !currentRestricts || !nextRestricts {
		return false
	}
	return allowlistBroadens(current, next)
}

func allowlistBroadens(current, next *AccessPolicy) bool {
	if next == nil {
		return false
	}
	currentAllow := normalizedAllowSet(current)
	for _, entry := range next.Allow {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if _, ok := currentAllow[entry]; !ok {
			return true
		}
	}
	return false
}

func normalizedAllowSet(policy *AccessPolicy) map[string]struct{} {
	out := make(map[string]struct{}, len(policy.Allow))
	for _, entry := range policy.Allow {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry != "" {
			out[entry] = struct{}{}
		}
	}
	return out
}

func cloneAccessPolicy(policy *AccessPolicy) *AccessPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	clone.Allow = append([]string(nil), policy.Allow...)
	return &clone
}

// claimDeleter releases a site name claimed by a first deploy. It is an
// optional capability of the deploy authorizer, asserted at call time so
// authorizers without it (or test fakes) need not implement it.
type claimDeleter interface {
	DeleteClaim(ctx context.Context, site string) error
}

type failurePolicyCache struct {
	policy *AccessPolicy
	err    error
}

// failDeployStorage records the storage-failure audit and, when the
// failed deploy was the site's first (a "create"), releases the name it
// just claimed so the orphaned row does not lock the name forever. A
// redeploy keeps the existing site's claim untouched.
func (s *Server) failDeployStorage(r *http.Request, site string, actor Identity, action string, files []deployFile, policyOnFailure *failurePolicyCache, message string) {
	if s.policies != nil {
		if policyOnFailure != nil {
			s.policies.Set(site, policyOnFailure.policy, policyOnFailure.err)
		} else {
			s.policies.Invalidate(site)
		}
	}
	s.recordDeployFailure(r, site, actor, action, files, message)
	if action != "create" {
		return
	}
	if deleter, ok := s.deployAuth.(claimDeleter); ok {
		if err := deleter.DeleteClaim(r.Context(), site); err != nil {
			log.Printf("deploy %s: release orphaned claim: %v", site, err)
		}
	}
}

func (s *Server) recordDeployFailure(r *http.Request, site string, actor Identity, action string, files []deployFile, message string) {
	s.recordDeployAudit(r, DeployAuditEvent{
		Site:       site,
		Actor:      actor,
		Action:     action,
		Status:     "failed",
		Message:    message,
		FileCount:  len(files),
		TotalBytes: totalDeployBytes(files),
	})
}

func totalDeployBytes(files []deployFile) int64 {
	var total int64
	for _, f := range files {
		total += int64(len(f.data))
	}
	return total
}

func deployReadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		httpError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("deploy exceeds the %d MB limit", maxDeploySize>>20))
		return
	}
	httpError(w, http.StatusBadRequest, "malformed multipart upload")
}

// partFilename returns the part's filename with directory components
// intact. Part.FileName() strips them (it applies filepath.Base), but
// deploy parts deliberately encode site-relative paths in the filename.
func partFilename(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return params["filename"]
}

// contentTypeFor picks the stored content type by extension first;
// sniffing alone would mislabel CSS and JS as text/plain.
func contentTypeFor(path string, data []byte) string {
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		return ct
	}
	return http.DetectContentType(data)
}

// normalizeDeploy validates the uploaded paths and reshapes them the
// way "spot deploy" would: OS/editor junk is dropped, and any folder
// wrapping shared by every file is stripped until index.html sits at
// the site root (a browser folder pick names files "mysite/index.html",
// but visitors expect "/index.html").
func normalizeDeploy(files []deployFile) ([]deployFile, error) {
	var kept []deployFile
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		path, err := cleanSitePath(f.path)
		if err != nil {
			return nil, err
		}
		if junkPath(path) {
			continue
		}
		if seen[path] {
			return nil, fmt.Errorf("duplicate file path %q", path)
		}
		seen[path] = true
		f.path = path
		kept = append(kept, f)
	}
	if len(kept) == 0 {
		return nil, errors.New("the deploy contains no files")
	}
	for !hasRootIndex(kept) {
		root, ok := commonRoot(kept)
		if !ok {
			return nil, errors.New("the deploy needs an index.html at the site root")
		}
		for i := range kept {
			kept[i].path = kept[i].path[len(root)+1:]
		}
	}
	if err := validateDeployPathShapes(kept); err != nil {
		return nil, err
	}
	return kept, nil
}

func validateDeployPathShapes(files []deployFile) error {
	seen := make(map[string]struct{}, len(files))
	for i, f := range files {
		if _, ok := seen[f.path]; ok {
			return fmt.Errorf("duplicate file path %q", f.path)
		}
		seen[f.path] = struct{}{}
		for _, other := range files[:i] {
			if pathShapeConflict(f.path, other.path) {
				return fmt.Errorf("file path %q conflicts with %q", f.path, other.path)
			}
		}
	}
	return nil
}

func hasRootIndex(files []deployFile) bool {
	for _, f := range files {
		if f.path == "index.html" {
			return true
		}
	}
	return false
}

// commonRoot returns the first path segment when every file lives under
// it with a remainder, i.e. when one more level of folder wrapping can
// be stripped.
func commonRoot(files []deployFile) (string, bool) {
	i := strings.IndexByte(files[0].path, '/')
	if i < 0 {
		return "", false
	}
	root := files[0].path[:i]
	for _, f := range files {
		if !strings.HasPrefix(f.path, root+"/") {
			return "", false
		}
	}
	return root, true
}

// cleanSitePath validates one site-relative file path from a deploy.
// Paths become storage keys or local filesystem paths, so traversal and
// oddball segments are rejected rather than sanitized.
func cleanSitePath(raw string) (string, error) {
	raw = strings.TrimPrefix(raw, "/")
	if raw == "" {
		return "", errors.New("a file in the deploy has an empty path")
	}
	if len(raw) > 512 {
		return "", fmt.Errorf("file path too long (%d chars, max 512)", len(raw))
	}
	if strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("file path %q must use forward slashes", raw)
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("file path %q contains control characters", raw)
		}
	}
	for _, seg := range strings.Split(raw, "/") {
		switch seg {
		case "":
			return "", fmt.Errorf("file path %q has an empty segment", raw)
		case ".", "..":
			return "", fmt.Errorf("file path %q must not contain . or .. segments", raw)
		}
	}
	if !validSitePath(raw) {
		return "", fmt.Errorf("file path %q contains unsupported characters", raw)
	}
	return raw, nil
}

// junkPath reports paths nobody means to publish: editor/OS droppings
// and dependency trees. Dot segments cover .git, .DS_Store, .env and
// friends; _access.json has no leading dot and deploys normally.
func junkPath(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, ".") || seg == "node_modules" || seg == "Thumbs.db" {
			return true
		}
	}
	return false
}
