package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
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

type siteContentMutationMarker interface {
	MarkSiteContentDirty(ctx context.Context, site string) error
}

type SiteManager interface {
	CanManageSite(ctx context.Context, site string, actor Identity) (bool, error)
}

type manageableSiteAdmin interface {
	SitesManageableBy(ctx context.Context, actor Identity) ([]ManageableSite, error)
}

type ownedSiteJSON struct {
	Name            string          `json:"name"`
	URL             string          `json:"url"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	Tags            []string        `json:"tags"`
	DownloadAllowed bool            `json:"download_allowed"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	FileCount       int             `json:"file_count"`
	TotalBytes      int64           `json:"total_bytes"`
	Restricted      bool            `json:"restricted"`
	AllowCount      int             `json:"allow_count"`
	Cloudflare      any             `json:"cloudflare,omitempty"`
	Owner           string          `json:"owner,omitempty"`
	ManagementRole  string          `json:"management_role,omitempty"`
	State           SiteState       `json:"state,omitempty"`
	LastDeploy      *lastDeployJSON `json:"last_deploy,omitempty"`
}

type lastDeployJSON struct {
	At            string `json:"at"`
	AuthMethod    string `json:"auth_method,omitempty"`
	PublisherName string `json:"publisher_name,omitempty"`
}

func lastDeployForSite(site OwnedSite) *lastDeployJSON {
	if site.LastDeployAt == "" {
		return nil
	}
	return &lastDeployJSON{At: site.LastDeployAt, AuthMethod: site.LastDeployAuthMethod, PublisherName: site.LastDeployPublisher}
}

