package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	typeAssetDir         = "_type"
	typeManifestFile     = "type.json"
	typeEntryFile        = "index.html"
	typeSkillFile        = "SKILL.md"
	typeManifestFormat   = 1
	maxTypeTitleLength   = 80
	maxTypeDescLength    = 500
	maxTypeDataEntries   = 64
	typeTrackPinned      = "pinned"
	typeTrackMajor       = "major"
	typeGlobCharacters   = "*?[]{}"
	typeObjectRootPrefix = "_types/"
)

var (
	ErrTypeNotFound   = errors.New("site type version not found")
	ErrTypeExists     = errors.New("site type version already exists")
	ErrTypePinChanged = errors.New("site type pin changed concurrently")
)

// siteTypeRef is the type a site pins. The zero value is an ordinary site.
type siteTypeRef struct {
	Name    string
	Version string
	Track   string
}

func (r siteTypeRef) typed() bool { return r.Name != "" }

func (r siteTypeRef) String() string {
	if !r.typed() {
		return ""
	}
	return r.Name + "@" + r.Version
}

type typeInventoryFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type typeManifest struct {
	Format      int      `json:"format"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Data        []string `json:"data"`
}

// siteTypeVersion is one immutable uploaded version. Only yank state
// changes after upload.
type siteTypeVersion struct {
	Name           string
	Version        string
	Sem            semVersion
	Title          string
	Description    string
	Data           []string
	Manifest       json.RawMessage
	Hash           string
	Files          []typeInventoryFile
	HasSkill       bool
	UploaderEmail  string
	UploaderPeerIP string
	UploaderName   string
	UploadedAt     time.Time
	YankedAt       *time.Time
}

func (v *siteTypeVersion) Yanked() bool { return v.YankedAt != nil }

// Selectable versions are what "latest" and following may choose.
func (v *siteTypeVersion) Selectable() bool { return !v.Yanked() && !v.Sem.Prerelease() }

func (v *siteTypeVersion) file(path string) (typeInventoryFile, bool) {
	i := sort.Search(len(v.Files), func(i int) bool { return v.Files[i].Path >= path })
	if i < len(v.Files) && v.Files[i].Path == path {
		return v.Files[i], true
	}
	return typeInventoryFile{}, false
}

// typeError is an API error with a machine-readable code, shared by the
// types API, typed deploys and repins.
type typeError struct {
	status  int
	code    string
	message string
	fields  map[string]any
}

func newTypeError(status int, code, message string) *typeError {
	return &typeError{status: status, code: code, message: message}
}

func (e *typeError) Error() string { return e.message }

func (e *typeError) with(key string, value any) *typeError {
	if e.fields == nil {
		e.fields = map[string]any{}
	}
	e.fields[key] = value
	return e
}

func writeTypeError(w http.ResponseWriter, e *typeError) {
	body := map[string]any{"error": e.message}
	if e.code != "" {
		body["code"] = e.code
	}
	for k, v := range e.fields {
		body[k] = v
	}
	writeJSON(w, e.status, body)
}

func invalidPackage(message, path string) *typeError {
	e := newTypeError(http.StatusBadRequest, "invalid_package", message)
	if path != "" {
		e.with("path", path)
	}
	return e
}

// parseTypePackage validates an uploaded package and builds its version
// record. It is strict: anything an ordinary deploy would drop is an error,
// so an author learns about it instead of shipping without it.
func parseTypePackage(files []deployFile) (*siteTypeVersion, *typeError) {
	if len(files) == 0 {
		return nil, invalidPackage("the package contains no files", "")
	}
	if len(files) > maxDeployFiles {
		return nil, invalidPackage(fmt.Sprintf("too many files in the package (max %d)", maxDeployFiles), "")
	}
	var manifestData []byte
	hasEntry, hasSkill := false, false
	for _, f := range files {
		if err := validateContractPath(f.path); err != nil {
			return nil, invalidPackage(err.Error(), f.path)
		}
		if junkPath(f.path) {
			return nil, invalidPackage(fmt.Sprintf("path %q is not allowed in a package: dot files, node_modules and Thumbs.db are rejected", f.path), f.path)
		}
		switch f.path {
		case typeManifestFile:
			manifestData = f.data
		case typeEntryFile:
			hasEntry = true
		case typeSkillFile:
			hasSkill = true
		}
	}
	if err := validateDeployPathShapes(files); err != nil {
		return nil, invalidPackage(err.Error(), "")
	}
	if manifestData == nil {
		return nil, invalidPackage("the package needs a type.json at its root", typeManifestFile)
	}
	if !hasEntry {
		return nil, invalidPackage("the package needs an index.html at its root", typeEntryFile)
	}
	manifest, sem, err := parseTypeManifest(manifestData)
	if err != nil {
		return nil, invalidPackage("invalid type.json: "+err.Error(), typeManifestFile)
	}
	inventory := make([]typeInventoryFile, 0, len(files))
	for _, f := range files {
		sum := sha256.Sum256(f.data)
		inventory = append(inventory, typeInventoryFile{Path: f.path, Size: int64(len(f.data)), SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(inventory, func(i, j int) bool { return inventory[i].Path < inventory[j].Path })
	return &siteTypeVersion{
		Name:        manifest.Name,
		Version:     sem.String(),
		Sem:         sem,
		Title:       manifest.Title,
		Description: manifest.Description,
		Data:        manifest.Data,
		Manifest:    append(json.RawMessage(nil), manifestData...),
		Hash:        typePackageHash(inventory),
		Files:       inventory,
		HasSkill:    hasSkill,
	}, nil
}

func parseTypeManifest(data []byte) (typeManifest, semVersion, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return typeManifest{}, semVersion{}, errors.New("must be a JSON object")
	}
	for _, field := range []string{"format", "name", "version", "title", "data"} {
		if _, ok := raw[field]; !ok {
			return typeManifest{}, semVersion{}, fmt.Errorf("%q is required", field)
		}
	}
	var m typeManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return typeManifest{}, semVersion{}, err
	}
	if m.Format != typeManifestFormat {
		return typeManifest{}, semVersion{}, fmt.Errorf("format must be %d", typeManifestFormat)
	}
	if !siteNameRe.MatchString(m.Name) {
		return typeManifest{}, semVersion{}, errors.New("name must be a DNS label: 1-63 lowercase letters, digits or hyphens, starting and ending with a letter or digit")
	}
	sem, err := parseSemVer(m.Version)
	if err != nil {
		return typeManifest{}, semVersion{}, err
	}
	m.Title = strings.TrimSpace(m.Title)
	if m.Title == "" || len([]rune(m.Title)) > maxTypeTitleLength {
		return typeManifest{}, semVersion{}, fmt.Errorf("title must be 1-%d characters", maxTypeTitleLength)
	}
	if len([]rune(m.Description)) > maxTypeDescLength {
		return typeManifest{}, semVersion{}, fmt.Errorf("description must be at most %d characters", maxTypeDescLength)
	}
	if err := validateTypeDataEntries(m.Data); err != nil {
		return typeManifest{}, semVersion{}, err
	}
	return m, sem, nil
}

func validateTypeDataEntries(entries []string) error {
	if len(entries) == 0 || len(entries) > maxTypeDataEntries {
		return fmt.Errorf("data must list 1-%d paths", maxTypeDataEntries)
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if strings.ContainsAny(entry, typeGlobCharacters) {
			return fmt.Errorf("data entry %q must be an exact path or a prefix ending in /, not a glob", entry)
		}
		if err := validateContractPath(strings.TrimSuffix(entry, "/")); err != nil {
			return fmt.Errorf("data entry %q: %v", entry, err)
		}
		if reservedDataSegment(entry) {
			return fmt.Errorf("data entry %q covers a name Spot reserves (index.html, _type/, platform files, SDK files, api/)", entry)
		}
		if seen[entry] {
			return fmt.Errorf("data entry %q is listed twice", entry)
		}
		seen[entry] = true
	}
	return nil
}

// typePackageHash is sha256 over the sorted inventory lines
// "path\tsize\tsha256\n", the documented package identity.
func typePackageHash(inventory []typeInventoryFile) string {
	h := sha256.New()
	for _, f := range inventory {
		fmt.Fprintf(h, "%s\t%d\t%s\n", f.Path, f.Size, f.SHA256)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// TypeFileStorage stores package files under a root no site name can
// occupy, through its own key builder.
type TypeFileStorage interface {
	PutTypeFile(ctx context.Context, name, version, path, contentType string, data []byte) error
	OpenTypeFile(ctx context.Context, name, version, path string) (io.ReadCloser, error)
}

func typeObjectKey(name, version, path string) (string, error) {
	if !siteNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid type name %q", name)
	}
	if sem, err := parseRegistryVersion(version); err != nil || sem.String() != version {
		return "", fmt.Errorf("invalid type version %q", version)
	}
	if err := validateContractPath(path); err != nil {
		return "", err
	}
	return typeObjectRootPrefix + name + "/" + version + "/" + path, nil
}

// TypeRegistry is the SQLite side of site types: immutable versions and
// the pins sites hold.
type TypeRegistry struct {
	db *sql.DB
}

func NewTypeRegistry(db *sql.DB) *TypeRegistry {
	return &TypeRegistry{db: db}
}

const selectTypeVersionSQL = `SELECT name, version, title, manifest, package_hash, inventory, has_skill,
	uploader_email, uploader_peer_ip, uploader_name, uploaded_at, yanked_at
	FROM site_type_versions`

func scanTypeVersion(row siteRecordScanner) (*siteTypeVersion, error) {
	var v siteTypeVersion
	var manifest, inventory string
	var yanked sql.NullTime
	if err := row.Scan(&v.Name, &v.Version, &v.Title, &manifest, &v.Hash, &inventory, &v.HasSkill,
		&v.UploaderEmail, &v.UploaderPeerIP, &v.UploaderName, &v.UploadedAt, &yanked); err != nil {
		return nil, err
	}
	sem, err := parseRegistryVersion(v.Version)
	if err != nil {
		return nil, fmt.Errorf("stored type version %s@%s: %w", v.Name, v.Version, err)
	}
	v.Sem = sem
	var m typeManifest
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		return nil, fmt.Errorf("stored manifest %s@%s: %w", v.Name, v.Version, err)
	}
	v.Description, v.Data, v.Manifest = m.Description, m.Data, json.RawMessage(manifest)
	if err := json.Unmarshal([]byte(inventory), &v.Files); err != nil {
		return nil, fmt.Errorf("stored inventory %s@%s: %w", v.Name, v.Version, err)
	}
	if yanked.Valid {
		t := yanked.Time
		v.YankedAt = &t
	}
	return &v, nil
}

func (r *TypeRegistry) Version(ctx context.Context, name, version string) (*siteTypeVersion, error) {
	v, err := scanTypeVersion(r.db.QueryRowContext(ctx, selectTypeVersionSQL+` WHERE name = ? AND version = ?`, name, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTypeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read type %s@%s: %w", name, version, err)
	}
	return v, nil
}

// Versions returns a type's versions, newest first by SemVer precedence.
func (r *TypeRegistry) Versions(ctx context.Context, name string) ([]*siteTypeVersion, error) {
	return r.queryVersions(ctx, ` WHERE name = ?`, name)
}

// AllVersions returns every version of every type, grouped by name and
// newest first within a type.
func (r *TypeRegistry) AllVersions(ctx context.Context) ([]*siteTypeVersion, error) {
	return r.queryVersions(ctx, "")
}

func (r *TypeRegistry) queryVersions(ctx context.Context, where string, args ...any) ([]*siteTypeVersion, error) {
	rows, err := r.db.QueryContext(ctx, selectTypeVersionSQL+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list type versions: %w", err)
	}
	defer rows.Close()
	var out []*siteTypeVersion
	for rows.Next() {
		v, err := scanTypeVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list type versions: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return compareSemVer(out[i].Sem, out[j].Sem) > 0
	})
	return out, nil
}

// NewestSelectable returns the newest non-yanked, non-prerelease version of
// a type within one major, or nil.
func (r *TypeRegistry) NewestSelectable(ctx context.Context, name string, major uint64) (*siteTypeVersion, error) {
	versions, err := r.Versions(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.Selectable() && v.Sem.Major == major {
			return v, nil
		}
	}
	return nil, nil
}

func (r *TypeRegistry) Insert(ctx context.Context, v *siteTypeVersion) error {
	inventory, err := json.Marshal(v.Files)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO site_type_versions
		(name, version, major, minor, patch, prerelease, title, manifest, package_hash, inventory, has_skill,
		 uploader_email, uploader_peer_ip, uploader_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.Name, v.Version, v.Sem.Major, v.Sem.Minor, v.Sem.Patch, strings.Join(v.Sem.Pre, "."),
		v.Title, string(v.Manifest), v.Hash, string(inventory), v.HasSkill,
		strings.ToLower(v.UploaderEmail), v.UploaderPeerIP, v.UploaderName)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return ErrTypeExists
		}
		return fmt.Errorf("record type %s@%s: %w", v.Name, v.Version, err)
	}
	return nil
}

// SetYanked yanks or un-yanks a version; both are idempotent and keep the
// original yank time on a repeated yank.
func (r *TypeRegistry) SetYanked(ctx context.Context, name, version string, yanked bool) error {
	query := `UPDATE site_type_versions SET yanked_at = NULL WHERE name = ? AND version = ?`
	if yanked {
		query = `UPDATE site_type_versions SET yanked_at = COALESCE(yanked_at, strftime('%Y-%m-%d %H:%M:%f', 'now'))
			WHERE name = ? AND version = ?`
	}
	result, err := r.db.ExecContext(ctx, query, name, version)
	if err != nil {
		return fmt.Errorf("yank type %s@%s: %w", name, version, err)
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return ErrTypeNotFound
	}
	return nil
}

// PinCounts maps a type's versions to the number of active sites pinning
// them.
func (r *TypeRegistry) PinCounts(ctx context.Context, name string) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT type_version, COUNT(*) FROM sites
		WHERE type_name = ? AND state = 'active' GROUP BY type_version`, name)
	if err != nil {
		return nil, fmt.Errorf("count type pins: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var version string
		var n int
		if err := rows.Scan(&version, &n); err != nil {
			return nil, err
		}
		counts[version] = n
	}
	return counts, rows.Err()
}

