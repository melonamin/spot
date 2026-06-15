package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthReportsVersion locks the /health contract: it is reachable without
// a valid Spot host and reports the build version embedded via -ldflags.
func TestHealthReportsVersion(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "http://spot-api/health", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", rec.Code)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode /health body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if body.Version != version {
		t.Errorf("version = %q, want %q (the build version var)", body.Version, version)
	}
}
