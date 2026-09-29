package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/zeebo/blake3"
)

const (
	cloudflareConfigDisabled = "disabled"
	cloudflareConfigPartial  = "partial"
	cloudflareConfigEnabled  = "enabled"

	defaultCloudflareProjectPrefix = "spot-"
	// Cloudflare's project-name schema reserves room for generated hostname
	// suffixes, so it is stricter than the 63-byte DNS-label limit Spot uses.
	maxCloudflareProjectNameLength = 58
	cloudflareProjectHashLength    = 12
	maxCloudflareFileSize          = 25 << 20
	maxCloudflareFiles             = 20_000
	// Match Wrangler's conservative pre-base64 upload bucket size.
	maxCloudflareAssetBatchSize   = 40 << 20
	maxCloudflareAssetBatchFiles  = 1000
	cloudflareStateWriteTimeout   = 5 * time.Second
	cloudflareDomainActiveTimeout = 3 * time.Minute
	cloudflarePublishTimeout      = 10 * time.Minute
)

var (
	errCloudflareNotFound          = errors.New("cloudflare resource not found")
	errCloudflareConflict          = errors.New("cloudflare resource conflict")
	errCloudflarePublicationExists = errors.New("cloudflare publication exists")
	errCloudflareNotConfigured     = errors.New("cloudflare publishing is not configured")
	errCloudflareCleanupUnknown    = errors.New("legacy Cloudflare cleanup location is unknown")
	errCloudflareCleanupNotUnknown = errors.New("Cloudflare publication does not have unknown legacy cleanup state")
	errCloudflareProjectUncertain  = errors.New("Cloudflare Pages project creation ownership is uncertain")
	errCloudflareProjectNotPending = errors.New("Cloudflare Pages project operation is not awaiting manual resolution")
	errCloudflareProjectResolution = errors.New("project resolution must be owned, unmanaged, or absent")
	errCloudflareLegacyConfirm     = errors.New("confirm that the legacy Pages project, DNS record, and Access application were removed")
)

type cloudflareConfig struct {
	APIToken      string
	AccountID     string
	ZoneID        string
	BaseDomain    string
	ProjectPrefix string
	AccessIDPID   string
	Status        string
	Missing       []string
}

func loadCloudflareConfigFromEnv() cloudflareConfig {
	cfg := cloudflareConfig{
		APIToken:      strings.TrimSpace(os.Getenv("SPOT_CLOUDFLARE_API_TOKEN")),
		AccountID:     strings.TrimSpace(os.Getenv("SPOT_CLOUDFLARE_ACCOUNT_ID")),
		ZoneID:        strings.TrimSpace(os.Getenv("SPOT_CLOUDFLARE_ZONE_ID")),
		BaseDomain:    strings.Trim(strings.ToLower(strings.TrimSpace(os.Getenv("SPOT_CLOUDFLARE_BASE_DOMAIN"))), "."),
		ProjectPrefix: strings.TrimSpace(envOr("SPOT_CLOUDFLARE_PROJECT_PREFIX", defaultCloudflareProjectPrefix)),
		AccessIDPID:   strings.TrimSpace(os.Getenv("SPOT_CLOUDFLARE_ACCESS_IDP_ID")),
	}
	required := []struct {
		key string
		val string
	}{
		{"SPOT_CLOUDFLARE_API_TOKEN", cfg.APIToken},
		{"SPOT_CLOUDFLARE_ACCOUNT_ID", cfg.AccountID},
		{"SPOT_CLOUDFLARE_ZONE_ID", cfg.ZoneID},
		{"SPOT_CLOUDFLARE_BASE_DOMAIN", cfg.BaseDomain},
	}
	for _, req := range required {
		if req.val == "" {
			cfg.Missing = append(cfg.Missing, req.key)
		}
	}
	switch len(cfg.Missing) {
	case len(required):
		cfg.Status = cloudflareConfigDisabled
	case 0:
		cfg.Status = cloudflareConfigEnabled
	default:
		cfg.Status = cloudflareConfigPartial
	}
	if cfg.ProjectPrefix == "" {
		cfg.ProjectPrefix = defaultCloudflareProjectPrefix
	}
	return cfg
}

func (c cloudflareConfig) Enabled() bool {
	return c.Status == cloudflareConfigEnabled
}

func (c cloudflareConfig) AccessEnabled() bool {
	return c.Enabled() && c.AccessIDPID != ""
}

func (c cloudflareConfig) Hostname(site string) string {
	if c.BaseDomain == "" {
		return ""
	}
	return site + "." + c.BaseDomain
}

func (c cloudflareConfig) ProjectName(site string) string {
	return cloudflareProjectName(c.ProjectPrefix + site)
}

// cloudflareProjectName preserves readable names when they already satisfy
// Cloudflare's schema. Longer names retain a readable prefix and gain a stable
// hash suffix so every valid Spot site name remains publishable without
// collisions caused by truncation. Normalizing the optional operator-supplied
// prefix also prevents provider-side failures from invalid punctuation or case.
func cloudflareProjectName(raw string) string {
	var normalized strings.Builder
	normalized.Grow(len(raw))
	separator := false
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if separator && normalized.Len() > 0 {
				normalized.WriteByte('-')
			}
			normalized.WriteRune(r)
			separator = false
		case r == '-':
			separator = normalized.Len() > 0
		default:
			separator = normalized.Len() > 0
		}
	}

	name := normalized.String()
	if name == "" {
		name = "spot"
	}
	if len(name) <= maxCloudflareProjectNameLength {
		return name
	}

	digest := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(digest[:])[:cloudflareProjectHashLength]
	head := strings.TrimRight(name[:maxCloudflareProjectNameLength-len(suffix)], "-")
	return head + suffix
}

type cloudflarePublication struct {
	Site                  string    `json:"site"`
	AccountID             string    `json:"-"`
	ZoneID                string    `json:"-"`
	DNSRecordID           string    `json:"-"`
	DNSManaged            bool      `json:"dns_managed"`
	ProjectManaged        bool      `json:"project_managed"`
	CleanupUnknown        bool      `json:"cleanup_unknown"`
	AccessMode            string    `json:"access_mode"`
	AccessEmails          []string  `json:"access_emails,omitempty"`
	RequestedAccessMode   string    `json:"requested_access_mode,omitempty"`
	RequestedAccessEmails []string  `json:"requested_access_emails,omitempty"`
	AccessAppID           string    `json:"-"`
	AccessManaged         bool      `json:"access_managed"`
	ProjectName           string    `json:"project_name"`
	Hostname              string    `json:"hostname"`
	DeploymentID          string    `json:"deployment_id"`
	DeploymentURL         string    `json:"deployment_url"`
	ContentHash           string    `json:"content_hash"`
	FileCount             int       `json:"file_count"`
	TotalBytes            int64     `json:"total_bytes"`
	Status                string    `json:"status"`
	LastError             string    `json:"last_error"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type CloudflarePublicationStore struct {
	db *sql.DB
}

func NewCloudflarePublicationStore(db *sql.DB) *CloudflarePublicationStore {
	return &CloudflarePublicationStore{db: db}
}

func (s *CloudflarePublicationStore) Get(ctx context.Context, site string) (*cloudflarePublication, error) {
	var p cloudflarePublication
	var accessEmails, requestedAccessEmails string
	err := s.db.QueryRowContext(ctx, `SELECT site, account_id, zone_id, dns_record_id, dns_managed, project_managed, cleanup_unknown,
		access_mode, access_emails, requested_access_mode, requested_access_emails, access_app_id, access_managed,
		project_name, hostname, deployment_id,
		deployment_url, content_hash, file_count, total_bytes, status, last_error,
		created_at, updated_at
		FROM site_cloudflare_publications WHERE site = ?`, site).
		Scan(&p.Site, &p.AccountID, &p.ZoneID, &p.DNSRecordID, &p.DNSManaged, &p.ProjectManaged, &p.CleanupUnknown,
			&p.AccessMode, &accessEmails, &p.RequestedAccessMode, &requestedAccessEmails, &p.AccessAppID, &p.AccessManaged,
			&p.ProjectName, &p.Hostname, &p.DeploymentID, &p.DeploymentURL,
			&p.ContentHash, &p.FileCount, &p.TotalBytes, &p.Status, &p.LastError,
			&p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cloudflare publication %s: %w", site, err)
	}
	if err := json.Unmarshal([]byte(accessEmails), &p.AccessEmails); err != nil {
		return nil, fmt.Errorf("read cloudflare publication %s access emails: %w", site, err)
	}
	if err := json.Unmarshal([]byte(requestedAccessEmails), &p.RequestedAccessEmails); err != nil {
		return nil, fmt.Errorf("read cloudflare publication %s requested access emails: %w", site, err)
	}
	if p.AccessMode == "" {
		p.AccessMode = cloudflareAccessPublic
	}
	return &p, nil
}

func (s *CloudflarePublicationStore) Has(ctx context.Context, site string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM site_cloudflare_publications WHERE site = ?)`, site).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check cloudflare publication %s: %w", site, err)
	}
	return exists == 1, nil
}

