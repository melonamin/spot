package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		ai:         NewAIProxy("test-key", option.WithBaseURL(upstream)),
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

	// Server-side defaults: current Opus model, adaptive thinking.
	if upstreamBody["model"] != defaultAIModel {
		t.Errorf("upstream model = %v, want %s", upstreamBody["model"], defaultAIModel)
	}
	thinking, _ := upstreamBody["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("upstream thinking = %v, want adaptive", upstreamBody["thinking"])
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
