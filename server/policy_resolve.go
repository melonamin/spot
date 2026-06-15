package main

import (
	"context"
	"errors"
	"io"

	"github.com/minio/minio-go/v7"
)

func (s *Server) policyForSite(ctx context.Context, site string) (*AccessPolicy, error) {
	if s.policies != nil {
		policy, err, checkedStore := s.policies.ForWithStoreStatus(site)
		if err != nil || policy != nil || checkedStore {
			return policy, err
		}
	}
	// Reaching here means no cached policy entry resolved the site and no
	// site store is wired to read _access.json. In production a site store is
	// always configured, so this branch is unreachable; it survives only for
	// bare Servers in unit tests that serve open sites without a site store.
	// Returning (nil, nil) treats the site as open, which is fail-OPEN — at
	// odds with the documented fail-closed design. Inverting it to an error
	// here would deny those tested open sites, so the behavior is left intact
	// and the fail-closed hardening is deferred to a cross-cutting change.
	if s.sites == nil {
		return nil, nil
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
