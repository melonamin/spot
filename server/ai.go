package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const (
	defaultAIModel          = "gpt-5"
	defaultAITokens         = 16000
	maxAITokens             = 16000
	defaultOpenAIImageModel = "gpt-image-2"
	defaultGeminiImageModel = "gemini-3.1-flash-image"
	aiAccessOwners          = "owners"
	aiAccessVisitors        = "visitors"
)

// AIProxy forwards AI requests through an OpenAI-compatible gateway, so sites
// can call text and image models with zero browser-side configuration.
type AIProxy struct {
	httpClient *http.Client
	apiKey     string
	baseURL    string

	model         string
	allowedModels map[string]struct{}

	imageModel         string
	allowedImageModels map[string]struct{}
}

func NewAIProxy(apiKey, baseURL, model string, allowedModels, allowedImageModels []string) *AIProxy {
	proxy := &AIProxy{
		httpClient: http.DefaultClient,
		apiKey:     strings.TrimSpace(apiKey),
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		model:      strings.TrimSpace(model),
	}
	if proxy.baseURL == "" {
		proxy.baseURL = "https://api.openai.com"
	}
	if proxy.model == "" {
		proxy.model = defaultAIModel
	}
	proxy.setAllowedModels(allowedModels)
	proxy.setAllowedImageModels(allowedImageModels)
	return proxy
}

func (p *AIProxy) configured() bool {
	return p != nil && p.apiKey != ""
}

func (p *AIProxy) setAllowedModels(models []string) {
	p.allowedModels = map[string]struct{}{}
	p.allowedModels[p.model] = struct{}{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" {
			p.allowedModels[model] = struct{}{}
		}
	}
}

func (p *AIProxy) modelAllowed(model string) bool {
	_, ok := p.allowedModels[model]
	return ok
}

func (p *AIProxy) ConfigureImages(defaultModel string, allowedModels []string) {
	p.imageModel = strings.TrimSpace(defaultModel)
	p.setAllowedImageModels(allowedModels)
}

func (p *AIProxy) setAllowedImageModels(models []string) {
	p.allowedImageModels = map[string]struct{}{}
	p.allowedImageModels[defaultOpenAIImageModel] = struct{}{}
	p.allowedImageModels[defaultGeminiImageModel] = struct{}{}
	if p.imageModel == "" {
		p.imageModel = defaultOpenAIImageModel
	}
	p.allowedImageModels[p.imageModel] = struct{}{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" {
			p.allowedImageModels[model] = struct{}{}
		}
	}
}

func (p *AIProxy) imageModelAllowed(model string) bool {
	_, ok := p.allowedImageModels[model]
	return ok
}

type aiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type aiChatRequest struct {
	Messages  []aiChatMessage `json:"messages"`
	Model     string          `json:"model"`
	System    string          `json:"system"`
	MaxTokens int64           `json:"max_tokens"`
}

type aiChatResponse struct {
	Text       string `json:"text"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type aiImageRequest struct {
	Prompt       string `json:"prompt"`
	Model        string `json:"model"`
	Size         string `json:"size"`
	AspectRatio  string `json:"aspect_ratio"`
	ImageSize    string `json:"image_size"`
	Quality      string `json:"quality"`
	OutputFormat string `json:"output_format"`
	Background   string `json:"background"`
	N            int    `json:"n"`
}

type aiImageResponse struct {
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Images   []aiImageAsset  `json:"images"`
	Usage    json.RawMessage `json:"usage,omitempty"`
}

type aiImageAsset struct {
	B64           string `json:"b64,omitempty"`
	MIMEType      string `json:"mime_type,omitempty"`
	DataURL       string `json:"data_url,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// decodeChatRequest reads and validates a chat request body, writing the
// matching 400 response and returning ok=false when the body is not a JSON
// object or carries no messages. Shared by the buffered and streaming handlers
// so both enforce the same 1 MB cap and validation.
func decodeChatRequest(w http.ResponseWriter, r *http.Request) (aiChatRequest, bool) {
	var req aiChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "request body must be a JSON object with a messages array")
		return aiChatRequest{}, false
	}
	if len(req.Messages) == 0 {
		httpError(w, http.StatusBadRequest, "messages must contain at least one entry")
		return aiChatRequest{}, false
	}
	return req, true
}