func (s *Server) handleManageableSites(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return
	}
	admin, ok := s.siteAdmin.(manageableSiteAdmin)
	if !ok {
		httpError(w, http.StatusServiceUnavailable, "manageable site listing is not configured")
		return
	}
	sites, err := admin.SitesManageableBy(r.Context(), actor)
	if err != nil {
		log.Printf("manageable sites: %v", err)
		httpError(w, http.StatusInternalServerError, "could not list manageable sites")
		return
	}
	out := make([]ownedSiteJSON, 0, len(sites))
	for _, site := range sites {
		entry := ownedSiteJSON{
			Name: site.Name, URL: s.siteURL(r, site.Name), Title: site.Title,
			Description: site.Description, Tags: cloneSiteTags(site.Tags),
			CreatedAt: site.CreatedAt, UpdatedAt: site.UpdatedAt,
			FileCount: site.FileCount, TotalBytes: site.TotalBytes,
			Owner: ownerDisplay(site.SiteRecord), ManagementRole: string(site.ManagementRole), State: site.State,
			LastDeploy: lastDeployForSite(site.OwnedSite),
		}
		if site.State == SiteStateActive {
			entry.Restricted, entry.AllowCount, entry.DownloadAllowed = s.policySummaryForSite(r.Context(), site.Name)
			contentHash := site.ContentHash
			if site.ContentHashUncertain {
				contentHash = ""
			}
			entry.Cloudflare = s.cloudflareSummaryForSite(r.Context(), site.Name, contentHash, false)
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

type publicSiteJSON struct {
	Name            string    `json:"name"`
	URL             string    `json:"url"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	Tags            []string  `json:"tags"`
	DownloadAllowed bool      `json:"download_allowed"`
	Owner           string    `json:"owner"`
	Yours           bool      `json:"yours"`
	Preview         string    `json:"preview,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type siteStatsJSON struct {
	Totals    siteStatsTotalsJSON     `json:"totals"`
	Growth    []siteStatsGrowthJSON   `json:"growth"`
	Activity  []siteStatsActivityJSON `json:"activity"`
	Tags      []siteStatsTagJSON      `json:"tags"`
	Freshness []siteStatsBucketJSON   `json:"freshness"`
	Quality   []siteStatsBucketJSON   `json:"quality"`
}

type siteStatsTotalsJSON struct {
	Total         int `json:"total"`
	Public        int `json:"public"`
	Private       int `json:"private"`
	UpdatedWeek   int `json:"updated_week"`
	Creators      int `json:"creators"`
	Tagged        int `json:"tagged"`
	WithMetadata  int `json:"with_metadata"`
	WithPreviews  int `json:"with_previews"`
	Downloadable  int `json:"downloadable"`
	NoDownloadZip int `json:"no_download_zip"`
}

type siteStatsGrowthJSON struct {
	Date    string `json:"date"`
	Total   int    `json:"total"`
	Public  int    `json:"public"`
	Private int    `json:"private"`
}

type siteStatsActivityJSON struct {
	Date    string `json:"date"`
	Created int    `json:"created"`
	Updated int    `json:"updated"`
}

type siteStatsTagJSON struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

type siteStatsBucketJSON struct {
	Label string `json:"label"`
	Count int    `json:"count"`
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
		restricted, allowCount, downloadAllowed := s.policySummaryForSite(r.Context(), site.Name)
		contentHash := site.ContentHash
		if site.ContentHashUncertain {
			contentHash = ""
		}
		out = append(out, ownedSiteJSON{
			Name:            site.Name,
			URL:             s.siteURL(r, site.Name),
			Title:           site.Title,
			Description:     site.Description,
			Tags:            cloneSiteTags(site.Tags),
			DownloadAllowed: downloadAllowed,
			CreatedAt:       site.CreatedAt,
			UpdatedAt:       site.UpdatedAt,
			FileCount:       site.FileCount,
			TotalBytes:      site.TotalBytes,
			Restricted:      restricted,
			AllowCount:      allowCount,
			Cloudflare: s.cloudflareSummaryForSite(
				r.Context(), site.Name, contentHash, false),
			LastDeploy: lastDeployForSite(site),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

func (s *Server) handleSiteStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	all, err := s.siteAdmin.AllSites(r.Context())
	if err != nil {
		log.Printf("site stats: %v", err)
		httpError(w, http.StatusInternalServerError, "could not load site stats")
		return
	}
	writeJSON(w, http.StatusOK, buildSiteStats(r.Context(), s, all, time.Now()))
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
		restricted, _, downloadAllowed := s.policySummaryForSite(r.Context(), site.Name)
		if restricted {
			continue
		}
		preview := ""
		if s.hasSitePreview(r.Context(), site.Name) {
			preview = "/api/sites/" + site.Name + "/preview"
		}
		out = append(out, publicSiteJSON{
			Name:            site.Name,
			URL:             s.siteURL(r, site.Name),
			Title:           site.Title,
			Description:     site.Description,
			Tags:            cloneSiteTags(site.Tags),
			DownloadAllowed: downloadAllowed,
			Owner:           ownerDisplay(site),
			Yours:           site.OwnedBy(viewer),
			Preview:         preview,
			CreatedAt:       site.CreatedAt,
			UpdatedAt:       site.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

func buildSiteStats(ctx context.Context, s *Server, sites []SiteRecord, now time.Time) siteStatsJSON {
	now = now.UTC()
	cutoffWeek := now.AddDate(0, 0, -7)
	creators := map[string]struct{}{}
	tagCounts := map[string]int{}
	created := map[string]int{}
	createdPublic := map[string]int{}
	createdPrivate := map[string]int{}
	activityCreated := map[string]int{}
	activityUpdated := map[string]int{}
	activityStart := dayKey(now.AddDate(0, 0, -29))

	var out siteStatsJSON
	for _, site := range sites {
		restricted, _, downloadAllowed := s.policySummaryForSite(ctx, site.Name)
		hasPreview := !restricted && s.hasSitePreview(ctx, site.Name)
		out.Totals.Total++
		if restricted {
			out.Totals.Private++
		} else {
			out.Totals.Public++
		}
		if downloadAllowed {
			out.Totals.Downloadable++
		} else {
			out.Totals.NoDownloadZip++
		}
		if site.UpdatedAt.After(cutoffWeek) {
			out.Totals.UpdatedWeek++
		}
		if key := siteOwnerKey(site); key != "" {
			creators[key] = struct{}{}
		}
		if len(site.Tags) > 0 {
			out.Totals.Tagged++
		}
		if site.Title != "" || site.Description != "" {
			out.Totals.WithMetadata++
		}
		if hasPreview {
			out.Totals.WithPreviews++
		}

		cday := dayKey(site.CreatedAt)
		if cday != "" {
			created[cday]++
			if restricted {
				createdPrivate[cday]++
			} else {
				createdPublic[cday]++
			}
			if cday >= activityStart {
				activityCreated[cday]++
			}
		}
		uday := dayKey(site.UpdatedAt)
		if uday != "" && uday >= activityStart && !sameDay(site.CreatedAt, site.UpdatedAt) {
			activityUpdated[uday]++
		}
		if !restricted {
			for _, tag := range site.Tags {
				tagCounts[tag]++
			}
		}
	}
	out.Totals.Creators = len(creators)
	out.Growth = buildGrowthSeries(created, createdPublic, createdPrivate, now)
	out.Activity = buildActivitySeries(activityCreated, activityUpdated, now)
	out.Tags = buildTopTags(tagCounts, 10)
	out.Freshness = buildFreshnessBuckets(sites, now)
	out.Quality = []siteStatsBucketJSON{
		{Label: "tagged", Count: out.Totals.Tagged},
		{Label: "metadata", Count: out.Totals.WithMetadata},
		{Label: "previews", Count: out.Totals.WithPreviews},
		{Label: "downloadable", Count: out.Totals.Downloadable},
	}
	return out
}

func buildGrowthSeries(created, createdPublic, createdPrivate map[string]int, now time.Time) []siteStatsGrowthJSON {
	if len(created) == 0 {
		return nil
	}
	keys := make([]string, 0, len(created))
	for key := range created {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	start, err := time.Parse("2006-01-02", keys[0])
	if err != nil {
		return nil
	}
	end := dateOnly(now)
	total, public, private := 0, 0, 0
	points := make([]siteStatsGrowthJSON, 0, int(end.Sub(start).Hours()/24)+1)
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		total += created[key]
		public += createdPublic[key]
		private += createdPrivate[key]
		points = append(points, siteStatsGrowthJSON{
			Date:    key,
			Total:   total,
			Public:  public,
			Private: private,
		})
	}
	return points
}

func buildActivitySeries(created, updated map[string]int, now time.Time) []siteStatsActivityJSON {
	start := dateOnly(now).AddDate(0, 0, -29)
	points := make([]siteStatsActivityJSON, 0, 30)
	for day := start; !day.After(dateOnly(now)); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		points = append(points, siteStatsActivityJSON{
			Date:    key,
			Created: created[key],
			Updated: updated[key],
		})
	}
	return points
}

func buildTopTags(counts map[string]int, limit int) []siteStatsTagJSON {
	tags := make([]siteStatsTagJSON, 0, len(counts))
	for tag, count := range counts {
		tags = append(tags, siteStatsTagJSON{Tag: tag, Count: count})
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Count != tags[j].Count {
			return tags[i].Count > tags[j].Count
		}
		return tags[i].Tag < tags[j].Tag
	})
	if len(tags) > limit {
		tags = tags[:limit]
	}
	return tags
}

func buildFreshnessBuckets(sites []SiteRecord, now time.Time) []siteStatsBucketJSON {
	buckets := []siteStatsBucketJSON{
		{Label: "today"},
		{Label: "this week"},
		{Label: "this month"},
		{Label: "older"},
	}
	today := dateOnly(now)
	week := now.AddDate(0, 0, -7)
	month := now.AddDate(0, -1, 0)
	for _, site := range sites {
		switch {
		case sameDay(site.UpdatedAt, today):
			buckets[0].Count++
		case site.UpdatedAt.After(week):
			buckets[1].Count++
		case site.UpdatedAt.After(month):
			buckets[2].Count++
		default:
			buckets[3].Count++
		}
	}
	return buckets
}

func siteOwnerKey(site SiteRecord) string {
	if site.OwnerEmail != "" {
		return "email:" + strings.ToLower(site.OwnerEmail)
	}
	if site.OwnerPeerIP != "" {
		return "peer:" + site.OwnerPeerIP
	}
	if site.OwnerName != "" {
		return "name:" + strings.ToLower(site.OwnerName)
	}
	return ""
}

func sameDay(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return dayKey(a) == dayKey(b)
}

func dayKey(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return dateOnly(t).Format("2006-01-02")
}

func dateOnly(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func (s *Server) siteURL(r *http.Request, site string) string {
	return s.requestScheme(r) + "://" + siteHostForRequest(s.requestHost(r), s.spotDomain, site) + "/"
}

func siteHostForRequest(requestHost, spotDomain, site string) string {
	host := requestHost
	port := ""
	if h, p, err := net.SplitHostPort(requestHost); err == nil {
		host = h
		port = p
	}
	host = strings.TrimSuffix(host, ".")
	if !sameHost(host, spotDomain) {
		host = strings.TrimSuffix(spotDomain, ".")
	}
	siteHost := site + "." + host
	if port != "" {
		return net.JoinHostPort(siteHost, port)
	}
	return siteHost
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

	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	defer siteLock.Unlock()

	var decision ManagementDecision
	if manager, ok := s.siteManager.(interface {
		ManagementDecision(context.Context, string, Identity) (ManagementDecision, error)
	}); ok {
		var err error
		decision, err = manager.ManagementDecision(r.Context(), site, actor)
		switch {
		case errors.Is(err, ErrSiteNotFound):
			httpError(w, http.StatusNotFound, "no site named "+site)
			return
		case err != nil:
			log.Printf("delete site %s: authorize: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not authorize site deletion")
			return
		case !decision.Allowed:
			s.recordDeployAudit(r, DeployAuditEvent{
				Site: site, Actor: actor, Action: "delete", Status: "denied",
				Message: "actor is not the site owner, a maintainer, or a platform admin",
			})
			httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can delete this site")
			return
		case decision.State == SiteStateProvisioning:
			httpError(w, http.StatusConflict, "a provisioning site cannot be deleted")
			return
		}
		if decision.State == SiteStateActive {
			if s.cloudflarePubs != nil {
				published, err := s.cloudflarePubs.Has(r.Context(), site)
				if err != nil {
					log.Printf("delete site %s: check cloudflare publication: %v", site, err)
					httpError(w, http.StatusInternalServerError, "could not check the site's publication status")
					return
				}
				if published {
					httpError(w, http.StatusConflict, "unpublish this site from Cloudflare before deleting it")
					return
				}
			}
		}
	}

	// Everything scoped to the site goes: served files, uploads, and
	// private collections. If purge fails, the registry row stays claimed
	// so the owner can retry instead of freeing a partially purged name.
	removedFiles := 0
	mutationStarted := false
	hadAccessPolicy := decision.State == SiteStateActive
	purge := func(ctx context.Context) error {
		if decision.State == SiteStateDeleted {
			return nil
		}
		if s.cloudflarePubs != nil {
			published, err := s.cloudflarePubs.Has(ctx, site)
			if err != nil {
				return fmt.Errorf("check cloudflare publication: %w", err)
			}
			if published {
				return errCloudflarePublicationExists
			}
		}
		if decision.State == SiteStateActive {
			current, policyErr := s.policyForSite(ctx, site)
			if policyErr != nil && decision.Role == ManagementRoleMaintainer {
				return fmt.Errorf("resolve management policy before deletion: %w", policyErr)
			}
			staging := restrictiveStagingPolicy(current)
			data, err := marshalRestrictiveStagingPolicy(staging)
			if err != nil {
				return fmt.Errorf("prepare private deletion policy: %w", err)
			}
			generation := int64(0)
			if reader, ok := s.siteAdmin.(interface {
				SiteContentGeneration(context.Context, string) (int64, error)
			}); ok {
				generation, err = reader.SiteContentGeneration(ctx, site)
				if err != nil {
					return fmt.Errorf("read access-policy deletion generation: %w", err)
				}
			}
			if generation > 0 {
				err = s.commitPolicyObject(ctx, site, generation, data, false)
			} else {
				err = s.sites.Put(ctx, site, accessFileName, "application/json", data)
			}
			if err != nil {
				return fmt.Errorf("make site private before deletion: %w", err)
			}
			if s.policies != nil {
				s.policies.Set(site, staging, nil)
			}
			s.disconnectSiteRealtime(site)
			if marker, ok := s.siteAdmin.(siteContentMutationMarker); ok {
				if err := marker.MarkSiteContentDirty(ctx, site); err != nil {
					return err
				}
			}
			mutationStarted = true
		}
		paths, err := s.sites.List(ctx, site)
		if err != nil {
			return err
		}
		if !mutationStarted {
			marker, ok := s.siteAdmin.(siteContentMutationMarker)
			if ok {
				if err := marker.MarkSiteContentDirty(ctx, site); err != nil {
					return err
				}
			}
		}
		mutationStarted = true
		for _, path := range paths {
			if path == accessFileName {
				hadAccessPolicy = true
				continue
			}
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
			Message: "actor is not the site owner, a maintainer, or a platform admin",
		})
		httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can delete this site")
	case errors.Is(err, errCloudflarePublicationExists):
		httpError(w, http.StatusConflict, "unpublish this site from Cloudflare before deleting it")
	case err != nil:
		log.Printf("delete site %s: %v", site, err)
		action := "delete-check"
		message := "pre-delete check failed before content mutation"
		if mutationStarted {
			action = "delete"
			message = "purge or registry delete failed"
		}
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: action, Status: "failed",
			Message: message,
		})
		httpError(w, http.StatusInternalServerError, "could not delete the site")
	default:
		if decision.State != SiteStateDeleted && hadAccessPolicy {
			var err error
			if decision.Role == ManagementRoleMaintainer {
				if reader, ok := s.siteAdmin.(interface {
					SiteContentGeneration(context.Context, string) (int64, error)
				}); ok {
					generation, generationErr := reader.SiteContentGeneration(r.Context(), site)
					if generationErr == nil {
						err = s.commitPolicyObject(r.Context(), site, generation, nil, true)
					} else {
						err = generationErr
					}
				}
			} else {
				err = s.sites.Remove(r.Context(), site, accessFileName)
			}
			if err != nil && !siteObjectNotFound(err) {
				log.Printf("delete site %s: remove final policy: %v", site, err)
				httpError(w, http.StatusInternalServerError, "site data was deleted but policy cleanup needs owner or admin recovery")
				return
			}
		}
		if s.policies != nil {
			s.policies.Invalidate(site)
		}
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "delete", Status: "success",
			FileCount: removedFiles, AuthorizedAs: decision.Role,
		})
		writeJSON(w, http.StatusOK, map[string]any{"site": site, "files": removedFiles})
	}
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

type cloudflareStatusJSON struct {
	ConfigStatus    string                 `json:"config_status"`
	Enabled         bool                   `json:"enabled"`
	AccessEnabled   bool                   `json:"access_enabled"`
	OperationActive bool                   `json:"operation_active"`
	Site            string                 `json:"site,omitempty"`
	Hostname        string                 `json:"hostname,omitempty"`
	ContentHash     string                 `json:"content_hash,omitempty"`
	Publication     *cloudflarePublication `json:"publication,omitempty"`
	Eligibility     *cloudflareEligibility `json:"eligibility,omitempty"`
}

func (s *Server) cloudflareSummaryForSite(ctx context.Context, site, knownContentHash string, inspectFiles bool) any {
	status := cloudflareConfigDisabled
	enabled := false
	hostname := ""
	if s.cloudflare != nil {
		status = s.cloudflare.status()
		enabled = s.cloudflare.cfg.Enabled()
		hostname = s.cloudflare.cfg.Hostname(site)
	}
	out := cloudflareStatusJSON{
		ConfigStatus:    status,
		Enabled:         enabled,
		AccessEnabled:   s.cloudflare != nil && s.cloudflare.cfg.AccessEnabled(),
		OperationActive: s.cloudflareJobActive(site),
		Site:            site,
		Hostname:        hostname,
		ContentHash:     knownContentHash,
	}
	if s.cloudflarePubs != nil {
		pub, err := s.cloudflarePubs.Get(ctx, site)
		if err == nil {
			out.Publication = cloudflarePublicationForAPI(pub)
			if pub != nil {
				out.Hostname = pub.Hostname
			}
		}
	}
	if !enabled || s.sites == nil {
		return out
	}
	// The registry hash is deliberately cleared after an interrupted or
	// external content mutation. If an internet publication exists, calculate
	// the exact storage snapshot instead of turning that unknown state into a
	// false "needs update" warning. The full status endpoint also requests
	// eligibility details; the list endpoint only needs the comparison hash.
	if !inspectFiles && (out.Publication == nil || out.ContentHash != "") {
		return out
	}
	snap, err := s.snapshotCloudflareSite(ctx, site)
	if err == nil {
		out.ContentHash = snap.ContentHash
		if inspectFiles {
			eligibility := checkCloudflareEligibility(snap)
			out.Eligibility = &eligibility
		}
	}
	return out
}

func cloudflarePublicationForAPI(pub *cloudflarePublication) *cloudflarePublication {
	if pub == nil {
		return nil
	}
	copy := *pub
	copy.AccessEmails = append([]string(nil), pub.AccessEmails...)
	copy.RequestedAccessEmails = append([]string(nil), pub.RequestedAccessEmails...)
	copy.LastError = cloudflarePublicError(pub.LastError)
	return &copy
}

func cloudflarePublicError(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "access edge protection"):
		return "Email access protection did not become active in time; retry to continue"
	case strings.Contains(lower, "already exists but is not managed by spot"):
		return "An internet project with this name already exists and is not managed by Spot"
	case strings.Contains(lower, "dns already has a conflicting record"):
		return "The internet hostname already has a conflicting record"
	case strings.Contains(lower, "conflicts with a domain outside project"):
		return "The internet hostname is attached outside project managed by Spot"
	case strings.Contains(lower, "context canceled"):
		return "Publishing was interrupted; retry to continue"
	case strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "timeout"):
		return "Publishing timed out; retry to continue"
	case strings.Contains(lower, "cleanup location is unknown"):
		return "The legacy internet cleanup location is unknown"
	case strings.Contains(lower, "project ownership"):
		return "Internet project ownership must be resolved before continuing"
	case strings.Contains(lower, "access application") && strings.Contains(lower, "uncertain"):
		return "Email access setup must be resolved before continuing"
	default:
		return "Publishing could not be completed; retry to continue"
	}
}

