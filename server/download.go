package main

import (
	"archive/zip"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
)

func (s *Server) handleSiteDownload(w http.ResponseWriter, r *http.Request) {
	if s.sites == nil {
		httpError(w, http.StatusServiceUnavailable,
			"site store not configured: set SPOT_S3_ENDPOINT and credentials")
		return
	}
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest,
			"site downloads are served on site subdomains, not on the platform root")
		return
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return
	}
	policy, err := s.policyForSite(r.Context(), site)
	if err != nil {
		log.Printf("download policy %s: %v", site, err)
		httpError(w, http.StatusServiceUnavailable,
			"this site's "+accessFileName+" is unreadable; download denied until it is fixed")
		return
	}
	if !policy.AllowsDownload() {
		httpError(w, http.StatusForbidden,
			"this site has disabled source downloads in "+accessFileName)
		return
	}

	paths, err := s.sites.List(r.Context(), site)
	if err != nil {
		log.Printf("download %s: list: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not list site files")
		return
	}
	paths = cleanDownloadPaths(paths)
	if len(paths) == 0 {
		httpError(w, http.StatusNotFound, "no site named "+site)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": site + ".zip",
	}))
	zw := zip.NewWriter(w)
	defer zw.Close()

	for _, name := range paths {
		rc, info, err := s.sites.Open(r.Context(), site, name)
		if err != nil {
			log.Printf("download %s: open %s: %v", site, name, err)
			return
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o644)
		if !info.LastModified.IsZero() {
			h.SetModTime(info.LastModified)
		}
		dst, err := zw.CreateHeader(h)
		if err != nil {
			rc.Close()
			log.Printf("download %s: zip header %s: %v", site, name, err)
			return
		}
		if _, err := io.Copy(dst, rc); err != nil {
			rc.Close()
			log.Printf("download %s: copy %s: %v", site, name, err)
			return
		}
		rc.Close()
	}
}

func cleanDownloadPaths(paths []string) []string {
	out := paths[:0]
	for _, p := range paths {
		if validDownloadPath(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func validDownloadPath(p string) bool {
	if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.ContainsAny(p, `\:`) {
		return false
	}
	clean := path.Clean(p)
	return clean == p && clean != ".." && !strings.HasPrefix(clean, "../") && !strings.Contains(clean, "/../")
}
