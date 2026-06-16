package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

const defaultSlackBaseURL = "https://slack.com/api"

// SlackProxy forwards Slack messages through chat.postMessage so sites can
// send notifications without exposing the deployment's bot token.
type SlackProxy struct {
	httpClient *http.Client
	token      string
	baseURL    string
}

func NewSlackProxy(token, baseURL string) *SlackProxy {
	proxy := &SlackProxy{
		httpClient: http.DefaultClient,
		token:      strings.TrimSpace(token),
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
	if proxy.baseURL == "" {
		proxy.baseURL = defaultSlackBaseURL
	}
	return proxy
}

func (p *SlackProxy) configured() bool {
	return p != nil && p.token != ""
}

type slackSendRequest struct {
	Channel string          `json:"channel"`
	Text    string          `json:"text,omitempty"`
	Blocks  json.RawMessage `json:"blocks,omitempty"`
	Mrkdwn  *bool           `json:"mrkdwn,omitempty"`
}

type slackSendResponse struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

type slackAPIResponse struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Error   string `json:"error"`
}

type slackError struct {
	slackErr   string
	status     int
	code       string
	retryAfter string
}

func (e slackError) Error() string {
	if e.slackErr == "" {
		return "Slack API request failed"
	}
	return "Slack API rejected the message: " + e.slackErr
}

// decodeSlackRequest reads and validates a Slack send request body, writing the
// matching 400 response and returning ok=false when the JSON is malformed or
// missing the required channel/message fields.
func decodeSlackRequest(w http.ResponseWriter, r *http.Request) (slackSendRequest, bool) {
	var req slackSendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "request body must be a JSON object")
		return slackSendRequest{}, false
	}
	if strings.TrimSpace(req.Channel) == "" {
		httpError(w, http.StatusBadRequest, "channel is required")
		return slackSendRequest{}, false
	}
	if strings.TrimSpace(req.Text) == "" && emptySlackBlocks(req.Blocks) {
		httpError(w, http.StatusBadRequest, "text or blocks is required")
		return slackSendRequest{}, false
	}
	return req, true
}

func emptySlackBlocks(blocks json.RawMessage) bool {
	return len(bytes.TrimSpace(blocks)) == 0 || bytes.Equal(bytes.TrimSpace(blocks), []byte("null"))
}

func (p *SlackProxy) postMessage(ctx context.Context, req slackSendRequest) (slackSendResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return slackSendResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return slackSendResponse{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.token)
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")

	res, err := p.httpClient.Do(httpReq)
	if err != nil {
		return slackSendResponse{}, err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusTooManyRequests {
		return slackSendResponse{}, slackError{
			slackErr:   "rate_limited",
			status:     http.StatusTooManyRequests,
			code:       "rate_limited",
			retryAfter: res.Header.Get("Retry-After"),
		}
	}

	var apiRes slackAPIResponse
	if err := json.NewDecoder(res.Body).Decode(&apiRes); err != nil {
		return slackSendResponse{}, fmt.Errorf("decode Slack response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return slackSendResponse{}, slackError{
			slackErr:   apiRes.Error,
			status:     http.StatusBadGateway,
			code:       "server",
			retryAfter: res.Header.Get("Retry-After"),
		}
	}
	if !apiRes.OK {
		status, code := slackErrorStatus(apiRes.Error)
		return slackSendResponse{}, slackError{
			slackErr:   apiRes.Error,
			status:     status,
			code:       code,
			retryAfter: res.Header.Get("Retry-After"),
		}
	}
	return slackSendResponse{OK: true, Channel: apiRes.Channel, TS: apiRes.TS}, nil
}

// slackErrorStatus maps Slack chat.postMessage error strings to the HTTP status
// and coarse SDK error code the browser should see. Caller-actionable request
// problems stay in the 4xx range; deployment credential and unknown failures
// fail closed as a server-side 502.
func slackErrorStatus(slackErr string) (status int, code string) {
	switch slackErr {
	case "channel_not_found":
		return http.StatusNotFound, "not_found"
	case "not_in_channel", "msg_too_long", "no_text", "is_archived":
		return http.StatusBadRequest, "bad_request"
	case "rate_limited":
		return http.StatusTooManyRequests, "rate_limited"
	case "invalid_auth", "token_revoked", "account_inactive":
		return http.StatusBadGateway, "server"
	default:
		return http.StatusBadGateway, "server"
	}
}

// requireSlackSite runs the shared preamble for the Slack handler: it checks
// the proxy is configured, resolves the site from the request host, and
// authorizes both site access and Slack use.
func (s *Server) requireSlackSite(w http.ResponseWriter, r *http.Request) bool {
	if s.slack == nil || !s.slack.configured() {
		httpError(w, http.StatusServiceUnavailable, "Slack proxy not configured: set SLACK_BOT_TOKEN")
		return false
	}
	site := siteFromHost(s.requestHost(r), s.spotDomain)
	if site == "" {
		httpError(w, http.StatusBadRequest, "Slack API must be called from a site subdomain")
		return false
	}
	if !s.authorizeSiteAccess(w, r, site) {
		return false
	}
	if !s.authorizeSlackUse(w, r, site) {
		return false
	}
	return true
}

// slackVisitorsAllowed reports whether Slack use is open to any site visitor:
// the deployment runs in visitors mode, or the site's policy opts visitors in.
func (s *Server) slackVisitorsAllowed(policy *AccessPolicy) bool {
	return s.slackAccess == slackAccessVisitors || policy.AllowsSlackVisitors()
}

func (s *Server) authorizeSlackUse(w http.ResponseWriter, r *http.Request, site string) bool {
	if s.slackAccess == slackAccessVisitors {
		return true
	}
	policy, err := s.policyForSite(r.Context(), site)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable,
			"this site's "+accessFileName+" is unreadable; Slack access denied until it is fixed")
		return false
	}
	if s.slackVisitorsAllowed(policy) {
		return true
	}
	if s.siteManager == nil {
		httpError(w, http.StatusServiceUnavailable, "Slack owner checks are not configured")
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
		log.Printf("slack auth %s: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize Slack access")
		return false
	}
	if !allowed {
		httpError(w, http.StatusForbidden, "Slack is restricted to the site owner or a platform admin")
		return false
	}
	return true
}

func (s *Server) handleSlackSend(w http.ResponseWriter, r *http.Request) {
	if !s.requireSlackSite(w, r) {
		return
	}
	req, ok := decodeSlackRequest(w, r)
	if !ok {
		return
	}
	res, err := s.slack.postMessage(r.Context(), req)
	if err != nil {
		var slackErr slackError
		if errors.As(err, &slackErr) {
			if slackErr.retryAfter != "" {
				w.Header().Set("Retry-After", slackErr.retryAfter)
			}
			httpError(w, slackErr.status, slackErr.Error())
			return
		}
		log.Printf("slack: %v", err)
		httpError(w, http.StatusBadGateway, "could not reach the Slack API")
		return
	}
	writeJSON(w, http.StatusOK, res)
}