// requireAISite runs the shared preamble for the AI handlers: it checks the
// proxy is configured (writing unconfiguredMsg on failure), resolves the site
// from the request host, and authorizes both site access and AI use. It writes
// the matching error response and returns false when any check fails. The site
// is consumed by the authorization checks; the AI handlers do not need it
// afterward.
func (s *Server) requireAISite(w http.ResponseWriter, r *http.Request, unconfiguredMsg string) bool {
	if s.ai == nil || !s.ai.configured() {
		httpError(w, http.StatusServiceUnavailable, unconfiguredMsg)
		return false
	}
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "AI API must be called from a site subdomain")
		return false
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return false
	}
	if !s.authorizeAIUse(w, r, site) {
		return false
	}
	return true
}

func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
	if !s.requireAISite(w, r, "AI proxy not configured: set OPENAI_API_KEY") {
		return
	}

	req, ok := decodeChatRequest(w, r)
	if !ok {
		return
	}

	res, err := s.ai.generateChat(r.Context(), req)
	if err != nil {
		writeAIChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeAIChatError maps a chat generation error onto the right HTTP status.
// Shared by the buffered and streaming handlers so both classify a bad
// request, a forbidden model, and an upstream failure the same way.
func writeAIChatError(w http.ResponseWriter, err error) {
	var bad badAIRequestError
	if errors.As(err, &bad) {
		httpError(w, http.StatusBadRequest, bad.Error())
		return
	}
	var forbidden forbiddenAIModelError
	if errors.As(err, &forbidden) {
		httpError(w, http.StatusForbidden, forbidden.Error())
		return
	}
	var upstream upstreamAIError
	if errors.As(err, &upstream) {
		httpError(w, http.StatusBadGateway, upstream.Error())
		return
	}
	log.Printf("ai: %v", err)
	httpError(w, http.StatusBadGateway, "could not reach the OpenAI-compatible API")
}

// handleAIChatStream streams a chat completion to the browser as
// server-sent events: one {"delta": "..."} frame per token, a terminal
// {"done": true, ...} frame with the model, stop reason, and usage, and
// {"error": "..."} if generation fails after streaming has begun. Errors
// that occur before the first token are reported as ordinary HTTP errors.
func (s *Server) handleAIChatStream(w http.ResponseWriter, r *http.Request) {
	if !s.requireAISite(w, r, "AI proxy not configured: set OPENAI_API_KEY") {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	req, ok := decodeChatRequest(w, r)
	if !ok {
		return
	}

	started := false
	res, err := s.ai.streamChat(r.Context(), req, func(delta string) error {
		if !started {
			started = true
			writeSSEHeaders(w)
		}
		return writeSSE(w, flusher, map[string]any{"delta": delta})
	})
	if err != nil {
		if !started {
			writeAIChatError(w, err)
			return
		}
		// Headers are already sent; surface the failure in-band.
		writeSSE(w, flusher, map[string]any{"error": err.Error()})
		return
	}
	if !started {
		writeSSEHeaders(w)
	}
	writeSSE(w, flusher, map[string]any{
		"done":        true,
		"model":       res.Model,
		"stop_reason": res.StopReason,
		"usage":       res.Usage,
	})
}

func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Stop intermediary proxies (e.g. Caddy) from buffering the stream, so
	// tokens reach the browser as they are produced.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIUsage accepts either OpenAI's prompt/completion field names or the
// input/output aliases some gateways use. Fields are pointers so an absent
// field can be told apart from a genuine zero, and the pair is coalesced by
// presence rather than by truthiness.
type openAIUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
}

func (u openAIUsage) input() int64  { return coalesceTokens(u.PromptTokens, u.InputTokens) }
func (u openAIUsage) output() int64 { return coalesceTokens(u.CompletionTokens, u.OutputTokens) }

// coalesceTokens returns the first field that was actually present, preserving
// a legitimate zero instead of overwriting it with the alias.
func coalesceTokens(primary, fallback *int64) int64 {
	if primary != nil {
		return *primary
	}
	if fallback != nil {
		return *fallback
	}
	return 0
}

type openAIChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage openAIUsage `json:"usage"`
}

