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
	bob := create(`{"title": "bob note"}`)

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
	firstPage := list("?limit=1")
	if len(firstPage) != 1 {
		t.Fatalf("first page = %d docs, want 1", len(firstPage))
	}
	secondPage := list("?limit=1&after=" + firstPage[0].ID)
	if len(secondPage) != 1 {
		t.Fatalf("second page = %d docs, want 1", len(secondPage))
	}
	if secondPage[0].ID == firstPage[0].ID {
		t.Fatalf("second page repeated cursor document %s", firstPage[0].ID)
	}

	updateMine := func(id, body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "http://demo.spot.localhost/api/db/notes/"+id+"?mine=true",
			strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := updateMine(alice.ID, `{"title": "stolen"}`); code != http.StatusNotFound {
		t.Fatalf("bob update alice mine = %d, want 404", code)
	}
	if code := updateMine(bob.ID, `{"title": "bob edited"}`); code != http.StatusOK {
		t.Fatalf("bob update bob mine = %d, want 200", code)
	}

	deleteMine := func(id string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "http://demo.spot.localhost/api/db/notes/"+id+"?mine=true", nil)
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := deleteMine(alice.ID); code != http.StatusNotFound {
		t.Fatalf("bob delete alice mine = %d, want 404", code)
	}
	if code := deleteMine(bob.ID); code != http.StatusNoContent {
		t.Fatalf("bob delete bob mine = %d, want 204", code)
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
