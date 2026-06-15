package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultTailscaleAPIURL = "https://api.tailscale.com"

// TailscaleResolver maps WireGuard peer IPs to users via the Tailscale
// API, caching the device, user, and policy lists for ttl.
type TailscaleResolver struct {
	apiURL  string
	tokens  tailscaleTokenSource
	tailnet string
	ttl     time.Duration
	client  *http.Client

	mu        sync.Mutex
	byIP      map[string]Identity
	directory []AccessSuggestion
	fetchedAt time.Time
}

func NewTailscaleResolver(apiURL, token, tailnet string, ttl time.Duration) *TailscaleResolver {
	if apiURL == "" {
		apiURL = defaultTailscaleAPIURL
	}
	if tailnet == "" {
		tailnet = "-"
	}
	return &TailscaleResolver{
		apiURL:  strings.TrimSuffix(apiURL, "/"),
		tokens:  staticTailscaleToken(token),
		tailnet: tailnet,
		ttl:     ttl,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func NewTailscaleOAuthResolver(apiURL, clientID, clientSecret, tailnet string, ttl time.Duration) *TailscaleResolver {
	r := NewTailscaleResolver(apiURL, "", tailnet, ttl)
	r.tokens = &oauthTailscaleTokenSource{clientID: clientID, clientSecret: clientSecret}
	return r
}

type tailscaleTokenSource interface {
	token(ctx context.Context, client *http.Client, apiURL string) (string, error)
}

type staticTailscaleToken string

func (s staticTailscaleToken) token(context.Context, *http.Client, string) (string, error) {
	return string(s), nil
}

type oauthTailscaleTokenSource struct {
	clientID     string
	clientSecret string
	accessToken  string
	expiresAt    time.Time
}

// tailscaleTokenRefreshSkew is the buffer before actual token expiry at
// which a fresh token is minted, so requests never carry a token that
// expires in flight.
const tailscaleTokenRefreshSkew = 60 * time.Second

func (s *oauthTailscaleTokenSource) token(ctx context.Context, client *http.Client, apiURL string) (string, error) {
	if s.accessToken != "" && time.Now().Before(s.expiresAt) {
		return s.accessToken, nil
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("tailscale oauth token: %w", err)
	}
	req.SetBasicAuth(s.clientID, s.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tailscale oauth token: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tailscale oauth token: unexpected status %d", res.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("tailscale oauth token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("tailscale oauth token response: missing access_token")
	}
	if out.ExpiresIn <= 0 {
		return "", fmt.Errorf("tailscale oauth token response: invalid expires_in %s", strconv.Itoa(out.ExpiresIn))
	}
	lifetime := time.Duration(out.ExpiresIn) * time.Second
	skew := tailscaleTokenRefreshSkew
	if skew >= lifetime {
		// Short-lived tokens would be treated as instantly expired if the
		// full skew were applied; refresh just before actual expiry instead.
		skew = lifetime / 2
	}
	s.accessToken = out.AccessToken
	s.expiresAt = time.Now().Add(lifetime - skew)
	return s.accessToken, nil
}

func (r *TailscaleResolver) Resolve(ctx context.Context, ip string) (Identity, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureFresh(ctx); err != nil {
		return Identity{}, false, err
	}
	id, ok := r.byIP[normalizeTailscaleIP(ip)]
	return id, ok, nil
}

// Directory returns the cached user/group list for the access picker.
func (r *TailscaleResolver) Directory(ctx context.Context) ([]AccessSuggestion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureFresh(ctx); err != nil {
		return nil, err
	}
	out := make([]AccessSuggestion, len(r.directory))
	copy(out, r.directory)
	return out, nil
}

// ensureFresh refreshes the peer map and directory if the cache is
// stale. The caller must hold r.mu.
func (r *TailscaleResolver) ensureFresh(ctx context.Context) error {
	if r.byIP != nil && time.Since(r.fetchedAt) < r.ttl {
		return nil
	}
	byIP, directory, err := r.fetch(ctx)
	if err != nil {
		return err
	}
	r.byIP, r.directory, r.fetchedAt = byIP, directory, time.Now()
	return nil
}

type tailscaleDevicesResponse struct {
	Devices []tailscaleDevice `json:"devices"`
}

type tailscaleDevice struct {
	Addresses []string `json:"addresses"`
	User      string   `json:"user"`
	Hostname  string   `json:"hostname"`
	Tags      []string `json:"tags"`
}

type tailscaleUsersResponse struct {
	Users []tailscaleUser `json:"users"`
}

type tailscaleUser struct {
	LoginName   string `json:"loginName"`
	DisplayName string `json:"displayName"`
}

type tailscaleACLResponse struct {
	Groups map[string][]string `json:"groups"`
}

func (r *TailscaleResolver) fetch(ctx context.Context) (map[string]Identity, []AccessSuggestion, error) {
	tailnetPath := url.PathEscape(r.tailnet)
	var devices tailscaleDevicesResponse
	if err := r.get(ctx, "/api/v2/tailnet/"+tailnetPath+"/devices", &devices); err != nil {
		return nil, nil, err
	}
	var users tailscaleUsersResponse
	if err := r.get(ctx, "/api/v2/tailnet/"+tailnetPath+"/users", &users); err != nil {
		return nil, nil, err
	}
	var acl tailscaleACLResponse
	if err := r.get(ctx, "/api/v2/tailnet/"+tailnetPath+"/acl", &acl); err != nil {
		return nil, nil, err
	}

	usersByLogin := make(map[string]tailscaleUser, len(users.Users))
	for _, u := range users.Users {
		usersByLogin[u.LoginName] = u
	}
	groupsByLogin, groupNames := tailscaleGroupsByLogin(normalizeGroups(acl.Groups))

	byIP := make(map[string]Identity)
	for _, d := range devices.Devices {
		peerIP := tailscaleCanonicalAddress(d.Addresses)
		if peerIP == "" {
			continue
		}
		id := Identity{PeerName: d.Hostname, PeerIP: peerIP}
		if len(d.Tags) == 0 {
			if u, ok := usersByLogin[d.User]; ok {
				id.Email = u.LoginName
				id.Name = u.DisplayName
				id.Groups = sortedStrings(groupsByLogin[strings.ToLower(u.LoginName)])
			}
		}
		for _, addr := range d.Addresses {
			if key := normalizeTailscaleIP(addr); key != "" {
				byIP[key] = id
			}
		}
	}

	return byIP, buildTailscaleDirectory(users.Users, groupNames), nil
}

// normalizeTailscaleIP canonicalizes an address string so semantically
// equal forms (notably differing IPv6 textual representations) compare
// equal as map keys. Addresses that fail to parse are dropped by
// returning the empty string. IPv4 addresses round-trip unchanged.
func normalizeTailscaleIP(addr string) string {
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return ""
	}
	return parsed.Unmap().String()
}

func tailscaleCanonicalAddress(addresses []string) string {
	if len(addresses) == 0 {
		return ""
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	for _, addr := range addresses {
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() == nil {
			continue
		}
		if cgnat.Contains(ip) {
			return addr
		}
	}
	return addresses[0]
}

// normalizeGroups reduces raw policy-file group member lists to plain
// user logins. Tailscale groups cannot contain other groups, but policy
// files may include autogroups, domains, or other non-user selectors in
// member lists. Spot identity groups are concrete user memberships, so
// non-email members are ignored.
func normalizeGroups(raw map[string][]string) map[string][]string {
	out := make(map[string][]string, len(raw))
	for group, members := range raw {
		for _, member := range members {
			member = strings.TrimSpace(member)
			if !strings.Contains(member, "@") {
				continue
			}
			out[group] = append(out[group], member)
		}
	}
	return out
}

func tailscaleGroupsByLogin(groups map[string][]string) (map[string][]string, []string) {
	byLogin := map[string][]string{}
	groupSet := map[string]struct{}{}
	for group, members := range groups {
		stripped := strings.TrimPrefix(group, "group:")
		if stripped == "" {
			continue
		}
		groupSet[stripped] = struct{}{}
		names := []string{stripped}
		if group != stripped {
			names = append(names, group)
		}
		for _, member := range members {
			key := strings.ToLower(member)
			seen := map[string]struct{}{}
			for _, existing := range byLogin[key] {
				seen[existing] = struct{}{}
			}
			for _, name := range names {
				if _, ok := seen[name]; !ok {
					byLogin[key] = append(byLogin[key], name)
				}
			}
		}
	}
	groupNames := make([]string, 0, len(groupSet))
	for name := range groupSet {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	return byLogin, groupNames
}

func buildTailscaleDirectory(users []tailscaleUser, groupNames []string) []AccessSuggestion {
	directory := make([]AccessSuggestion, 0, len(users)+len(groupNames))
	for _, u := range users {
		if u.LoginName == "" {
			continue
		}
		meta := u.DisplayName
		if meta == "" {
			meta = "User"
		}
		directory = append(directory, AccessSuggestion{
			Type: "user", Value: u.LoginName, Label: u.LoginName, Meta: meta,
		})
	}
	for _, name := range groupNames {
		if name == "" {
			continue
		}
		directory = append(directory, AccessSuggestion{
			Type: "group", Value: name, Label: name, Meta: "Group",
		})
	}
	sortDirectory(directory)
	return directory
}

func (r *TailscaleResolver) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiURL+path, nil)
	if err != nil {
		return fmt.Errorf("tailscale request %s: %w", path, err)
	}
	token, err := r.tokens.token(ctx, r.client, r.apiURL)
	if err != nil {
		return fmt.Errorf("tailscale request %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	res, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("tailscale request %s: %w", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("tailscale request %s: unexpected status %d", path, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("tailscale response %s: %w", path, err)
	}
	return nil
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
