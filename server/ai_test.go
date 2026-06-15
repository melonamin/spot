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

func newOpenAICompatAPI(t *testing.T, lastChatBody, lastImageBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			if err := json.NewDecoder(r.Body).Decode(lastChatBody); err != nil {
				t.Errorf("decode chat body: %v", err)
			}
			w.Write([]byte(`{
				"model": "gpt-5",
				"choices": [{
					"finish_reason": "stop",
					"message": {"role": "assistant", "content": "Hello from the gateway"}
				}],
				"usage": {"prompt_tokens": 12, "completion_tokens": 5}
			}`))
		case "/v1/images/generations":
			if err := json.NewDecoder(r.Body).Decode(lastImageBody); err != nil {
				t.Errorf("decode image body: %v", err)
			}
			w.Write([]byte(`{
				"data": [{
					"b64_json": "/9j/aW1hZ2U=",
					"revised_prompt": "A small blue house"
				}],
				"usage": {"total_tokens": 42}
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func aiTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	return &Server{
		ai:         NewAIProxy("test-key", upstream, "", []string{"gpt-4o-mini"}, nil),
		aiAccess:   aiAccessVisitors,
		sites:      newTestSiteStore(t),
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

func postImage(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://demo.spot.localhost/api/ai/image",
		strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestAIChat(t *testing.T) {
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
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
	if res.Text != "Hello from the gateway" || res.StopReason != "stop" {
		t.Errorf("response = %+v", res)
	}
	if res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if chatBody["model"] != defaultAIModel {
		t.Errorf("upstream model = %v, want %s", chatBody["model"], defaultAIModel)
	}
	if chatBody["max_completion_tokens"] != float64(defaultAITokens) {
		t.Errorf("upstream max_completion_tokens = %v, want %d", chatBody["max_completion_tokens"], defaultAITokens)
	}
}

func TestAIChatOverrides(t *testing.T) {
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
	defer api.Close()
	srv := aiTestServer(t, api.URL)

	rec := postChat(t, srv, `{
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
			{"role": "user", "content": "again"}
		],
		"model": "gpt-4o-mini",
		"system": "Be terse.",
		"max_tokens": 999999
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status %d body %s", rec.Code, rec.Body)
	}
	if chatBody["model"] != "gpt-4o-mini" {
		t.Errorf("upstream model = %v", chatBody["model"])
	}
	if chatBody["max_completion_tokens"] != float64(maxAITokens) {
		t.Errorf("upstream max_completion_tokens = %v, want clamped %d", chatBody["max_completion_tokens"], maxAITokens)
	}
	msgs, _ := chatBody["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("upstream messages count = %d, want 4", len(msgs))
	}
	system, _ := msgs[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "Be terse." {
		t.Errorf("system message = %+v", system)
	}
}

func TestAIChatDeploymentDefaultModel(t *testing.T) {
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
	defer api.Close()
	srv := &Server{
		ai:         NewAIProxy("test-key", api.URL, "gateway-default", []string{"gpt-4o-mini"}, nil),
		aiAccess:   aiAccessVisitors,
		sites:      newTestSiteStore(t),
		spotDomain: "spot.localhost",
	}

	rec := postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: status %d body %s", rec.Code, rec.Body)
	}
	if chatBody["model"] != "gateway-default" {
		t.Errorf("upstream model = %v, want gateway-default", chatBody["model"])
	}

	rec = postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}], "model": "gpt-4o-mini"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat with model: status %d body %s", rec.Code, rec.Body)
	}
	if chatBody["model"] != "gpt-4o-mini" {
		t.Errorf("upstream model = %v, want gpt-4o-mini", chatBody["model"])
	}
}

