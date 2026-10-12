package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func pagingTestSites() []SiteRecord {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return []SiteRecord{
		{Name: "alpha", Title: "Zebra board", Tags: []string{"ops"}, CreatedAt: base, UpdatedAt: base.Add(3 * time.Hour)},
		{Name: "bravo", Tags: []string{"ops", "demo"}, CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(3 * time.Hour)},
		{Name: "charlie", Description: "Quarterly numbers", CreatedAt: base.Add(2 * time.Hour), UpdatedAt: base.Add(time.Hour)},
		{Name: "delta", OwnerName: "Dana", Tags: []string{"demo"}, CreatedAt: base.Add(3 * time.Hour), UpdatedAt: base.Add(2 * time.Hour)},
		{Name: "echo", Title: "apple", CreatedAt: base.Add(4 * time.Hour), UpdatedAt: base},
	}
}

func siteListQueryFor(t *testing.T, rawQuery string) siteListQuery {
	t.Helper()
	q, err := parseSiteListQuery(httptest.NewRequest("GET", "/api/sites/public?"+rawQuery, nil))
	if err != nil {
		t.Fatalf("parse %q: %v", rawQuery, err)
	}
	return q
}

func pageNames(sites []SiteRecord) []string {
	names := make([]string, len(sites))
	for i, site := range sites {
		names[i] = site.Name
	}
	return names
}

func walkSitePages(t *testing.T, sites []SiteRecord, rawQuery string) []string {
	t.Helper()
	var names []string
	cursor := ""
	for range len(sites) + 1 {
		query := rawQuery
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		page := pageSites(sites, func(s SiteRecord) SiteRecord { return s }, siteListQueryFor(t, query))
		names = append(names, pageNames(page.Items)...)
		if page.NextCursor == "" {
			return names
		}
		cursor = page.NextCursor
	}
	t.Fatalf("paging %q did not terminate", rawQuery)
	return nil
}

func TestPageSitesOrdersEachSortWithNameTiebreak(t *testing.T) {
	sites := pagingTestSites()
	tests := map[string][]string{
		"":             {"alpha", "bravo", "delta", "charlie", "echo"},
		"sort=updated": {"alpha", "bravo", "delta", "charlie", "echo"},
		"sort=created": {"echo", "delta", "charlie", "bravo", "alpha"},
		// Name sorts by the displayed label, case-insensitively.
		"sort=name": {"echo", "bravo", "charlie", "delta", "alpha"},
	}
	for query, want := range tests {
		page := pageSites(sites, func(s SiteRecord) SiteRecord { return s }, siteListQueryFor(t, query))
		if got := pageNames(page.Items); !slices.Equal(got, want) {
			t.Errorf("%q order = %v, want %v", query, got, want)
		}
		if page.NextCursor != "" {
			t.Errorf("%q unpaged list has next cursor", query)
		}
		for _, limit := range []int{1, 2, 4} {
			got := walkSitePages(t, sites, fmt.Sprintf("%s&limit=%d", query, limit))
			if !slices.Equal(got, want) {
				t.Errorf("%q limit %d pages = %v, want %v", query, limit, got, want)
			}
		}
	}
}

func TestPageSitesFiltersAndCounts(t *testing.T) {
	sites := pagingTestSites()
	page := pageSites(sites, func(s SiteRecord) SiteRecord { return s }, siteListQueryFor(t, "tag=demo&limit=1"))
	if got := pageNames(page.Items); !slices.Equal(got, []string{"bravo"}) || page.NextCursor == "" {
		t.Fatalf("tag page = %v next %q", got, page.NextCursor)
	}
	if page.Total != 5 || page.Matched != 2 {
		t.Fatalf("tag counts total=%d matched=%d, want 5 and 2", page.Total, page.Matched)
	}
	wantTags := []siteStatsTagJSON{{Tag: "demo", Count: 2}, {Tag: "ops", Count: 2}}
	if !slices.Equal(page.TagCounts, wantTags) {
		t.Fatalf("tag counts = %v, want %v", page.TagCounts, wantTags)
	}

	for query, want := range map[string][]string{
		"q=QUARTERLY":            {"charlie"},
		"q=dana":                 {"delta"},
		"q=zebra":                {"alpha"},
		"q=demo":                 {"bravo", "delta"},
		"names=echo,alpha,ghost": {"alpha", "echo"},
		"names=bravo&tag=ops":    {"bravo"},
		"q=nothing-matches":      nil,
	} {
		page := pageSites(sites, func(s SiteRecord) SiteRecord { return s }, siteListQueryFor(t, query))
		if got := pageNames(page.Items); !slices.Equal(got, want) {
			t.Errorf("%q = %v, want %v", query, got, want)
		}
		if page.Matched != len(want) || page.Total != len(sites) {
			t.Errorf("%q matched=%d total=%d", query, page.Matched, page.Total)
		}
	}
}

