package main

import (
	"embed"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"time"
)

// Regenerate with `just default-icons` after editing the SVG sources.
//
//go:embed default_icons/favicon.ico default_icons/apple-touch-icon.png
var embeddedDefaultIcons embed.FS

// defaultIconFiles lists the root paths browsers and iOS request on their own
// when a page declares no icon, so no site HTML has to change.
var defaultIconFiles = map[string]struct{ name, contentType string }{
	"/favicon.ico":                      {"favicon.ico", "image/x-icon"},
	"/apple-touch-icon.png":             {"apple-touch-icon.png", "image/png"},
	"/apple-touch-icon-precomposed.png": {"apple-touch-icon.png", "image/png"},
}

// serveDefaultIcon answers a not-found icon path with the default icon and
// reports whether it did. SPOT_DEFAULT_ICONS_DIR replaces the built-in set
// entirely, so a white-labeled deployment never shows the built-in icon.
func (s *Server) serveDefaultIcon(w http.ResponseWriter, r *http.Request) bool {
	icon, ok := defaultIconFiles[r.URL.Path]
	if !ok {
		return false
	}
	var file fs.File
	var err error
	if s.defaultIconsDir != "" {
		file, err = os.DirFS(s.defaultIconsDir).Open(icon.name)
	} else {
		file, err = embeddedDefaultIcons.Open(path.Join("default_icons", icon.name))
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		log.Printf("default icon %s: %v", icon.name, err)
		httpError(w, http.StatusInternalServerError, "could not read default icon")
		return true
	}
	defer file.Close()
	seeker, ok := file.(io.ReadSeeker)
	if !ok {
		httpError(w, http.StatusInternalServerError, "default icon is not seekable")
		return true
	}
	h := w.Header()
	h.Set("Content-Type", icon.contentType)
	// Revalidate so an icon a site deploys later replaces the default at once.
	// Restricted sites have already set no-store.
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache")
	}
	// This URL may previously have served a site's own icon. Its timestamp
	// cannot validate this fallback, which may be older or keep its mtime
	// when replaced by the operator.
	http.ServeContent(w, r, icon.name, time.Time{}, seeker)
	return true
}
