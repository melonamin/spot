package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// typedDeployFields are the type fields of POST /api/deploy.
type typedDeployFields struct {
	Name      string
	Version   string
	Target    string
	HasTarget bool
	Track     string
	HasTrack  bool
}

func invalidTypeField(message string) *typeError {
	return newTypeError(http.StatusBadRequest, "invalid_type_field", message)
}

// parseTypedDeployFields validates the raw form values. present reports
// which of type, type_target and type_track the form carried, so an empty
// value is an error rather than an absent field.
func parseTypedDeployFields(present map[string]bool, typ, target, track string) (*typedDeployFields, *typeError) {
	if !present["type"] {
		if present["type_target"] || present["type_track"] {
			return nil, invalidTypeField("type_target and type_track require type")
		}
		return nil, nil
	}
	name, version, ok := strings.Cut(typ, "@")
	if !ok || !siteNameRe.MatchString(name) {
		return nil, invalidTypeField("type must be name@version")
	}
	if sem, err := parseRegistryVersion(version); err != nil || sem.String() != version {
		return nil, invalidTypeField("type must name an exact version: " + typ)
	}
	fields := &typedDeployFields{Name: name, Version: version}
	if present["type_target"] {
		if sem, err := parseRegistryVersion(target); err != nil || sem.String() != target {
			return nil, invalidTypeField("type_target must be an exact version")
		}
		fields.Target, fields.HasTarget = target, true
	}
	if present["type_track"] {
		if track != typeTrackPinned && track != typeTrackMajor {
			return nil, invalidTypeField("type_track must be pinned or major")
		}
		fields.Track, fields.HasTrack = track, true
	}
	return fields, nil
}

func typeDataInvalid(path, message string) *typeError {
	return newTypeError(http.StatusBadRequest, "type_data_invalid", message).with("path", path)
}

// normalizeTypedDeploy replaces normalizeDeploy for typed deploys: paths are
// kept exactly as sent (no folder stripping, no index.html requirement) and
// follow the path contract. Junk is dropped as in ordinary deploys.
func normalizeTypedDeploy(files []deployFile) ([]deployFile, *typeError) {
	kept := make([]deployFile, 0, len(files))
	for _, f := range files {
		if err := validateContractPath(f.path); err != nil {
			return nil, typeDataInvalid(f.path, err.Error())
		}
		if junkPath(f.path) {
			continue
		}
		kept = append(kept, f)
	}
	if err := validateDeployPathShapes(kept); err != nil {
		return nil, newTypeError(http.StatusBadRequest, "type_data_invalid", err.Error())
	}
	return kept, nil
}

// validateTypedSiteFiles checks a typed site's files against the version
// whose data contract they must follow. read returns a file's bytes; it is
// called only for JSON files under data paths.
func validateTypedSiteFiles(paths []string, v *siteTypeVersion, read func(string) ([]byte, error)) (*typeError, error) {
	for _, p := range paths {
		first, _, _ := strings.Cut(p, "/")
		switch {
		case p == typeEntryFile:
			return typeDataInvalid(p, "index.html is the type's viewer; a typed site cannot ship its own"), nil
		case first == typeAssetDir:
			return typeDataInvalid(p, "_type/ is reserved for the type's viewer files"), nil
		case typedPlatformFile(p):
			continue
		case !dataPathCovers(v.Data, p):
			return typeDataInvalid(p, fmt.Sprintf("%s is outside the data paths of %s@%s (%s)", p, v.Name, v.Version, strings.Join(v.Data, ", "))), nil
		}
		if strings.HasSuffix(strings.ToLower(p), ".json") {
			data, err := read(p)
			if err != nil {
				return nil, err
			}
			if !json.Valid(data) {
				return typeDataInvalid(p, p+" is not valid JSON"), nil
			}
		}
	}
	return nil, nil
}

func validateTypedDeployFiles(files []deployFile, v *siteTypeVersion) *typeError {
	paths := make([]string, len(files))
	byPath := make(map[string][]byte, len(files))
	for i, f := range files {
		paths[i] = f.path
		byPath[f.path] = f.data
	}
	terr, _ := validateTypedSiteFiles(paths, v, func(p string) ([]byte, error) { return byPath[p], nil })
	return terr
}

// siteTypePlan is a typed deploy's outcome decided under the site lock.
type siteTypePlan struct {
	pin      siteTypeRef      // the pin after the deploy, before following
	validate *siteTypeVersion // the data contract the files must follow
	commit   bool             // whether the pin differs from the stored one
}

