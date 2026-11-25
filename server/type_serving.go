package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// serveTypedSitePath serves the pinned viewer on a typed site's origin: the
// entry at / and /index.html, and inventory files under /_type/. It reports
// false for every other path, which is the site's own data. The caller has
// already checked lifecycle and visitor access.
func (s *Server) serveTypedSitePath(w http.ResponseWriter, r *http.Request, site string, policy *AccessPolicy, ref siteTypeRef) bool {
	var filePath string
	switch p := r.URL.Path; {
	case p == "/" || p == "/"+typeEntryFile:
		filePath = typeEntryFile
	case p == "/"+typeAssetDir || strings.HasPrefix(p, "/"+typeAssetDir+"/"):
		filePath = strings.TrimPrefix(p, "/"+typeAssetDir+"/")
		if filePath == p || filePath == "" || !typeServedPath(filePath) || validateContractPath(filePath) != nil {
			s.serveEmbedded404(w, r)
			return true
		}
	default:
		return false
	}
	s.serveTypeFile(w, r, site, policy, ref, filePath)
	return true
}

func (s *Server) serveTypeFile(w http.ResponseWriter, r *http.Request, site string, policy *AccessPolicy, ref siteTypeRef, filePath string) {
	v, err := s.cachedTypeVersion(r.Context(), ref.Name, ref.Version)
	if err != nil {
		log.Printf("static: type %s: %v", ref, err)
		httpError(w, http.StatusServiceUnavailable, "could not read the site's viewer")
		return
	}
	// Only recorded inventory paths are served, so objects left by an
	// interrupted upload are unreachable.
	file, ok := v.file(filePath)
	if !ok {
		s.serveEmbedded404(w, r)
		return
	}
	etag := `"` + v.Version + "-" + file.SHA256 + `"`
	// An HTML viewer page carries the access bar, so its hash covers the
	// inventory file and the bar, still known before storage is opened.
	ctype := contentTypeForName(filePath)
	var markup string
	if isHTMLType(ctype) {
		markup, err = s.accessBarMarkupFor(r.Context(), site, policy)
		if err != nil {
			log.Printf("static: type %s: access bar: %v", ref, err)
			httpError(w, http.StatusServiceUnavailable, "could not describe the site's access")
			return
		}
		etag = accessBarETag(v.Version+"-", file.SHA256, markup)
	}
	h := w.Header()
	// Overrides the no-store authorizeSiteAccess sets for restricted sites:
	// viewer bytes are not secret, and every use revalidates after access
	// checks, so a revoked visitor never gets a 304.
	h.Set("Cache-Control", "private, no-cache")
	h.Set("ETag", etag)
	h.Del("Last-Modified")
	if notModifiedShortcut(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	store, ok := s.sites.(TypeFileStorage)
	if !ok {
		httpError(w, http.StatusServiceUnavailable, "site store not configured for site types")
		return
	}
	rc, err := store.OpenTypeFile(r.Context(), v.Name, v.Version, filePath)
	if errors.Is(err, ErrNotFound) {
		log.Printf("static: type %s: inventory file %s is missing from storage", ref, filePath)
		httpError(w, http.StatusInternalServerError, "the site's viewer is incomplete")
		return
	}
	if err != nil {
		log.Printf("static: type %s/%s: %v", ref, filePath, err)
		httpError(w, http.StatusInternalServerError, "could not read the site's viewer")
		return
	}
	defer rc.Close()
	if markup != "" {
		writeHTMLWithAccessBar(w, r, filePath, ctype, markup, rc)
		return
	}
	h.Set("Content-Type", ctype)
	if seeker, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, filePath, time.Time{}, seeker)
		return
	}
	h.Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc)
	}
}

// ifNoneMatchMatches applies If-None-Match's weak comparison to one strong
// entity tag. Tags are read as quoted strings, so a comma or "*" inside a
// quoted tag is not a separator or a wildcard.
func ifNoneMatchMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	for header != "" {
		header = strings.TrimLeft(header, " \t,")
		candidate := strings.TrimPrefix(header, "W/")
		if !strings.HasPrefix(candidate, `"`) {
			return false
		}
		end := strings.IndexByte(candidate[1:], '"')
		if end < 0 {
			return false
		}
		if candidate[:end+2] == etag {
			return true
		}
		header = candidate[end+2:]
	}
	return false
}
