package main

import (
	"embed"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:generate sh -c "rm -rf static_assets/sdk && mkdir -p static_assets/sdk && cp ../sdk/index.html ../sdk/spots.html ../sdk/404.html ../sdk/spot.js ../sdk/spot.d.ts ../sdk/install.sh ../sdk/agent.md ../sdk/spot static_assets/sdk/"
//go:embed static_assets/sdk/*
var staticAssets embed.FS

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		s.handleApexStatic(w, r)
		return
	}
	s.handleSiteStatic(w, r, site)
}

func (s *Server) handleApexStatic(w http.ResponseWriter, r *http.Request) {
	name := "index.html"
	switch strings.TrimRight(r.URL.Path, "/") {
	case "":
		name = "index.html"
	case "/spots", "/gallery":
		name = "spots.html"
	default:
		name = strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	}
	if name == "." || name == "" {
		name = "index.html"
	}
	s.serveEmbeddedAsset(w, r, name)
}

func (s *Server) handleSiteStatic(w http.ResponseWriter, r *http.Request, site string) {
	if !s.authorizeSiteAccess(w, r, site) {
		return
	}
	if r.URL.Path == "/spot.js" || r.URL.Path == "/spot.d.ts" {
		s.serveEmbeddedAsset(w, r, strings.TrimPrefix(r.URL.Path, "/"))
		return
	}
	if s.sites == nil {
		httpError(w, http.StatusServiceUnavailable, "site store not configured")
		return
	}
	requestPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	var indexPath string
	if requestPath == "." || requestPath == "" {
		requestPath = "index.html"
	} else if strings.HasSuffix(r.URL.Path, "/") {
		requestPath = path.Join(requestPath, "index.html")
	} else {
		indexPath = path.Join(requestPath, "index.html")
	}
	if !validSitePath(requestPath) {
		s.serveEmbedded404(w, r)
		return
	}
	rc, info, err := s.sites.Open(r.Context(), site, requestPath)
	if errors.Is(err, ErrNotFound) {
		if indexPath != "" && validSitePath(indexPath) {
			rc, info, err = s.sites.Open(r.Context(), site, indexPath)
			if err == nil {
				rc.Close()
				redirectToDir(w, r, requestPath)
				return
			}
		}
	}
	if errors.Is(err, ErrNotFound) {
		s.serveEmbedded404(w, r)
		return
	}
	if err != nil {
		log.Printf("static %s/%s: %v", site, requestPath, err)
		httpError(w, http.StatusInternalServerError, "could not read site file")
		return
	}
	defer rc.Close()
	if seeker, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, requestPath, info.LastModified, seeker)
		return
	}
	w.Header().Set("Content-Type", contentTypeForName(requestPath))
	if _, err := io.Copy(w, rc); err != nil {
		return
	}
}

func redirectToDir(w http.ResponseWriter, r *http.Request, cleanPath string) {
	// Build the Location from the cleaned request path so it can never carry
	// a leading "//" (protocol-relative open redirect) or other uncleaned
	// segments from r.URL.Path.
	target := "/" + cleanPath + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func (s *Server) serveEmbeddedAsset(w http.ResponseWriter, r *http.Request, name string) {
	clean := path.Clean(strings.TrimPrefix(name, "/"))
	if clean == "." || strings.HasPrefix(clean, "../") {
		s.serveEmbedded404(w, r)
		return
	}
	file, err := staticAssets.Open("static_assets/sdk/" + clean)
	if errors.Is(err, fs.ErrNotExist) {
		s.serveEmbedded404(w, r)
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not read platform asset")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not read platform asset")
		return
	}
	seeker, ok := file.(io.ReadSeeker)
	if !ok {
		httpError(w, http.StatusInternalServerError, "platform asset is not seekable")
		return
	}
	http.ServeContent(w, r, clean, info.ModTime(), seeker)
}

func (s *Server) serveEmbedded404(w http.ResponseWriter, r *http.Request) {
	file, err := staticAssets.Open("static_assets/sdk/404.html")
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.Copy(w, file)
}

func contentTypeForName(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
