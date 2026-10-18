package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"time"
)

type siteTypeJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Track   string `json:"track"`
	Title   string `json:"title"`
}

func siteTypeJSONFor(ref siteTypeRef, title string) *siteTypeJSON {
	if !ref.typed() {
		return nil
	}
	return &siteTypeJSON{Name: ref.Name, Version: ref.Version, Track: ref.Track, Title: title}
}

type typeListEntryJSON struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Skill       bool   `json:"skill"`
	Selectable  *bool  `json:"selectable,omitempty"`
}

type typeVersionSummaryJSON struct {
	Version    string     `json:"version"`
	Title      string     `json:"title"`
	Prerelease bool       `json:"prerelease"`
	Yanked     bool       `json:"yanked"`
	YankedAt   *time.Time `json:"yanked_at"`
	UploadedAt time.Time  `json:"uploaded_at"`
	Skill      bool       `json:"skill"`
	UploadedBy string     `json:"uploaded_by,omitempty"`
	Pins       *int       `json:"pins,omitempty"`
}

type typeVersionDetailJSON struct {
	Name        string              `json:"name"`
	Version     string              `json:"version"`
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Data        []string            `json:"data"`
	Hash        string              `json:"hash"`
	Skill       bool                `json:"skill"`
	Prerelease  bool                `json:"prerelease"`
	Yanked      bool                `json:"yanked"`
	YankedAt    *time.Time          `json:"yanked_at"`
	UploadedAt  time.Time           `json:"uploaded_at"`
	Manifest    json.RawMessage     `json:"manifest"`
	Files       []typeInventoryFile `json:"files"`
}

type typeFollowMoveJSON struct {
	Site string `json:"site"`
	From string `json:"from"`
	To   string `json:"to"`
}

func typeVersionDetail(v *siteTypeVersion) typeVersionDetailJSON {
	return typeVersionDetailJSON{
		Name: v.Name, Version: v.Version, Title: v.Title, Description: v.Description,
		Data: append([]string{}, v.Data...), Hash: v.Hash, Skill: v.HasSkill,
		Prerelease: v.Sem.Prerelease(), Yanked: v.Yanked(), YankedAt: v.YankedAt,
		UploadedAt: v.UploadedAt, Manifest: v.Manifest, Files: append([]typeInventoryFile{}, v.Files...),
	}
}

func typeVersionSummary(v *siteTypeVersion) typeVersionSummaryJSON {
	return typeVersionSummaryJSON{
		Version: v.Version, Title: v.Title, Prerelease: v.Sem.Prerelease(), Yanked: v.Yanked(),
		YankedAt: v.YankedAt, UploadedAt: v.UploadedAt, Skill: v.HasSkill,
	}
}

func uploaderDisplay(v *siteTypeVersion) string {
	switch {
	case v.UploaderEmail != "":
		return v.UploaderEmail
	case v.UploaderName != "":
		return v.UploaderName
	}
	return v.UploaderPeerIP
}

// requireTypesAPI gates /api/types like the sites API: apex only, so site
// JavaScript cannot reach it with a visitor's identity.
func (s *Server) requireTypesAPI(w http.ResponseWriter, r *http.Request) bool {
	if s.types == nil {
		httpError(w, http.StatusServiceUnavailable, "site types are not configured")
		return false
	}
	if siteFromHost(s.requestHost(r), s.spotDomain) != "" {
		httpError(w, http.StatusBadRequest, "the types API is served on the platform root, not on site subdomains")
		return false
	}
	return true
}

// requireTypeAdmin admits only a platform admin resolved by identity. A
// publishing key is refused outright rather than ignored, so a CI job never
// administers types with the ambient identity of the machine it runs on.
func (s *Server) requireTypeAdmin(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	if !s.requireTypesAPI(w, r) {
		return Identity{}, false
	}
	if r.Header.Get("Authorization") != "" {
		writeTypeError(w, newTypeError(http.StatusForbidden, "admin_required", "publishing keys cannot administer site types"))
		return Identity{}, false
	}
	actor, ok := s.requireDeployIdentity(w, r)
	if !ok {
		return Identity{}, false
	}
	if !allowsAdmin(s.adminPolicy, actor) {
		writeTypeError(w, newTypeError(http.StatusForbidden, "admin_required", "only a platform admin can administer site types"))
		return Identity{}, false
	}
	return actor, true
}

