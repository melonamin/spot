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

	// The pin and the file list are read together under the site lock so
	// the type header describes the listed files; the lock is released
	// before streaming so a slow client never blocks deploys.
	var siteType siteTypeRef
	lock := s.siteMutationLock(site)
	lock.Lock()
	if s.types != nil {
		siteType, err = s.types.SiteType(r.Context(), site)
		if err != nil {
			lock.Unlock()
			log.Printf("download %s: read site type: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not read the site's type")
			return
		}
	}
	paths, err := s.sites.List(r.Context(), site)
	lock.Unlock()
	if err != nil {
		log.Printf("download %s: list: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not list site files")
		return
	}
	paths = cleanDownloadPaths(paths)
	// An empty typed instance is valid: its export is an empty ZIP.
	if len(paths) == 0 && !siteType.typed() {
		httpError(w, http.StatusNotFound, "no site named "+site)
		return
	}

	if siteType.typed() {
		w.Header().Set("X-Spot-Type", siteType.String())
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": site + ".zip",
	}))
	zw := zip.NewWriter(w)

	for _, name := range paths {
		rc, info, err := s.sites.Open(r.Context(), site, name)
		if err != nil {
			// Status 200 is already committed, so a partial zip would
			// look valid to the client. Abort the response instead.
			log.Printf("download %s: open %s: %v", site, name, err)
			panic(http.ErrAbortHandler)
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
			panic(http.ErrAbortHandler)
		}
		if _, err := io.Copy(dst, rc); err != nil {
			rc.Close()
			log.Printf("download %s: copy %s: %v", site, name, err)
			panic(http.ErrAbortHandler)
		}
		rc.Close()
	}
	if err := zw.Close(); err != nil {
		log.Printf("download %s: finalize zip: %v", site, err)
		panic(http.ErrAbortHandler)
	}
}

func cleanDownloadPaths(paths []string) []string {
	out := paths[:0]
	for _, p := range paths {
		if validDownloadPath(p) && !isSiteAccessFile(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func validDownloadPath(p string) bool {
	if strings.IndexByte(p, 0) >= 0 {
		return false
	}
	if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.ContainsAny(p, `\:`) {
		return false
	}
	clean := path.Clean(p)
	return clean == p && clean != ".." && !strings.HasPrefix(clean, "../") && !strings.Contains(clean, "/../")
}
