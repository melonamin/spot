package main

import (
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

// AccessPolicy restricts a site to the users and groups listed in
// "allow". Entries containing "@" match the visitor's email, all other
// entries match a NetBird group name; both case-insensitive. An empty
// list denies everyone.
type AccessPolicy struct {
	Allow []string `json:"allow"`
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

// PolicyStore reads site access policies from the mounted sites
// directory, caching results briefly to keep authz subrequests off the
// FUSE mount's hot path.
type PolicyStore struct {
	dir string
	ttl time.Duration

	mu    sync.Mutex
	cache map[string]policyEntry
}

type policyEntry struct {
	policy    *AccessPolicy
	err       error
	fetchedAt time.Time
}

func NewPolicyStore(dir string, ttl time.Duration) *PolicyStore {
	return &PolicyStore{dir: dir, ttl: ttl, cache: map[string]policyEntry{}}
}

// For returns the access policy for a site, or nil when the site is
// open. A policy file that exists but cannot be read or parsed is
// returned as an error so callers fail closed.
func (s *PolicyStore) For(site string) (*AccessPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.cache[site]; ok && time.Since(entry.fetchedAt) < s.ttl {
		return entry.policy, entry.err
	}
	policy, err := s.load(site)
	s.cache[site] = policyEntry{policy: policy, err: err, fetchedAt: time.Now()}
	return policy, err
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
	var policy AccessPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, fmt.Errorf("parse %s for site %s: %w", accessFileName, site, err)
	}
	return &policy, nil
}