// SiteType returns the site's pin; the zero ref for an ordinary site.
func (r *TypeRegistry) SiteType(ctx context.Context, site string) (siteTypeRef, error) {
	var ref siteTypeRef
	err := r.db.QueryRowContext(ctx, `SELECT type_name, type_version, type_track FROM sites WHERE name = ?`, site).
		Scan(&ref.Name, &ref.Version, &ref.Track)
	if errors.Is(err, sql.ErrNoRows) {
		return siteTypeRef{}, ErrSiteNotFound
	}
	if err != nil {
		return siteTypeRef{}, fmt.Errorf("read site type for %s: %w", site, err)
	}
	return ref, nil
}

// SetDeployedSiteType commits a deploy's pin, fenced by the content
// generation the deploy reserved, so it cannot land on a site another
// mutation has since claimed.
func (r *TypeRegistry) SetDeployedSiteType(ctx context.Context, site string, generation int64, ref siteTypeRef) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sites SET type_name = ?, type_version = ?, type_track = ?
		WHERE name = ? AND content_generation = ?`, ref.Name, ref.Version, ref.Track, site, generation)
	if err != nil {
		return fmt.Errorf("record site type for %s: %w", site, err)
	}
	if n, err := result.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("record site type for %s: %w", site, ErrTypePinChanged)
	}
	return nil
}

// Repin moves a site from one pin to another, conditioned on the pin it
// was decided against.
func (r *TypeRegistry) Repin(ctx context.Context, site string, from, to siteTypeRef) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sites SET type_version = ?, type_track = ?
		WHERE name = ? AND type_name = ? AND type_version = ? AND type_track = ? AND type_name = ?`,
		to.Version, to.Track, site, from.Name, from.Version, from.Track, to.Name)
	if err != nil {
		return fmt.Errorf("repin %s: %w", site, err)
	}
	if n, err := result.RowsAffected(); err == nil && n != 1 {
		return ErrTypePinChanged
	}
	return nil
}