func TestAIChatRejectsUnallowedModel(t *testing.T) {
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
	defer api.Close()
	srv := &Server{
		ai:         NewAIProxy("test-key", api.URL, "gateway-default", nil, nil),
		aiAccess:   aiAccessVisitors,
		sites:      newTestSiteStore(t),
		spotDomain: "spot.localhost",
	}

	rec := postChat(t, srv, `{"messages": [{"role": "user", "content": "hi"}], "model": "gpt-4o-mini"}`)
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
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
	defer api.Close()

	srv := &Server{
		ai:          NewAIProxy("test-key", api.URL, "", nil, nil),
		aiAccess:    aiAccessOwners,
		siteManager: fakeSiteManager{allowed: true},
		resolver:    NewStaticResolver("owner@example.com", "Owner", nil),
		sites:       newTestSiteStore(t),
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
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
	defer api.Close()
	policies := NewPolicyStore(t.TempDir(), time.Hour)
	policies.Set("demo", &AccessPolicy{Allow: []string{"visitor@example.com"}, AI: aiAccessVisitors}, nil)
	srv := &Server{
		ai:         NewAIProxy("test-key", api.URL, "", nil, nil),
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
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
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

func TestAIImageOpenAICompatible(t *testing.T) {
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
	defer api.Close()

	srv := aiTestServer(t, api.URL)
	rec := postImage(t, srv, `{
		"prompt": "draw a house",
		"size": "1024x1024",
		"quality": "high",
		"output_format": "webp"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("image: status %d body %s", rec.Code, rec.Body)
	}
	if imageBody["model"] != defaultOpenAIImageModel {
		t.Errorf("upstream model = %v, want %s", imageBody["model"], defaultOpenAIImageModel)
	}
	if imageBody["size"] != "1024x1024" || imageBody["quality"] != "high" || imageBody["output_format"] != "webp" {
		t.Errorf("upstream options = %+v", imageBody)
	}
	var res aiImageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Provider != "openai" || res.Model != defaultOpenAIImageModel {
		t.Errorf("response provider/model = %s/%s", res.Provider, res.Model)
	}
	if len(res.Images) != 1 || res.Images[0].B64 != "/9j/aW1hZ2U=" || res.Images[0].MIMEType != "image/jpeg" {
		t.Fatalf("images = %+v", res.Images)
	}
	if res.Images[0].DataURL != "data:image/jpeg;base64,/9j/aW1hZ2U=" {
		t.Errorf("data_url = %q", res.Images[0].DataURL)
	}
}

func TestAIImageGatewayModelAlias(t *testing.T) {
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
	defer api.Close()
	srv := &Server{
		ai:         NewAIProxy("test-key", api.URL, "", nil, nil),
		aiAccess:   aiAccessVisitors,
		sites:      newTestSiteStore(t),
		spotDomain: "spot.localhost",
	}
	srv.ai.ConfigureImages(defaultGeminiImageModel, nil)

	rec := postImage(t, srv, `{"prompt": "draw a pear", "aspect_ratio": "16:9", "image_size": "2K"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("image: status %d body %s", rec.Code, rec.Body)
	}
	if imageBody["model"] != defaultGeminiImageModel {
		t.Errorf("upstream model = %v, want %s", imageBody["model"], defaultGeminiImageModel)
	}
	if imageBody["aspect_ratio"] != "16:9" || imageBody["image_size"] != "2K" {
		t.Errorf("image sizing options = %+v", imageBody)
	}
}

func TestAIImageValidation(t *testing.T) {
	var chatBody, imageBody map[string]any
	api := newOpenAICompatAPI(t, &chatBody, &imageBody)
	defer api.Close()
	srv := aiTestServer(t, api.URL)

	if rec := postImage(t, srv, `{"prompt": ""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty prompt: status %d, want 400", rec.Code)
	}
	if rec := postImage(t, srv, `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: status %d, want 400", rec.Code)
	}
	if rec := postImage(t, srv, `{"prompt": "x", "model": "other-image-model"}`); rec.Code != http.StatusForbidden {
		t.Errorf("unallowed model: status %d, want 403", rec.Code)
	}

	unconfigured := &Server{spotDomain: "spot.localhost"}
	if rec := postImage(t, unconfigured, `{"prompt": "x"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured proxy: status %d, want 503", rec.Code)
	}
}