func (s *CloudflarePublicationStore) Upsert(ctx context.Context, p cloudflarePublication) error {
	accessEmails, err := json.Marshal(p.AccessEmails)
	if err != nil {
		return fmt.Errorf("encode cloudflare publication %s access emails: %w", p.Site, err)
	}
	if p.AccessMode == "" {
		p.AccessMode = cloudflareAccessPublic
	}
	requestedAccessEmails, err := json.Marshal(p.RequestedAccessEmails)
	if err != nil {
		return fmt.Errorf("encode cloudflare publication %s requested access emails: %w", p.Site, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO site_cloudflare_publications
		(site, account_id, zone_id, dns_record_id, dns_managed, project_managed, cleanup_unknown,
		 access_mode, access_emails, requested_access_mode, requested_access_emails, access_app_id, access_managed,
		 project_name, hostname, deployment_id, deployment_url, content_hash, file_count, total_bytes, status, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(site) DO UPDATE SET
			account_id = excluded.account_id,
			zone_id = excluded.zone_id,
			dns_record_id = excluded.dns_record_id,
			dns_managed = excluded.dns_managed,
			project_managed = excluded.project_managed,
			cleanup_unknown = excluded.cleanup_unknown,
			access_mode = excluded.access_mode,
			access_emails = excluded.access_emails,
			requested_access_mode = excluded.requested_access_mode,
			requested_access_emails = excluded.requested_access_emails,
			access_app_id = excluded.access_app_id,
			access_managed = excluded.access_managed,
			project_name = excluded.project_name,
			hostname = excluded.hostname,
			deployment_id = excluded.deployment_id,
			deployment_url = excluded.deployment_url,
			content_hash = excluded.content_hash,
			file_count = excluded.file_count,
			total_bytes = excluded.total_bytes,
			status = excluded.status,
			last_error = excluded.last_error,
			updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')`,
		p.Site, p.AccountID, p.ZoneID, p.DNSRecordID, p.DNSManaged, p.ProjectManaged, p.CleanupUnknown,
		p.AccessMode, string(accessEmails), p.RequestedAccessMode, string(requestedAccessEmails), p.AccessAppID, p.AccessManaged,
		p.ProjectName, p.Hostname, p.DeploymentID, p.DeploymentURL,
		p.ContentHash, p.FileCount, p.TotalBytes, p.Status, p.LastError)
	if err != nil {
		return fmt.Errorf("upsert cloudflare publication %s: %w", p.Site, err)
	}
	return nil
}

func (s *CloudflarePublicationStore) InsertReservation(ctx context.Context, p cloudflarePublication) error {
	accessEmails, err := json.Marshal(p.AccessEmails)
	if err != nil {
		return fmt.Errorf("encode cloudflare reservation %s access emails: %w", p.Site, err)
	}
	requestedAccessEmails, err := json.Marshal(p.RequestedAccessEmails)
	if err != nil {
		return fmt.Errorf("encode cloudflare reservation %s requested access emails: %w", p.Site, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO site_cloudflare_publications
		(site, account_id, zone_id, access_mode, access_emails, requested_access_mode, requested_access_emails, project_name, hostname, content_hash,
		 file_count, total_bytes, status, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		p.Site, p.AccountID, p.ZoneID, p.AccessMode, string(accessEmails), p.RequestedAccessMode, string(requestedAccessEmails),
		p.ProjectName, p.Hostname, p.ContentHash, p.FileCount, p.TotalBytes, p.Status)
	if err != nil {
		return fmt.Errorf("reserve cloudflare publication %s: %w", p.Site, err)
	}
	return nil
}

func (s *CloudflarePublicationStore) RecordFailure(ctx context.Context, site string, snap cloudflareSnapshot, cause error) error {
	result, err := s.db.ExecContext(ctx, `UPDATE site_cloudflare_publications SET
		content_hash = CASE WHEN deployment_id = '' THEN ? ELSE content_hash END,
		file_count = CASE WHEN deployment_id = '' THEN ? ELSE file_count END,
		total_bytes = CASE WHEN deployment_id = '' THEN ? ELSE total_bytes END,
		status = 'failed', last_error = ?,
		updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
		WHERE site = ?`, snap.ContentHash, snap.FileCount, snap.TotalBytes, cause.Error(), site)
	if err != nil {
		return fmt.Errorf("record cloudflare publication failure %s: %w", site, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count cloudflare publication failure update %s: %w", site, err)
	}
	if updated != 1 {
		return fmt.Errorf("record cloudflare publication failure %s: reservation not found", site)
	}
	return nil
}

func (s *CloudflarePublicationStore) RecordRecoveryFailure(ctx context.Context, site string, snap cloudflareSnapshot, cause error) error {
	result, err := s.db.ExecContext(ctx, `UPDATE site_cloudflare_publications SET
		content_hash = CASE WHEN deployment_id = '' THEN ? ELSE content_hash END,
		file_count = CASE WHEN deployment_id = '' THEN ? ELSE file_count END,
		total_bytes = CASE WHEN deployment_id = '' THEN ? ELSE total_bytes END,
		status = CASE
			WHEN status IN ('creating', 'claiming-project', 'restricting',
				'activating-domain', 'protecting-custom-domain', 'deploying-restricted', 'deleting-project') THEN status
			ELSE 'failed'
		END,
		last_error = ?, updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
		WHERE site = ?`, snap.ContentHash, snap.FileCount, snap.TotalBytes, cause.Error(), site)
	if err != nil {
		return fmt.Errorf("record Cloudflare recovery failure %s: %w", site, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count Cloudflare recovery failure update %s: %w", site, err)
	}
	if updated != 1 {
		return fmt.Errorf("record Cloudflare recovery failure %s: reservation not found", site)
	}
	return nil
}

func (s *CloudflarePublicationStore) RecordReservationFailure(ctx context.Context, site string, cause error) error {
	result, err := s.db.ExecContext(ctx, `UPDATE site_cloudflare_publications SET
		last_error = ?, updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
		WHERE site = ? AND status = 'reserving'`, cause.Error(), site)
	if err != nil {
		return fmt.Errorf("record Cloudflare reservation failure %s: %w", site, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count Cloudflare reservation failure update %s: %w", site, err)
	}
	if updated != 1 {
		return fmt.Errorf("record Cloudflare reservation failure %s: reservation not found", site)
	}
	return nil
}

func (s *CloudflarePublicationStore) Delete(ctx context.Context, site string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM site_cloudflare_publications WHERE site = ?`, site); err != nil {
		return fmt.Errorf("delete cloudflare publication %s: %w", site, err)
	}
	return nil
}

type cloudflareSiteFile struct {
	Path        string
	Data        []byte
	ContentType string
	Hash        string
	Digest      string
}

type cloudflareSnapshot struct {
	Files       []cloudflareSiteFile
	ContentHash string
	FileCount   int
	TotalBytes  int64
}

type cloudflareEligibility struct {
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons"`
}

func (s *Server) snapshotCloudflareSite(ctx context.Context, site string) (cloudflareSnapshot, error) {
	paths, err := s.sites.List(ctx, site)
	if err != nil {
		return cloudflareSnapshot{}, fmt.Errorf("list site files: %w", err)
	}
	exportedPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		if !isSiteAccessFile(path) {
			exportedPaths = append(exportedPaths, path)
		}
	}
	paths = exportedPaths
	sort.Strings(paths)
	if len(paths) > maxCloudflareFiles {
		return cloudflareSnapshot{FileCount: len(paths)}, nil
	}
	files := make([]cloudflareSiteFile, 0, len(paths))
	for _, path := range paths {
		rc, _, err := s.sites.Open(ctx, site, path)
		if err != nil {
			return cloudflareSnapshot{}, fmt.Errorf("open %s: %w", path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(rc, maxCloudflareFileSize+1))
		closeErr := rc.Close()
		if readErr != nil {
			return cloudflareSnapshot{}, fmt.Errorf("read %s: %w", path, readErr)
		}
		if closeErr != nil {
			return cloudflareSnapshot{}, fmt.Errorf("close %s: %w", path, closeErr)
		}
		sum := sha256.Sum256(data)
		files = append(files, cloudflareSiteFile{
			Path:        path,
			Data:        data,
			ContentType: contentTypeFor(path, data),
			Hash:        cloudflareAssetHash(path, data),
			Digest:      hex.EncodeToString(sum[:]),
		})
	}
	var total int64
	siteHash := sha256.New()
	for _, file := range files {
		total += int64(len(file.Data))
		siteHash.Write([]byte(file.Path))
		siteHash.Write([]byte{0})
		siteHash.Write([]byte(file.Digest))
		siteHash.Write([]byte{0})
	}
	return cloudflareSnapshot{
		Files:       files,
		ContentHash: hex.EncodeToString(siteHash.Sum(nil)),
		FileCount:   len(files),
		TotalBytes:  total,
	}, nil
}

// cloudflareAssetHash matches Wrangler's Pages asset identity: BLAKE3 over
// the base64-encoded contents followed by the final file extension, truncated
// to 16 bytes. Including the extension keeps identical bytes with different
// MIME types from collapsing onto one asset key.
func cloudflareAssetHash(filePath string, data []byte) string {
	h := blake3.New()
	encoder := base64.NewEncoder(base64.StdEncoding, h)
	_, _ = encoder.Write(data)
	_ = encoder.Close()
	_, _ = io.WriteString(h, cloudflareAssetExtension(filePath))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func cloudflareAssetExtension(filePath string) string {
	base := path.Base(filePath)
	if base == "." || base == ".." {
		return ""
	}
	ext := path.Ext(base)
	// Node path.extname—the implementation Wrangler uses—treats a basename
	// made solely of its leading dot and name (for example .env) as
	// extensionless, unlike Go path.Ext.
	if ext == base {
		return ""
	}
	return strings.TrimPrefix(ext, ".")
}

func cloudflareContentHashForDeploy(files []deployFile) string {
	type digestFile struct {
		path   string
		digest string
	}
	digests := make([]digestFile, 0, len(files))
	for _, file := range files {
		if isSiteAccessFile(file.path) {
			continue
		}
		sum := sha256.Sum256(file.data)
		digests = append(digests, digestFile{path: file.path, digest: hex.EncodeToString(sum[:])})
	}
	sort.Slice(digests, func(i, j int) bool { return digests[i].path < digests[j].path })
	h := sha256.New()
	for _, file := range digests {
		h.Write([]byte(file.path))
		h.Write([]byte{0})
		h.Write([]byte(file.digest))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func checkCloudflareEligibility(snap cloudflareSnapshot) cloudflareEligibility {
	reasons := make([]string, 0)
	if snap.FileCount > maxCloudflareFiles || len(snap.Files) > maxCloudflareFiles {
		reasons = append(reasons, fmt.Sprintf("site has more than the %d-file Cloudflare Pages Direct Upload limit", maxCloudflareFiles))
	}
	for _, file := range snap.Files {
		if isSiteAccessFile(file.Path) {
			continue
		}
		switch {
		case file.Path == "spot.js":
			reasons = append(reasons, "/spot.js depends on Spot's same-origin runtime")
		case file.Path == "_headers" || file.Path == "_redirects":
			reasons = append(reasons, file.Path+" is Cloudflare Pages configuration that Spot publishing does not apply")
		case strings.HasPrefix(file.Path, "functions/"):
			reasons = append(reasons, "functions/ is a Cloudflare Pages Functions directory")
		case file.Path == "_worker.js" || file.Path == "_worker.bundle" || file.Path == "_routes.json":
			reasons = append(reasons, file.Path+" is Cloudflare worker or routing configuration")
		}
		if len(file.Data) > maxCloudflareFileSize {
			reasons = append(reasons, fmt.Sprintf("%s is over the 25 MiB Cloudflare Pages file limit", file.Path))
		}
		text := string(file.Data)
		if strings.Contains(text, "window.spot") {
			reasons = append(reasons, file.Path+" references window.spot")
		}
		if strings.Contains(text, "spot.") {
			reasons = append(reasons, file.Path+" references Spot's browser SDK")
		}
		if strings.Contains(text, "/api/") {
			reasons = append(reasons, file.Path+" references same-origin /api/ paths")
		}
	}
	reasons = uniqueStrings(reasons)
	return cloudflareEligibility{Eligible: len(reasons) == 0, Reasons: reasons}
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

type cloudflareAPI interface {
	GetProject(ctx context.Context, accountID, projectName string) (*cloudflareProject, error)
	CreateProject(ctx context.Context, accountID, projectName string) error
	GetUploadToken(ctx context.Context, accountID, projectName string) (string, error)
	CheckMissing(ctx context.Context, uploadToken string, hashes []string) ([]string, error)
	UploadAssets(ctx context.Context, uploadToken string, files []cloudflareSiteFile) error
	UpsertHashes(ctx context.Context, uploadToken string, hashes []string) error
	CreateDeployment(ctx context.Context, accountID, projectName string, manifest map[string]string) (cloudflareDeployment, error)
	AddDomain(ctx context.Context, accountID, projectName, hostname string) error
	GetDomain(ctx context.Context, accountID, projectName, hostname string) (*cloudflareDomain, error)
	DeleteDomain(ctx context.Context, accountID, projectName, hostname string) error
	ListDNSRecords(ctx context.Context, zoneID, hostname string) ([]cloudflareDNSRecord, error)
	CreateDNSRecord(ctx context.Context, zoneID, hostname, target string) (cloudflareDNSRecord, error)
	UpdateDNSRecord(ctx context.Context, zoneID, recordID, hostname, target string, proxied bool) (cloudflareDNSRecord, error)
	DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error
	DeleteProject(ctx context.Context, accountID, projectName string) error
	CreateAccessApplication(ctx context.Context, accountID string, spec cloudflareAccessApplicationSpec) (cloudflareAccessApplication, error)
	FindAccessApplications(ctx context.Context, accountID, name string) ([]cloudflareAccessApplication, error)
	UpdateAccessApplication(ctx context.Context, accountID, appID string, spec cloudflareAccessApplicationSpec) error
	DeleteAccessApplication(ctx context.Context, accountID, appID string) error
	AccessChallenge(ctx context.Context, rawURL string) (bool, error)
}

type cloudflareProject struct {
	Name string `json:"name"`
}

type cloudflareDeployment struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type cloudflareDNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

type cloudflareDomain struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type CloudflarePublisher struct {
	cfg                     cloudflareConfig
	repo                    *CloudflarePublicationStore
	client                  cloudflareAPI
	domainActivationTimeout time.Duration
	accessActivationTimeout time.Duration
}

func (p *CloudflarePublisher) status() string {
	if p == nil {
		return cloudflareConfigDisabled
	}
	return p.cfg.Status
}

func (p *CloudflarePublisher) reserve(ctx context.Context, site string, snap cloudflareSnapshot, requested ...cloudflareAccessPolicy) (*cloudflarePublication, error) {
	if p == nil || !p.cfg.Enabled() || p.repo == nil || p.client == nil {
		return nil, errCloudflareNotConfigured
	}
	existing, err := p.repo.Get(ctx, site)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	reservation := cloudflarePublication{
		Site:                  site,
		AccountID:             p.cfg.AccountID,
		ZoneID:                p.cfg.ZoneID,
		ProjectName:           p.cfg.ProjectName(site),
		Hostname:              p.cfg.Hostname(site),
		ContentHash:           snap.ContentHash,
		FileCount:             snap.FileCount,
		TotalBytes:            snap.TotalBytes,
		Status:                "reserving",
		RequestedAccessMode:   cloudflarePolicyArg(requested).Mode,
		RequestedAccessEmails: append([]string(nil), cloudflarePolicyArg(requested).Emails...),
	}
	// Applied protection remains public until Access returns an application ID
	// that Spot owns. The requested policy is separate durable retry intent.
	reservation.AccessMode = cloudflareAccessPublic
	if err := p.repo.InsertReservation(ctx, reservation); err != nil {
		return nil, err
	}
	return &reservation, nil
}

func (p *CloudflarePublisher) publish(ctx context.Context, site string, snap cloudflareSnapshot, requested ...cloudflareAccessPolicy) (cloudflarePublication, error) {
	if p == nil || !p.cfg.Enabled() || p.repo == nil || p.client == nil {
		return cloudflarePublication{}, errCloudflareNotConfigured
	}
	publication, err := p.repo.Get(ctx, site)
	if err != nil {
		return cloudflarePublication{}, err
	}
	if publication == nil {
		return cloudflarePublication{}, errors.New("cloudflare publication was not reserved")
	}
	if publication.CleanupUnknown {
		return cloudflarePublication{}, errCloudflareCleanupUnknown
	}
	accountID, zoneID := p.publicationLocation(publication)
	publication.AccountID = accountID
	publication.ZoneID = zoneID
	projectName := publication.ProjectName
	hostname := publication.Hostname
	policy := cloudflarePolicyArg(requested)
	// Persist the requested security policy before any external side effect. It
	// is intentionally separate from AccessMode/AccessEmails, which describe the
	// protection that Spot has actually applied and owns.
	publication.RequestedAccessMode = policy.Mode
	publication.RequestedAccessEmails = append([]string(nil), policy.Emails...)
	if err := p.storePublication(*publication); err != nil {
		return cloudflarePublication{}, err
	}
	if publication.Status == "restricting" && !publication.AccessManaged && publication.AccessAppID == "" && policy.Mode == cloudflareAccessPublic {
		// A public retry must not erase the only marker for an Access POST whose
		// response was lost. Claim the exact app first so the normal public
		// transition can delete it, or leave the publication untouched until the
		// eventually consistent lookup can resolve it.
		if err := p.reconcileUncertainAccessApplication(ctx, publication); err != nil {
			return cloudflarePublication{}, err
		}
	}
	existing, lookupErr := p.client.GetProject(ctx, accountID, projectName)
	if lookupErr != nil && !errors.Is(lookupErr, errCloudflareNotFound) {
		if publication.Status == "reserving" {
			_ = p.recordReservationFailure(site, lookupErr)
		} else {
			_ = p.recordFailure(site, snap, lookupErr)
		}
		return cloudflarePublication{}, lookupErr
	}
	if existing != nil && !publication.ProjectManaged {
		if publication.Status == "claiming-project" {
			// This marker is written only after CreateProject returned success. A
			// retry may therefore finish recording ownership without guessing.
			publication.ProjectManaged = true
			if err := p.storePublication(*publication); err != nil {
				return cloudflarePublication{}, err
			}
		} else if publication.Status == "creating" {
			return cloudflarePublication{}, errCloudflareProjectUncertain
		} else {
			return cloudflarePublication{}, p.rejectUnmanagedProject(*publication)
		}
	}
	projectMissing := errors.Is(lookupErr, errCloudflareNotFound) || existing == nil
	if projectMissing {
		if !publication.ProjectManaged && (publication.Status == "creating" || publication.Status == "claiming-project") {
			return cloudflarePublication{}, errCloudflareProjectUncertain
		}
		// Clear durable ownership before every create attempt. A conflict or
		// ambiguous response can then never make retry/unpublish delete a
		// project whose ownership Spot could not prove.
		publication.AccountID = accountID
		publication.ZoneID = zoneID
		publication.ProjectManaged = false
		publication.Status = "creating"
		publication.LastError = ""
		if err := p.storePublication(*publication); err != nil {
			return cloudflarePublication{}, err
		}
		if err := p.client.CreateProject(ctx, accountID, projectName); err != nil {
			if errors.Is(err, errCloudflareConflict) {
				return cloudflarePublication{}, p.rejectUnmanagedProject(*publication)
			}
			if cloudflareRequestDefinitelyRejected(err) {
				// Cloudflare proved that no project was created, so a later publish may
				// safely retry the POST.
				_ = p.recordTerminalFailure(site, snap, err)
			} else {
				// A transport failure or lost response may have committed remotely.
				// Preserve the creating marker so only explicit project reconciliation
				// can decide whether Spot owns the project or may retry creation.
				publication.LastError = err.Error()
				if stateErr := p.storePublication(*publication); stateErr != nil {
					return cloudflarePublication{}, errors.Join(err,
						fmt.Errorf("store uncertain Cloudflare Pages project creation: %w", stateErr))
				}
			}
			return cloudflarePublication{}, err
		}
		// Record the definite successful response separately before claiming
		// cleanup ownership. If the ownership write fails, a retry can safely
		// finish it; if this marker write fails, the older "creating" state stays
		// fail-closed and requires explicit reconciliation.
		publication.Status = "claiming-project"
		if err := p.storePublication(*publication); err != nil {
			return cloudflarePublication{}, err
		}
		publication.ProjectManaged = true
		if err := p.storePublication(*publication); err != nil {
			return cloudflarePublication{}, err
		}
	}
	firstRestrictedPublish := policy.Mode == cloudflareAccessRestricted && publication.DeploymentID == ""
	resumeProtectedFirstPublish := firstRestrictedPublish &&
		(publication.Status == "protecting-custom-domain" || publication.Status == "deploying-restricted")
	if policy.Mode == cloudflareAccessRestricted {
		// Pages cannot attach a custom domain while Access already protects that
		// hostname. On first publication, protect pages.dev now and add the custom
		// destination after Pages validates it. Existing custom domains can be
		// protected immediately.
		if err := p.ensureAccessApplication(ctx, publication, policy, !firstRestrictedPublish || resumeProtectedFirstPublish); err != nil {
			if !errors.Is(err, errCloudflareAccessCreateUncertain) {
				if publication.Status == "restricting" && !publication.AccessManaged && cloudflareRequestDefinitelyRejected(err) {
					_ = p.recordTerminalFailure(site, snap, err)
				} else {
					_ = p.recordFailure(site, snap, err)
				}
			}
			return cloudflarePublication{}, err
		}
	}
	publication.Status = "publishing"
	if resumeProtectedFirstPublish {
		publication.Status = "protecting-custom-domain"
	}
	publication.LastError = ""
	if err := p.storePublication(*publication); err != nil {
		return cloudflarePublication{}, err
	}

	var deployment cloudflareDeployment
	if firstRestrictedPublish {
		if !resumeProtectedFirstPublish {
			// Pages domain validation expects a successful deployment, but deploying
			// the site's real files before the custom hostname is protected would leak
			// them. Publish a generated, content-free placeholder for validation; the
			// real deployment is created only after Access covers the custom hostname.
			placeholder := cloudflarePendingDeploymentFiles()
			if _, err := p.createDeployment(ctx, site, accountID, projectName, placeholder); err != nil {
				_ = p.recordFailure(site, snap, err)
				return cloudflarePublication{}, err
			}
		}
	} else if policy.Mode == cloudflareAccessPublic {
		deployment, err = p.createDeployment(ctx, site, accountID, projectName, snap.Files)
		if err != nil {
			_ = p.recordFailure(site, snap, err)
			return cloudflarePublication{}, err
		}
		if err := p.storeSuccessfulDeployment(publication, deployment, snap); err != nil {
			return cloudflarePublication{}, err
		}
	}
	target := projectName + ".pages.dev"
	guardRestrictedDomain := firstRestrictedPublish
	preexistingDNS, err := p.matchingDNSRecord(ctx, zoneID, hostname, target)
	if err != nil {
		_ = p.recordFailure(site, snap, err)
		return cloudflarePublication{}, err
	}
	preexistingManaged := preexistingDNS != nil && publication.DNSManaged &&
		publication.DNSRecordID != "" && publication.DNSRecordID == preexistingDNS.ID
	if preexistingDNS == nil {
		// Persist ownership intent before AddDomain: Cloudflare may create the
		// DNS record even if the request later times out, and a retry must still
		// know that no user-owned record existed before this side effect.
		publication.DNSRecordID = ""
		publication.DNSManaged = true
		if err := p.storePublication(*publication); err != nil {
			return cloudflarePublication{}, err
		}
	}
	// Reassert the Pages attachment on every publish. The custom domain can be
	// removed independently in Cloudflare while the project and DNS remain; an
	// update must reattach it instead of recording a false published state.
	if err := p.client.AddDomain(ctx, accountID, projectName, hostname); err != nil {
		if !errors.Is(err, errCloudflareConflict) {
			if guardRestrictedDomain {
				err = errors.Join(err, p.rollbackUnprotectedCustomDomain(publication, zoneID))
			}
			_ = p.recordFailure(site, snap, err)
			return cloudflarePublication{}, err
		}
		if _, getErr := p.client.GetDomain(ctx, accountID, projectName, hostname); getErr != nil {
			conflictErr := fmt.Errorf("Cloudflare Pages domain %q conflicts with a domain outside project %q", hostname, projectName)
			_ = p.recordFailure(site, snap, conflictErr)
			return cloudflarePublication{}, conflictErr
		}
	}
	dnsRecord, dnsManaged, err := p.ensureDNS(ctx, zoneID, hostname, target, preexistingDNS, preexistingManaged)
	if err != nil {
		if guardRestrictedDomain {
			err = errors.Join(err, p.rollbackUnprotectedCustomDomain(publication, zoneID))
		}
		_ = p.recordFailure(site, snap, err)
		return cloudflarePublication{}, err
	}
	dnsManaged = dnsManaged || preexistingManaged
	publication.DNSRecordID = dnsRecord.ID
	publication.DNSManaged = dnsManaged
	if err := p.storePublication(*publication); err != nil {
		if guardRestrictedDomain {
			return cloudflarePublication{}, errors.Join(err, p.rollbackUnprotectedCustomDomain(publication, zoneID))
		}
		return cloudflarePublication{}, err
	}
	publication.Status = "activating-domain"
	if err := p.storePublication(*publication); err != nil {
		return cloudflarePublication{}, err
	}
	// A Pages domain can be accepted before it is routable at the edge. Keep the
	// operation active until the provider reports it ready so the UI never labels
	// a hostname "published" while visitors still receive DNS or edge errors.
	if err := p.waitForDomainActive(ctx, accountID, projectName, hostname); err != nil {
		if guardRestrictedDomain {
			err = errors.Join(err, p.rollbackUnprotectedCustomDomain(publication, zoneID))
		}
		_ = p.recordFailure(site, snap, err)
		return cloudflarePublication{}, err
	}
	if policy.Mode == cloudflareAccessRestricted {
		publication.Status = "protecting-custom-domain"
		if err := p.storePublication(*publication); err != nil {
			return cloudflarePublication{}, err
		}
		if err := p.ensureAccessApplication(ctx, publication, policy, true); err != nil {
			if guardRestrictedDomain {
				err = errors.Join(err, p.rollbackUnprotectedCustomDomain(publication, zoneID))
			}
			_ = p.recordFailure(site, snap, err)
			return cloudflarePublication{}, err
		}
		// An Access API success can precede edge propagation. Never deploy a new
		// restricted snapshot until unauthenticated probes prove that the custom,
		// apex pages.dev, and wildcard preview hosts are challenged. On first
		// publication this keeps the placeholder live; on public-to-restricted
		// transitions it keeps the previously public deployment live until the
		// restriction is actually enforced.
		if err := p.waitForAccessProtection(ctx, publication); err != nil {
			if firstRestrictedPublish {
				err = errors.Join(err, p.rollbackUnprotectedCustomDomain(publication, zoneID))
			}
			_ = p.recordFailure(site, snap, err)
			return cloudflarePublication{}, err
		}
		publication.Status = "deploying-restricted"
		if err := p.storePublication(*publication); err != nil {
			return cloudflarePublication{}, err
		}
		deployment, err = p.createDeployment(ctx, site, accountID, projectName, snap.Files)
		if err != nil {
			// The deployment POST may have committed even when its response was
			// lost. Preserve the protected transition marker so a retry never
			// removes custom-hostname Access before reconciling the real content.
			publication.LastError = err.Error()
			if stateErr := p.storePublication(*publication); stateErr != nil {
				return cloudflarePublication{}, errors.Join(err, stateErr)
			}
			return cloudflarePublication{}, err
		}
		if err := p.storeSuccessfulDeployment(publication, deployment, snap); err != nil {
			return cloudflarePublication{}, err
		}
	} else if publication.AccessManaged || publication.AccessAppID != "" {
		// Keep an existing restriction in place until the replacement content
		// and custom hostname are ready. Public is therefore a deliberate final
		// transition, never an accidental intermediate state.
		if err := p.deleteAccessApplication(ctx, publication); err != nil {
			_ = p.recordFailure(site, snap, err)
			return cloudflarePublication{}, err
		}
	}

	next := cloudflarePublication{
		Site:           site,
		AccountID:      accountID,
		ZoneID:         zoneID,
		DNSRecordID:    dnsRecord.ID,
		DNSManaged:     dnsManaged,
		ProjectManaged: true,
		AccessMode:     policy.Mode,
		AccessEmails:   append([]string(nil), policy.Emails...),
		// A completed publication has no pending policy; the applied fields above
		// are now the durable source of truth.
		RequestedAccessMode:   "",
		RequestedAccessEmails: nil,
		AccessAppID:           publication.AccessAppID,
		AccessManaged:         publication.AccessManaged,
		ProjectName:           projectName,
		Hostname:              hostname,
		DeploymentID:          deployment.ID,
		DeploymentURL:         deployment.URL,
		ContentHash:           snap.ContentHash,
		FileCount:             snap.FileCount,
		TotalBytes:            snap.TotalBytes,
		Status:                "published",
	}
	if err := p.storePublication(next); err != nil {
		return cloudflarePublication{}, err
	}
	stateCtx, cancel := context.WithTimeout(context.Background(), cloudflareStateWriteTimeout)
	defer cancel()
	stored, err := p.repo.Get(stateCtx, site)
	if err != nil {
		return cloudflarePublication{}, err
	}
	if stored != nil {
		return *stored, nil
	}
	return next, nil
}

// storeSuccessfulDeployment records a committed remote content mutation before
// domain, DNS, or access-policy reconciliation continues. A later interruption
// can then report incomplete hostname setup without losing which content is
// already live on the provider project.
func (p *CloudflarePublisher) storeSuccessfulDeployment(pub *cloudflarePublication, deployment cloudflareDeployment, snap cloudflareSnapshot) error {
	pub.DeploymentID = deployment.ID
	pub.DeploymentURL = deployment.URL
	pub.ContentHash = snap.ContentHash
	pub.FileCount = snap.FileCount
	pub.TotalBytes = snap.TotalBytes
	return p.storePublication(*pub)
}

func (p *CloudflarePublisher) publicationLocation(pub *cloudflarePublication) (string, string) {
	accountID := pub.AccountID
	if accountID == "" {
		accountID = p.cfg.AccountID
	}
	zoneID := pub.ZoneID
	if zoneID == "" {
		zoneID = p.cfg.ZoneID
	}
	return accountID, zoneID
}

func (p *CloudflarePublisher) storePublication(pub cloudflarePublication) error {
	stateCtx, cancel := context.WithTimeout(context.Background(), cloudflareStateWriteTimeout)
	defer cancel()
	return p.repo.Upsert(stateCtx, pub)
}

func (p *CloudflarePublisher) deletePublicationState(site string) error {
	stateCtx, cancel := context.WithTimeout(context.Background(), cloudflareStateWriteTimeout)
	defer cancel()
	return p.repo.Delete(stateCtx, site)
}

func (p *CloudflarePublisher) rejectUnmanagedProject(pub cloudflarePublication) error {
	conflictErr := fmt.Errorf("Cloudflare Pages project %q already exists but is not managed by Spot", pub.ProjectName)
	pub.ProjectManaged = false
	pub.Status = "reserving"
	pub.LastError = conflictErr.Error()
	if stateErr := p.storePublication(pub); stateErr != nil {
		return errors.Join(conflictErr, fmt.Errorf("store non-owning Cloudflare reservation state: %w", stateErr))
	}
	if pub.DNSManaged || pub.DNSRecordID != "" || pub.AccessManaged || pub.AccessAppID != "" {
		// A project recreated outside Spot is not ours to delete, but ownership
		// of the old publication's DNS and Access resources remains valid. Keep
		// the row so unpublish (and site deletion guards) can finish that cleanup.
		return conflictErr
	}
	if deleteErr := p.deletePublicationState(pub.Site); deleteErr != nil {
		return errors.Join(conflictErr, deleteErr)
	}
	return conflictErr
}

// resolveLegacyCleanup removes only Spot's local fail-closed marker. The owner
// must first remove the old publication's Pages, DNS, and Access resources in
// Cloudflare because legacy rows do not retain enough location data for Spot to
// verify or clean them safely.
func (p *CloudflarePublisher) resolveLegacyCleanup(ctx context.Context, site string, confirmRemoved bool) error {
	if p == nil || p.repo == nil {
		return errCloudflareNotConfigured
	}
	pub, err := p.repo.Get(ctx, site)
	if err != nil {
		return err
	}
	if pub == nil {
		return ErrSiteNotFound
	}
	if !pub.CleanupUnknown {
		return errCloudflareCleanupNotUnknown
	}
	if !confirmRemoved {
		return errCloudflareLegacyConfirm
	}
	return p.deletePublicationState(site)
}

func (p *CloudflarePublisher) resolveUncertainProject(ctx context.Context, site, resolution string) (*cloudflarePublication, string, error) {
	if p == nil || p.repo == nil || !p.cfg.Enabled() || p.client == nil {
		return nil, "", errCloudflareNotConfigured
	}
	pub, err := p.repo.Get(ctx, site)
	if err != nil {
		return nil, "", err
	}
	if pub == nil {
		return nil, "", ErrSiteNotFound
	}
	if pub.CleanupUnknown {
		return nil, "", errCloudflareCleanupUnknown
	}
	if (pub.Status != "creating" && pub.Status != "deleting-project") || pub.ProjectManaged {
		return nil, "", errCloudflareProjectNotPending
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if resolution != "owned" && resolution != "unmanaged" && resolution != "absent" {
		return nil, "", errCloudflareProjectResolution
	}

	accountID, _ := p.publicationLocation(pub)
	project, lookupErr := p.client.GetProject(ctx, accountID, pub.ProjectName)
	present := lookupErr == nil && project != nil
	if lookupErr != nil && !errors.Is(lookupErr, errCloudflareNotFound) {
		return nil, "", lookupErr
	}
	switch resolution {
	case "owned":
		if !present {
			return nil, "", errors.New("Cloudflare Pages project is absent; choose absent after verifying the dashboard")
		}
		pub.ProjectManaged = true
		pub.Status = "failed"
		pub.LastError = "Cloudflare Pages project ownership was manually confirmed; retry publish or unpublish"
		if err := p.storePublication(*pub); err != nil {
			return nil, "", err
		}
		return pub, "owned", nil
	case "unmanaged":
		if !present {
			return nil, "", errors.New("Cloudflare Pages project is absent; choose absent after verifying the dashboard")
		}
		pub.ProjectManaged = false
		pub.Status = "reserving"
		pub.LastError = fmt.Sprintf("Cloudflare Pages project %q was manually confirmed as unmanaged", pub.ProjectName)
		if pub.DNSManaged || pub.DNSRecordID != "" || pub.AccessManaged || pub.AccessAppID != "" {
			if err := p.storePublication(*pub); err != nil {
				return nil, "", err
			}
			return pub, "unmanaged", nil
		}
		if err := p.deletePublicationState(site); err != nil {
			return nil, "", err
		}
		return nil, "unmanaged", nil
	case "absent":
		if present {
			return nil, "", errors.New("Cloudflare Pages project exists; choose owned or unmanaged after verifying the dashboard")
		}
		pub.Status = "failed"
		pub.LastError = "Uncertain Cloudflare Pages project operation was manually confirmed absent; retry publish or unpublish"
		if err := p.storePublication(*pub); err != nil {
			return nil, "", err
		}
		return pub, "absent", nil
	}
	panic("unreachable project resolution")
}

func cloudflarePendingDeploymentFiles() []cloudflareSiteFile {
	data := []byte("<!doctype html><meta charset=utf-8><title>Publication pending</title>")
	return []cloudflareSiteFile{{
		Path:        "index.html",
		Data:        data,
		ContentType: "text/html; charset=utf-8",
		Hash:        cloudflareAssetHash("index.html", data),
	}}
}

func (p *CloudflarePublisher) createDeployment(ctx context.Context, site, accountID, projectName string, files []cloudflareSiteFile) (cloudflareDeployment, error) {
	hashes := make([]string, 0, len(files))
	seenHashes := make(map[string]struct{}, len(files))
	for _, file := range files {
		if _, seen := seenHashes[file.Hash]; seen {
			continue
		}
		seenHashes[file.Hash] = struct{}{}
		hashes = append(hashes, file.Hash)
	}
	uploadToken, err := p.client.GetUploadToken(ctx, accountID, projectName)
	if err != nil {
		return cloudflareDeployment{}, err
	}
	missing, err := p.client.CheckMissing(ctx, uploadToken, hashes)
	if err != nil {
		return cloudflareDeployment{}, err
	}
	missingSet := make(map[string]struct{}, len(missing))
	for _, hash := range missing {
		missingSet[hash] = struct{}{}
	}
	for _, batch := range cloudflareAssetBatches(files, missingSet) {
		if err := p.client.UploadAssets(ctx, uploadToken, batch); err != nil {
			return cloudflareDeployment{}, err
		}
	}
	if err := p.client.UpsertHashes(ctx, uploadToken, hashes); err != nil {
		// Wrangler treats this cache update as best-effort: uploaded assets are
		// still valid, but a later deployment may upload them again.
		log.Printf("cloudflare publish %s: update asset hash cache: %v", site, err)
	}
	manifest := make(map[string]string, len(files))
	for _, file := range files {
		manifest["/"+file.Path] = file.Hash
	}
	return p.client.CreateDeployment(ctx, accountID, projectName, manifest)
}

func cloudflareAssetBatches(files []cloudflareSiteFile, missing map[string]struct{}) [][]cloudflareSiteFile {
	var batches [][]cloudflareSiteFile
	var batch []cloudflareSiteFile
	var size int
	added := make(map[string]struct{}, len(missing))
	flush := func() {
		if len(batch) == 0 {
			return
		}
		batches = append(batches, batch)
		batch = nil
		size = 0
	}
	for _, file := range files {
		if _, ok := missing[file.Hash]; !ok {
			continue
		}
		if _, ok := added[file.Hash]; ok {
			continue
		}
		added[file.Hash] = struct{}{}
		if len(batch) >= maxCloudflareAssetBatchFiles || size+len(file.Data) > maxCloudflareAssetBatchSize {
			flush()
		}
		batch = append(batch, file)
		size += len(file.Data)
	}
	flush()
	return batches
}

func (p *CloudflarePublisher) matchingDNSRecord(ctx context.Context, zoneID, hostname, target string) (*cloudflareDNSRecord, error) {
	records, err := p.client.ListDNSRecords(ctx, zoneID, hostname)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if strings.EqualFold(record.Type, "CNAME") &&
			strings.EqualFold(strings.TrimSuffix(record.Name, "."), hostname) &&
			strings.EqualFold(strings.TrimSuffix(record.Content, "."), target) {
			matched := record
			return &matched, nil
		}
	}
	if len(records) > 0 {
		return nil, fmt.Errorf("Cloudflare DNS already has a conflicting record for %s", hostname)
	}
	return nil, nil
}

func (p *CloudflarePublisher) managedDNSRecord(ctx context.Context, zoneID, hostname, target, expectedID string) (*cloudflareDNSRecord, error) {
	// Ownership intent without an exact record ID is not deletion proof. A
	// matching record may have been created by an operator after an interrupted
	// AddDomain, so fail closed and preserve it.
	if expectedID == "" {
		return nil, nil
	}
	records, err := p.client.ListDNSRecords(ctx, zoneID, hostname)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.ID == expectedID &&
			strings.EqualFold(record.Type, "CNAME") &&
			strings.EqualFold(strings.TrimSuffix(record.Name, "."), hostname) &&
			strings.EqualFold(strings.TrimSuffix(record.Content, "."), target) {
			matched := record
			return &matched, nil
		}
	}
	// A replacement record belongs to whoever changed DNS after publication.
	// Leave it untouched and continue cleaning up the Pages domain/project.
	return nil, nil
}

func (p *CloudflarePublisher) ensureDNS(ctx context.Context, zoneID, hostname, target string, preexisting *cloudflareDNSRecord, preexistingManaged bool) (cloudflareDNSRecord, bool, error) {
	if preexisting != nil {
		if preexistingManaged && !preexisting.Proxied {
			updated, err := p.client.UpdateDNSRecord(ctx, zoneID, preexisting.ID, hostname, target, true)
			if err != nil {
				return cloudflareDNSRecord{}, false, err
			}
			return updated, false, nil
		}
		return *preexisting, false, nil
	}
	// Adding a Pages custom domain may create the DNS record automatically
	// when the zone belongs to the same Cloudflare account. Since no record
	// existed before AddDomain, that record is part of this publication.
	createdByDomain, err := p.matchingDNSRecord(ctx, zoneID, hostname, target)
	if err != nil {
		return cloudflareDNSRecord{}, false, err
	}
	if createdByDomain != nil {
		if !createdByDomain.Proxied {
			updated, err := p.client.UpdateDNSRecord(ctx, zoneID, createdByDomain.ID, hostname, target, true)
			if err != nil {
				return cloudflareDNSRecord{}, false, err
			}
			return updated, true, nil
		}
		return *createdByDomain, true, nil
	}
	record, err := p.client.CreateDNSRecord(ctx, zoneID, hostname, target)
	if err != nil {
		return cloudflareDNSRecord{}, false, err
	}
	return record, true, nil
}

// rollbackUnprotectedCustomDomain removes a first restricted publication's
// custom hostname if Access could not be attached after Pages accepted it.
// The pages.dev destinations are already protected at this point. Cleanup uses
// a short background context so a disconnected browser cannot leave the custom
// hostname exposed.
func (p *CloudflarePublisher) rollbackUnprotectedCustomDomain(pub *cloudflarePublication, zoneID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var cleanupErrs []error
	if err := p.client.DeleteDomain(cleanupCtx, pub.AccountID, pub.ProjectName, pub.Hostname); err != nil && !errors.Is(err, errCloudflareNotFound) {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("remove unprotected Cloudflare Pages domain: %w", err))
	}
	if pub.DNSManaged && pub.DNSRecordID != "" {
		target := pub.ProjectName + ".pages.dev"
		record, err := p.managedDNSRecord(cleanupCtx, zoneID, pub.Hostname, target, pub.DNSRecordID)
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("find unprotected Cloudflare DNS record: %w", err))
		} else if record != nil {
			if err := p.client.DeleteDNSRecord(cleanupCtx, zoneID, record.ID); err != nil && !errors.Is(err, errCloudflareNotFound) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("remove unprotected Cloudflare DNS record: %w", err))
			}
		}
	}
	if len(cleanupErrs) == 0 {
		pub.DNSManaged = false
		pub.DNSRecordID = ""
		// Both the custom domain and its managed DNS record are proven absent.
		// Restart a retry before domain attachment so Access is established on
		// the pages.dev destinations before the public hostname is exposed.
		pub.Status = "failed"
		if err := p.storePublication(*pub); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	return errors.Join(cleanupErrs...)
}

func (p *CloudflarePublisher) recordFailure(site string, snap cloudflareSnapshot, cause error) error {
	if p.repo == nil {
		return nil
	}
	stateCtx, cancel := context.WithTimeout(context.Background(), cloudflareStateWriteTimeout)
	defer cancel()
	return p.repo.RecordRecoveryFailure(stateCtx, site, snap, cause)
}

func (p *CloudflarePublisher) recordTerminalFailure(site string, snap cloudflareSnapshot, cause error) error {
	if p.repo == nil {
		return nil
	}
	stateCtx, cancel := context.WithTimeout(context.Background(), cloudflareStateWriteTimeout)
	defer cancel()
	return p.repo.RecordFailure(stateCtx, site, snap, cause)
}

func (p *CloudflarePublisher) recordReservationFailure(site string, cause error) error {
	if p.repo == nil {
		return nil
	}
	stateCtx, cancel := context.WithTimeout(context.Background(), cloudflareStateWriteTimeout)
	defer cancel()
	return p.repo.RecordReservationFailure(stateCtx, site, cause)
}

func (p *CloudflarePublisher) unpublish(ctx context.Context, site string) error {
	if p == nil || p.repo == nil {
		return errCloudflareNotConfigured
	}
	pub, err := p.repo.Get(ctx, site)
	if err != nil {
		return err
	}
	if pub == nil {
		return ErrSiteNotFound
	}
	if pub.CleanupUnknown {
		return errCloudflareCleanupUnknown
	}
	if !pub.ProjectManaged && (pub.Status == "creating" || pub.Status == "claiming-project" || pub.Status == "deleting-project") {
		if pub.Status == "creating" || pub.Status == "deleting-project" {
			return errCloudflareProjectUncertain
		}
		if !p.cfg.Enabled() || p.client == nil {
			return errCloudflareNotConfigured
		}
		accountID, _ := p.publicationLocation(pub)
		project, projectErr := p.client.GetProject(ctx, accountID, pub.ProjectName)
		switch {
		case errors.Is(projectErr, errCloudflareNotFound) || project == nil && projectErr == nil:
			// The project is proven absent, but this row may still own DNS or
			// Access resources from the prior publication. Retain the row until
			// those independently managed resources have been cleaned below.
			pub.Status = "project-deleted"
			pub.LastError = ""
			if err := p.storePublication(*pub); err != nil {
				return err
			}
		case projectErr != nil:
			return projectErr
		default:
			pub.ProjectManaged = true
			if err := p.storePublication(*pub); err != nil {
				return err
			}
		}
	}
	if !pub.ProjectManaged && !pub.DNSManaged && !pub.AccessManaged && pub.AccessAppID == "" {
		return p.deletePublicationState(site)
	}
	if !p.cfg.Enabled() || p.client == nil {
		return errCloudflareNotConfigured
	}
	accountID, zoneID := p.publicationLocation(pub)
	if pub.Status == "restricting" && !pub.AccessManaged && pub.AccessAppID == "" {
		// A create may have committed without returning its ID. Do not delete the
		// publication record until exact lookup makes that app cleanable.
		if err := p.reconcileUncertainAccessApplication(ctx, pub); err != nil {
			return err
		}
	}
	if pub.DNSManaged {
		target := pub.ProjectName + ".pages.dev"
		record, err := p.managedDNSRecord(ctx, zoneID, pub.Hostname, target, pub.DNSRecordID)
		if err != nil {
			return err
		}
		if record != nil {
			if err := p.client.DeleteDNSRecord(ctx, zoneID, record.ID); err != nil && !errors.Is(err, errCloudflareNotFound) {
				return err
			}
		}
	}
	if pub.ProjectManaged {
		if err := p.client.DeleteDomain(ctx, accountID, pub.ProjectName, pub.Hostname); err != nil && !errors.Is(err, errCloudflareNotFound) {
			return err
		}
		// Relinquish automatic deletion ownership before the remote DELETE. If its
		// response is lost, a replacement project with the deterministic name must
		// never be deleted by a retry; the owner must reconcile it explicitly.
		pub.ProjectManaged = false
		pub.Status = "deleting-project"
		pub.LastError = ""
		if err := p.storePublication(*pub); err != nil {
			return err
		}
		deleteErr := p.client.DeleteProject(ctx, accountID, pub.ProjectName)
		if deleteErr != nil && !errors.Is(deleteErr, errCloudflareNotFound) {
			if cloudflareRequestDefinitelyRejected(deleteErr) {
				pub.ProjectManaged = true
				pub.Status = "failed"
			} else {
				pub.Status = "deleting-project"
			}
			pub.LastError = deleteErr.Error()
			if stateErr := p.storePublication(*pub); stateErr != nil {
				return errors.Join(deleteErr,
					fmt.Errorf("store uncertain Cloudflare Pages project deletion: %w", stateErr))
			}
			return deleteErr
		}
		pub.Status = "project-deleted"
		if err := p.storePublication(*pub); err != nil {
			return err
		}
	}
	if pub.AccessManaged || pub.AccessAppID != "" {
		if err := p.deleteAccessApplication(ctx, pub); err != nil {
			return err
		}
	}
	return p.deletePublicationState(site)
}

type CloudflareClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewCloudflareClient(token string) *CloudflareClient {
	return &CloudflareClient{
		baseURL: "https://api.cloudflare.com/client/v4",
		token:   token,
		client:  http.DefaultClient,
	}
}

type cloudflareResponse struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

type cloudflareAPIError struct {
	method     string
	path       string
	statusCode int
	code       int
	message    string
	rejected   bool
}

func (e *cloudflareAPIError) Error() string {
	if e.statusCode != 0 {
		return fmt.Sprintf("cloudflare API %s %s returned %d: %s", e.method, e.path, e.statusCode, e.message)
	}
	return fmt.Sprintf("cloudflare API %s %s: %s", e.method, e.path, e.message)
}

func cloudflareRequestDefinitelyRejected(err error) bool {
	if errors.Is(err, errCloudflareConflict) || errors.Is(err, errCloudflareNotFound) {
		return true
	}
	var apiErr *cloudflareAPIError
	return errors.As(err, &apiErr) && (apiErr.rejected || apiErr.statusCode >= 400 && apiErr.statusCode < 500)
}

func (c *CloudflareClient) do(ctx context.Context, method, path, token string, body []byte, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if token == "" {
		token = c.token
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusNotFound {
		return errCloudflareNotFound
	}
	if res.StatusCode == http.StatusConflict {
		return errCloudflareConflict
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var envelope cloudflareResponse
		if json.Unmarshal(raw, &envelope) == nil && len(envelope.Errors) > 0 {
			return &cloudflareAPIError{
				method: method, path: path, statusCode: res.StatusCode,
				code: envelope.Errors[0].Code, message: envelope.Errors[0].Message,
			}
		}
		return &cloudflareAPIError{
			method: method, path: path, statusCode: res.StatusCode,
			message: strings.TrimSpace(string(raw)),
		}
	}
	var envelope cloudflareResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode cloudflare API response: %w", err)
	}
	if !envelope.Success {
		if len(envelope.Errors) > 0 {
			return &cloudflareAPIError{
				method: method, path: path, code: envelope.Errors[0].Code,
				message: envelope.Errors[0].Message, rejected: true,
			}
		}
		return &cloudflareAPIError{method: method, path: path, message: "request was rejected", rejected: true}
	}
	if out == nil {
		return nil
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("decode cloudflare API result: %w", err)
	}
	return nil
}

func (c *CloudflareClient) httpClient() *http.Client {
	if c.client != nil {
		return c.client
	}
	return http.DefaultClient
}

func (c *CloudflareClient) GetProject(ctx context.Context, accountID, projectName string) (*cloudflareProject, error) {
	var project cloudflareProject
	if err := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(accountID)+"/pages/projects/"+url.PathEscape(projectName), "", nil, "", &project); err != nil {
		return nil, err
	}
	return &project, nil
}

func (c *CloudflareClient) CreateProject(ctx context.Context, accountID, projectName string) error {
	body, _ := json.Marshal(map[string]string{
		"name":              projectName,
		"production_branch": "main",
	})
	return c.do(ctx, http.MethodPost, "/accounts/"+url.PathEscape(accountID)+"/pages/projects", "", body, "application/json", nil)
}

func (c *CloudflareClient) GetUploadToken(ctx context.Context, accountID, projectName string) (string, error) {
	var out struct {
		JWT string `json:"jwt"`
	}
	err := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(accountID)+"/pages/projects/"+url.PathEscape(projectName)+"/upload-token", "", nil, "", &out)
	return out.JWT, err
}

func (c *CloudflareClient) CheckMissing(ctx context.Context, uploadToken string, hashes []string) ([]string, error) {
	body, _ := json.Marshal(map[string][]string{"hashes": hashes})
	var out []string
	err := c.do(ctx, http.MethodPost, "/pages/assets/check-missing", uploadToken, body, "application/json", &out)
	return out, err
}

func (c *CloudflareClient) UploadAssets(ctx context.Context, uploadToken string, files []cloudflareSiteFile) error {
	payload := make([]map[string]any, 0, len(files))
	for _, file := range files {
		payload = append(payload, map[string]any{
			"key":    file.Hash,
			"value":  base64.StdEncoding.EncodeToString(file.Data),
			"base64": true,
			"metadata": map[string]string{
				"contentType": file.ContentType,
			},
		})
	}
	body, _ := json.Marshal(payload)
	return c.do(ctx, http.MethodPost, "/pages/assets/upload", uploadToken, body, "application/json", nil)
}

func (c *CloudflareClient) UpsertHashes(ctx context.Context, uploadToken string, hashes []string) error {
	body, _ := json.Marshal(map[string][]string{"hashes": hashes})
	return c.do(ctx, http.MethodPost, "/pages/assets/upsert-hashes", uploadToken, body, "application/json", nil)
}

func (c *CloudflareClient) CreateDeployment(ctx context.Context, accountID, projectName string, manifest map[string]string) (cloudflareDeployment, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	manifestRaw, _ := json.Marshal(manifest)
	if err := mw.WriteField("manifest", string(manifestRaw)); err != nil {
		return cloudflareDeployment{}, err
	}
	if err := mw.WriteField("branch", "main"); err != nil {
		return cloudflareDeployment{}, err
	}
	if err := mw.Close(); err != nil {
		return cloudflareDeployment{}, err
	}
	var out cloudflareDeployment
	err := c.do(ctx, http.MethodPost, "/accounts/"+url.PathEscape(accountID)+"/pages/projects/"+url.PathEscape(projectName)+"/deployments", "", buf.Bytes(), mw.FormDataContentType(), &out)
	return out, err
}

func (c *CloudflareClient) AddDomain(ctx context.Context, accountID, projectName, hostname string) error {
	body, _ := json.Marshal(map[string]string{"name": hostname})
	err := c.do(ctx, http.MethodPost, "/accounts/"+url.PathEscape(accountID)+"/pages/projects/"+url.PathEscape(projectName)+"/domains", "", body, "application/json", nil)
	var apiErr *cloudflareAPIError
	if errors.As(err, &apiErr) && apiErr.code == 8000018 {
		return errCloudflareConflict
	}
	return err
}

func (c *CloudflareClient) GetDomain(ctx context.Context, accountID, projectName, hostname string) (*cloudflareDomain, error) {
	var domain cloudflareDomain
	err := c.do(ctx, http.MethodGet, "/accounts/"+url.PathEscape(accountID)+"/pages/projects/"+url.PathEscape(projectName)+"/domains/"+url.PathEscape(hostname), "", nil, "", &domain)
	if err != nil {
		return nil, err
	}
	return &domain, nil
}

func (c *CloudflareClient) DeleteDomain(ctx context.Context, accountID, projectName, hostname string) error {
	return c.do(ctx, http.MethodDelete, "/accounts/"+url.PathEscape(accountID)+"/pages/projects/"+url.PathEscape(projectName)+"/domains/"+url.PathEscape(hostname), "", nil, "", nil)
}

func (c *CloudflareClient) ListDNSRecords(ctx context.Context, zoneID, hostname string) ([]cloudflareDNSRecord, error) {
	q := url.Values{}
	q.Set("name", hostname)
	var out []cloudflareDNSRecord
	err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/dns_records?"+q.Encode(), "", nil, "", &out)
	return out, err
}

func (c *CloudflareClient) CreateDNSRecord(ctx context.Context, zoneID, hostname, target string) (cloudflareDNSRecord, error) {
	body, _ := json.Marshal(map[string]any{
		"type":    "CNAME",
		"name":    hostname,
		"content": target,
		"proxied": true,
	})
	var out cloudflareDNSRecord
	err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", "", body, "application/json", &out)
	return out, err
}

func (c *CloudflareClient) UpdateDNSRecord(ctx context.Context, zoneID, recordID, hostname, target string, proxied bool) (cloudflareDNSRecord, error) {
	body, _ := json.Marshal(map[string]any{
		"type":    "CNAME",
		"name":    hostname,
		"content": target,
		"proxied": proxied,
	})
	var out cloudflareDNSRecord
	err := c.do(ctx, http.MethodPatch, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), "", body, "application/json", &out)
	return out, err
}

func (c *CloudflareClient) DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error {
	return c.do(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), "", nil, "", nil)
}

func (c *CloudflareClient) DeleteProject(ctx context.Context, accountID, projectName string) error {
	return c.do(ctx, http.MethodDelete, "/accounts/"+url.PathEscape(accountID)+"/pages/projects/"+url.PathEscape(projectName), "", nil, "", nil)
}

func logCloudflareConfig(cfg cloudflareConfig) {
	switch cfg.Status {
	case cloudflareConfigEnabled:
		log.Printf("cloudflare: Pages publishing enabled for %s with project prefix %s", cfg.BaseDomain, cfg.ProjectPrefix)
	case cloudflareConfigPartial:
		log.Printf("cloudflare: Pages publishing disabled; missing %s", strings.Join(cfg.Missing, ", "))
	default:
		log.Printf("cloudflare: Pages publishing disabled")
	}
}