// A keyset cursor resumes after the last site shown, so a redeploy between
// requests never repeats a site already shown. An unseen site redeployed
// meanwhile moves above the cursor and waits for a fresh first page.
func TestPageSitesCursorSurvivesReordering(t *testing.T) {
	sites := pagingTestSites()
	first := pageSites(sites, func(s SiteRecord) SiteRecord { return s }, siteListQueryFor(t, "limit=2"))
	if got := pageNames(first.Items); !slices.Equal(got, []string{"alpha", "bravo"}) {
		t.Fatalf("first page = %v", got)
	}
	// alpha (already shown) and charlie (unseen) are both redeployed; only
	// sites still below the cursor remain.
	sites[0].UpdatedAt = sites[0].UpdatedAt.Add(24 * time.Hour)
	sites[2].UpdatedAt = sites[2].UpdatedAt.Add(24 * time.Hour)
	rest := walkSitePages(t, sites, "limit=2&cursor="+url.QueryEscape(first.NextCursor))
	if !slices.Equal(rest, []string{"delta", "echo"}) {
		t.Fatalf("remaining pages = %v, want delta and echo without repeats", rest)
	}
}

func TestParseSiteListQueryRejectsBadInput(t *testing.T) {
	nameCursor := siteListQueryFor(t, "sort=name").encodeCursor(siteSortKey{label: "x", name: "alpha"})
	tooMany := make([]string, siteListMaxNames+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("site-%d", i)
	}
	for query, want := range map[string]string{
		"limit=0":                               "limit must be",
		"limit=201":                             "limit must be",
		"limit=ten":                             "limit must be",
		"sort=owner":                            "sort must be",
		"cursor=not-base64!":                    "cursor is not valid",
		"cursor=" + url.QueryEscape(nameCursor): "different sort",
		"names=UPPER":                           "names must be",
		"names=" + strings.Join(tooMany, ","):   "at most",
	} {
		_, err := parseSiteListQuery(httptest.NewRequest("GET", "/api/sites/public?"+query, nil))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.40q error = %v, want %q", query, err, want)
		}
	}
	q := siteListQueryFor(t, "sort=name&cursor="+url.QueryEscape(nameCursor))
	if q.Limit != siteListMaxLimit {
		t.Errorf("cursor without limit = %d, want %d", q.Limit, siteListMaxLimit)
	}
}

type siteListResponse struct {
	Sites []struct {
		Name string `json:"name"`
	} `json:"sites"`
	Total      int                `json:"total"`
	Matched    int                `json:"matched"`
	NextCursor string             `json:"next_cursor"`
	TagCounts  []siteStatsTagJSON `json:"tag_counts"`
	Active     int                `json:"active"`
	Deleted    int                `json:"deleted"`
}