// buildChatPayload validates the request and assembles the upstream
// /v1/chat/completions body. Streaming and non-streaming chat share it so
// the model allowlist and role checks stay identical on both paths.
func (p *AIProxy) buildChatPayload(req aiChatRequest) (map[string]any, string, error) {
	model := p.model
	if req.Model != "" {
		if !p.modelAllowed(req.Model) {
			return nil, "", forbiddenAIModelError{kind: "AI model"}
		}
		model = req.Model
	}
	messages := []openAIChatMessage{}
	if req.System != "" {
		messages = append(messages, openAIChatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "user", "assistant":
			messages = append(messages, openAIChatMessage{Role: m.Role, Content: m.Content})
		default:
			return nil, "", badAIRequestError(fmt.Sprintf("message role must be user or assistant, got %q", m.Role))
		}
	}

	payload := map[string]any{
		"model":                 model,
		"messages":              messages,
		"max_completion_tokens": defaultAITokens,
	}
	if req.MaxTokens > 0 {
		payload["max_completion_tokens"] = min(req.MaxTokens, maxAITokens)
	}
	return payload, model, nil
}

func (p *AIProxy) generateChat(ctx context.Context, req aiChatRequest) (aiChatResponse, error) {
	payload, model, err := p.buildChatPayload(req)
	if err != nil {
		return aiChatResponse{}, err
	}

	var apiRes openAIChatResponse
	if err := p.postJSON(ctx, p.baseURL+"/v1/chat/completions", payload, &apiRes); err != nil {
		return aiChatResponse{}, err
	}

	var res aiChatResponse
	res.Model = apiRes.Model
	if res.Model == "" {
		res.Model = model
	}
	if len(apiRes.Choices) > 0 {
		res.Text = apiRes.Choices[0].Message.Content
		res.StopReason = apiRes.Choices[0].FinishReason
	}
	res.Usage.InputTokens = apiRes.Usage.input()
	res.Usage.OutputTokens = apiRes.Usage.output()
	return res, nil
}

