package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// accessFileName sits at the root of a site to restrict who can view it.
// No file means the site is open to everyone on the mesh — open is the
// default and the platform norm.
const accessFileName = "_access.json"

// AccessPolicy restricts a site only when "allow" is present. Entries
// containing "@" match the visitor's email, all other entries match a
// mesh group name; both case-insensitive. An omitted allow keeps the
// site public; an empty allow list denies everyone.
type AccessPolicy struct {
	Allow    []string `json:"allow,omitempty"`
	AI       string   `json:"ai,omitempty"`
	Download *bool    `json:"download,omitempty"`

	restrictAccess bool
}

func (p *AccessPolicy) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("access policy must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}

	var allowRaw, aiRaw, downloadRaw *json.RawMessage
	for key, value := range fields {
		value := value
		switch strings.ToLower(key) {
		case "allow":
			if allowRaw != nil {
				return errors.New("duplicate allow field")
			}
			allowRaw = &value
		case "ai":
			if aiRaw != nil {
				return errors.New("duplicate ai field")
			}
			aiRaw = &value
		case "download":
			if downloadRaw != nil {
				return errors.New("duplicate download field")
			}
			downloadRaw = &value
		default:
			return fmt.Errorf("unknown access policy field %q", key)
		}
	}

	if allowRaw != nil {
		if bytes.Equal(bytes.TrimSpace(*allowRaw), []byte("null")) {
			return errors.New("allow must be an array")
		}
		if err := json.Unmarshal(*allowRaw, &p.Allow); err != nil {
			return fmt.Errorf("allow must be an array: %w", err)
		}
		p.restrictAccess = true
	} else {
		p.Allow = nil
		p.restrictAccess = false
	}
	if aiRaw != nil {
		if err := json.Unmarshal(*aiRaw, &p.AI); err != nil {
			return fmt.Errorf("ai must be a string: %w", err)
		}
		// Only "", "owners", and "visitors" carry meaning. Reject anything
		// else (a typo like "visitor" or "all") so the policy fails closed
		// instead of silently behaving owner-only.
		switch strings.ToLower(strings.TrimSpace(p.AI)) {
		case "", aiAccessOwners, aiAccessVisitors:
		default:
			return fmt.Errorf("ai must be one of %q, %q, or empty, got %q", aiAccessOwners, aiAccessVisitors, p.AI)
		}
	}
	if downloadRaw != nil {
		if bytes.Equal(bytes.TrimSpace(*downloadRaw), []byte("null")) {
			return errors.New("download must be a boolean")
		}
		if err := json.Unmarshal(*downloadRaw, &p.Download); err != nil {
			return fmt.Errorf("download must be a boolean: %w", err)
		}
	}
	return nil
}

func (p *AccessPolicy) Allows(id Identity) bool {
	for _, entry := range p.Allow {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "@") {
			if id.Email != "" && entry == strings.ToLower(id.Email) {
				return true
			}
			continue
		}
		for _, group := range id.Groups {
			if entry == strings.ToLower(group) {
				return true
			}
		}
	}
	return false
}

func (p *AccessPolicy) RestrictsAccess() bool {
	return p != nil && (p.restrictAccess || p.Allow != nil)
}

func (p *AccessPolicy) AllowsDownload() bool {
	return p == nil || p.Download == nil || *p.Download
}

func (p *AccessPolicy) AllowsAIVisitors() bool {
	return p != nil && strings.EqualFold(strings.TrimSpace(p.AI), aiAccessVisitors)
}

// PolicyStore reads site access policies from local site storage and
// caches results briefly. S3-backed deployments read policies through
// SiteStorage in policy_resolve.go.
type PolicyStore struct {
	dir string
	ttl time.Duration

	mu    sync.Mutex
	cache map[string]policyEntry
}

type policyEntry struct {
	policy       *AccessPolicy
	err          error
	fetchedAt    time.Time
	checkedStore bool
}

func NewPolicyStore(dir string, ttl time.Duration) *PolicyStore {
	return &PolicyStore{dir: dir, ttl: ttl, cache: map[string]policyEntry{}}
}

// For returns the access policy for a site, or nil when the site is
// open. A policy file that exists but cannot be read or parsed is
// returned as an error so callers fail closed.
func (s *PolicyStore) For(site string) (*AccessPolicy, error) {
	policy, err, _ := s.ForWithStoreStatus(site)
	return policy, err
}

func (s *PolicyStore) ForWithStoreStatus(site string) (*AccessPolicy, error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.cache[site]; ok && time.Since(entry.fetchedAt) < s.ttl {
		return entry.policy, entry.err, entry.checkedStore
	}
	policy, err := s.load(site)
	s.cache[site] = policyEntry{policy: policy, err: err, fetchedAt: time.Now()}
	return policy, err, false
}

// Set records a known policy for a site. Deploys use it only for
// fail-closed errors or policy changes that do not broaden access before
// the mounted file view has caught up.
func (s *PolicyStore) Set(site string, policy *AccessPolicy, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[site] = policyEntry{policy: policy, err: err, fetchedAt: time.Now(), checkedStore: true}
}

func (s *PolicyStore) Invalidate(site string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, site)
}

func (s *PolicyStore) load(site string) (*AccessPolicy, error) {
	if !siteNameRe.MatchString(site) {
		return nil, fmt.Errorf("invalid site name %q", site)
	}
	file, err := os.OpenInRoot(s.dir, filepath.Join(site, accessFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read access policy for site %s: %w", site, err)
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read access policy for site %s: %w", site, err)
	}
	return parseAccessPolicy(site, raw)
}

func parseAccessPolicy(site string, raw []byte) (*AccessPolicy, error) {
	var policy AccessPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, fmt.Errorf("parse %s for site %s: %w", accessFileName, site, err)
	}
	return &policy, nil
}
