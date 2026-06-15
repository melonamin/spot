package main

import (
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

func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil || !s.ai.configured() {
		httpError(w, http.StatusServiceUnavailable,
			"AI proxy not configured: set OPENAI_API_KEY")
		return
	}
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "AI API must be called from a site subdomain")
		return
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return
	}
	if !s.authorizeAIUse(w, r, site) {
		return
	}

	var req aiChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "request body must be a JSON object with a messages array")
		return
	}
	if len(req.Messages) == 0 {
		httpError(w, http.StatusBadRequest, "messages must contain at least one entry")
		return
	}

	res, err := s.ai.generateChat(r.Context(), req)
	if err != nil {
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
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (p *AIProxy) generateChat(ctx context.Context, req aiChatRequest) (aiChatResponse, error) {
	model := p.model
	if req.Model != "" {
		if !p.modelAllowed(req.Model) {
			return aiChatResponse{}, forbiddenAIModelError{kind: "AI model"}
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
			return aiChatResponse{}, badAIRequestError(fmt.Sprintf("message role must be user or assistant, got %q", m.Role))
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
	res.Usage.InputTokens = apiRes.Usage.PromptTokens
	if res.Usage.InputTokens == 0 {
		res.Usage.InputTokens = apiRes.Usage.InputTokens
	}
	res.Usage.OutputTokens = apiRes.Usage.CompletionTokens
	if res.Usage.OutputTokens == 0 {
		res.Usage.OutputTokens = apiRes.Usage.OutputTokens
	}
	return res, nil
}

func (s *Server) handleAIImage(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil || !s.ai.configured() {
		httpError(w, http.StatusServiceUnavailable,
			"AI image proxy not configured: set OPENAI_API_KEY")
		return
	}
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "AI API must be called from a site subdomain")
		return
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return
	}
	if !s.authorizeAIUse(w, r, site) {
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
	if policy.AllowsAIVisitors() {
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
