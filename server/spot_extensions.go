package main

import "net/http"

// Embedding services read one namespace without exposing the site's access lists.
func (s *Server) handleSiteExtensionSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.forwardAuthIdentity(r); !ok {
		httpError(w, http.StatusForbidden, "internal identity required")
		return
	}
	site, namespace := r.PathValue("name"), r.PathValue("extension")
	if !siteNameRe.MatchString(site) || !siteNameRe.MatchString(namespace) {
		httpError(w, http.StatusBadRequest, "invalid site or extension namespace")
		return
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return
	}
	policy, err := s.policyForSite(r.Context(), site)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "could not read site settings")
		return
	}
	var settings any = map[string]any{}
	if policy != nil && policy.Extensions[namespace] != nil {
		settings = policy.Extensions[namespace]
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"site": site, "settings": settings})
}