func (s *Server) typeFileStorage(w http.ResponseWriter) (TypeFileStorage, bool) {
	store, ok := s.sites.(TypeFileStorage)
	if !ok || s.sites == nil {
		httpError(w, http.StatusServiceUnavailable, "site store not configured for site types")
		return nil, false
	}
	return store, true
}

func (s *Server) handleTypeUpload(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireTypeAdmin(w, r)
	if !ok {
		return
	}
	store, ok := s.typeFileStorage(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDeploySize)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, http.StatusBadRequest, "request must be multipart/form-data")
		return
	}
	var files []deployFile
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			deployReadError(w, err)
			return
		}
		if part.FormName() != "files" {
			continue
		}
		if len(files) >= maxDeployFiles {
			writeTypeError(w, invalidPackage(fmt.Sprintf("too many files in the package (max %d)", maxDeployFiles), ""))
			return
		}
		data, err := io.ReadAll(part)
		if err != nil {
			deployReadError(w, err)
			return
		}
		files = append(files, deployFile{path: partFilename(part), data: data})
	}
	pkg, terr := parseTypePackage(files)
	if terr != nil {
		writeTypeError(w, terr)
		return
	}
	pkg.UploaderEmail, pkg.UploaderPeerIP, pkg.UploaderName = actor.Email, actor.PeerIP, ownerNameForIdentity(actor)

	lock := s.typeMutationLock(pkg.Name)
	lock.Lock()
	created, stored, terr := s.storeTypePackage(r.Context(), store, pkg, files)
	lock.Unlock()
	if terr != nil {
		writeTypeError(w, terr)
		return
	}
	if created {
		log.Printf("types: %s uploaded %s@%s (%s)", actorKey(actor), stored.Name, stored.Version, stored.Hash)
	}
	// Every successful upload, including an identical retry, resolves
	// followers, so a crash between recording the version and following
	// is repaired by the next upload.
	followed := s.followType(r, stored.Name, actor)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"created":  created,
		"type":     typeVersionDetail(stored),
		"followed": followed,
	})
}

// storeTypePackage runs under the type's upload lock. It checks the existing
// version before writing any object, so a conflicting upload never touches
// a published version's bytes, then writes every file and records the
// version last; only recorded inventory paths are ever served.
func (s *Server) storeTypePackage(ctx context.Context, store TypeFileStorage, pkg *siteTypeVersion, files []deployFile) (bool, *siteTypeVersion, *typeError) {
	existing, err := s.types.Version(ctx, pkg.Name, pkg.Version)
	switch {
	case err == nil:
		if existing.Hash != pkg.Hash {
			return false, nil, newTypeError(http.StatusConflict, "type_version_exists",
				fmt.Sprintf("%s@%s already exists with different contents; versions are immutable, upload a new version", pkg.Name, pkg.Version)).
				with("hash", existing.Hash)
		}
		return false, existing, nil
	case !errors.Is(err, ErrTypeNotFound):
		log.Printf("types: read %s@%s: %v", pkg.Name, pkg.Version, err)
		return false, nil, newTypeError(http.StatusInternalServerError, "", "could not read the type registry")
	}
	for _, f := range files {
		if err := store.PutTypeFile(ctx, pkg.Name, pkg.Version, f.path, contentTypeFor(f.path, f.data), f.data); err != nil {
			log.Printf("types: store %s@%s/%s: %v", pkg.Name, pkg.Version, f.path, err)
			return false, nil, newTypeError(http.StatusInternalServerError, "", "could not store "+f.path)
		}
	}
	if err := s.types.Insert(ctx, pkg); err != nil {
		log.Printf("types: record %s@%s: %v", pkg.Name, pkg.Version, err)
		return false, nil, newTypeError(http.StatusInternalServerError, "", "could not record the type version")
	}
	stored, err := s.types.Version(ctx, pkg.Name, pkg.Version)
	if err != nil {
		log.Printf("types: reread %s@%s: %v", pkg.Name, pkg.Version, err)
		return false, nil, newTypeError(http.StatusInternalServerError, "", "could not read the recorded type version")
	}
	return true, stored, nil
}

