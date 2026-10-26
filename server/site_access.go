package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
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

// allowListFor is policyAllowList for a resolved policy; an unreadable policy
// reports no allowlist.
func allowListFor(policy *AccessPolicy, err error) []string {
	if err != nil {
		return nil
	}
	return policyAllowList(policy)
}

// siteAccessManager is the management decision the access API needs from
// the site registry; siteContentGenerations fences its writes.
type siteAccessManager interface {
	ManagementDecision(context.Context, string, Identity) (ManagementDecision, error)
}

type siteContentGenerations interface {
	SiteContentGeneration(context.Context, string) (int64, error)
}

// accessPolicyVersion is the strong entity tag of a stored policy object: its
// digest, or the absent-policy sentinel when the site has no _access.json.
func accessPolicyVersion(data []byte, absent bool) string {
	return quoteETag(policyObjectHash(data, absent))
}

func quoteETag(digest string) string { return `"` + digest + `"` }

// parseIfMatch returns the policy digest an If-Match header names, "" when
// the header is absent. Only one strong tag is accepted: "*" and weak or
// listed tags have no meaning for a policy that may legitimately be absent.
func parseIfMatch(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", true
	}
	if len(header) < 3 || header[0] != '"' || header[len(header)-1] != '"' {
		return "", false
	}
	digest := header[1 : len(header)-1]
	if strings.ContainsAny(digest, `", `) {
		return "", false
	}
	return digest, true
}

// policyListChange describes how a policy list changed for the audit log.
func policyListChange(name string, before, after *AccessPolicy, list func(*AccessPolicy) []string, present func(*AccessPolicy) bool) string {
	describe := func(p *AccessPolicy) string {
		if p == nil || !present(p) {
			return "none"
		}
		return strconv.Itoa(len(list(p)))
	}
	return name + " " + describe(before) + " -> " + describe(after)
}

// accessChangeSummary is the audit message for an access change: how many
// visitor and maintainer entries the policy had before and after, with
// "none" for an open site or no maintainers list, and "unreadable" when the
// previous policy could not be read.
func accessChangeSummary(before *AccessPolicy, beforeErr error, after *AccessPolicy) string {
	if beforeErr != nil {
		before = nil
	}
	allow := policyListChange("visitors", before, after,
		func(p *AccessPolicy) []string { return p.Allow }, (*AccessPolicy).RestrictsAccess)
	maintainers := policyListChange("maintainers", before, after,
		func(p *AccessPolicy) []string { return p.Maintainers }, func(p *AccessPolicy) bool { return p.Maintainers != nil })
	summary := allow + "; " + maintainers
	if beforeErr != nil {
		summary = "previous policy unreadable; " + summary
	}
	return summary
}

// accessRequestSite validates the common preconditions of the access API and
// returns the site name, the management lookup, and the authenticated actor.
func (s *Server) accessRequestSite(w http.ResponseWriter, r *http.Request) (string, siteAccessManager, Identity, bool) {
	if !s.requireSitesAPI(w, r) {
		return "", nil, Identity{}, false
	}
	if s.sites == nil {
		httpError(w, http.StatusServiceUnavailable, "site store not configured")
		return "", nil, Identity{}, false
	}
	site := r.PathValue("name")
	if !siteNameRe.MatchString(site) {
		httpError(w, http.StatusBadRequest, "invalid site name")
		return "", nil, Identity{}, false
	}
	manager, ok := s.siteManager.(siteAccessManager)
	if !ok {
		httpError(w, http.StatusServiceUnavailable, "site access management is not configured")
		return "", nil, Identity{}, false
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return "", nil, Identity{}, false
	}
	return site, manager, actor, true
}

