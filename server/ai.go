package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const (
	// Sites get Claude without managing keys; the default model is the
	// current Opus tier per Anthropic guidance.
	defaultAIModel   = "claude-opus-4-8"
	defaultAITokens  = 16000
	maxAITokens      = 16000
	aiAccessOwners   = "owners"
	aiAccessVisitors = "visitors"

	// Thinking and the text response share max_tokens, so cap the thinking
	// budget below it and always reserve aiOutputReserveTokens for the reply.
	// Without this a hard prompt can spend the whole budget reasoning and
	// return empty text with a 200 OK.
	aiOutputReserveTokens = 4000
	// Anthropic requires a thinking budget of at least 1024 tokens.
	aiMinThinkingTokens = 1024
)

// AIProxy forwards chat requests to the Claude API with the server-side
// key, so sites can call an LLM with zero configuration.
type AIProxy struct {
	client anthropic.Client
	// model is the default when a request names none. Deployments
	// behind a gateway that doesn't serve the platform default
	// override it via SPOT_AI_MODEL.
	model         string
	allowedModels map[string]struct{}
}

func NewAIProxy(apiKey string, allowedModels []string, opts ...option.RequestOption) *AIProxy {
	opts = append([]option.RequestOption{option.WithAPIKey(apiKey)}, opts...)
	proxy := &AIProxy{client: anthropic.NewClient(opts...), model: defaultAIModel}
	proxy.setAllowedModels(allowedModels)
	return proxy
}

// NewAIProxyWithUpstream builds the proxy against a custom
// Anthropic-compatible base URL (an LLM gateway or proxy) and default
// model; empty values mean the Claude API itself and the platform
// default model. The base URL is pinned explicitly even then, because
// the SDK honors a set-but-empty ANTHROPIC_BASE_URL in the environment
// (which is how compose renders an unset variable) and would otherwise
// dial a URL of "".
func NewAIProxyWithUpstream(apiKey, baseURL, model string, allowedModels []string) *AIProxy {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/"
	}
	proxy := NewAIProxy(apiKey, allowedModels, option.WithBaseURL(baseURL))
	if model != "" {
		proxy.model = model
	}
	proxy.setAllowedModels(allowedModels)
	return proxy
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

func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil {
		httpError(w, http.StatusServiceUnavailable,
			"AI proxy not configured: set ANTHROPIC_API_KEY")
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

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(s.ai.model),
		MaxTokens: defaultAITokens,
	}
	if req.Model != "" {
		if !s.ai.modelAllowed(req.Model) {
			httpError(w, http.StatusForbidden, "requested AI model is not allowed by this deployment")
			return
		}
		params.Model = anthropic.Model(req.Model)
	}
	if req.MaxTokens > 0 {
		params.MaxTokens = min(req.MaxTokens, maxAITokens)
	}
	// Bound thinking below max_tokens so the text response always has room.
	// The proxy sees arbitrary tasks, so let the model reason freely up to a
	// budget that reserves aiOutputReserveTokens for the reply.
	thinkingBudget := params.MaxTokens - aiOutputReserveTokens
	if thinkingBudget >= aiMinThinkingTokens {
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: thinkingBudget},
		}
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			params.Messages = append(params.Messages,
				anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case "assistant":
			params.Messages = append(params.Messages,
				anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		default:
			httpError(w, http.StatusBadRequest,
				fmt.Sprintf("message role must be user or assistant, got %q", m.Role))
			return
		}
	}

	message, err := s.ai.client.Messages.New(r.Context(), params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			httpError(w, http.StatusBadGateway,
				fmt.Sprintf("Claude API error (status %d): %s", apiErr.StatusCode, apiErr.Error()))
			return
		}
		log.Printf("ai: %v", err)
		httpError(w, http.StatusBadGateway, "could not reach the Claude API")
		return
	}

	var res aiChatResponse
	res.Model = string(message.Model)
	res.StopReason = string(message.StopReason)
	res.Usage.InputTokens = message.Usage.InputTokens
	res.Usage.OutputTokens = message.Usage.OutputTokens
	for _, block := range message.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			res.Text += text.Text
		}
	}
	writeJSON(w, http.StatusOK, res)
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