// Followers lists the sites following a type's major.
func (r *TypeRegistry) Followers(ctx context.Context, name string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name FROM sites
		WHERE type_name = ? AND type_track = 'major' AND state IN ('active', 'provisioning')
		ORDER BY name`, name)
	if err != nil {
		return nil, fmt.Errorf("list type followers: %w", err)
	}
	defer rows.Close()
	var sites []string
	for rows.Next() {
		var site string
		if err := rows.Scan(&site); err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

// typeMutationLock serializes uploads and yank changes of one type name.
func (s *Server) typeMutationLock(name string) *sync.Mutex {
	s.typeLocksMu.Lock()
	defer s.typeLocksMu.Unlock()
	if s.typeLocks == nil {
		s.typeLocks = make(map[string]*sync.Mutex)
	}
	lock := s.typeLocks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		s.typeLocks[name] = lock
	}
	return lock
}

// typeVersion reads a version through a process cache. Everything but the
// yank state is immutable, so only the inventory-bearing record is cached
// and callers that decide on yank state read the registry directly.
func (s *Server) cachedTypeVersion(ctx context.Context, name, version string) (*siteTypeVersion, error) {
	key := name + "@" + version
	if v, ok := s.typeVersionCache.Load(key); ok {
		return v.(*siteTypeVersion), nil
	}
	v, err := s.types.Version(ctx, name, version)
	if err != nil {
		return nil, err
	}
	s.typeVersionCache.Store(key, v)
	return v, nil
}
