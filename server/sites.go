package main

import (
	"context"
	"errors"
	"fmt"
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

type SiteManager interface {
	CanManageSite(ctx context.Context, site string, actor Identity) (bool, error)
}

type ownedSiteJSON struct {
	Name            string    `json:"name"`
	URL             string    `json:"url"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	Tags            []string  `json:"tags"`
	DownloadAllowed bool      `json:"download_allowed"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	FileCount       int       `json:"file_count"`
	TotalBytes      int64     `json:"total_bytes"`
	Restricted      bool      `json:"restricted"`
	AllowCount      int       `json:"allow_count"`
	Cloudflare      any       `json:"cloudflare,omitempty"`
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
			Cloudflare:      s.cloudflareSummaryForSite(r.Context(), site.Name),
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

	// Everything scoped to the site goes: served files, uploads, and
	// private collections. If purge fails, the registry row stays claimed
	// so the owner can retry instead of freeing a partially purged name.
	removedFiles := 0
	purge := func(ctx context.Context) error {
		if s.cloudflarePubs != nil {
			published, err := s.cloudflarePubs.Has(ctx, site)
			if err != nil {
				return fmt.Errorf("check cloudflare publication: %w", err)
			}
			if published {
				return errCloudflarePublicationExists
			}
		}
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
	case errors.Is(err, errCloudflarePublicationExists):
		httpError(w, http.StatusConflict, "unpublish this site from Cloudflare before deleting it")
	case err != nil:
		log.Printf("delete site %s: %v", site, err)
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "delete", Status: "failed",
			Message: "purge or registry delete failed",
		})
		httpError(w, http.StatusInternalServerError, "could not delete the site")
	default:
		if s.policies != nil {
			s.policies.Invalidate(site)
		}
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "delete", Status: "success",
			FileCount: removedFiles,
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
	ConfigStatus string                 `json:"config_status"`
	Enabled      bool                   `json:"enabled"`
	Site         string                 `json:"site,omitempty"`
	Hostname     string                 `json:"hostname,omitempty"`
	ContentHash  string                 `json:"content_hash,omitempty"`
	Publication  *cloudflarePublication `json:"publication,omitempty"`
	Eligibility  *cloudflareEligibility `json:"eligibility,omitempty"`
}

func (s *Server) cloudflareSummaryForSite(ctx context.Context, site string) any {
	status := cloudflareConfigDisabled
	enabled := false
	hostname := ""
	if s.cloudflare != nil {
		status = s.cloudflare.status()
		enabled = s.cloudflare.cfg.Enabled()
		hostname = s.cloudflare.cfg.Hostname(site)
	}
	out := cloudflareStatusJSON{
		ConfigStatus: status,
		Enabled:      enabled,
		Site:         site,
		Hostname:     hostname,
	}
	if s.cloudflarePubs != nil {
		pub, err := s.cloudflarePubs.Get(ctx, site)
		if err == nil {
			out.Publication = pub
		}
	}
	if !enabled || s.sites == nil {
		return out
	}
	snap, err := s.snapshotCloudflareSite(ctx, site)
	if err == nil {
		eligibility := checkCloudflareEligibility(snap)
		out.ContentHash = snap.ContentHash
		out.Eligibility = &eligibility
	}
	return out
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
		httpError(w, http.StatusForbidden, "only the site owner or a platform admin can manage this Cloudflare publication")
		return "", Identity{}, false
	}
	return site, actor, true
}

func (s *Server) handleCloudflareStatus(w http.ResponseWriter, r *http.Request) {
	site, _, ok := s.requireCloudflareSite(w, r)
	if !ok {
		return
	}
	out := s.cloudflareSummaryForSite(r.Context(), site)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCloudflarePublish(w http.ResponseWriter, r *http.Request) {
	site, _, ok := s.requireCloudflareSite(w, r)
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

	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	snap, err := s.snapshotCloudflareSite(r.Context(), site)
	siteLock.Unlock()
	if err != nil {
		log.Printf("cloudflare publish %s: snapshot: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not read the site's current files")
		return
	}
	eligibility := checkCloudflareEligibility(snap)
	if !eligibility.Eligible {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":       "site is not eligible for Cloudflare Pages publishing",
			"eligibility": eligibility,
		})
		return
	}
	pub, err := s.cloudflare.publish(r.Context(), site, snap)
	if err != nil {
		log.Printf("cloudflare publish %s: %v", site, err)
		httpError(w, http.StatusBadGateway, "could not publish to Cloudflare: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cloudflareStatusJSON{
		ConfigStatus: s.cloudflare.status(),
		Enabled:      true,
		Site:         site,
		Hostname:     s.cloudflare.cfg.Hostname(site),
		ContentHash:  snap.ContentHash,
		Publication:  &pub,
		Eligibility:  &eligibility,
	})
}

func (s *Server) handleCloudflareUnpublish(w http.ResponseWriter, r *http.Request) {
	site, _, ok := s.requireCloudflareSite(w, r)
	if !ok {
		return
	}
	if s.cloudflare == nil || !s.cloudflare.cfg.Enabled() {
		httpError(w, http.StatusServiceUnavailable, "Cloudflare publishing is not configured")
		return
	}
	if err := s.cloudflare.unpublish(r.Context(), site); err != nil {
		if errors.Is(err, ErrSiteNotFound) {
			httpError(w, http.StatusNotFound, "this site is not published to Cloudflare")
			return
		}
		log.Printf("cloudflare unpublish %s: %v", site, err)
		httpError(w, http.StatusBadGateway, "could not unpublish from Cloudflare: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": site, "unpublished": true})
}