// planSiteType applies the pin rules. current is the stored pin; newPin is
// true when the deploy creates, recreates or recovers the site, so there is
// no active pin to protect.
func (s *Server) planSiteType(ctx context.Context, principal DeployPrincipal, req *typedDeployFields, current siteTypeRef, newPin bool) (*siteTypePlan, *typeError, error) {
	if req == nil {
		switch {
		case newPin && current.typed():
			return &siteTypePlan{commit: true}, nil, nil
		case !newPin && current.typed():
			return nil, newTypeError(http.StatusConflict, "type_required",
				"site now pins "+current.String()+"; deploy it with type="+current.String()).with("current", current.String()), nil
		}
		return nil, nil, nil
	}
	if newPin || !current.typed() {
		if req.HasTarget {
			return nil, invalidTypeField("type_target applies only to an existing typed site"), nil
		}
		v, terr, err := s.selectableTypeVersion(ctx, req.Name, req.Version)
		if terr != nil || err != nil {
			return nil, terr, err
		}
		track := typeTrackPinned
		if req.HasTrack {
			track = req.Track
		}
		pin := siteTypeRef{Name: req.Name, Version: req.Version, Track: track}
		return &siteTypePlan{pin: pin, validate: v, commit: pin != current}, nil, nil
	}
	if req.Name != current.Name {
		return nil, newTypeError(http.StatusConflict, "type_name_mismatch",
			"site now pins "+current.String()+", not type "+req.Name).with("current", current.String()), nil
	}
	if principal.PublishingKey && (req.HasTarget || (req.HasTrack && req.Track != current.Track)) {
		return nil, newTypeError(http.StatusForbidden, "type_repin_forbidden",
			"a publishing key cannot repin a site or change its track"), nil
	}
	if req.Version == current.Version && req.HasTarget {
		v, terr, err := s.selectableTypeVersion(ctx, current.Name, req.Target)
		if terr != nil || err != nil {
			return nil, terr, err
		}
		track := typeTrackPinned
		if req.HasTrack {
			track = req.Track
		}
		pin := siteTypeRef{Name: current.Name, Version: req.Target, Track: track}
		return &siteTypePlan{pin: pin, validate: v, commit: pin != current}, nil, nil
	}
	if req.Version != current.Version {
		pinMismatch := newTypeError(http.StatusConflict, "type_pin_mismatch",
			"site now pins "+current.String()+"; re-import it before publishing").with("current", current.String())
		if current.Track != typeTrackMajor || req.HasTarget {
			return nil, pinMismatch, nil
		}
		// A follower's pin moved within an unchanged data contract; a deploy
		// written against the same major does not change the pin.
		if _, err := s.types.Version(ctx, req.Name, req.Version); errors.Is(err, ErrTypeNotFound) {
			return nil, newTypeError(http.StatusBadRequest, "type_not_found", "no site type version "+req.Name+"@"+req.Version), nil
		} else if err != nil {
			return nil, nil, err
		}
		reqSem, _ := parseRegistryVersion(req.Version)
		curSem, err := parseRegistryVersion(current.Version)
		if err != nil {
			return nil, nil, err
		}
		if reqSem.Major != curSem.Major {
			return nil, pinMismatch, nil
		}
	}
	v, err := s.types.Version(ctx, current.Name, current.Version)
	if err != nil {
		return nil, nil, fmt.Errorf("read pinned version %s: %w", current, err)
	}
	pin := current
	if req.HasTrack {
		pin.Track = req.Track
	}
	return &siteTypePlan{pin: pin, validate: v, commit: pin != current}, nil, nil
}

// missingTypeError decides, for an authorized deploy whose bundle failed
// ordinary normalization, whether the real problem is a missing type: the
// site is active and typed. It returns nil when the ordinary error stands.
func (s *Server) missingTypeError(ctx context.Context, principal DeployPrincipal, site string, authz DeployAuthorization) (*typeError, error) {
	if authz.Action != "update" || authz.PreviousState != SiteStateActive {
		return nil, nil
	}
	current, err := s.types.SiteType(ctx, site)
	if err != nil || !current.typed() {
		return nil, err
	}
	_, terr, err := s.planSiteType(ctx, principal, nil, current, false)
	return terr, err
}

// selectableTypeVersion reads a version a site is about to pin: it must
// exist and not be yanked.
func (s *Server) selectableTypeVersion(ctx context.Context, name, version string) (*siteTypeVersion, *typeError, error) {
	v, err := s.types.Version(ctx, name, version)
	if errors.Is(err, ErrTypeNotFound) {
		return nil, newTypeError(http.StatusBadRequest, "type_not_found", "no site type version "+name+"@"+version), nil
	}
	if err != nil {
		return nil, nil, err
	}
	if v.Yanked() {
		return nil, newTypeError(http.StatusConflict, "type_yanked", name+"@"+version+" is yanked; choose another version"), nil
	}
	return v, nil, nil
}

// siteTypeTitle is the pinned version's title for API responses.
func (s *Server) siteTypeTitle(ctx context.Context, ref siteTypeRef) string {
	if !ref.typed() {
		return ""
	}
	v, err := s.cachedTypeVersion(ctx, ref.Name, ref.Version)
	if err != nil {
		log.Printf("types: title %s: %v", ref, err)
		return ""
	}
	return v.Title
}
