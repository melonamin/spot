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

	"github.com/anthropics/anthropic-sdk-go/option"
)

// newClaudeAPI serves the Messages API wire shape, capturing the request
// body so defaults can be asserted.
func newClaudeAPI(t *testing.T, lastBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("x-api-key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(lastBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "msg_test", "type": "message", "role": "assistant",
			"model": "claude-opus-4-8",
			"content": [{"type": "text", "text": "Hello from Claude"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 12, "output_tokens": 5}
		}`))
	}))
}

func aiTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	return &Server{
		ai:         NewAIProxy("test-key", []string{"claude-haiku-4-5"}, option.WithBaseURL(upstream)),
		aiAccess:   aiAccessVisitors,
		spotDomain: "spot.localhost",
	}
}

func postChat(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://demo.spot.localhost/api/ai/chat",
		strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestAIChat(t *testing.T) {
	var upstreamBody map[string]any
	api := newClaudeAPI(t, &upstreamBody)
	defer api.Close()
	srv := aiTestServer(t, api.URL)

	rec := postChat(t, srv, `{"messages": [{"role": "user", "content": "Say hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status %d body %s", rec.Code, rec.Body)
	}
	var res aiChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Text != "Hello from Claude" || res.StopReason != "end_turn" {
		t.Errorf("response = %+v", res)
	}
	if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", res.Usage)
	}

	// Server-side defaults: current Opus model, thinking enabled with a budget
	// bounded below max_tokens so the text reply always has room.
	if upstreamBody["model"] != defaultAIModel {
		t.Errorf("upstream model = %v, want %s", upstreamBody["model"], defaultAIModel)
	}
	thinking, _ := upstreamBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Errorf("upstream thinking = %v, want enabled", upstreamBody["thinking"])
	}
	wantBudget := float64(defaultAITokens - aiOutputReserveTokens)
	if thinking["budget_tokens"] != wantBudget {
		t.Errorf("upstream thinking budget = %v, want %v", thinking["budget_tokens"], wantBudget)
	}
	if upstreamBody["max_tokens"] != float64(defaultAITokens) {
		t.Errorf("upstream max_tokens = %v, want %d", upstreamBody["max_tokens"], defaultAITokens)
	}
}

func TestAIChatOverrides(t *testing.T) {
	var upstreamBody map[string]any
	api := newClaudeAPI(t, &upstreamBody)
	defer api.Close()
	srv := aiTestServer(t, api.URL)

	rec := postChat(t, srv, `{
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
			{"role": "user", "content": "again"}
		],
		"model": "claude-haiku-4-5",
		"system": "Be terse.",
		"max_tokens": 999999
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status %d body %s", rec.Code, rec.Body)
	}
	if upstreamBody["model"] != "claude-haiku-4-5" {
		t.Errorf("upstream model = %v", upstreamBody["model"])
	}
	// Requested max_tokens above the cap is clamped, not rejected.
	if upstreamBody["max_tokens"] != float64(maxAITokens) {
		t.Errorf("upstream max_tokens = %v, want clamped %d", upstreamBody["max_tokens"], maxAITokens)
	}
	msgs, _ := upstreamBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Errorf("upstream messages count = %d, want 3", len(msgs))
	}
}

func TestAIChatCustomUpstream(t *testing.T) {
	var upstreamBody map[string]any
	api := newClaudeAPI(t, &upstreamBody)
	defer api.Close()
	// The config-driven constructor: a base URL routes requests to the
	// custom upstream, empty means the real Claude API. A set-but-empty
	// ANTHROPIC_BASE_URL in the process environment (compose renders an
	// unset variable that way) must not override the configured value.
	t.Setenv("ANTHROPIC_BASE_URL", "")
	srv := &Server{
		ai:         NewAIProxyWithUpstream("test-key", api.URL, "", nil),
		aiAccess:   aiAccessVisitors,
		spotDomain: "spot.localhost",
	}

	rec := postChat(t, srv, `{"messages": [{"role": "user", "content": "Say hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat via custom upstream: status %d body %s", rec.Code, rec.Body)
	}
	var res aiChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Text != "Hello from Claude" {
		t.Errorf("response = %+v", res)
	}
	// An empty model config keeps the platform default.
	if upstreamBody["model"] != defaultAIModel {
		t.Errorf("upstream model = %v, want %s", upstreamBody["model"], defaultAIModel)
	}
}

func TestAIChatDeploymentDefaultModel(t *testing.T) {
	var upstreamBody map[string]any
	api := newClaudeAPI(t, &upstreamBody)
	defer api.Close()
	srv := &Server{
		ai:         NewAIProxyWithUpstream("test-key", api.URL, "claude-sonnet-4-6", []string{"claude-haiku-4-5"}),
		aiAccess:   aiAccessVisitors,
		spotDomain: "spot.localhost",
	}

	// A request that names no model gets the deployment default.
	rec := postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status %d body %s", rec.Code, rec.Body)
	}
	if upstreamBody["model"] != "claude-sonnet-4-6" {
		t.Errorf("upstream model = %v, want claude-sonnet-4-6", upstreamBody["model"])
	}

	// A per-request model still wins over the deployment default.
	rec = postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}], "model": "claude-haiku-4-5"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat with model: status %d body %s", rec.Code, rec.Body)
	}
	if upstreamBody["model"] != "claude-haiku-4-5" {
		t.Errorf("upstream model = %v, want claude-haiku-4-5", upstreamBody["model"])
	}
}

