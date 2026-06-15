package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// errResolver fails every lookup, standing in for a transient identity
// resolver outage.
type errResolver struct{ err error }

func (e errResolver) Resolve(_ context.Context, _ string) (Identity, bool, error) {
	return Identity{}, false, e.err
}

func newQueryTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openSQLiteDB(context.Background(), filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Server{
		store:      &DocStore{db: db},
		resolver:   NewStaticResolver("alice@example.com", "Alice", nil),
		policies:   NewPolicyStore(t.TempDir(), time.Second),
		spotDomain: "spot.localhost",
	}
}

func doJSON(t *testing.T, srv *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, r)
	return rec
}

// TestWhereEmptyOperatorRejected covers the parseWhere fix: an empty operator
// object must 400 rather than silently dropping the filter and returning the
// whole collection.
func TestWhereEmptyOperatorRejected(t *testing.T) {
	srv := newQueryTestServer(t)
	for i := 0; i < 3; i++ {
		if rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/tasks",
			`{"status":"open"}`); rec.Code != http.StatusCreated {
			t.Fatalf("seed create: %d %s", rec.Code, rec.Body)
		}
	}
	// {"status":{}} would otherwise contribute no filter and match everything.
	const where = `?where=%7B%22status%22%3A%7B%7D%7D`
	if rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/tasks"+where, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("list with empty operator = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/tasks/count"+where, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("count with empty operator = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
}

// TestWhereNullRejected covers the parseWhere fix: top-level JSON null must
// 400 rather than being treated as an omitted filter.
func TestWhereNullRejected(t *testing.T) {
	srv := newQueryTestServer(t)
	if rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/tasks",
		`{"status":"open"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed create: %d %s", rec.Code, rec.Body)
	}

	const where = `?where=null`
	if rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/tasks"+where, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("list with null where = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/tasks/count"+where, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("count with null where = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
}

// TestComparisonNullRejected covers the buildWhere fix: a null value for an
// ordering operator must 400 rather than silently matching nothing.
func TestComparisonNullRejected(t *testing.T) {
	srv := newQueryTestServer(t)
	// {"score":{"gte":null}}
	const where = `?where=%7B%22score%22%3A%7B%22gte%22%3Anull%7D%7D`
	if rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/tasks"+where, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("gte:null = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
}

// TestListOrderHonoredWithoutSort covers the Query fix: order applies to the
// default created-at ordering, so order=asc is the exact reverse of the
// default and is not silently ignored.
func TestListOrderHonoredWithoutSort(t *testing.T) {
	srv := newQueryTestServer(t)
	for i := 0; i < 4; i++ {
		if rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/events",
			`{"n":1}`); rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", rec.Code, rec.Body)
		}
	}
	list := func(query string) []Document {
		t.Helper()
		rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/events"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s: %d %s", query, rec.Code, rec.Body)
		}
		var out struct {
			Documents []Document `json:"documents"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Documents
	}
	desc := list("")
	asc := list("?order=asc")
	if len(asc) != len(desc) || len(asc) != 4 {
		t.Fatalf("len asc=%d desc=%d, want 4", len(asc), len(desc))
	}
	for i := range asc {
		if asc[i].ID != desc[len(desc)-1-i].ID {
			t.Fatalf("order=asc is not the reverse of the default order at %d", i)
		}
	}
	// Ascending cursor paging walks forward, not in circles.
	first := list("?order=asc&limit=1")
	if len(first) != 1 || first[0].ID != asc[0].ID {
		t.Fatalf("asc page 1 = %+v, want oldest", first)
	}
	second := list("?order=asc&limit=1&after=" + first[0].ID)
	if len(second) != 1 || second[0].ID != asc[1].ID {
		t.Fatalf("asc page 2 = %+v, want second oldest", second)
	}
	if rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/events?order=sideways", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad order = %d, want 400", rec.Code)
	}
}