func (s *Server) handleTypeYank(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireTypeAdmin(w, r)
	if !ok {
		return
	}
	name, version, ok := typePathValues(w, r)
	if !ok {
		return
	}
	yank := r.Method == http.MethodPost
	lock := s.typeMutationLock(name)
	lock.Lock()
	err := s.types.SetYanked(r.Context(), name, version, yank)
	lock.Unlock()
	if errors.Is(err, ErrTypeNotFound) {
		writeTypeError(w, typeNotFound(name, version))
		return
	}
	if err != nil {
		log.Printf("types: yank %s@%s: %v", name, version, err)
		httpError(w, http.StatusInternalServerError, "could not change the version's yank state")
		return
	}
	verb := "un-yanked"
	if yank {
		verb = "yanked"
	}
	log.Printf("types: %s %s %s@%s", actorKey(actor), verb, name, version)
	followed := []typeFollowMoveJSON{}
	if !yank {
		followed = s.followType(r, name, actor)
	}
	v, err := s.types.Version(r.Context(), name, version)
	if err != nil {
		log.Printf("types: reread %s@%s: %v", name, version, err)
		httpError(w, http.StatusInternalServerError, "could not read the type version")
		return
	}
	body := map[string]any{"name": name}
	summary, _ := json.Marshal(typeVersionSummary(v))
	_ = json.Unmarshal(summary, &body)
	if !yank {
		body["followed"] = followed
	}
	writeJSON(w, http.StatusOK, body)
}

func typeNotFound(name, version string) *typeError {
	if version == "" {
		return newTypeError(http.StatusNotFound, "type_not_found", "no site type named "+name)
	}
	return newTypeError(http.StatusNotFound, "type_not_found", "no site type version "+name+"@"+version)
}

// typePathValues reads {name} and {version}; a malformed one cannot name a
// stored version, so it is reported as not found.
func typePathValues(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	name, version := r.PathValue("name"), r.PathValue("version")
	if !siteNameRe.MatchString(name) {
		writeTypeError(w, typeNotFound(name, version))
		return "", "", false
	}
	if version != "" {
		sem, err := parseRegistryVersion(version)
		if err != nil || sem.String() != version {
			writeTypeError(w, typeNotFound(name, version))
			return "", "", false
		}
	}
	return name, version, true
}

func (s *Server) requireTypeReader(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	if !s.requireTypesAPI(w, r) {
		return Identity{}, false
	}
	return s.requireDeployIdentity(w, r)
}

func (s *Server) handleTypesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireTypeReader(w, r); !ok {
		return
	}
	all := false
	if raw := r.URL.Query().Get("all"); raw != "" {
		switch raw {
		case "true":
			all = true
		case "false":
		default:
			writeTypeError(w, newTypeError(http.StatusBadRequest, "invalid_type_field", "all must be true or false"))
			return
		}
	}
	versions, err := s.types.AllVersions(r.Context())
	if err != nil {
		log.Printf("types: list: %v", err)
		httpError(w, http.StatusInternalServerError, "could not list site types")
		return
	}
	out := []typeListEntryJSON{}
	// Versions arrive grouped by name, newest first within a name.
	for i := 0; i < len(versions); {
		j := i
		for j < len(versions) && versions[j].Name == versions[i].Name {
			j++
		}
		group := versions[i:j]
		i = j
		var chosen *siteTypeVersion
		for _, v := range group {
			if v.Selectable() {
				chosen = v
				break
			}
		}
		selectable := chosen != nil
		if !selectable {
			if !all {
				continue
			}
			chosen = group[0]
		}
		entry := typeListEntryJSON{
			Name: chosen.Name, Version: chosen.Version, Title: chosen.Title,
			Description: chosen.Description, Skill: chosen.HasSkill,
		}
		if all {
			entry.Selectable = &selectable
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"types": out})
}

