package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	siteListMaxLimit = 200
	siteListMaxNames = 200
	siteListTopTags  = 18
	// siteListWorkers bounds the storage reads (policies, screenshots,
	// publication summaries) one listing request runs at a time.
	siteListWorkers = 8
)

// siteListQuery is the shared filter and paging contract of the
// /api/sites listing endpoints. Filters apply after visibility, so they
// only narrow what the caller may already see. A request without limit
// or cursor gets the whole filtered list.
type siteListQuery struct {
	Limit  int // 0 means unpaged
	Sort   string
	Search string
	Tag    string
	Names  map[string]bool
	// Yours keeps only the viewer's own sites (true) or only other people's
	// (false); nil keeps both.
	Yours  *bool
	viewer Identity
	cursor *siteListCursor
}

// siteListCursor is the keyset position of the last site on a page. It
// carries the sort key rather than an offset, so a redeploy that reorders
// the list between requests never repeats a site already returned. Under
// the time sorts, a later-page site redeployed meanwhile moves above the
// cursor and is left out of the remaining pages; a fresh first page shows
// it.
type siteListCursor struct {
	Sort string `json:"s"`
	Key  string `json:"k"`
	Name string `json:"n"`
}

type siteListPage[T any] struct {
	Items      []T
	Total      int
	Matched    int
	NextCursor string
	TagCounts  []siteStatsTagJSON
}

func parseSiteListQuery(r *http.Request, viewer Identity) (siteListQuery, error) {
	values := r.URL.Query()
	q := siteListQuery{Sort: "updated", viewer: viewer}
	switch sortMode := values.Get("sort"); sortMode {
	case "":
	case "updated", "created", "name":
		q.Sort = sortMode
	default:
		return q, errors.New("sort must be updated, created, or name")
	}
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > siteListMaxLimit {
			return q, errors.New("limit must be an integer between 1 and " + strconv.Itoa(siteListMaxLimit))
		}
		q.Limit = n
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeSiteListCursor(raw)
		if err != nil {
			return q, errors.New("cursor is not valid")
		}
		if cursor.Sort != q.Sort {
			return q, errors.New("cursor belongs to a different sort; restart from the first page")
		}
		q.cursor = &cursor
		if q.Limit == 0 {
			q.Limit = siteListMaxLimit
		}
	}
	if raw := values.Get("yours"); raw != "" {
		yours, err := strconv.ParseBool(raw)
		if err != nil {
			return q, errors.New("yours must be true or false")
		}
		q.Yours = &yours
	}
	q.Search = strings.ToLower(strings.TrimSpace(values.Get("q")))
	q.Tag = strings.TrimSpace(values.Get("tag"))
	if raw := values.Get("names"); raw != "" {
		names := strings.Split(raw, ",")
		if len(names) > siteListMaxNames {
			return q, errors.New("names accepts at most " + strconv.Itoa(siteListMaxNames) + " site names")
		}
		q.Names = make(map[string]bool, len(names))
		for _, name := range names {
			if !siteNameRe.MatchString(name) {
				return q, errors.New("names must be a comma-separated list of site names")
			}
			q.Names[name] = true
		}
	}
	return q, nil
}

func decodeSiteListCursor(raw string) (siteListCursor, error) {
	var cursor siteListCursor
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor, err
	}
	if err := json.Unmarshal(data, &cursor); err != nil {
		return cursor, err
	}
	if !siteNameRe.MatchString(cursor.Name) {
		return cursor, errors.New("cursor site name")
	}
	if cursor.Sort != "name" {
		if _, err := time.Parse(time.RFC3339Nano, cursor.Key); err != nil {
			return cursor, err
		}
	}
	return cursor, nil
}

type siteSortKey struct {
	at    time.Time
	label string
	name  string
}

func (q siteListQuery) sortKey(site SiteRecord) siteSortKey {
	key := siteSortKey{name: site.Name}
	switch q.Sort {
	case "created":
		key.at = site.CreatedAt
	case "name":
		label := site.Title
		if label == "" {
			label = site.Name
		}
		key.label = strings.ToLower(label)
	default:
		key.at = site.UpdatedAt
	}
	return key
}

