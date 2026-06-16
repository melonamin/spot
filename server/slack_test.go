package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type slackAPIFixture struct {
	status     int
	ok         bool
	slackErr   string
	retryAfter string
}

type slackAPIServer struct {
	*httptest.Server
	lastBody          map[string]any
	lastAuthorization string
	lastContentType   string
	fixture           slackAPIFixture
}

func newSlackAPI(t *testing.T) *slackAPIServer {
	t.Helper()
	api := &slackAPIServer{fixture: slackAPIFixture{status: http.StatusOK, ok: true}}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.lastAuthorization = r.Header.Get("Authorization")
		api.lastContentType = r.Header.Get("Content-Type")
		if r.URL.Path != "/chat.postMessage" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&api.lastBody); err != nil {
			t.Errorf("decode Slack body: %v", err)
		}
		if api.fixture.retryAfter != "" {
			w.Header().Set("Retry-After", api.fixture.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		status := api.fixture.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if api.fixture.ok {
			w.Write([]byte(`{"ok":true,"channel":"C123","ts":"1503435956.000247"}`))
			return
		}
		if api.fixture.slackErr == "" {
			api.fixture.slackErr = "unknown_error"
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": api.fixture.slackErr})
	}))
	t.Cleanup(api.Close)
	return api
}

func slackTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	return &Server{
		slack:       NewSlackProxy("test-token", upstream),
		slackAccess: slackAccessVisitors,
		sites:       newTestSiteStore(t),
		spotDomain:  "spot.localhost",
	}
}

