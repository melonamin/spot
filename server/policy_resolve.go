package main

import (
	"context"
	"errors"
	"io"

	"github.com/minio/minio-go/v7"
)

// errNoSiteStore is returned when a site's policy cannot be resolved
// because no site store is configured to read its _access.json. Callers
// treat it like any other policy error and fail closed.
var errNoSiteStore = errors.New("no site store configured to resolve site policy")

func (s *Server) policyForSite(ctx context.Context, site string) (*AccessPolicy, error) {
	if s.policies != nil {
		policy, err, checkedStore := s.policies.ForWithStoreStatus(site)
		if err != nil || policy != nil || checkedStore {
			return policy, err
		}
	}
	// Reaching here means no cached policy entry resolved the site and no
	// site store is wired to read _access.json. Without a store the site's
	// policy cannot be determined, so fail closed (deny) rather than treat
	// the site as open. Production always wires a site store, so this is a
	// defense-in-depth guard against a future caller losing one.
	if s.sites == nil {
		return nil, errNoSiteStore
	}
	rc, _, err := s.sites.Open(ctx, site, accessFileName)
	if err != nil {
		if siteObjectNotFound(err) {
			if s.policies != nil {
				s.policies.Set(site, nil, nil)
			}
			return nil, nil
		}
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	policy, err := parseAccessPolicy(site, raw)
	if s.policies != nil {
		if err != nil {
			s.policies.Set(site, nil, err)
		} else {
			s.policies.Set(site, policy, nil)
		}
	}
	return policy, err
}

func siteObjectNotFound(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var res minio.ErrorResponse
	return errors.As(err, &res) && (res.StatusCode == 404 || res.Code == "NoSuchKey" || res.Code == "NoSuchBucket")
}

func (s *Server) policySummaryForSite(ctx context.Context, site string) (bool, int, bool) {
	policy, err := s.policyForSite(ctx, site)
	if err != nil {
		return true, 0, false
	}
	if policy == nil {
		return false, 0, true
	}
	return policy.RestrictsAccess(), len(policy.Allow), policy.AllowsDownload()
}
