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

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// siteNameRe is a DNS label: site names become hostnames, so they are
// stricter than the CLI's [a-z0-9-]+ (no leading/trailing hyphen).
var siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const (
	maxDeploySize  = 100 << 20
	maxDeployFiles = 2000
)

// SiteStore writes deployed sites into the same S3 bucket the CLI syncs
// to; the rclone FUSE mount makes new files visible to Caddy within
// seconds. Deploys go through the server so browsers never see storage
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
// CLI's rclone sync — the uploaded set replaces the site and stale
// objects are removed.
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
	if siteFromHost(requestHost(r), s.spotDomain) != "" {
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
			if len(files) >= maxDeployFiles {
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

	existing, err := s.sites.List(r.Context(), site)
	if err != nil {
		log.Printf("deploy %s: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not read the site's current files")
		return
	}
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		if err := s.sites.Put(r.Context(), site, f.path, contentTypeFor(f.path, f.data), f.data); err != nil {
			log.Printf("deploy %s: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not store "+f.path)
			return
		}
		keep[f.path] = true
	}
	for _, old := range existing {
		if keep[old] {
			continue
		}
		if err := s.sites.Remove(r.Context(), site, old); err != nil {
			log.Printf("deploy %s: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not remove stale file "+old)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"site":  site,
		"url":   "https://" + site + "." + s.spotDomain + "/",
		"files": len(files),
	})
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

// contentTypeFor picks the stored content type by extension first:
// Caddy serves the FUSE mount by extension anyway, and sniffing alone
// would mislabel CSS and JS as text/plain.
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
	return kept, nil
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
// Paths become S3 keys and, through the FUSE mount, filesystem paths
// Caddy serves — so traversal and oddball segments are rejected rather
// than sanitized.
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
