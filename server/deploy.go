package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
	var principal DeployPrincipal
	if len(r.Header.Values("Authorization")) > 0 {
		var ok bool
		principal, ok = s.requireDeployPrincipal(w, r)
		if !ok {
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDeploySize)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, http.StatusBadRequest, "request must be multipart/form-data")
		return
	}

	var site string
	var files []deployFile
	preserveAccess := false
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
		case "preserve_access":
			raw, err := io.ReadAll(io.LimitReader(part, 32))
			if err != nil {
				deployReadError(w, err)
				return
			}
			preserveAccess = parseDeployBool(string(raw))
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
	if principal.PublishingKey && (!strings.HasPrefix(site, principal.RequiredPrefix) || len(site) == len(principal.RequiredPrefix)) {
		log.Printf("publishing key rejected: id=%s site=%s", principal.PublisherKeyID, site)
		httpError(w, http.StatusForbidden, "publishing key is not authorized for this site name")
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
	if actorKey(principal.Actor) == "" {
		var ok bool
		principal, ok = s.requireDeployPrincipal(w, r)
		if !ok {
			return
		}
	}
	r = withDeployPrincipal(r, principal)
	actor := principal.Actor
	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	defer siteLock.Unlock()
	if err := s.reconcilePolicyTransition(r.Context(), site, principal); err != nil && !errors.Is(err, ErrSiteNotFound) {
		log.Printf("deploy %s: reconcile policy transition: %v", site, err)
		httpError(w, http.StatusServiceUnavailable, "the site's access policy needs owner or admin recovery")
		return
	}

	var authz DeployAuthorization
	if principal.PublishingKey {
		authorizer, ok := s.deployAuth.(interface {
			AuthorizePublishingKeyDeploy(context.Context, string, Identity, string) (DeployAuthorization, error)
		})
		if !ok {
			httpError(w, http.StatusServiceUnavailable, "publishing-key deploy authorization is not configured")
			return
		}
		authz, err = authorizer.AuthorizePublishingKeyDeploy(r.Context(), site, actor, principal.PublisherKeyID)
	} else {
		authz, err = s.deployAuth.AuthorizeDeploy(r.Context(), site, actor)
	}
	if errors.Is(err, errPublishingKeyInvalid) {
		httpError(w, http.StatusUnauthorized, "invalid or revoked publishing key")
		return
	}
	if errors.Is(err, ErrDeployForbidden) {
		s.recordDeployAudit(r, DeployAuditEvent{
			Site:       site,
			Actor:      actor,
			Action:     "deploy",
			Status:     "denied",
			Message:    "actor is not the site owner, a maintainer, or a platform admin",
			FileCount:  len(files),
			TotalBytes: totalDeployBytes(files),
		})
		httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can deploy this site")
		return
	}
	if err != nil {
		log.Printf("deploy %s: authorize: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize deploy")
		return
	}
	cancelAuthorization := func() {
		canceler, ok := s.deployAuth.(deployAuthorizationCanceler)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := canceler.CancelDeployAuthorization(ctx, site, authz); err != nil {
			log.Printf("deploy %s: cancel authorization: %v", site, err)
		}
	}
	if preserveAccess && authz.Action == "update" && authz.PreviousState == SiteStateActive {
		files, err = s.preserveExistingAccessPolicy(r.Context(), site, files)
		if err != nil {
			cancelAuthorization()
			log.Printf("deploy %s: preserve access: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not preserve existing "+accessFileName)
			return
		}
		if len(files) > maxDeployFiles {
			cancelAuthorization()
			httpError(w, http.StatusBadRequest,
				fmt.Sprintf("too many files in the deploy (max %d)", maxDeployFiles))
			return
		}
		if err := validateDeployPolicy(site, files); err != nil {
			cancelAuthorization()
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		incomingPolicy, hasIncomingPolicy, incomingPolicyErr = deployAccessPolicy(site, files)
		restricted = policyRestrictsAccess(incomingPolicy, hasIncomingPolicy, incomingPolicyErr)
	}
	var policyOnFailure *failurePolicyCache
	var previousPolicy *AccessPolicy
	var previousPolicyErr error
	if authz.Action == "create" || authz.Action == "recreate" {
		s.cacheIncomingPolicyForCreate(site, incomingPolicy, hasIncomingPolicy, incomingPolicyErr)
		if hasIncomingPolicy {
			policyOnFailure = &failurePolicyCache{
				policy: immediatePolicyForCreate(incomingPolicy),
				err:    incomingPolicyErr,
			}
		}
	} else {
		previousPolicy, previousPolicyErr = s.policyForSite(r.Context(), site)
		if preservePolicyOnFailure(previousPolicy, previousPolicyErr, incomingPolicy, hasIncomingPolicy, incomingPolicyErr) {
			policyOnFailure = &failurePolicyCache{policy: previousPolicy, err: previousPolicyErr}
		}
	}
	writeAccessFirst := authz.Action == "create" && restricted && s.policies == nil && hasIncomingPolicy
	stageRestrictiveUpdate := authz.Action == "update" && previousPolicyErr == nil &&
		policyNarrowsAccess(previousPolicy, incomingPolicy, hasIncomingPolicy)
	deferAccessChange := (authz.Action != "update" || policyOnFailure != nil || stageRestrictiveUpdate) && !writeAccessFirst
	updateBeforePolicyBroadening := deferAccessChange && !restricted
	if stageRestrictiveUpdate {
		staging := restrictiveStagingPolicy(previousPolicy)
		data, err := marshalRestrictiveStagingPolicy(staging)
		if err != nil {
			cancelAuthorization()
			httpError(w, http.StatusInternalServerError, "could not prepare fail-closed policy")
			return
		}
		if err := s.commitPolicyObject(r.Context(), site, authz.ContentGeneration, data, false); err != nil {
			cancelAuthorization()
			s.recordDeployFailureAs(r, site, actor, authz.Action, authz.AuthorizedAs, files, "could not store fail-closed policy")
			httpError(w, http.StatusInternalServerError, "could not store fail-closed policy")
			return
		}
		if s.policies != nil {
			s.policies.Set(site, staging, nil)
		}
		policyOnFailure = &failurePolicyCache{policy: staging}
		s.disconnectSiteRealtime(site)
	}

	existing, err := s.sites.List(r.Context(), site)
	if err != nil {
		log.Printf("deploy %s: %v", site, err)
		cancelAuthorization()
		if authz.Action == "create" && s.policies != nil {
			s.policies.Invalidate(site)
		}
		// Listing is read-only, so record the operational failure without
		// classifying the site's content as mutation-ambiguous.
		s.recordDeployFailureAs(r, site, actor, "deploy", authz.AuthorizedAs, files, "could not read current files")
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
			s.failDeployStorage(r, site, actor, authz, files, policyOnFailure, "could not remove stale file "+old)
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
		if err := s.commitPolicyObject(r.Context(), site, authz.ContentGeneration, accessData, false); err != nil {
			log.Printf("deploy %s: %v", site, err)
			s.failPolicyCommit(r, site, actor, authz, files, policyOnFailure, err, "could not store "+accessFileName)
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
			s.failDeployStorage(r, site, actor, authz, files, policyOnFailure, "could not store "+f.path)
			httpError(w, http.StatusInternalServerError, "could not store "+f.path)
			return
		}
	}

	var existingSiteTags []string
	if reader, ok := s.deployAuth.(siteMetadataReader); ok {
		if prev, err := reader.SiteMetadata(r.Context(), site); err == nil {
			existingSiteTags = prev.Tags
		}
	}
	metadataUpdated := false
	updateMetadata := func() error {
		resolved := resolveSiteMetadata(metadata, existingSiteTags)
		if updater, ok := s.deployAuth.(deploySiteMetadataUpdater); ok {
			if err := updater.UpdateDeploySiteMetadata(r.Context(), site, resolved); err != nil {
				return err
			}
		} else if updater, ok := s.deployAuth.(siteMetadataUpdater); ok {
			if err := updater.UpdateSiteMetadata(r.Context(), site, resolved); err != nil {
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
			s.failDeployStorage(r, site, actor, authz, files, policyOnFailure, "could not remove stale file "+old)
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
				s.failDeployStorage(r, site, actor, authz, files, policyOnFailure, "could not read previous site metadata")
				httpError(w, http.StatusInternalServerError, "could not read previous site metadata")
				return
			}
			rollbackMetadata = func() {
				if updater, ok := s.deployAuth.(deploySiteMetadataUpdater); ok {
					if err := updater.UpdateDeploySiteMetadata(r.Context(), site, previousMetadata); err != nil {
						log.Printf("deploy %s: rollback site metadata: %v", site, err)
					}
					return
				}
				updater, ok := s.deployAuth.(siteMetadataUpdater)
				if !ok {
					return
				}
				if err := updater.UpdateSiteMetadata(r.Context(), site, previousMetadata); err != nil {
					log.Printf("deploy %s: rollback site metadata: %v", site, err)
				}
			}
		}
		if err := updateMetadata(); err != nil {
			log.Printf("deploy %s: update site metadata: %v", site, err)
			s.failDeployStorage(r, site, actor, authz, files, policyOnFailure, "could not update site metadata")
			httpError(w, http.StatusInternalServerError, "could not update site metadata")
			return
		}
	}
	if putAccessLast {
		if err := s.commitPolicyObject(r.Context(), site, authz.ContentGeneration, deferredAccessPut.data, false); err != nil {
			log.Printf("deploy %s: %v", site, err)
			rollbackMetadata()
			s.failPolicyCommit(r, site, actor, authz, files, policyOnFailure, err, "could not store "+deferredAccessPut.path)
			httpError(w, http.StatusInternalServerError, "could not store "+deferredAccessPut.path)
			return
		}
	}
	if removeAccessLast {
		if err := s.commitPolicyObject(r.Context(), site, authz.ContentGeneration, nil, true); err != nil {
			log.Printf("deploy %s: %v", site, err)
			rollbackMetadata()
			s.failPolicyCommit(r, site, actor, authz, files, policyOnFailure, err, "could not remove stale file "+accessFileName)
			httpError(w, http.StatusInternalServerError, "could not remove stale file "+accessFileName)
			return
		}
	}
	if updateBeforePolicyBroadening {
		metadataUpdated = false
	}
	s.updatePolicyCacheFromDeploy(site, files)
	if completer, ok := s.deployAuth.(deployCompleter); ok {
		if err := completer.CompleteDeploy(r.Context(), site, authz); err != nil {
			log.Printf("deploy %s: activate: %v", site, err)
			if authz.Action == "recreate" {
				if canceler, ok := s.deployAuth.(deployAuthorizationCanceler); ok {
					if cancelErr := canceler.CancelDeployAuthorization(r.Context(), site, authz); cancelErr != nil {
						log.Printf("deploy %s: restore deleted tombstone after activation failure: %v", site, cancelErr)
					}
				}
			}
			s.recordDeployFailureAs(r, site, actor, authz.Action, authz.AuthorizedAs, files, "could not activate deployed site")
			httpError(w, http.StatusInternalServerError, "could not activate deployed site")
			return
		}
	}
	s.recordDeployAudit(r, DeployAuditEvent{
		Site:              site,
		Actor:             actor,
		Action:            authz.Action,
		Status:            "success",
		FileCount:         len(files),
		TotalBytes:        totalDeployBytes(files),
		ContentHash:       cloudflareContentHashForDeploy(files),
		AuthorizedAs:      authz.AuthorizedAs,
		ContentGeneration: authz.ContentGeneration,
	})
	if principal.PublishingKey && s.publishingKeys != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := s.publishingKeys.TouchUsed(ctx, principal.PublisherKeyID); err != nil {
			log.Printf("publishing key %s: update last used: %v", principal.PublisherKeyID, err)
		}
		cancel()
	}
	if !metadataUpdated {
		if err := updateMetadata(); err != nil {
			log.Printf("deploy %s: update site metadata: %v", site, err)
		}
	}
	if s.shouldAutoTag(metadata, existingSiteTags, restricted) {
		s.scheduleAutoTag(site, files, resolveSiteMetadata(metadata, existingSiteTags))
	}
	version := time.Now().UTC().Format("20060102T150405.000000000Z")
	url := s.siteURL(r, site)
	s.publishDeployEvent(site, version, url)
	writeJSON(w, http.StatusOK, map[string]any{
		"site":    site,
		"url":     url,
		"files":   len(files),
		"version": version,
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

func parseDeployBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (s *Server) preserveExistingAccessPolicy(ctx context.Context, site string, files []deployFile) ([]deployFile, error) {
	for _, f := range files {
		if f.path == accessFileName {
			return files, nil
		}
	}
	rc, _, err := s.sites.Open(ctx, site, accessFileName)
	if errors.Is(err, ErrNotFound) {
		return files, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return append(files, deployFile{path: accessFileName, data: data}), nil
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
		return current != nil && (policyRemovalBroadens(current) || len(current.Maintainers) > 0)
	}
	return policyBroadens(current, next) || maintainersChanged(current, next)
}

func maintainersChanged(current, next *AccessPolicy) bool {
	var currentEntries, nextEntries []string
	if current != nil {
		currentEntries = current.Maintainers
	}
	if next != nil {
		nextEntries = next.Maintainers
	}
	if len(currentEntries) != len(nextEntries) {
		return true
	}
	for i := range currentEntries {
		if !strings.EqualFold(strings.TrimSpace(currentEntries[i]), strings.TrimSpace(nextEntries[i])) {
			return true
		}
	}
	return false
}

func policyNarrowsAccess(current, next *AccessPolicy, hasNext bool) bool {
	if !hasNext || next == nil {
		return current != nil && (current.AllowsAIVisitors() || current.AllowsSlackVisitors())
	}
	if next.RestrictsAccess() && (current == nil || !current.RestrictsAccess()) {
		return true
	}
	if current != nil && current.RestrictsAccess() && next.RestrictsAccess() {
		nextAllow := normalizedAllowSet(next)
		for entry := range normalizedAllowSet(current) {
			if _, ok := nextAllow[entry]; !ok {
				return true
			}
		}
	}
	return (current == nil || current.AllowsDownload()) && !next.AllowsDownload() ||
		(current != nil && current.AllowsAIVisitors() && !next.AllowsAIVisitors()) ||
		(current != nil && current.AllowsSlackVisitors() && !next.AllowsSlackVisitors())
}

func restrictiveStagingPolicy(current *AccessPolicy) *AccessPolicy {
	download := false
	staging := &AccessPolicy{
		Allow: []string{}, AI: aiAccessOwners, Slack: slackAccessOwners, Download: &download,
		restrictAccess: true,
	}
	if current != nil {
		staging.Maintainers = append([]string(nil), current.Maintainers...)
	}
	return staging
}

func marshalRestrictiveStagingPolicy(policy *AccessPolicy) ([]byte, error) {
	return json.Marshal(struct {
		Allow       []string `json:"allow"`
		Maintainers []string `json:"maintainers,omitempty"`
		AI          string   `json:"ai"`
		Slack       string   `json:"slack"`
		Download    bool     `json:"download"`
	}{
		Allow: []string{}, Maintainers: append([]string(nil), policy.Maintainers...),
		AI: aiAccessOwners, Slack: slackAccessOwners, Download: false,
	})
}

func (s *Server) disconnectSiteRealtime(site string) {
	if s.hub != nil {
		s.hub.DisconnectScope(site)
	}
	if s.roomHub != nil {
		s.roomHub.DisconnectScope(site)
	}
}

const absentPolicyHash = "absent"

var errPolicyTransitionUnresolved = errors.New("stored policy transition outcome is unresolved")

type policyTransitionRegistry interface {
	BeginPolicyTransition(ctx context.Context, site string, generation int64, previousHash, nextHash string) error
	ClearPolicyTransition(ctx context.Context, site string, generation int64) error
}

func policyObjectHash(data []byte, absent bool) string {
	if absent {
		return absentPolicyHash
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func (s *Server) storedPolicyBytes(ctx context.Context, site string) ([]byte, bool, error) {
	rc, _, err := s.sites.Open(ctx, site, accessFileName)
	if err != nil {
		if siteObjectNotFound(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	return data, false, err
}

func (s *Server) commitPolicyObject(ctx context.Context, site string, generation int64, next []byte, remove bool) error {
	previous, previousAbsent, err := s.storedPolicyBytes(ctx, site)
	if err != nil {
		return err
	}
	previousHash := policyObjectHash(previous, previousAbsent)
	nextHash := policyObjectHash(next, remove)
	registry, fenced := s.deployAuth.(policyTransitionRegistry)
	if fenced {
		if err := registry.BeginPolicyTransition(ctx, site, generation, previousHash, nextHash); err != nil {
			return err
		}
	}
	if remove {
		err = s.sites.Remove(ctx, site, accessFileName)
		if siteObjectNotFound(err) {
			err = nil
		}
	} else {
		err = s.sites.Put(ctx, site, accessFileName, contentTypeFor(accessFileName, next), next)
	}
	if err != nil {
		stored, absent, readErr := s.storedPolicyBytes(ctx, site)
		if readErr != nil {
			if s.policies != nil {
				s.policies.Set(site, nil, errPolicyTransitionUnresolved)
			}
			return fmt.Errorf("%w: %v (read-back: %v)", errPolicyTransitionUnresolved, err, readErr)
		}
		storedHash := policyObjectHash(stored, absent)
		switch storedHash {
		case nextHash:
			err = nil
		case previousHash:
			if fenced {
				if clearErr := registry.ClearPolicyTransition(ctx, site, generation); clearErr != nil {
					return clearErr
				}
			}
			return err
		default:
			if s.policies != nil {
				s.policies.Set(site, nil, errPolicyTransitionUnresolved)
			}
			return fmt.Errorf("%w: stored digest %s", errPolicyTransitionUnresolved, storedHash)
		}
	}
	if s.policies != nil {
		if remove {
			s.policies.Set(site, nil, nil)
		} else if policy, parseErr := parseAccessPolicy(site, next); parseErr != nil {
			s.policies.Set(site, nil, parseErr)
		} else {
			s.policies.Set(site, policy, nil)
		}
	}
	if fenced {
		if err := registry.ClearPolicyTransition(ctx, site, generation); err != nil {
			if s.policies != nil {
				s.policies.Set(site, nil, errPolicyTransitionUnresolved)
			}
			return fmt.Errorf("%w: clear policy transition: %v", errPolicyTransitionUnresolved, err)
		}
	}
	return nil
}

type pendingPolicyTransitionRegistry interface {
	PendingPolicyTransition(ctx context.Context, site string) (PolicyTransition, error)
	ClearPolicyTransition(ctx context.Context, site string, generation int64) error
	ManagementDecision(ctx context.Context, site string, actor Identity) (ManagementDecision, error)
}

type publishingKeyPolicyTransitionRegistry interface {
	ClearPolicyTransitionForPublishingKey(ctx context.Context, site string, generation int64, actor Identity, keyID string) error
}

func (s *Server) reconcilePolicyTransition(ctx context.Context, site string, principal DeployPrincipal) error {
	registry, ok := s.deployAuth.(pendingPolicyTransitionRegistry)
	if !ok {
		return nil
	}
	transition, err := registry.PendingPolicyTransition(ctx, site)
	if err != nil || transition.Generation == 0 {
		return err
	}
	if !principal.PublishingKey {
		decision, err := registry.ManagementDecision(ctx, site, principal.Actor)
		if err != nil {
			return err
		}
		if decision.Role != ManagementRoleOwner && decision.Role != ManagementRoleAdmin {
			return errPolicyTransitionUnresolved
		}
	}
	stored, absent, err := s.storedPolicyBytes(ctx, site)
	if err != nil {
		return fmt.Errorf("%w: %v", errPolicyTransitionUnresolved, err)
	}
	hash := policyObjectHash(stored, absent)
	if hash != transition.PreviousHash && hash != transition.NextHash {
		return fmt.Errorf("%w: stored digest %s", errPolicyTransitionUnresolved, hash)
	}
	if principal.PublishingKey {
		keyRegistry, ok := s.deployAuth.(publishingKeyPolicyTransitionRegistry)
		if !ok {
			return errPolicyTransitionUnresolved
		}
		err = keyRegistry.ClearPolicyTransitionForPublishingKey(
			ctx, site, transition.Generation, principal.Actor, principal.PublisherKeyID)
	} else {
		err = registry.ClearPolicyTransition(ctx, site, transition.Generation)
	}
	if err != nil {
		if s.policies != nil {
			s.policies.Set(site, nil, errPolicyTransitionUnresolved)
		}
		return err
	}
	if s.policies != nil {
		if absent {
			s.policies.Set(site, nil, nil)
		} else if policy, parseErr := parseAccessPolicy(site, stored); parseErr != nil {
			s.policies.Set(site, nil, parseErr)
		} else {
			s.policies.Set(site, policy, nil)
		}
	}
	return nil
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
	clone.Maintainers = append([]string(nil), policy.Maintainers...)
	if policy.Download != nil {
		download := *policy.Download
		clone.Download = &download
	}
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
func (s *Server) failDeployStorage(r *http.Request, site string, actor Identity, authz DeployAuthorization, files []deployFile, policyOnFailure *failurePolicyCache, message string) {
	if s.policies != nil {
		if policyOnFailure != nil {
			s.policies.Set(site, policyOnFailure.policy, policyOnFailure.err)
		} else {
			s.policies.Invalidate(site)
		}
	}
	s.recordDeployFailureAs(r, site, actor, authz.Action, authz.AuthorizedAs, files, message)
	if authz.Action != "update" {
		canceler, ok := s.deployAuth.(deployAuthorizationCanceler)
		if !ok {
			return
		}
		if err := canceler.CancelDeployAuthorization(r.Context(), site, authz); err != nil {
			log.Printf("deploy %s: cancel failed storage authorization: %v", site, err)
		}
	}
}

func (s *Server) failPolicyCommit(r *http.Request, site string, actor Identity, authz DeployAuthorization, files []deployFile, policyOnFailure *failurePolicyCache, cause error, message string) {
	if errors.Is(cause, errPolicyTransitionUnresolved) {
		s.recordDeployFailureAs(r, site, actor, authz.Action, authz.AuthorizedAs, files, message)
		return
	}
	s.failDeployStorage(r, site, actor, authz, files, policyOnFailure, message)
}

func (s *Server) recordDeployFailure(r *http.Request, site string, actor Identity, action string, files []deployFile, message string) {
	s.recordDeployFailureAs(r, site, actor, action, "", files, message)
}

func (s *Server) recordDeployFailureAs(r *http.Request, site string, actor Identity, action string, role ManagementRole, files []deployFile, message string) {
	s.recordDeployAudit(r, DeployAuditEvent{
		Site:         site,
		Actor:        actor,
		Action:       action,
		Status:       "failed",
		Message:      message,
		FileCount:    len(files),
		TotalBytes:   totalDeployBytes(files),
		AuthorizedAs: role,
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