// authorizeAccessManager answers whether actor may read and change site's
// access policy, writing the error response when it may not. Call it under
// the site mutation lock. While a policy transition is pending the
// maintainers list is unknown, so only the owner or an admin passes. A
// denial is passed to denied before the 403 is written.
func authorizeAccessManager(w http.ResponseWriter, r *http.Request, manager siteAccessManager, site string, actor Identity, denied func(string)) (ManagementDecision, bool) {
	decision, err := manager.ManagementDecision(r.Context(), site, actor)
	denial := "actor is not the site owner, a maintainer, or a platform admin"
	if errors.Is(err, ErrManagementPolicyUnresolved) && decision.State == SiteStateActive {
		decision, err = ManagementDecision{State: decision.State}, nil
		denial = "actor is not the site owner or a platform admin, and a pending policy transition hides the maintainers list"
	}
	switch {
	case errors.Is(err, ErrSiteNotFound) || (err == nil && decision.State != SiteStateActive):
		httpError(w, http.StatusNotFound, "no active site named "+site)
		return decision, false
	case err != nil:
		log.Printf("access %s: authorize: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize access management")
		return decision, false
	case !decision.Allowed:
		if denied != nil {
			denied(denial)
		}
		httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can manage this site's access")
		return decision, false
	}
	return decision, true
}

// accessPolicyBody is a stored policy as the access API returns it. Allow is
// present only when the policy restricts visitors, so an empty list still
// means nobody may view the site.
func accessPolicyBody(policy *AccessPolicy) map[string]any {
	body := map[string]any{}
	if policy == nil {
		return body
	}
	if policy.RestrictsAccess() {
		body["allow"] = policyAllowList(policy)
	}
	if policy.Maintainers != nil {
		body["maintainers"] = append([]string{}, policy.Maintainers...)
	}
	if policy.AI != "" {
		body["ai"] = policy.AI
	}
	if policy.Slack != "" {
		body["slack"] = policy.Slack
	}
	if policy.Download != nil {
		body["download"] = *policy.Download
	}
	return body
}

