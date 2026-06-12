package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"
)

// SiteAdmin is the registry surface behind the platform's site pages:
// listing a deployer's own sites, listing everything for the gallery,
// and deleting a site. SiteRegistry implements it.
type SiteAdmin interface {
	SitesOwnedBy(ctx context.Context, actor Identity) ([]OwnedSite, error)
	AllSites(ctx context.Context) ([]SiteRecord, error)
	DeleteSite(ctx context.Context, site string, actor Identity, purge func(context.Context) error) error
}

type ownedSiteJSON struct {
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FileCount  int       `json:"file_count"`
	TotalBytes int64     `json:"total_bytes"`
	Restricted bool      `json:"restricted"`
	AllowCount int       `json:"allow_count"`
}

type publicSiteJSON struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Owner     string    `json:"owner"`
	Yours     bool      `json:"yours"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// requireSitesAPI gates the sites endpoints the same way as /api/deploy:
// they answer only on the apex, so a deployed site's JavaScript cannot
// enumerate or delete sites with a visitor's ambient mesh identity.
func (s *Server) requireSitesAPI(w http.ResponseWriter, r *http.Request) bool {
	if s.siteAdmin == nil {
		httpError(w, http.StatusServiceUnavailable, "site registry not configured")
		return false
	}
	if siteFromHost(s.requestHost(r), s.spotDomain) != "" {
		httpError(w, http.StatusBadRequest,
			"the sites API is served on the platform root, not on site subdomains")
		return false
	}
	return true
}

func (s *Server) handleMySites(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return
	}
	owned, err := s.siteAdmin.SitesOwnedBy(r.Context(), actor)
	if err != nil {
		log.Printf("my sites: %v", err)
		httpError(w, http.StatusInternalServerError, "could not list your sites")
		return
	}
	out := make([]ownedSiteJSON, 0, len(owned))
	for _, site := range owned {
		restricted, allowCount := s.sitePolicySummary(site.Name)
		out = append(out, ownedSiteJSON{
			Name:       site.Name,
			URL:        "https://" + site.Name + "." + s.spotDomain + "/",
			CreatedAt:  site.CreatedAt,
			UpdatedAt:  site.UpdatedAt,
			FileCount:  site.FileCount,
			TotalBytes: site.TotalBytes,
			Restricted: restricted,
			AllowCount: allowCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

// handlePublicSites lists the gallery: every site without an access
// policy. Restricted sites stay out entirely — their existence is the
// owner's business — and so do sites whose policy is unreadable, since
// authz fails closed for those too.
func (s *Server) handlePublicSites(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	viewer, ok := s.resolveIdentity(w, r, "sites")
	if !ok {
		return
	}
	all, err := s.siteAdmin.AllSites(r.Context())
	if err != nil {
		log.Printf("public sites: %v", err)
		httpError(w, http.StatusInternalServerError, "could not list sites")
		return
	}
	out := make([]publicSiteJSON, 0, len(all))
	for _, site := range all {
		if restricted, _ := s.sitePolicySummary(site.Name); restricted {
			continue
		}
		out = append(out, publicSiteJSON{
			Name:      site.Name,
			URL:       "https://" + site.Name + "." + s.spotDomain + "/",
			Owner:     ownerDisplay(site),
			Yours:     site.OwnedBy(viewer),
			CreatedAt: site.CreatedAt,
			UpdatedAt: site.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	if s.sites == nil {
		httpError(w, http.StatusServiceUnavailable,
			"site store not configured: set SPOT_S3_ENDPOINT and credentials")
		return
	}
	site := r.PathValue("name")
	if !siteNameRe.MatchString(site) {
		httpError(w, http.StatusBadRequest, "invalid site name")
		return
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return
	}

	// The purge runs while the registry holds the site's row lock, so a
	// concurrent redeploy cannot interleave with it. Everything scoped to
	// the site goes: served files, uploads, and private collections —
	// otherwise the next claimant of the name would inherit them.
	removedFiles := 0
	purge := func(ctx context.Context) error {
		paths, err := s.sites.List(ctx, site)
		if err != nil {
			return err
		}
		for _, path := range paths {
			if err := s.sites.Remove(ctx, site, path); err != nil {
				return err
			}
		}
		removedFiles = len(paths)
		if s.files != nil {
			if err := s.files.RemoveSite(ctx, site); err != nil {
				return err
			}
		}
		if s.store != nil {
			if err := s.store.PurgeScope(ctx, site); err != nil {
				return err
			}
		}
		return nil
	}

	err := s.siteAdmin.DeleteSite(r.Context(), site, actor, purge)
	switch {
	case errors.Is(err, ErrSiteNotFound):
		httpError(w, http.StatusNotFound, "no site named "+site)
	case errors.Is(err, ErrDeployForbidden):
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "delete", Status: "denied",
			Message: "actor is not the site owner or a platform admin",
		})
		httpError(w, http.StatusForbidden, "only the site owner or a platform admin can delete this site")
	case err != nil:
		log.Printf("delete site %s: %v", site, err)
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "delete", Status: "failed",
			Message: "purge or registry delete failed",
		})
		httpError(w, http.StatusInternalServerError, "could not delete the site")
	default:
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "delete", Status: "success",
			FileCount: removedFiles,
		})
		writeJSON(w, http.StatusOK, map[string]any{"site": site, "files": removedFiles})
	}
}

// sitePolicySummary reports whether a site is restricted and by how many
// allow entries. An unreadable policy counts as restricted with zero
// entries — authz fails closed on those sites, so nobody can view them.
func (s *Server) sitePolicySummary(site string) (bool, int) {
	if s.policies == nil {
		return false, 0
	}
	policy, err := s.policies.For(site)
	if err != nil {
		return true, 0
	}
	if policy == nil {
		return false, 0
	}
	return true, len(policy.Allow)
}

// ownerDisplay is the name the gallery shows for a site's owner.
func ownerDisplay(site SiteRecord) string {
	if site.OwnerName != "" {
		return site.OwnerName
	}
	if site.OwnerEmail != "" {
		return site.OwnerEmail
	}
	return site.OwnerPeerIP
}