func postSlack(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://demo.spot.localhost/api/slack/send",
		strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestSlackSend(t *testing.T) {
	api := newSlackAPI(t)
	srv := slackTestServer(t, api.URL)

	rec := postSlack(t, srv, `{"channel":"#signups","text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: status %d body %s", rec.Code, rec.Body)
	}
	var res slackSendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !res.OK || res.Channel != "C123" || res.TS != "1503435956.000247" {
		t.Fatalf("response = %+v", res)
	}
	if api.lastAuthorization != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want Bearer token", api.lastAuthorization)
	}
	if api.lastContentType != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", api.lastContentType)
	}
	if api.lastBody["channel"] != "#signups" || api.lastBody["text"] != "hello" {
		t.Fatalf("upstream body = %+v", api.lastBody)
	}
}

func TestSlackSendOwnersOnly(t *testing.T) {
	api := newSlackAPI(t)
	srv := &Server{
		slack:       NewSlackProxy("test-token", api.URL),
		slackAccess: slackAccessOwners,
		siteManager: fakeSiteManager{allowed: false},
		resolver:    NewStaticResolver("visitor@example.com", "Visitor", nil),
		sites:       newTestSiteStore(t),
		spotDomain:  "spot.localhost",
	}

	rec := postSlack(t, srv, `{"channel":"#signups","text":"hello"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner Slack send: status %d body %s, want 403", rec.Code, rec.Body)
	}

	srv.siteManager = fakeSiteManager{allowed: true}
	rec = postSlack(t, srv, `{"channel":"#signups","text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner Slack send: status %d body %s, want 200", rec.Code, rec.Body)
	}
}

func TestSlackSendPolicyCanOptVisitorsIn(t *testing.T) {
	api := newSlackAPI(t)
	policies := NewPolicyStore(t.TempDir(), time.Hour)
	policies.Set("demo", &AccessPolicy{Allow: []string{"visitor@example.com"}, Slack: slackAccessVisitors}, nil)
	srv := &Server{
		slack:       NewSlackProxy("test-token", api.URL),
		slackAccess: slackAccessOwners,
		policies:    policies,
		resolver:    NewStaticResolver("visitor@example.com", "Visitor", nil),
		spotDomain:  "spot.localhost",
	}

	rec := postSlack(t, srv, `{"channel":"#signups","text":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("visitor opt-in Slack send: status %d body %s, want 200", rec.Code, rec.Body)
	}
}

func TestSlackSendValidation(t *testing.T) {
	api := newSlackAPI(t)
	srv := slackTestServer(t, api.URL)

	if rec := postSlack(t, srv, `{"text":"hello"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing channel: status %d, want 400", rec.Code)
	}
	if rec := postSlack(t, srv, `{"channel":"#signups"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing text and blocks: status %d, want 400", rec.Code)
	}
	if rec := postSlack(t, srv, `{"channel":"#signups","blocks":[]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty blocks array: status %d, want 400", rec.Code)
	}
	if rec := postSlack(t, srv, `{"channel":"#signups","blocks":"oops"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("non-array blocks: status %d, want 400", rec.Code)
	}
	if rec := postSlack(t, srv, `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: status %d, want 400", rec.Code)
	}

	unconfigured := &Server{spotDomain: "spot.localhost"}
	if rec := postSlack(t, unconfigured, `{"channel":"#signups","text":"hello"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured proxy: status %d, want 503", rec.Code)
	}
}

func TestSlackSendBlocksAndMrkdwn(t *testing.T) {
	api := newSlackAPI(t)
	srv := slackTestServer(t, api.URL)

	rec := postSlack(t, srv, `{"channel":"#signups","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*hi*"}}],"mrkdwn":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("send blocks: status %d body %s", rec.Code, rec.Body)
	}
	if api.lastBody["text"] != nil {
		t.Fatalf("upstream body unexpectedly included text: %+v", api.lastBody)
	}
	if api.lastBody["mrkdwn"] != false {
		t.Fatalf("upstream mrkdwn = %v, want false", api.lastBody["mrkdwn"])
	}
	blocks, ok := api.lastBody["blocks"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("upstream blocks = %+v", api.lastBody["blocks"])
	}
}

func TestSlackSendUpstreamErrors(t *testing.T) {
	tests := []struct {
		name           string
		slackErr       string
		retryAfter     string
		wantStatus     int
		wantRetryAfter string
	}{
		{name: "channel not found", slackErr: "channel_not_found", wantStatus: http.StatusNotFound},
		{name: "bad request", slackErr: "msg_too_long", wantStatus: http.StatusBadRequest},
		{name: "rate limited", slackErr: "rate_limited", retryAfter: "3", wantStatus: http.StatusTooManyRequests, wantRetryAfter: "3"},
		{name: "invalid auth", slackErr: "invalid_auth", wantStatus: http.StatusBadGateway},
		{name: "unknown", slackErr: "something_new", wantStatus: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newSlackAPI(t)
			api.fixture = slackAPIFixture{
				status:     http.StatusOK,
				ok:         false,
				slackErr:   tt.slackErr,
				retryAfter: tt.retryAfter,
			}
			srv := slackTestServer(t, api.URL)

			rec := postSlack(t, srv, `{"channel":"#signups","text":"hello"}`)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d body %s, want %d", rec.Code, rec.Body, tt.wantStatus)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			// 5xx (credential/server-class) errors must not leak the raw Slack
			// error to the browser; caller-actionable 4xx errors keep it.
			body := rec.Body.String()
			if tt.wantStatus >= 500 {
				if strings.Contains(body, tt.slackErr) {
					t.Errorf("5xx response leaks raw Slack error %q: %s", tt.slackErr, body)
				}
			} else if !strings.Contains(body, tt.slackErr) {
				t.Errorf("4xx response should retain Slack error %q: %s", tt.slackErr, body)
			}
		})
	}
}

func TestSlackErrorStatus(t *testing.T) {
	tests := []struct {
		slackErr   string
		wantStatus int
	}{
		{"channel_not_found", http.StatusNotFound},
		{"not_in_channel", http.StatusBadRequest},
		{"msg_too_long", http.StatusBadRequest},
		{"no_text", http.StatusBadRequest},
		{"is_archived", http.StatusBadRequest},
		{"rate_limited", http.StatusTooManyRequests},
		{"invalid_auth", http.StatusBadGateway},
		{"token_revoked", http.StatusBadGateway},
		{"account_inactive", http.StatusBadGateway},
		{"something_new", http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.slackErr, func(t *testing.T) {
			if status := slackErrorStatus(tt.slackErr); status != tt.wantStatus {
				t.Fatalf("slackErrorStatus(%q) = %d; want %d", tt.slackErr, status, tt.wantStatus)
			}
		})
	}
}

func TestSlackVisitorsAllowed(t *testing.T) {
	if !(&Server{slackAccess: slackAccessVisitors}).slackVisitorsAllowed(nil) {
		t.Fatal("visitors deployment should allow Slack for visitors")
	}
	if !(&Server{slackAccess: slackAccessOwners}).slackVisitorsAllowed(&AccessPolicy{Slack: slackAccessVisitors}) {
		t.Fatal("site opt-in should allow Slack for visitors")
	}
	if (&Server{slackAccess: slackAccessOwners}).slackVisitorsAllowed(&AccessPolicy{Slack: slackAccessOwners}) {
		t.Fatal("owners-only site should not allow Slack for visitors")
	}
}

func TestSlackPostMessageNonOKHTTP(t *testing.T) {
	api := newSlackAPI(t)
	api.fixture = slackAPIFixture{status: http.StatusInternalServerError, ok: false, slackErr: "server_error"}
	proxy := NewSlackProxy("test-token", api.URL)

	_, err := proxy.postMessage(context.Background(), slackSendRequest{Channel: "#signups", Text: "hello"})
	var slackErr slackError
	if err == nil || !strings.Contains(err.Error(), "server_error") {
		t.Fatalf("postMessage error = %v, want Slack server_error", err)
	}
	if !errors.As(err, &slackErr) || slackErr.status != http.StatusBadGateway {
		t.Fatalf("postMessage error = %#v, want slackError 502", err)
	}
}

func TestSlackPostMessageHTTPRateLimit(t *testing.T) {
	api := newSlackAPI(t)
	api.fixture = slackAPIFixture{status: http.StatusTooManyRequests, ok: false, slackErr: "rate_limited", retryAfter: "7"}
	proxy := NewSlackProxy("test-token", api.URL)

	_, err := proxy.postMessage(context.Background(), slackSendRequest{Channel: "#signups", Text: "hello"})
	var slackErr slackError
	if !errors.As(err, &slackErr) {
		t.Fatalf("postMessage error = %#v, want slackError", err)
	}
	if slackErr.status != http.StatusTooManyRequests || slackErr.retryAfter != "7" {
		t.Fatalf("slackError = %+v, want 429/Retry-After 7", slackErr)
	}
}

func TestSlackPostMessageHTTPRateLimitWithoutJSON(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "11")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(api.Close)
	proxy := NewSlackProxy("test-token", api.URL)

	_, err := proxy.postMessage(context.Background(), slackSendRequest{Channel: "#signups", Text: "hello"})
	var slackErr slackError
	if !errors.As(err, &slackErr) {
		t.Fatalf("postMessage error = %#v, want slackError", err)
	}
	if slackErr.status != http.StatusTooManyRequests || slackErr.retryAfter != "11" {
		t.Fatalf("slackError = %+v, want 429/Retry-After 11", slackErr)
	}
}
