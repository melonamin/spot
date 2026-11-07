package main

import (
	"fmt"
	"net/http"
	"strings"
)

const maxContractPathLength = 512

// validateContractPath is the one path contract for type packages, typed
// deploys and data declarations. Unlike cleanSitePath it rejects instead of
// repairing: a leading slash is an error, not something to strip.
func validateContractPath(p string) error {
	if p == "" {
		return fmt.Errorf("a path is empty")
	}
	if len(p) > maxContractPathLength {
		return fmt.Errorf("path %q is too long (%d chars, max %d)", p, len(p), maxContractPathLength)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q must be relative", p)
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("path %q must use forward slashes", p)
	}
	if strings.ContainsRune(p, ':') {
		return fmt.Errorf("path %q must not contain ':'", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path %q contains control characters", p)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("path %q has an empty segment", p)
		case ".", "..":
			return fmt.Errorf("path %q must not contain . or .. segments", p)
		}
	}
	if !validSitePath(p) {
		return fmt.Errorf("path %q contains unsupported characters", p)
	}
	return nil
}

// dataPathCovers reports whether a type's data entries cover a site path.
// Prefix entries end in "/" and match whole segments only.
func dataPathCovers(entries []string, p string) bool {
	for _, entry := range entries {
		if strings.HasSuffix(entry, "/") {
			if strings.HasPrefix(p, entry) {
				return true
			}
		} else if p == entry {
			return true
		}
	}
	return false
}

// reservedDataSegment reports whether a data entry's first segment names
// something a typed site origin serves before, or instead of, site data.
func reservedDataSegment(entry string) bool {
	first, _, _ := strings.Cut(strings.TrimSuffix(entry, "/"), "/")
	first = strings.ToLower(first)
	switch first {
	case "index.html", typeAssetDir, accessFileName, siteMetadataFileName,
		"spot.js", "spot-live.js", "spot.d.ts", "api":
		return true
	}
	return strings.HasPrefix(first, "_screenshot.")
}

// typedPlatformFile reports the platform files a typed deploy may carry at
// its root besides its data.
func typedPlatformFile(p string) bool {
	if p == accessFileName || p == siteMetadataFileName {
		return true
	}
	for _, name := range previewFileNames {
		if p == name {
			return true
		}
	}
	return false
}

// rejectInvalidSitePaths applies the request side of the path contract on
// site hosts before the ServeMux, which would otherwise clean and redirect
// the path first. Encoded separators are refused outright because Go
// decodes them into ordinary slashes.
func (s *Server) rejectInvalidSitePaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if siteFromHost(s.requestHost(r), s.spotDomain) != "" && !validSiteRequestPath(r.URL.EscapedPath(), r.URL.Path) {
			httpError(w, http.StatusBadRequest, "request path is not valid")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validSiteRequestPath(escaped, decoded string) bool {
	lower := strings.ToLower(escaped)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return false
	}
	if !strings.HasPrefix(decoded, "/") {
		return false
	}
	rest := strings.TrimPrefix(decoded, "/")
	if rest == "" {
		return true
	}
	rest = strings.TrimSuffix(rest, "/")
	if strings.ContainsRune(rest, '\\') {
		return false
	}
	for _, r := range rest {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