func TestAIChatRejectsUnallowedModel(t *testing.T) {
	api := newClaudeAPI(t, &map[string]any{})
	defer api.Close()
	srv := &Server{
		ai:         NewAIProxyWithUpstream("test-key", api.URL, "claude-sonnet-4-6", nil),
		aiAccess:   aiAccessVisitors,
		spotDomain: "spot.localhost",
	}

	rec := postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}], "model": "claude-haiku-4-5"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat with unallowed model: status %d body %s, want 403", rec.Code, rec.Body)
	}
}

type fakeSiteManager struct {
	allowed bool
	err     error
}

func (m fakeSiteManager) CanManageSite(_ context.Context, _ string, _ Identity) (bool, error) {
	return m.allowed, m.err
}

func TestAIChatOwnersOnly(t *testing.T) {
	api := newClaudeAPI(t, &map[string]any{})
	defer api.Close()

	srv := &Server{
		ai:          NewAIProxyWithUpstream("test-key", api.URL, "", nil),
		aiAccess:    aiAccessOwners,
		siteManager: fakeSiteManager{allowed: true},
		resolver:    NewStaticResolver("owner@example.com", "Owner", nil),
		spotDomain:  "spot.localhost",
	}
	rec := postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner AI chat: status %d body %s, want 200", rec.Code, rec.Body)
	}

	srv.siteManager = fakeSiteManager{allowed: false}
	rec = postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner AI chat: status %d body %s, want 403", rec.Code, rec.Body)
	}

	srv.siteManager = fakeSiteManager{err: ErrSiteNotFound}
	rec = postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing site AI chat: status %d body %s, want 404", rec.Code, rec.Body)
	}

	srv.siteManager = fakeSiteManager{err: errors.New("db down")}
	rec = postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("manager error AI chat: status %d body %s, want 500", rec.Code, rec.Body)
	}
}

func TestAIChatPolicyCanOptVisitorsIn(t *testing.T) {
	api := newClaudeAPI(t, &map[string]any{})
	defer api.Close()
	policies := NewPolicyStore(t.TempDir(), time.Hour)
	policies.Set("demo", &AccessPolicy{Allow: []string{"visitor@example.com"}, AI: aiAccessVisitors}, nil)
	srv := &Server{
		ai:         NewAIProxyWithUpstream("test-key", api.URL, "", nil),
		aiAccess:   aiAccessOwners,
		policies:   policies,
		resolver:   NewStaticResolver("visitor@example.com", "Visitor", nil),
		spotDomain: "spot.localhost",
	}

	rec := postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("visitor opt-in AI chat: status %d body %s, want 200", rec.Code, rec.Body)
	}
}

func TestAIChatValidation(t *testing.T) {
	api := newClaudeAPI(t, &map[string]any{})
	defer api.Close()
	srv := aiTestServer(t, api.URL)

	if rec := postChat(t, srv, `{"messages": []}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty messages: status %d, want 400", rec.Code)
	}
	if rec := postChat(t, srv, `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: status %d, want 400", rec.Code)
	}
	if rec := postChat(t, srv, `{"messages": [{"role": "system", "content": "x"}]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad role: status %d, want 400", rec.Code)
	}

	unconfigured := &Server{spotDomain: "spot.localhost"}
	if rec := postChat(t, unconfigured, `{"messages": [{"role": "user", "content": "x"}]}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured proxy: status %d, want 503", rec.Code)
	}
}