// before orders newest first for the time sorts and alphabetically by
// label for name, with the unique site name breaking ties.
//
// Ordering is the gallery's editorial voice: strict recency turns it into a
// changelog, where one busy person's redeploys crowd out everything else. A
// variety boost (e.g. cap consecutive tiles per owner) would keep the wall
// communal.
func (q siteListQuery) before(a, b siteSortKey) bool {
	if q.Sort == "name" {
		if a.label != b.label {
			return a.label < b.label
		}
	} else if !a.at.Equal(b.at) {
		return a.at.After(b.at)
	}
	return a.name < b.name
}

func (q siteListQuery) encodeCursor(key siteSortKey) string {
	cursor := siteListCursor{Sort: q.Sort, Key: key.label, Name: key.name}
	if q.Sort != "name" {
		cursor.Key = key.at.UTC().Format(time.RFC3339Nano)
	}
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func (q siteListQuery) cursorKey() siteSortKey {
	key := siteSortKey{name: q.cursor.Name}
	if q.Sort == "name" {
		key.label = q.cursor.Key
	} else {
		key.at, _ = time.Parse(time.RFC3339Nano, q.cursor.Key)
	}
	return key
}

func (q siteListQuery) matches(site SiteRecord) bool {
	if q.Names != nil && !q.Names[site.Name] {
		return false
	}
	if q.Tag != "" && !containsString(site.Tags, q.Tag) {
		return false
	}
	if q.Yours != nil && site.OwnedBy(q.viewer) != *q.Yours {
		return false
	}
	if q.Search == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join(append([]string{
		site.Name, site.Title, site.Description, ownerDisplay(site),
	}, site.Tags...), " "))
	return strings.Contains(haystack, q.Search)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// pageSites filters, orders, and slices sites the caller may already see.
// Total and TagCounts describe that whole visible set, so a client can
// show what a filter narrowed down from.
func pageSites[T any](items []T, record func(T) SiteRecord, q siteListQuery) siteListPage[T] {
	page := siteListPage[T]{Total: len(items)}
	tagCounts := map[string]int{}
	type keyed struct {
		item T
		key  siteSortKey
	}
	matched := make([]keyed, 0, len(items))
	for _, item := range items {
		site := record(item)
		for _, tag := range site.Tags {
			tagCounts[tag]++
		}
		if q.matches(site) {
			matched = append(matched, keyed{item: item, key: q.sortKey(site)})
		}
	}
	page.TagCounts = buildTopTags(tagCounts, siteListTopTags)
	page.Matched = len(matched)
	sort.Slice(matched, func(i, j int) bool { return q.before(matched[i].key, matched[j].key) })

	start := 0
	if q.cursor != nil {
		after := q.cursorKey()
		start = sort.Search(len(matched), func(i int) bool { return q.before(after, matched[i].key) })
	}
	end := len(matched)
	if q.Limit > 0 && start+q.Limit < end {
		end = start + q.Limit
		page.NextCursor = q.encodeCursor(matched[end-1].key)
	}
	page.Items = make([]T, 0, end-start)
	for _, entry := range matched[start:end] {
		page.Items = append(page.Items, entry.item)
	}
	return page
}

// siteListBody is the listing response. Paging fields are additive, so
// clients that only read "sites" keep working.
func siteListBody[T, J any](page siteListPage[T], sites []J) map[string]any {
	body := map[string]any{"sites": sites, "total": page.Total, "matched": page.Matched}
	if page.NextCursor != "" {
		body["next_cursor"] = page.NextCursor
	}
	return body
}

// forEachConcurrently runs fn for every index in [0, n) on a bounded
// worker pool. Listing enrichment reads site storage, and serial reads add
// up quickly on object storage.
func forEachConcurrently(n int, fn func(int)) {
	workers := min(siteListWorkers, n)
	if workers <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				fn(i)
			}
		}()
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}