func (s *Server) requireCloudflareSite(w http.ResponseWriter, r *http.Request) (string, Identity, bool) {
	if !s.requireSitesAPI(w, r) {
		return "", Identity{}, false
	}
	if s.siteManager == nil {
		httpError(w, http.StatusServiceUnavailable, "site manager not configured")
		return "", Identity{}, false
	}
	site := r.PathValue("name")
	if !siteNameRe.MatchString(site) {
		httpError(w, http.StatusBadRequest, "invalid site name")
		return "", Identity{}, false
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return "", Identity{}, false
	}
	allowed, err := s.siteManager.CanManageSite(r.Context(), site, actor)
	switch {
	case errors.Is(err, ErrSiteNotFound):
		httpError(w, http.StatusNotFound, "no site named "+site)
		return "", Identity{}, false
	case err != nil:
		log.Printf("cloudflare %s: authorize: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize Cloudflare action")
		return "", Identity{}, false
	case !allowed:
		httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can manage this Cloudflare publication")
		return "", Identity{}, false
	}
	return site, actor, true
}

func (s *Server) handleCloudflareStatus(w http.ResponseWriter, r *http.Request) {
	site, actor, ok := s.requireCloudflareSite(w, r)
	if !ok {
		return
	}
	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	defer siteLock.Unlock()
	allowed, err := s.siteManager.CanManageSite(r.Context(), site, actor)
	if errors.Is(err, ErrSiteNotFound) {
		httpError(w, http.StatusNotFound, "no site named "+site)
		return
	}
	if err != nil {
		log.Printf("cloudflare %s status: reauthorize: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize Cloudflare action")
		return
	}
	if !allowed {
		httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can manage this Cloudflare publication")
		return
	}
	out := s.cloudflareSummaryForSite(r.Context(), site, "", true)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCloudflarePublish(w http.ResponseWriter, r *http.Request) {
	site, actor, ok := s.requireCloudflareSite(w, r)
	if !ok {
		return
	}
	if s.cloudflare == nil || !s.cloudflare.cfg.Enabled() {
		httpError(w, http.StatusServiceUnavailable, "Cloudflare publishing is not configured")
		return
	}
	if s.sites == nil {
		httpError(w, http.StatusServiceUnavailable, "site store not configured")
		return
	}
	policy, err := decodeCloudflarePublishRequest(w, r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if policy.Mode == cloudflareAccessRestricted && !s.cloudflare.cfg.AccessEnabled() {
		httpError(w, http.StatusServiceUnavailable, "Cloudflare Access email restriction is not configured")
		return
	}
	if !s.beginCloudflareJob(site) {
		// A reload or duplicate click may submit the same operation while the
		// original request continues in the background. Do not queue a second
		// deployment or silently accept a different requested access policy.
		httpError(w, http.StatusConflict, "Publishing is already in progress")
		return
	}
	jobActive := true
	finishJob := func() {
		if !jobActive {
			return
		}
		s.endCloudflareJob(site)
		jobActive = false
	}
	defer finishJob()

	cloudflareLock := s.cloudflareMutationLock(site)
	cloudflareLock.Lock()
	defer cloudflareLock.Unlock()

	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	allowed, authErr := s.siteManager.CanManageSite(r.Context(), site, actor)
	if authErr != nil || !allowed {
		siteLock.Unlock()
		if errors.Is(authErr, ErrSiteNotFound) {
			httpError(w, http.StatusNotFound, "no site named "+site)
		} else if authErr != nil {
			log.Printf("cloudflare %s: reauthorize: %v", site, authErr)
			httpError(w, http.StatusInternalServerError, "could not authorize Cloudflare action")
		} else {
			httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can manage this Cloudflare publication")
		}
		return
	}
	snap, err := s.snapshotCloudflareSite(r.Context(), site)
	if err != nil {
		siteLock.Unlock()
		log.Printf("cloudflare publish %s: snapshot: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not read the site's current files")
		return
	}
	eligibility := checkCloudflareEligibility(snap)
	if !eligibility.Eligible {
		siteLock.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":       "site is not eligible for Cloudflare Pages publishing",
			"eligibility": eligibility,
		})
		return
	}
	if _, err := s.cloudflare.reserve(r.Context(), site, snap, policy); err != nil {
		siteLock.Unlock()
		log.Printf("cloudflare publish %s: reserve: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not reserve the Cloudflare publication")
		return
	}
	siteLock.Unlock()
	// External publication is intentionally independent of the browser request.
	// Closing or reloading the page must not turn a successful remote mutation
	// into a durable local failure. The operation remains bounded so shutdown and
	// genuinely stalled provider calls can still finish predictably.
	publishCtx, cancelPublish := context.WithTimeout(context.Background(), cloudflarePublishTimeout)
	defer cancelPublish()
	pub, err := s.cloudflare.publish(publishCtx, site, snap, policy)
	if err != nil {
		finishJob()
		if errors.Is(err, errCloudflareCleanupUnknown) {
			httpError(w, http.StatusConflict, cloudflarePublicError(err.Error()))
			return
		}
		log.Printf("cloudflare publish %s: %v", site, err)
		httpError(w, http.StatusBadGateway, cloudflarePublicError(err.Error()))
		return
	}
	finishJob()
	writeJSON(w, http.StatusOK, cloudflareStatusJSON{
		ConfigStatus:    s.cloudflare.status(),
		Enabled:         true,
		AccessEnabled:   s.cloudflare.cfg.AccessEnabled(),
		OperationActive: false,
		Site:            site,
		Hostname:        pub.Hostname,
		ContentHash:     snap.ContentHash,
		Publication:     cloudflarePublicationForAPI(&pub),
		Eligibility:     &eligibility,
	})
}