func (st *loginTestStack) listSites(t *testing.T, viewer, path string) siteListResponse {
	t.Helper()
	rec := st.do(asForwardUser(sitesRequestFor(path), viewer))
	if rec.Code != 200 {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	var body siteListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func (r siteListResponse) names() []string {
	names := make([]string, len(r.Sites))
	for i, site := range r.Sites {
		names[i] = site.Name
	}
	return names
}

func newPagingTestStack(t *testing.T) *loginTestStack {
	t.Helper()
	st := newLoginTestStack(t)
	st.deploy(t, "owner@example.com", "open-a", "")
	st.deploy(t, "owner@example.com", "open-b", "")
	st.deploy(t, "owner@example.com", "open-c", "")
	st.deploy(t, "owner@example.com", "secret", `{"allow":["owner@example.com"]}`)
	ctx := context.Background()
	tags := map[string][]string{"open-a": {"demo"}, "open-b": {"demo", "ops"}, "secret": {"classified"}}
	for i, name := range []string{"open-a", "open-b", "open-c", "secret"} {
		if err := st.registry.UpdateSiteMetadata(ctx, name, SiteMetadata{Tags: tags[name]}); err != nil {
			t.Fatal(err)
		}
		// Pin the order: secret newest, then open-c, open-b, open-a.
		at := time.Date(2026, 10, 1, i, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05.000")
		if _, err := st.registry.db.ExecContext(ctx, `UPDATE sites SET updated_at = ? WHERE name = ?`, at, name); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// Restricted sites a viewer cannot see must not leak into a page, its
// counts, or its tag facet.
func TestPublicSitesPagingHidesRestrictedSites(t *testing.T) {
	st := newPagingTestStack(t)
	first := st.listSites(t, "viewer@example.com", "/api/sites/public?limit=2")
	if got := first.names(); !slices.Equal(got, []string{"open-c", "open-b"}) || first.NextCursor == "" {
		t.Fatalf("first page = %v next %q", got, first.NextCursor)
	}
	if first.Total != 3 || first.Matched != 3 {
		t.Fatalf("counts total=%d matched=%d, want 3 and 3", first.Total, first.Matched)
	}
	wantTags := []siteStatsTagJSON{{Tag: "demo", Count: 2}, {Tag: "ops", Count: 1}}
	if !slices.Equal(first.TagCounts, wantTags) {
		t.Fatalf("tag counts = %v, want %v", first.TagCounts, wantTags)
	}
	second := st.listSites(t, "viewer@example.com", "/api/sites/public?limit=2&cursor="+url.QueryEscape(first.NextCursor))
	if got := second.names(); !slices.Equal(got, []string{"open-a"}) || second.NextCursor != "" {
		t.Fatalf("second page = %v next %q", got, second.NextCursor)
	}
	// Even an exact request by name and tag does not reveal the site.
	if got := st.listSites(t, "owner@example.com", "/api/sites/public?names=secret,open-a&tag=classified"); got.Matched != 0 {
		t.Fatalf("restricted site matched in public listing: %+v", got)
	}

	rec := st.do(asForwardUser(sitesRequestFor("/api/sites/public?limit=0"), "viewer@example.com"))
	if rec.Code != 400 {
		t.Fatalf("limit=0 = %d, want 400", rec.Code)
	}
}

func TestVisibleSitesPagingCountsOnlyViewableSites(t *testing.T) {
	st := newPagingTestStack(t)
	outsider := st.listSites(t, "viewer@example.com", "/api/sites/visible?limit=10&tag=classified")
	if outsider.Total != 3 || outsider.Matched != 0 || len(outsider.Sites) != 0 {
		t.Fatalf("outsider visible = %+v, want the restricted site hidden", outsider)
	}
	for _, tag := range outsider.TagCounts {
		if tag.Tag == "classified" {
			t.Fatalf("outsider tag counts leak restricted tag: %v", outsider.TagCounts)
		}
	}
	owner := st.listSites(t, "owner@example.com", "/api/sites/visible?limit=1")
	if got := owner.names(); !slices.Equal(got, []string{"secret"}) || owner.Total != 4 || owner.NextCursor == "" {
		t.Fatalf("owner visible = %+v", owner)
	}
}

func TestManageableSitesPagingReportsStateCounts(t *testing.T) {
	st := newPagingTestStack(t)
	rec := st.do(asForwardUser(siteRequest("DELETE", "sites.localhost:8443", "/api/sites/open-a"), "owner@example.com"))
	if rec.Code != 200 {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	page := st.listSites(t, "owner@example.com", "/api/sites/manageable?limit=2&q=open")
	if page.Active != 3 || page.Deleted != 1 || page.Total != 4 || page.Matched != 3 {
		t.Fatalf("manageable counts = %+v", page)
	}
	if len(page.Sites) != 2 || page.NextCursor == "" {
		t.Fatalf("manageable page = %+v", page)
	}
	if other := st.listSites(t, "viewer@example.com", "/api/sites/manageable?limit=2"); other.Total != 0 || len(other.Sites) != 0 {
		t.Fatalf("non-manager sees %+v", other)
	}
	mine := st.listSites(t, "owner@example.com", "/api/sites/mine?limit=2&sort=name")
	if got := mine.names(); !slices.Equal(got, []string{"open-b", "open-c"}) || mine.Total != 3 {
		t.Fatalf("mine page = %+v", mine)
	}
}
