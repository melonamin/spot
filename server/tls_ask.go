package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
)

// handleTLSAsk answers Caddy's on-demand TLS permission check: certificates
// are issued only for the apex and the names of active sites.
func (s *Server) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain"))), ".")
	apex := strings.TrimSuffix(strings.ToLower(s.spotDomain), ".")
	if domain != "" && domain == apex {
		w.WriteHeader(http.StatusOK)
		return
	}
	site := ""
	if !strings.Contains(domain, ":") {
		site = siteFromHost(domain, s.spotDomain)
	}
	lifecycle, ok := s.siteAdmin.(interface {
		SiteState(context.Context, string) (SiteState, error)
	})
	if site == "" || !ok {
		http.NotFound(w, r)
		return
	}
	state, err := lifecycle.SiteState(r.Context(), site)
	if errors.Is(err, ErrSiteNotFound) || (err == nil && state != SiteStateActive) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("tls ask %s: %v", site, err)
		httpError(w, http.StatusServiceUnavailable, "could not verify site lifecycle")
		return
	}
	w.WriteHeader(http.StatusOK)
}