type cloudflareResolveAccessRequest struct {
	ConfirmAbsent bool `json:"confirm_absent"`
}

func decodeCloudflareResolveAccessRequest(w http.ResponseWriter, r *http.Request) (cloudflareResolveAccessRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCloudflarePublishBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request cloudflareResolveAccessRequest
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("invalid Access resolution request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return request, errors.New("invalid Access resolution request: expected one JSON object")
	}
	return request, nil
}

func (s *Server) handleCloudflareResolveAccess(w http.ResponseWriter, r *http.Request) {
	site, actor, ok := s.requireCloudflareSite(w, r)
	if !ok {
		return
	}
	if s.cloudflare == nil || !s.cloudflare.cfg.Enabled() {
		httpError(w, http.StatusServiceUnavailable, "Cloudflare publishing is not configured")
		return
	}
	request, err := decodeCloudflareResolveAccessRequest(w, r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	cloudflareLock := s.cloudflareMutationLock(site)
	cloudflareLock.Lock()
	defer cloudflareLock.Unlock()

	// Ownership can change while this request waits behind a publication.
	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	allowed, authErr := s.siteManager.CanManageSite(r.Context(), site, actor)
	siteLock.Unlock()
	if authErr != nil || !allowed {
		if errors.Is(authErr, ErrSiteNotFound) {
			httpError(w, http.StatusNotFound, "no site named "+site)
		} else if authErr != nil {
			log.Printf("cloudflare Access resolution %s: reauthorize: %v", site, authErr)
			httpError(w, http.StatusInternalServerError, "could not authorize Cloudflare action")
		} else {
			httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can manage this Cloudflare publication")
		}
		return
	}

	pub, result, err := s.cloudflare.resolveUncertainAccessApplication(r.Context(), site, request.ConfirmAbsent)
	if err != nil {
		switch {
		case errors.Is(err, ErrSiteNotFound):
			httpError(w, http.StatusNotFound, "this site is not published to Cloudflare")
		case errors.Is(err, errCloudflareAccessNotUncertain):
			httpError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errCloudflareCleanupUnknown):
			httpError(w, http.StatusConflict, err.Error())
		default:
			log.Printf("cloudflare Access resolution %s: %v", site, err)
			httpError(w, http.StatusBadGateway, "could not resolve Cloudflare Access state: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, cloudflareStatusJSON{
		ConfigStatus:  s.cloudflare.status(),
		Enabled:       true,
		AccessEnabled: s.cloudflare.cfg.AccessEnabled(),
		Site:          site,
		Hostname:      pub.Hostname,
		Publication:   cloudflarePublicationForAPI(pub),
	})
	log.Printf("cloudflare Access resolution %s: %s", site, result)
}

type cloudflareResolveProjectRequest struct {
	Resolution string `json:"resolution"`
}

func (s *Server) handleCloudflareResolveProject(w http.ResponseWriter, r *http.Request) {
	site, actor, ok := s.requireCloudflareSite(w, r)
	if !ok {
		return
	}
	if s.cloudflare == nil || !s.cloudflare.cfg.Enabled() {
		httpError(w, http.StatusServiceUnavailable, "Cloudflare publishing is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCloudflarePublishBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request cloudflareResolveProjectRequest
	if err := decoder.Decode(&request); err != nil {
		httpError(w, http.StatusBadRequest, "invalid project resolution request: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "invalid project resolution request: expected one JSON object")
		return
	}

	cloudflareLock := s.cloudflareMutationLock(site)
	cloudflareLock.Lock()
	defer cloudflareLock.Unlock()
	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	allowed, authErr := s.siteManager.CanManageSite(r.Context(), site, actor)
	siteLock.Unlock()
	if authErr != nil || !allowed {
		if errors.Is(authErr, ErrSiteNotFound) {
			httpError(w, http.StatusNotFound, "no site named "+site)
		} else if authErr != nil {
			log.Printf("cloudflare project resolution %s: reauthorize: %v", site, authErr)
			httpError(w, http.StatusInternalServerError, "could not authorize Cloudflare action")
		} else {
			httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can manage this Cloudflare publication")
		}
		return
	}

	pub, result, err := s.cloudflare.resolveUncertainProject(r.Context(), site, request.Resolution)
	if err != nil {
		switch {
		case errors.Is(err, ErrSiteNotFound):
			httpError(w, http.StatusNotFound, "this site is not published to Cloudflare")
		case errors.Is(err, errCloudflareProjectNotPending), errors.Is(err, errCloudflareCleanupUnknown):
			httpError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errCloudflareProjectResolution):
			httpError(w, http.StatusBadRequest, err.Error())
		default:
			log.Printf("cloudflare project resolution %s: %v", site, err)
			httpError(w, http.StatusBadGateway, "could not resolve Cloudflare Pages project state: "+err.Error())
		}
		return
	}
	hostname := s.cloudflare.cfg.Hostname(site)
	if pub != nil {
		hostname = pub.Hostname
	}
	writeJSON(w, http.StatusOK, cloudflareStatusJSON{
		ConfigStatus: s.cloudflare.status(), Enabled: true, AccessEnabled: s.cloudflare.cfg.AccessEnabled(),
		Site: site, Hostname: hostname, Publication: cloudflarePublicationForAPI(pub),
	})
	log.Printf("cloudflare project resolution %s: %s", site, result)
}

type cloudflareResolveLegacyRequest struct {
	ConfirmResourcesRemoved bool `json:"confirm_resources_removed"`
}

func (s *Server) handleCloudflareResolveLegacy(w http.ResponseWriter, r *http.Request) {
	site, actor, ok := s.requireCloudflareSite(w, r)
	if !ok {
		return
	}
	if s.cloudflare == nil {
		httpError(w, http.StatusServiceUnavailable, "Cloudflare publication state is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCloudflarePublishBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request cloudflareResolveLegacyRequest
	if err := decoder.Decode(&request); err != nil {
		httpError(w, http.StatusBadRequest, "invalid legacy cleanup request: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "invalid legacy cleanup request: expected one JSON object")
		return
	}

	cloudflareLock := s.cloudflareMutationLock(site)
	cloudflareLock.Lock()
	defer cloudflareLock.Unlock()
	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	allowed, authErr := s.siteManager.CanManageSite(r.Context(), site, actor)
	siteLock.Unlock()
	if authErr != nil || !allowed {
		if errors.Is(authErr, ErrSiteNotFound) {
			httpError(w, http.StatusNotFound, "no site named "+site)
		} else if authErr != nil {
			log.Printf("cloudflare legacy resolution %s: reauthorize: %v", site, authErr)
			httpError(w, http.StatusInternalServerError, "could not authorize Cloudflare action")
		} else {
			httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can manage this Cloudflare publication")
		}
		return
	}
	if err := s.cloudflare.resolveLegacyCleanup(r.Context(), site, request.ConfirmResourcesRemoved); err != nil {
		switch {
		case errors.Is(err, ErrSiteNotFound):
			httpError(w, http.StatusNotFound, "this site is not published to Cloudflare")
		case errors.Is(err, errCloudflareCleanupNotUnknown):
			httpError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errCloudflareLegacyConfirm):
			httpError(w, http.StatusBadRequest, err.Error())
		default:
			log.Printf("cloudflare legacy resolution %s: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not resolve legacy Cloudflare cleanup")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": site, "resolved": true})
}

func (s *Server) handleCloudflareUnpublish(w http.ResponseWriter, r *http.Request) {
	site, actor, ok := s.requireCloudflareSite(w, r)
	if !ok {
		return
	}
	cloudflareLock := s.cloudflareMutationLock(site)
	cloudflareLock.Lock()
	defer cloudflareLock.Unlock()

	// Authorization can become stale while this request waits behind another
	// publish/unpublish. Recheck under the Cloudflare lock and coordinate with
	// site deletion/reclaim before touching the current publication.
	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	allowed, authErr := s.siteManager.CanManageSite(r.Context(), site, actor)
	siteLock.Unlock()
	if authErr != nil || !allowed {
		if errors.Is(authErr, ErrSiteNotFound) {
			httpError(w, http.StatusNotFound, "no site named "+site)
		} else if authErr != nil {
			log.Printf("cloudflare unpublish %s: reauthorize: %v", site, authErr)
			httpError(w, http.StatusInternalServerError, "could not authorize Cloudflare action")
		} else {
			httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can manage this Cloudflare publication")
		}
		return
	}
	if err := s.cloudflare.unpublish(r.Context(), site); err != nil {
		if errors.Is(err, ErrSiteNotFound) {
			httpError(w, http.StatusNotFound, "this site is not published to Cloudflare")
			return
		}
		if errors.Is(err, errCloudflareNotConfigured) {
			httpError(w, http.StatusServiceUnavailable, "Cloudflare publishing is not configured")
			return
		}
		if errors.Is(err, errCloudflareCleanupUnknown) {
			httpError(w, http.StatusConflict, err.Error()+"; resolve the legacy Cloudflare resources manually")
			return
		}
		log.Printf("cloudflare unpublish %s: %v", site, err)
		httpError(w, http.StatusBadGateway, "could not unpublish from Cloudflare: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": site, "unpublished": true})
}