// handleSiteAccessRead returns a site's stored _access.json to someone who
// may change it, with its version as an ETag for a conditional PUT. It reads
// storage rather than the policy cache so the version names exact bytes. An
// unparsable policy is reported as unreadable, and can then be replaced.
func (s *Server) handleSiteAccessRead(w http.ResponseWriter, r *http.Request) {
	site, manager, actor, ok := s.accessRequestSite(w, r)
	if !ok {
		return
	}
	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	defer siteLock.Unlock()
	if _, ok := authorizeAccessManager(w, r, manager, site, actor, nil); !ok {
		return
	}
	// A pending transition means the stored bytes may be either side of an
	// interrupted change; the next PUT reconciles it before writing.
	pending := false
	if registry, ok := s.deployAuth.(pendingPolicyTransitionRegistry); ok {
		transition, err := registry.PendingPolicyTransition(r.Context(), site)
		if err != nil {
			log.Printf("access %s: read policy transition: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not read the site's access state")
			return
		}
		pending = transition.Generation != 0
	}
	data, absent, err := s.storedPolicyBytes(r.Context(), site)
	if err != nil {
		log.Printf("access %s: read policy: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not read "+accessFileName)
		return
	}
	version := accessPolicyVersion(data, absent)
	body := map[string]any{"site": site, "version": version, "pending": pending}
	if absent {
		body["readable"], body["restricted"], body["policy"] = true, false, accessPolicyBody(nil)
	} else if policy, parseErr := parseAccessPolicy(site, data); parseErr != nil {
		// An unreadable policy denies every visitor; say so without
		// echoing the file, and let the manager replace it.
		body["readable"], body["restricted"], body["policy"] = false, true, nil
		body["error"] = parseErr.Error()
	} else {
		body["readable"], body["restricted"], body["policy"] = true, policy.RestrictsAccess(), accessPolicyBody(policy)
	}
	w.Header().Set("ETag", version)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

// handleSiteAccess replaces a site's _access.json without redeploying its
// content. It follows the deploy path's fencing: the site mutation lock, a
// management decision, a pending-transition reconcile, and a generation-fenced
// policy commit. An If-Match header makes the change conditional on the
// stored policy still having the version the caller read; every writer of
// _access.json holds the site mutation lock, and the check uses the read
// that fences the commit, so it cannot race.
func (s *Server) handleSiteAccess(w http.ResponseWriter, r *http.Request) {
	site, manager, actor, ok := s.accessRequestSite(w, r)
	if !ok {
		return
	}
	generations, ok := s.siteAdmin.(siteContentGenerations)
	if !ok {
		httpError(w, http.StatusServiceUnavailable, "site access management is not configured")
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
	expected, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		httpError(w, http.StatusBadRequest, "If-Match must be one quoted version from GET /api/sites/{name}/access")
		return
	}

	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	defer siteLock.Unlock()
	// Authorize before reconciling, so a caller who cannot manage the site
	// learns nothing about a pending policy transition.
	decision, ok := authorizeAccessManager(w, r, manager, site, actor, func(denial string) {
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "access", Status: "denied", Message: denial,
		})
	})
	if !ok {
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
	if err := s.commitPolicyObjectIf(r.Context(), site, generation, body, false, expected); err != nil {
		var mismatch *policyVersionMismatchError
		if errors.As(err, &mismatch) {
			current := quoteETag(mismatch.Current)
			w.Header().Set("ETag", current)
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{
				"error":   "the site's access changed since it was read; reload and try again",
				"version": current,
			})
			return
		}
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
		Message: accessChangeSummary(current, currentErr, next),
	})
	version := accessPolicyVersion(body, false)
	w.Header().Set("ETag", version)
	writeJSON(w, http.StatusOK, map[string]any{
		"site":       site,
		"allow":      policyAllowList(next),
		"restricted": next.RestrictsAccess(),
		"policy":     accessPolicyBody(next),
		"version":    version,
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
	query, err := parseSiteListQuery(r, viewer)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	all, err := s.siteAdmin.AllSites(r.Context())
	if err != nil {
		log.Printf("visible sites: %v", err)
		httpError(w, http.StatusInternalServerError, "could not list sites")
		return
	}
	type visibleCandidate struct {
		site    SiteRecord
		policy  *AccessPolicy
		manages bool
		visible bool
	}
	candidates := make([]visibleCandidate, len(all))
	forEachConcurrently(len(all), func(i int) {
		site := all[i]
		policy, err := s.policyForSite(r.Context(), site.Name)
		if err != nil {
			return
		}
		restricted := policy.RestrictsAccess()
		// Only a restricted site has an allowlist to hide, so only there does
		// management need resolving.
		manages := restricted && (site.OwnedBy(viewer) || s.canManageSite(r.Context(), site.Name, viewer))
		candidates[i] = visibleCandidate{
			site: site, policy: policy, manages: manages,
			visible: !restricted || manages || policy.Allows(viewer),
		}
	})
	visible := candidates[:0]
	for _, candidate := range candidates {
		if candidate.visible {
			visible = append(visible, candidate)
		}
	}
	page := pageSites(visible, func(c visibleCandidate) SiteRecord { return c.site }, query)
	out := make([]visibleSiteJSON, len(page.Items))
	forEachConcurrently(len(page.Items), func(i int) {
		site, policy := page.Items[i].site, page.Items[i].policy
		restricted := policy.RestrictsAccess()
		yours := site.OwnedBy(viewer)
		var allow []string
		if page.Items[i].manages {
			allow = policyAllowList(policy)
		}
		preview := ""
		if !restricted && s.hasSitePreview(r.Context(), site.Name) {
			preview = "/api/sites/" + site.Name + "/preview"
		}
		out[i] = visibleSiteJSON{
			publicSiteJSON: publicSiteJSON{
				Name: site.Name, URL: s.siteURL(r, site.Name), Title: site.Title,
				Description: site.Description, Tags: cloneSiteTags(site.Tags),
				DownloadAllowed: policy.AllowsDownload(), Owner: ownerDisplay(site), OwnerEmail: ownerEmailFor(site, yours || restricted), Yours: yours,
				Preview: preview, CreatedAt: site.CreatedAt, UpdatedAt: site.UpdatedAt,
				Type: siteTypeJSONFor(site.Type, site.TypeTitle),
			},
			Restricted: restricted,
			Allow:      allow,
		}
	})
	body := siteListBody(page, out)
	body["tag_counts"] = page.TagCounts
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) canManageSite(ctx context.Context, site string, actor Identity) bool {
	if s.siteManager == nil {
		return false
	}
	allowed, err := s.siteManager.CanManageSite(ctx, site, actor)
	return err == nil && allowed
}
