package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDocumentOwnershipOverHTTP exercises ownership end to end: the create
// handler stamps the resolved visitor as owner, list?mine=true returns only
// the caller's documents, and mine without an identity fails loud.
func TestDocumentOwnershipOverHTTP(t *testing.T) {
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	srv := &Server{
		store:      &DocStore{db: db},
		resolver:   NewStaticResolver("alice@example.com", "Alice", nil),
		policies:   NewPolicyStore(t.TempDir(), time.Second),
		spotDomain: "spot.localhost",
	}

	create := func(body string) Document {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://demo.spot.localhost/api/db/notes",
			strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: status %d body %s", rec.Code, rec.Body)
		}
		var doc Document
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode created: %v", err)
		}
		return doc
	}

	alice := create(`{"title": "alice note"}`)
	if alice.Owner != "alice@example.com" {
		t.Errorf("created owner = %q, want alice@example.com", alice.Owner)
	}

	srv.resolver = NewStaticResolver("bob@example.com", "Bob", nil)
	create(`{"title": "bob note"}`)

	list := func(query string) []Document {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://demo.spot.localhost/api/db/notes"+query, nil)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s: status %d body %s", query, rec.Code, rec.Body)
		}
		var out struct {
			Documents []Document `json:"documents"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return out.Documents
	}

	// Bob is the current visitor; mine returns only his document.
	if mine := list("?mine=true"); len(mine) != 1 || mine[0].Owner != "bob@example.com" {
		t.Errorf("bob mine = %+v, want 1 doc owned by bob", mine)
	}
	if all := list(""); len(all) != 2 {
		t.Errorf("list all = %d docs, want 2", len(all))
	}

	// Without a resolver the caller cannot be identified, so mine must fail
	// loud rather than silently returning the wrong set.
	srv.resolver = nil
	req := httptest.NewRequest(http.MethodGet, "http://demo.spot.localhost/api/db/notes?mine=true", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mine without identity = %d, want 400", rec.Code)
	}
}
