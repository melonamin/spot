package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
)

const maxRepinBody = 4 << 10

type siteTypeRepinRequest struct {
	Version *string `json:"version"`
	Track   *string `json:"track"`
}

// handleSiteTypeRepin changes a typed site's pinned version or track without
// a deploy. It follows the access route's gate: the site lock, then a
// management decision by identity, so _access.json and publishing keys
// never grant it.
func (s *Server) handleSiteTypeRepin(w http.ResponseWriter, r *http.Request) {
	if !s.requireSitesAPI(w, r) {
		return
	}
	if s.types == nil || s.sites == nil {
		httpError(w, http.StatusServiceUnavailable, "site types are not configured")
		return
	}
	site := r.PathValue("name")
	if !siteNameRe.MatchString(site) {
		httpError(w, http.StatusBadRequest, "invalid site name")
		return
	}
	manager, ok := s.siteManager.(interface {
		ManagementDecision(context.Context, string, Identity) (ManagementDecision, error)
	})
	if !ok {
		httpError(w, http.StatusServiceUnavailable, "site management is not configured")
		return
	}
	if r.Header.Get("Authorization") != "" {
		writeTypeError(w, newTypeError(http.StatusForbidden, "type_repin_forbidden", "a publishing key cannot repin a site"))
		return
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRepinBody))
	if err != nil {
		httpError(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	var req siteTypeRepinRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeTypeError(w, invalidTypeField("body must be a JSON object with version and/or track"))
		return
	}
	if req.Version == nil && req.Track == nil {
		writeTypeError(w, invalidTypeField("send version, track, or both"))
		return
	}
	if req.Version != nil {
		if sem, err := parseRegistryVersion(*req.Version); err != nil || sem.String() != *req.Version {
			writeTypeError(w, invalidTypeField("version must be an exact version"))
			return
		}
	}
	if req.Track != nil && *req.Track != typeTrackPinned && *req.Track != typeTrackMajor {
		writeTypeError(w, invalidTypeField("track must be pinned or major"))
		return
	}

	siteLock := s.siteMutationLock(site)
	siteLock.Lock()
	defer siteLock.Unlock()
	decision, err := manager.ManagementDecision(r.Context(), site, actor)
	denial := "actor is not the site owner, a maintainer, or a platform admin"
	if errors.Is(err, ErrManagementPolicyUnresolved) && decision.State == SiteStateActive {
		decision, err = ManagementDecision{State: decision.State}, nil
		denial = "actor is not the site owner or a platform admin, and a pending policy transition hides the maintainers list"
	}
	switch {
	case errors.Is(err, ErrSiteNotFound) || (err == nil && decision.State != SiteStateActive):
		httpError(w, http.StatusNotFound, "no active site named "+site)
		return
	case err != nil:
		log.Printf("repin %s: authorize: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not authorize the repin")
		return
	case !decision.Allowed:
		s.recordDeployAudit(r, DeployAuditEvent{Site: site, Actor: actor, Action: "type", Status: "denied", Message: denial})
		httpError(w, http.StatusForbidden, "only the site owner, a maintainer, or a platform admin can repin this site")
		return
	}
	current, err := s.types.SiteType(r.Context(), site)
	if err != nil {
		log.Printf("repin %s: read site type: %v", site, err)
		httpError(w, http.StatusInternalServerError, "could not read the site's type")
		return
	}
	if !current.typed() {
		writeTypeError(w, newTypeError(http.StatusConflict, "site_untyped", site+" is not a typed site"))
		return
	}
	next := current
	if req.Version != nil {
		target, terr, err := s.selectableTypeVersion(r.Context(), current.Name, *req.Version)
		if err != nil {
			log.Printf("repin %s: read target: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not read the type registry")
			return
		}
		if terr != nil {
			writeTypeError(w, terr)
			return
		}
		if terr, err := s.validateRetainedData(r.Context(), site, target); err != nil {
			log.Printf("repin %s: validate retained data: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not read the site's files")
			return
		} else if terr != nil {
			writeTypeError(w, terr)
			return
		}
		next.Version, next.Track = target.Version, typeTrackPinned
	}
	if req.Track != nil {
		next.Track = *req.Track
	}
	if next != current {
		if err := s.types.Repin(r.Context(), site, current, next); err != nil {
			log.Printf("repin %s: %v", site, err)
			httpError(w, http.StatusInternalServerError, "could not repin the site")
			return
		}
		s.recordDeployAudit(r, DeployAuditEvent{
			Site: site, Actor: actor, Action: "type", Status: "success", AuthorizedAs: decision.Role,
			Message: "repinned " + current.String() + " " + current.Track + " to " + next.String() + " " + next.Track,
		})
	}
	if next.Track == typeTrackMajor {
		if move, err := s.followSiteLocked(r, site, actor); err != nil {
			log.Printf("repin %s: follow: %v", site, err)
		} else if move != nil {
			next.Version = move.To
		}
	}
	// followSiteLocked already announced its own move.
	if next.Version != current.Version && (next.Track != typeTrackMajor || req.Version != nil) {
		s.publishSiteTypeEvent(r, site, next)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site":     site,
		"type":     siteTypeJSONFor(next, s.siteTypeTitle(r.Context(), next)),
		"previous": siteTypeJSONFor(current, s.siteTypeTitle(r.Context(), current)),
	})
}

// validateRetainedData checks the files a site already has against a repin
// target's data contract.
func (s *Server) validateRetainedData(ctx context.Context, site string, target *siteTypeVersion) (*typeError, error) {
	paths, err := s.sites.List(ctx, site)
	if err != nil {
		return nil, err
	}
	terr, err := validateTypedSiteFiles(paths, target, func(p string) ([]byte, error) {
		rc, _, err := s.sites.Open(ctx, site, p)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxDeploySize))
	})
	if terr != nil {
		terr = newTypeError(http.StatusConflict, "type_data_incompatible",
			"the site's data does not fit "+target.Name+"@"+target.Version+": "+terr.message).with("path", terr.fields["path"])
	}
	return terr, err
}