func (s *Server) handleTypeVersions(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireTypeReader(w, r)
	if !ok {
		return
	}
	name, _, ok := typePathValues(w, r)
	if !ok {
		return
	}
	versions, err := s.types.Versions(r.Context(), name)
	if err != nil {
		log.Printf("types: versions %s: %v", name, err)
		httpError(w, http.StatusInternalServerError, "could not list the type's versions")
		return
	}
	if len(versions) == 0 {
		writeTypeError(w, typeNotFound(name, ""))
		return
	}
	admin := allowsAdmin(s.adminPolicy, actor)
	var pins map[string]int
	if admin {
		if pins, err = s.types.PinCounts(r.Context(), name); err != nil {
			log.Printf("types: pin counts %s: %v", name, err)
			httpError(w, http.StatusInternalServerError, "could not count the type's pins")
			return
		}
	}
	var latest *string
	out := make([]typeVersionSummaryJSON, 0, len(versions))
	for _, v := range versions {
		if latest == nil && v.Selectable() {
			version := v.Version
			latest = &version
		}
		summary := typeVersionSummary(v)
		if admin {
			count := pins[v.Version]
			summary.Pins = &count
			summary.UploadedBy = uploaderDisplay(v)
		}
		out = append(out, summary)
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "latest": latest, "versions": out})
}

func (s *Server) readTypeVersion(w http.ResponseWriter, r *http.Request) (*siteTypeVersion, bool) {
	if _, ok := s.requireTypeReader(w, r); !ok {
		return nil, false
	}
	name, version, ok := typePathValues(w, r)
	if !ok {
		return nil, false
	}
	v, err := s.types.Version(r.Context(), name, version)
	if errors.Is(err, ErrTypeNotFound) {
		writeTypeError(w, typeNotFound(name, version))
		return nil, false
	}
	if err != nil {
		log.Printf("types: read %s@%s: %v", name, version, err)
		httpError(w, http.StatusInternalServerError, "could not read the type version")
		return nil, false
	}
	return v, true
}

func (s *Server) handleTypeVersion(w http.ResponseWriter, r *http.Request) {
	v, ok := s.readTypeVersion(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, typeVersionDetail(v))
}

func (s *Server) handleTypeSkill(w http.ResponseWriter, r *http.Request) {
	v, ok := s.readTypeVersion(w, r)
	if !ok {
		return
	}
	if !v.HasSkill {
		writeTypeError(w, newTypeError(http.StatusNotFound, "type_skill_not_found", v.Name+"@"+v.Version+" has no SKILL.md"))
		return
	}
	store, ok := s.typeFileStorage(w)
	if !ok {
		return
	}
	rc, err := store.OpenTypeFile(r.Context(), v.Name, v.Version, typeSkillFile)
	if err != nil {
		log.Printf("types: open %s@%s/%s: %v", v.Name, v.Version, typeSkillFile, err)
		httpError(w, http.StatusInternalServerError, "could not read SKILL.md")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(fileSize(v, typeSkillFile), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc)
	}
}

func fileSize(v *siteTypeVersion, path string) int64 {
	f, _ := v.file(path)
	return f.Size
}

// typeServedPath reports whether a package file is served under /_type/ on
// a typed site origin; the manifest and agent instructions are not.
func typeServedPath(path string) bool {
	return path != typeManifestFile && path != typeSkillFile
}

