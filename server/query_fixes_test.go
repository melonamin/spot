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

// TestCreateAndMineFailOnResolverError covers the callerKey fix: a resolver
// outage must fail loud (503) on create and on mine, never silently stamp or
// filter an empty owner.
func TestCreateAndMineFailOnResolverError(t *testing.T) {
	srv := newQueryTestServer(t)
	srv.resolver = errResolver{err: errors.New("resolver unreachable")}

	if rec := doJSON(t, srv, http.MethodPost, "http://demo.spot.localhost/api/db/notes",
		`{"title":"x"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("create with failing resolver = %d, want 503 (body %s)", rec.Code, rec.Body)
	}
	if rec := doJSON(t, srv, http.MethodGet, "http://demo.spot.localhost/api/db/notes?mine=true", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("list mine with failing resolver = %d, want 503 (body %s)", rec.Code, rec.Body)
	}
}
