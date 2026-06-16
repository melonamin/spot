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
	hasBlocks, err := slackBlocksPresent(req.Blocks)
	if err != nil {
		httpError(w, http.StatusBadRequest, "blocks must be a non-empty array")
		return slackSendRequest{}, false
	}
	if strings.TrimSpace(req.Text) == "" && !hasBlocks {
		httpError(w, http.StatusBadRequest, "text or blocks is required")
		return slackSendRequest{}, false
	}
	return req, true
}

// slackBlocksPresent reports whether blocks carries at least one block. An
// absent or null value is simply not present (no error), so the caller must
// then supply text. A value that is set but is not a non-empty JSON array — a
// bare string, an object, or an empty array — is malformed and returns an
// error so the request fails fast as a 400 instead of confusing Slack.
func slackBlocksPresent(blocks json.RawMessage) (bool, error) {
	trimmed := bytes.TrimSpace(blocks)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return false, err
	}
	if len(arr) == 0 {
		return false, errors.New("blocks must not be empty")
	}
	return true, nil
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
			retryAfter: res.Header.Get("Retry-After"),
		}
	}
	if !apiRes.OK {
		return slackSendResponse{}, slackError{
			slackErr:   apiRes.Error,
			status:     slackErrorStatus(apiRes.Error),
			retryAfter: res.Header.Get("Retry-After"),
		}
	}
	return slackSendResponse{OK: true, Channel: apiRes.Channel, TS: apiRes.TS}, nil
}

// slackErrorStatus maps Slack chat.postMessage error strings to the HTTP status
// the browser should see. Caller-actionable request problems stay in the 4xx
// range; deployment credential and unknown failures fail closed as a 502. The
// SDK derives its coarse error code from this status (codeForStatus in
// spot.js), so only the status is returned here.
func slackErrorStatus(slackErr string) int {
	switch slackErr {
	case "channel_not_found":
		return http.StatusNotFound
	case "not_in_channel", "msg_too_long", "no_text", "is_archived":
		return http.StatusBadRequest
	case "rate_limited":
		return http.StatusTooManyRequests
	case "invalid_auth", "token_revoked", "account_inactive":
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
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
			// A 5xx is a Slack-side or credential failure the caller cannot act
			// on; log the detail and return a generic message so raw Slack
			// errors (e.g. invalid_auth) are not leaked to visitors. 4xx errors
			// are caller-actionable, so keep their detail.
			if slackErr.status >= http.StatusInternalServerError {
				log.Printf("slack: %v", slackErr)
				httpError(w, slackErr.status, "could not deliver the Slack message")
				return
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