// handleTypePackage returns the viewer laid out as a site origin serves it,
// so unpacking it beside a copy of the data gives a local preview.
func (s *Server) handleTypePackage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.readTypeVersion(w, r)
	if !ok {
		return
	}
	store, ok := s.typeFileStorage(w)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": v.Name + "-" + v.Version + ".zip",
	}))
	if r.Method == http.MethodHead {
		return
	}
	zw := zip.NewWriter(w)
	write := func(name, path string) {
		rc, err := store.OpenTypeFile(r.Context(), v.Name, v.Version, path)
		if err != nil {
			log.Printf("types: package %s@%s: open %s: %v", v.Name, v.Version, path, err)
			panic(http.ErrAbortHandler)
		}
		defer rc.Close()
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o644)
		h.Modified = v.UploadedAt
		dst, err := zw.CreateHeader(h)
		if err == nil {
			_, err = io.Copy(dst, rc)
		}
		if err != nil {
			log.Printf("types: package %s@%s: write %s: %v", v.Name, v.Version, path, err)
			panic(http.ErrAbortHandler)
		}
	}
	write(typeEntryFile, typeEntryFile)
	for _, f := range v.Files {
		if typeServedPath(f.Path) {
			write(typeAssetDir+"/"+f.Path, f.Path)
		}
	}
	if err := zw.Close(); err != nil {
		log.Printf("types: package %s@%s: finalize: %v", v.Name, v.Version, err)
		panic(http.ErrAbortHandler)
	}
}

// followType moves every follower of a type forward. Each site is resolved
// under its own mutation lock; the follower list is read first so no site
// lock is awaited while SQLite's single connection is held. The work is
// detached from the request: once a version is recorded or un-yanked, a
// disconnecting client must not leave followers on the old version.
func (s *Server) followType(r *http.Request, name string, actor Identity) []typeFollowMoveJSON {
	moves := []typeFollowMoveJSON{}
	sites, err := s.types.Followers(context.WithoutCancel(r.Context()), name)
	if err != nil {
		log.Printf("types: followers of %s: %v", name, err)
		return moves
	}
	for _, site := range sites {
		lock := s.siteMutationLock(site)
		lock.Lock()
		move, err := s.followSiteLocked(r, site, actor)
		lock.Unlock()
		if err != nil {
			log.Printf("types: follow %s: %v", site, err)
			continue
		}
		if move != nil {
			moves = append(moves, *move)
		}
	}
	return moves
}

// followSiteLocked moves one following site to the newest selectable
// version of its major, forward only. The caller holds the site lock. Like
// the deploy audit, it is detached from the request's cancellation: the
// change that triggered it has already been committed.
func (s *Server) followSiteLocked(r *http.Request, site string, actor Identity) (*typeFollowMoveJSON, error) {
	ctx := context.WithoutCancel(r.Context())
	current, err := s.types.SiteType(ctx, site)
	if err != nil || !current.typed() || current.Track != typeTrackMajor {
		return nil, err
	}
	sem, err := parseRegistryVersion(current.Version)
	if err != nil {
		return nil, err
	}
	candidate, err := s.types.NewestSelectable(ctx, current.Name, sem.Major)
	if err != nil || candidate == nil || compareSemVer(candidate.Sem, sem) <= 0 {
		return nil, err
	}
	next := siteTypeRef{Name: current.Name, Version: candidate.Version, Track: typeTrackMajor}
	if err := s.types.Repin(ctx, site, current, next); err != nil {
		if errors.Is(err, ErrTypePinChanged) {
			return nil, nil
		}
		return nil, err
	}
	s.recordDeployAudit(r, DeployAuditEvent{
		Site: site, Actor: actor, Action: "type", Status: "success",
		Message: "followed " + current.String() + " to " + next.String(),
	})
	s.publishSiteTypeEvent(r, site, next)
	return &typeFollowMoveJSON{Site: site, From: current.Version, To: next.Version}, nil
}

func (s *Server) publishSiteTypeEvent(r *http.Request, site string, ref siteTypeRef) {
	version := time.Now().UTC().Format("20060102T150405.000000000Z")
	s.publishDeployEvent(site, version, s.siteURL(r, site), ref.String())
}
