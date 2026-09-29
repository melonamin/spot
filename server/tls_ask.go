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
//
// Caddy asks directly, by the service's internal name, with no forwarded
// headers. A request that carries them came through a proxy, whatever Host it
// names, and gets 404, as does one addressed to a Spot host, so outsiders
// cannot list site names. The check is not rate-limited: Caddy asks from one
// address for every unknown TLS name, and a shared bucket would let anyone
// block issuance.
func (s *Server) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	proxied := r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("Forwarded") != ""
	if proxied || validSpotHost(s.requestHost(r), s.spotDomain) {
		http.NotFound(w, r)
		return
	}
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