// TestSortTiebreakFollowsOrder covers the Query fix: with a custom sort the
// secondary id tiebreaker follows the requested order, so within a group of
// equal sort values order=asc is the exact reverse of order=desc rather than
// both collapsing to id DESC.
func TestSortTiebreakFollowsOrder(t *testing.T) {
	srv := newQueryTestServer(t)
	for i := 0; i < 4; i++ {
		if rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/scores",
			`{"score":1}`); rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", rec.Code, rec.Body)
		}
	}
	list := func(query string) []Document {
		t.Helper()
		rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/scores"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s: %d %s", query, rec.Code, rec.Body)
		}
		var out struct {
			Documents []Document `json:"documents"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Documents
	}
	asc := list("?sort=score&order=asc")
	desc := list("?sort=score&order=desc")
	if len(asc) != 4 || len(desc) != 4 {
		t.Fatalf("len asc=%d desc=%d, want 4", len(asc), len(desc))
	}
	for i := range asc {
		if asc[i].ID != desc[len(desc)-1-i].ID {
			t.Fatalf("sort tiebreak: order=asc is not the reverse of order=desc at %d", i)
		}
	}
}

// TestIncrementByZeroIsNoOp covers the increment fix: a zero delta returns the
// current document without bumping updated_at, while keeping the 404/409
// contract.
func TestIncrementByZeroIsNoOp(t *testing.T) {
	srv := newQueryTestServer(t)
	rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/counters",
		`{"views":5,"title":"post"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created Document
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec = doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/counters/"+created.ID+"/increment",
		`{"field":"views","by":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("increment 0: %d %s", rec.Code, rec.Body)
	}
	var got Document
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Data["views"] != float64(5) {
		t.Errorf("views after +0 = %v, want 5", got.Data["views"])
	}
	if !got.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("updated_at advanced on a no-op increment: %v -> %v", created.UpdatedAt, got.UpdatedAt)
	}

	// The 404 / 409 contract still holds at by:0.
	const missing = "00000000-0000-4000-8000-000000000000"
	if rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/counters/"+missing+"/increment",
		`{"field":"views","by":0}`); rec.Code != http.StatusNotFound {
		t.Errorf("increment 0 missing doc = %d, want 404", rec.Code)
	}
	if rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/counters/"+created.ID+"/increment",
		`{"field":"title","by":0}`); rec.Code != http.StatusConflict {
		t.Errorf("increment 0 non-numeric = %d, want 409", rec.Code)
	}
}

// TestIncrementNullCounterTreatsAsZero covers JSON null counters: null is a
// present JSON value in SQLite, but it should initialize the counter as zero.
func TestIncrementNullCounterTreatsAsZero(t *testing.T) {
	srv := newQueryTestServer(t)
	rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/counters",
		`{"views":null}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created Document
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec = doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/counters/"+created.ID+"/increment",
		`{"field":"views","by":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("increment null by 0: %d %s", rec.Code, rec.Body)
	}
	var unchanged Document
	if err := json.Unmarshal(rec.Body.Bytes(), &unchanged); err != nil {
		t.Fatalf("decode unchanged: %v", err)
	}
	if unchanged.Data["views"] != nil {
		t.Errorf("views after +0 = %v, want nil", unchanged.Data["views"])
	}

	rec = doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/counters/"+created.ID+"/increment",
		`{"field":"views","by":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("increment null by 2: %d %s", rec.Code, rec.Body)
	}
	var got Document
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if got.Data["views"] != float64(2) {
		t.Errorf("views after +2 = %v, want 2", got.Data["views"])
	}
}

// TestCreateBestEffortAndMineFailOnResolverError covers the callerKey policy:
// on an open site a resolver outage must not block writes — create proceeds
// best-effort with an empty owner — while a mine-scoped request still fails
// loud (503) rather than silently widening to every visitor's documents.
func TestCreateBestEffortAndMineFailOnResolverError(t *testing.T) {
	srv := newQueryTestServer(t)
	srv.resolver = errResolver{err: errors.New("resolver unreachable")}

	rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/notes",
		`{"title":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with failing resolver on open site = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	var doc struct {
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode created doc: %v", err)
	}
	if doc.Owner != "" {
		t.Errorf("created doc owner = %q, want empty (unattributed) during resolver outage", doc.Owner)
	}

	if rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/notes?mine=true", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("list mine with failing resolver = %d, want 503 (body %s)", rec.Code, rec.Body)
	}
}