type openAIChatStreamChunk struct {
	Model   string `json:"model"`
	Error   any    `json:"error"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

// streamChat forwards a chat request to the gateway with server-sent
// streaming enabled, invoking onDelta for each token as it arrives. The
// returned response carries the accumulated text plus the final model,
// stop reason, and usage. Errors before the first delta (a forbidden
// model or an upstream failure) come back synchronously so the handler
// can answer with an HTTP status; a failure mid-stream returns after some
// deltas have already been delivered.
func (p *AIProxy) streamChat(ctx context.Context, req aiChatRequest, onDelta func(string) error) (res aiChatResponse, err error) {
	payload, model, err := p.buildChatPayload(req)
	if err != nil {
		return aiChatResponse{}, err
	}
	payload["stream"] = true
	payload["stream_options"] = map[string]any{"include_usage": true}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return aiChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", &body)
	if err != nil {
		return aiChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return aiChatResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return aiChatResponse{}, decodeAIUpstreamError(resp)
	}

	res = aiChatResponse{Model: model}
	// Accumulate the streamed text in a Builder rather than res.Text += delta
	// (which is O(n^2) over a long completion); the named return's Text is
	// finalized from it on every exit path, including a mid-stream error.
	var text strings.Builder
	defer func() { res.Text = text.String() }()
	terminal := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			terminal = true
			break
		}
		var chunk openAIChatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return res, upstreamAIError{message: "AI stream returned malformed data"}
		}
		if msg, isErr := aiStreamError(chunk.Error); isErr {
			return res, upstreamAIError{message: msg}
		}
		if chunk.Model != "" {
			res.Model = chunk.Model
		}
		if len(chunk.Choices) > 0 {
			if reason := chunk.Choices[0].FinishReason; reason != "" {
				res.StopReason = reason
			}
			if delta := chunk.Choices[0].Delta.Content; delta != "" {
				text.WriteString(delta)
				if err := onDelta(delta); err != nil {
					return res, err
				}
			}
		}
		if chunk.Usage != nil {
			res.Usage.InputTokens = chunk.Usage.input()
			res.Usage.OutputTokens = chunk.Usage.output()
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("read AI stream: %w", err)
	}
	if !terminal {
		return res, upstreamAIError{message: "AI stream ended before completion"}
	}
	return res, nil
}

// aiStreamError extracts a human-readable message from a stream chunk's error
// field and reports whether it represents a real error. A nil, false,
// empty-string, or empty-object value carries no error (ok=false), so a
// placeholder error field does not abort an otherwise-valid stream.
func aiStreamError(raw any) (string, bool) {
	switch e := raw.(type) {
	case nil:
		return "", false
	case bool:
		if e {
			return "AI stream returned an error", true
		}
		return "", false
	case string:
		if s := strings.TrimSpace(e); s != "" {
			return s, true
		}
		return "", false
	case map[string]any:
		if msg, ok := e["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return msg, true
		}
		if len(e) > 0 {
			return "AI stream returned an error", true
		}
		return "", false
	default:
		return "AI stream returned an error", true
	}
}

func (s *Server) handleAIImage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAISite(w, r, "AI image proxy not configured: set OPENAI_API_KEY") {
		return
	}

	var req aiImageRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "request body must be a JSON object with a prompt")
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		httpError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.ai.imageModel
	}
	if !s.ai.imageModelAllowed(model) {
		httpError(w, http.StatusForbidden, "requested AI image model is not allowed by this deployment")
		return
	}

	res, err := s.ai.generateImage(r.Context(), req, model)
	if err != nil {
		var upstream upstreamAIError
		if errors.As(err, &upstream) {
			httpError(w, http.StatusBadGateway, upstream.Error())
			return
		}
		log.Printf("ai image: %v", err)
		httpError(w, http.StatusBadGateway, "could not reach the OpenAI-compatible image API")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type openAIImageResponse struct {
	Data []struct {
		B64JSON       string `json:"b64_json"`
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
	Usage json.RawMessage `json:"usage"`
}

func (p *AIProxy) generateImage(ctx context.Context, req aiImageRequest, model string) (aiImageResponse, error) {
	payload := map[string]any{
		"model":  model,
		"prompt": req.Prompt,
	}
	if req.Size != "" {
		payload["size"] = req.Size
	}
	if req.AspectRatio != "" {
		payload["aspect_ratio"] = req.AspectRatio
	}
	if req.ImageSize != "" {
		payload["image_size"] = req.ImageSize
	}
	if req.Quality != "" {
		payload["quality"] = req.Quality
	}
	if req.OutputFormat != "" {
		payload["output_format"] = req.OutputFormat
	}
	if req.Background != "" {
		payload["background"] = req.Background
	}
	if req.N > 0 {
		payload["n"] = req.N
	}

	var apiRes openAIImageResponse
	if err := p.postJSON(ctx, p.baseURL+"/v1/images/generations", payload, &apiRes); err != nil {
		return aiImageResponse{}, err
	}
	res := aiImageResponse{Provider: "openai", Model: model, Usage: apiRes.Usage}
	for _, img := range apiRes.Data {
		asset := aiImageAsset{
			B64:           img.B64JSON,
			URL:           img.URL,
			RevisedPrompt: img.RevisedPrompt,
			MIMEType:      mimeTypeForImageData(img.B64JSON, req.OutputFormat),
		}
		if asset.B64 != "" {
			asset.DataURL = "data:" + asset.MIMEType + ";base64," + asset.B64
		}
		res.Images = append(res.Images, asset)
	}
	if len(res.Images) == 0 {
		return aiImageResponse{}, upstreamAIError{message: "image response did not include image data"}
	}
	return res, nil
}

func (p *AIProxy) postJSON(ctx context.Context, endpoint string, payload any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAIUpstreamError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type upstreamAIError struct {
	status  int
	message string
}

func (e upstreamAIError) Error() string {
	if e.status > 0 {
		return fmt.Sprintf("OpenAI-compatible API error (status %d): %s", e.status, e.message)
	}
	return "OpenAI-compatible API error: " + e.message
}

func decodeAIUpstreamError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := strings.TrimSpace(string(b))
	var parsed struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(b, &parsed) == nil && parsed.Error != nil {
		switch e := parsed.Error.(type) {
		case string:
			msg = e
		case map[string]any:
			if m, ok := e["message"].(string); ok && m != "" {
				msg = m
			}
		}
	}
	if msg == "" {
		msg = resp.Status
	}
	return upstreamAIError{status: resp.StatusCode, message: msg}
}

type badAIRequestError string

func (e badAIRequestError) Error() string {
	return string(e)
}

type forbiddenAIModelError struct {
	kind string
}

func (e forbiddenAIModelError) Error() string {
	return "requested " + e.kind + " is not allowed by this deployment"
}

func mimeTypeForImageFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func mimeTypeForImageData(b64, requestedFormat string) string {
	switch {
	case strings.HasPrefix(b64, "/9j/"):
		return "image/jpeg"
	case strings.HasPrefix(b64, "iVBORw0KGgo"):
		return "image/png"
	case strings.HasPrefix(b64, "UklGR"):
		return "image/webp"
	case strings.HasPrefix(b64, "R0lG"):
		return "image/gif"
	default:
		return mimeTypeForImageFormat(requestedFormat)
	}
}

// aiVisitorsAllowed reports whether AI use is open to any site visitor — the
// deployment runs in visitors mode, or the site's policy opts visitors in.
// Both the AI gate (authorizeAIUse) and the /api/me capability (aiAllowedFor)
// consult it so the visitor rule cannot drift between them.
func (s *Server) aiVisitorsAllowed(policy *AccessPolicy) bool {
	return s.aiAccess == aiAccessVisitors || policy.AllowsAIVisitors()
}

func (s *Server) authorizeAIUse(w http.ResponseWriter, r *http.Request, site string) bool {
	if s.aiAccess == aiAccessVisitors {
		return true
	}
	// Resolve the site policy once and reuse it, so the visitor-AI check does
	// not cost a second _access.json fetch per request.
	policy, err := s.policyForSite(r.Context(), site)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable,
			"this site's "+accessFileName+" is unreadable; AI access denied until it is fixed")
		return false
	}
	if s.aiVisitorsAllowed(policy) {
		return true
	}
	if s.siteManager == nil {
		httpError(w, http.StatusServiceUnavailable, "AI owner checks are not configured")
		return false
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return false
	}
	allowed, err := s.siteManager.CanManageSite(r.Context(), site, actor)
	if errors.Is(err, ErrSiteNotFound) {
		httpError(w, http.StatusNotFound, "no site named "+site)
		return false
	}
	if err != nil {
		log.Printf("ai auth %s: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize AI access")
		return false
	}
	if !allowed {
		httpError(w, http.StatusForbidden, "AI is restricted to the site owner or a platform admin")
		return false
	}
	return true
}
