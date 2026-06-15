package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
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

type sitePreview struct {
	body        io.ReadCloser
	seeker      io.ReadSeeker
	contentType string
	modTime     time.Time
}

type readerCloser struct {
	io.Reader
	io.Closer
}

// openSitePreview opens the site's screenshot through the configured
// site store, trying each accepted name.
func (s *Server) openSitePreview(ctx context.Context, site string) (sitePreview, bool) {
	if s.sites == nil || !siteNameRe.MatchString(site) {
		return sitePreview{}, false
	}
	for _, name := range previewFileNames {
		rc, info, err := s.sites.Open(ctx, site, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) || siteObjectNotFound(err) {
				continue
			}
			return sitePreview{}, false
		}
		sniff := make([]byte, 512)
		n, err := io.ReadFull(rc, sniff)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			rc.Close()
			return sitePreview{}, false
		}
		contentType := http.DetectContentType(sniff[:n])
		if seeker, ok := rc.(interface {
			io.ReadCloser
			io.Seeker
		}); ok {
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				rc.Close()
				return sitePreview{}, false
			}
			return sitePreview{body: rc, seeker: seeker, contentType: contentType, modTime: info.LastModified}, true
		}
		body := readerCloser{
			Reader: io.MultiReader(bytes.NewReader(sniff[:n]), rc),
			Closer: rc,
		}
		return sitePreview{body: body, contentType: contentType, modTime: info.LastModified}, true
	}
	return sitePreview{}, false
}

// hasSitePreview reports whether the site ships a screenshot.
func (s *Server) hasSitePreview(ctx context.Context, site string) bool {
	if s.sites == nil || !siteNameRe.MatchString(site) {
		return false
	}
	for _, name := range previewFileNames {
		rc, _, err := s.sites.Open(ctx, site, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) || siteObjectNotFound(err) {
				continue
			}
			return false
		}
		sniff := make([]byte, 512)
		n, readErr := io.ReadFull(rc, sniff)
		closeErr := rc.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return false
		}
		if closeErr != nil {
			return false
		}
		if isPreviewImage(http.DetectContentType(sniff[:n])) {
			return true
		}
	}
	return false
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
	preview, ok := s.openSitePreview(r.Context(), site)
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer preview.body.Close()

	// Sniff the real type rather than trust the extension: an HTML or SVG
	// file named _screenshot.jpg would run script in the apex origin if we
	// served it as anything renderable, so anything that is not a raster
	// image is refused.
	if !isPreviewImage(preview.contentType) {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", preview.contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A screenshot can become private on the next redeploy if the owner
	// adds _access.json, so shared caches must not retain old previews.
	w.Header().Set("Cache-Control", "no-store")
	if preview.seeker != nil {
		http.ServeContent(w, r, "", preview.modTime, preview.seeker)
		return
	}
	if _, err := io.Copy(w, preview.body); err != nil {
		return
	}
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
