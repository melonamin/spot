package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// previewFileNames are the gallery-thumbnail filenames a site may ship at
// its root, in the order they win. The leading underscore marks them as
// platform metadata, the same convention as _access.json.
var previewFileNames = []string{
	"_screenshot.jpg",
	"_screenshot.jpeg",
	"_screenshot.png",
	"_screenshot.webp",
}

// openSitePreview opens the site's screenshot from the mounted sites
// directory, trying each accepted name. The caller closes the file.
func (s *Server) openSitePreview(site string) (*os.File, bool) {
	if s.sitesDir == "" || !siteNameRe.MatchString(site) {
		return nil, false
	}
	for _, name := range previewFileNames {
		// OpenInRoot contains the lookup inside the sites directory, so a
		// crafted site name can never escape it — same guard PolicyStore
		// uses to read _access.json.
		file, err := os.OpenInRoot(s.sitesDir, filepath.Join(site, name))
		if err == nil {
			return file, true
		}
	}
	return nil, false
}

// hasSitePreview reports whether the site ships a screenshot.
func (s *Server) hasSitePreview(site string) bool {
	file, ok := s.openSitePreview(site)
	if ok {
		file.Close()
	}
	return ok
}

// handleSitePreview serves a site's gallery thumbnail — the optional
// _screenshot.{jpg,jpeg,png,webp} a site ships at its root. It is served
// from the apex, same origin as the gallery, so the <img> never depends
// on the site subdomain's certificate (the failure mode a live preview
// has). Only open sites get one: a restricted site's preview would leak
// its rendered content to anyone who knows the name.
func (s *Server) handleSitePreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	site := r.PathValue("name")
	if !siteNameRe.MatchString(site) {
		httpError(w, http.StatusBadRequest, "invalid site name")
		return
	}
	if restricted, _, _ := s.policySummaryForSite(r.Context(), site); restricted {
		http.NotFound(w, r)
		return
	}
	file, ok := s.openSitePreview(site)
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	// Sniff the real type rather than trust the extension: an HTML or SVG
	// file named _screenshot.jpg would run script in the apex origin if we
	// served it as anything renderable, so anything that is not a raster
	// image is refused.
	sniff := make([]byte, 512)
	n, _ := io.ReadFull(file, sniff)
	contentType := http.DetectContentType(sniff[:n])
	if !isPreviewImage(contentType) {
		http.NotFound(w, r)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		httpError(w, http.StatusInternalServerError, "could not read the preview")
		return
	}
	info, err := file.Stat()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not read the preview")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A screenshot can become private on the next redeploy if the owner
	// adds _access.json, so shared caches must not retain old previews.
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func isPreviewImage(contentType string) bool {
	media := contentType
	if i := strings.IndexByte(media, ';'); i >= 0 {
		media = media[:i]
	}
	media = strings.TrimSpace(strings.ToLower(media))
	switch media {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	}
	return false
}
