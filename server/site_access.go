package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
)

const maxAccessPolicyBody = 64 << 10

// policyAllowList is the site's visitor allowlist for API responses: nil when
// the policy has no allow field (open site), [] when it denies everyone.
func policyAllowList(policy *AccessPolicy) []string {
	if !policy.RestrictsAccess() {
		return nil
	}
	return append(make([]string, 0, len(policy.Allow)), policy.Allow...)
}

func (s *Server) allowListForSite(ctx context.Context, site string) []string {
	policy, err := s.policyForSite(ctx, site)
	if err != nil {
		return nil
	}
	return policyAllowList(policy)
}

// handleSiteAccess replaces a site's _access.json without redeploying its
// content. It follows the deploy path's fencing: the site mutation lock, a
// management decision, a pending-transition reconcile, and a generation-fenced
// policy commit.
func (s *Server) handleSiteAccess(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	if s.sites == nil {
		httpError(w, http.StatusServiceUnavailable, "site store not configured")
		return
	}
	site := r.PathValue("name")
	if !siteNameRe.MatchString(site) {
		httpError(w, http.StatusBadRequest, "invalid site name")
		return
	}
	manager, ok := s.siteManager.(interface {
		ManagementDecision(context.Context, string, Identity) (ManagementDecision, error)
	})
	generations, hasGenerations := s.siteAdmin.(interface {
		SiteContentGeneration(context.Context, string) (int64, error)
	})
	if !ok || !hasGenerations {
		httpError(w, http.StatusServiceUnavailable, "site access management is not configured")
		return
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAccessPolicyBody))
	if err != nil {
		httpError(w, http.StatusBadRequest, "could not read the access policy")
		return
	}
	next, err := parseAccessPolicy(site, body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid "+accessFileName+": "+err.Error())
		return
	}

	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	defer siteLock.Unlock()
	// Authorize before reconciling, so a caller who cannot manage the site
	// learns nothing about a pending policy transition. While one is pending
	// the maintainers list is unknown, so only the owner or an admin passes.
	decision, err := manager.ManagementDecision(r.Context(), site, actor)
	denial := "actor is not the site owner, a maintainer, or a platform admin"
	if errors.Is(err, ErrManagementPolicyUnresolved) && decision.State == SiteStateActive {
		decision, err = ManagementDecision{State: decision.State}, nil
		denial = "actor is not the site owner or a platform admin, and a pending policy transition hides the maintainers list"
	}
	switch {
	case errors.Is(err, ErrSiteNotFound) || (err == nil && decision.State != SiteStateActive):
		httpError(w, http.StatusNotFound, "no active site named "+site)
		return
	case err != nil:
		log.Printf("access %s: authorize: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize the access change")
		return
	case !decision.Allowed:
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "access", Status: "denied", Message: denial,
		})
		httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can change this site's access")
		return
	}
	if err := s.reconcilePolicyTransition(r.Context(), site, DeployPrincipal{Actor: actor}); err != nil {
		if errors.Is(err, ErrSiteNotFound) {
			httpError(w, http.StatusNotFound, "no active site named "+site)
			return
		}
		log.Printf("access %s: reconcile policy transition: %v", site, err)
		httpError(w, http.StatusServiceUnavailable, "the site's access policy needs owner or admin recovery")
		return
	}
	// Like a deploy, any manager may change both lists: the immutable owner
	// keeps its recovery claim whatever the maintainers list says.
	current, currentErr := s.policyForSite(r.Context(), site)
	generation, err := generations.SiteContentGeneration(r.Context(), site)
	if err != nil {
		log.Printf("access %s: read content generation: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not read the site's content generation")
		return
	}
	if err := s.commitPolicyObject(r.Context(), site, generation, body, false); err != nil {
		if errors.Is(err, ErrPolicyTransitionConflict) {
			httpError(w, http.StatusConflict, "the site changed while its access was being updated; retry")
			return
		}
		// The new policy may already be stored; its outcome is unknown and
		// requests now fail closed, so live sessions must not outlast it.
		if errors.Is(err, errPolicyTransitionUnresolved) {
			s.disconnectSiteRealtime(site)
		}
		log.Printf("access %s: commit policy: %v", site, err)
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "access", Status: "failed", AuthorizedAs: decision.Role,
			ContentGeneration: generation, Message: "could not store " + accessFileName,
		})
		httpError(w, http.StatusInternalServerError, "could not store "+accessFileName)
		return
	}
	if currentErr != nil || policyNarrowsAccess(current, next, true) {
		s.disconnectSiteRealtime(site)
	}
	s.recordDeployAudit(r, DeployAuditEvent{
		Site: site, Actor: actor, Action: "access", Status: "success",
		AuthorizedAs: decision.Role, ContentGeneration: generation,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"site":       site,
		"allow":      policyAllowList(next),
		"restricted": next.RestrictsAccess(),
	})
}

type visibleSiteJSON struct {
	publicSiteJSON
	Restricted bool     `json:"restricted"`
	Allow      []string `json:"allow"`
}

// handleVisibleSites lists every active site the caller may view: open sites,
// restricted sites whose allowlist matches the caller, and sites the caller
// manages. Sites with an unreadable policy are omitted, as authz fails closed.
// Only the owner and managers see the allowlist; a viewer does not learn who
// else may view the site. A listed restricted site was shared with the caller
// or is managed by them, so its entry carries the owner's email; an open site
// carries it only for its owner.
func (s *Server) handleVisibleSites(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	viewer, ok := s.resolveIdentity(w, r, "visible sites")
	if !ok {
		return
	}
	all, err := s.siteAdmin.AllSites(r.Context())
	if err != nil {
		log.Printf("visible sites: %v", err)
		httpError(w, http.StatusInternalServerError, "could not list sites")
		return
	}
	out := make([]visibleSiteJSON, 0, len(all))
	for _, site := range all {
		policy, err := s.policyForSite(r.Context(), site.Name)
		if err != nil {
			continue
		}
		restricted := policy.RestrictsAccess()
		yours := site.OwnedBy(viewer)
		// Only a restricted site has an allowlist to hide, so only there does
		// management need resolving.
		manages := restricted && (yours || s.canManageSite(r.Context(), site.Name, viewer))
		if restricted && !manages && !policy.Allows(viewer) {
			continue
		}
		var allow []string
		if manages {
			allow = policyAllowList(policy)
		}
		preview := ""
		if !restricted && s.hasSitePreview(r.Context(), site.Name) {
			preview = "/api/sites/" + site.Name + "/preview"
		}
		out = append(out, visibleSiteJSON{
			publicSiteJSON: publicSiteJSON{
				Name: site.Name, URL: s.siteURL(r, site.Name), Title: site.Title,
				Description: site.Description, Tags: cloneSiteTags(site.Tags),
				DownloadAllowed: policy.AllowsDownload(), Owner: ownerDisplay(site), OwnerEmail: ownerEmailFor(site, yours || restricted), Yours: yours,
				Preview: preview, CreatedAt: site.CreatedAt, UpdatedAt: site.UpdatedAt,
			},
			Restricted: restricted,
			Allow:      allow,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

func (s *Server) canManageSite(ctx context.Context, site string, actor Identity) bool {
	if s.siteManager == nil {
		return false
	}
	allowed, err := s.siteManager.CanManageSite(ctx, site, actor)
	return err == nil && allowed
}
