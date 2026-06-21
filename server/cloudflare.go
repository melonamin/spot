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
	"sort"
	"strings"
	"time"
)

const (
	cloudflareConfigDisabled = "disabled"
	cloudflareConfigPartial  = "partial"
	cloudflareConfigEnabled  = "enabled"

	defaultCloudflareProjectPrefix = "spot-"
	maxCloudflareFileSize          = 25 << 20
	maxCloudflareAssetBatchSize    = 50 << 20
	maxCloudflareAssetBatchFiles   = 1000
)

var (
	errCloudflareNotFound          = errors.New("cloudflare resource not found")
	errCloudflareConflict          = errors.New("cloudflare resource conflict")
	errCloudflarePublicationExists = errors.New("cloudflare publication exists")
)

type cloudflareConfig struct {
	APIToken      string
	AccountID     string
	ZoneID        string
	BaseDomain    string
	ProjectPrefix string
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

func (c cloudflareConfig) Hostname(site string) string {
	if c.BaseDomain == "" {
		return ""
	}
	return site + "." + c.BaseDomain
}

func (c cloudflareConfig) ProjectName(site string) string {
	return c.ProjectPrefix + site
}

type cloudflarePublication struct {
	Site          string    `json:"site"`
	ProjectName   string    `json:"project_name"`
	Hostname      string    `json:"hostname"`
	DeploymentID  string    `json:"deployment_id"`
	DeploymentURL string    `json:"deployment_url"`
	ContentHash   string    `json:"content_hash"`
	FileCount     int       `json:"file_count"`
	TotalBytes    int64     `json:"total_bytes"`
	Status        string    `json:"status"`
	LastError     string    `json:"last_error"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CloudflarePublicationStore struct {
	db *sql.DB
}

func NewCloudflarePublicationStore(db *sql.DB) *CloudflarePublicationStore {
	return &CloudflarePublicationStore{db: db}
}

func (s *CloudflarePublicationStore) Get(ctx context.Context, site string) (*cloudflarePublication, error) {
	var p cloudflarePublication
	err := s.db.QueryRowContext(ctx, `SELECT site, project_name, hostname, deployment_id,
		deployment_url, content_hash, file_count, total_bytes, status, last_error,
		created_at, updated_at
		FROM site_cloudflare_publications WHERE site = ?`, site).
		Scan(&p.Site, &p.ProjectName, &p.Hostname, &p.DeploymentID, &p.DeploymentURL,
			&p.ContentHash, &p.FileCount, &p.TotalBytes, &p.Status, &p.LastError,
			&p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cloudflare publication %s: %w", site, err)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO site_cloudflare_publications
		(site, project_name, hostname, deployment_id, deployment_url, content_hash,
		 file_count, total_bytes, status, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(site) DO UPDATE SET
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
		p.Site, p.ProjectName, p.Hostname, p.DeploymentID, p.DeploymentURL,
		p.ContentHash, p.FileCount, p.TotalBytes, p.Status, p.LastError)
	if err != nil {
		return fmt.Errorf("upsert cloudflare publication %s: %w", p.Site, err)
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
	sort.Strings(paths)
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
			Hash:        hex.EncodeToString(sum[:]),
		})
	}
	var total int64
	siteHash := sha256.New()
	for _, file := range files {
		total += int64(len(file.Data))
		siteHash.Write([]byte(file.Path))
		siteHash.Write([]byte{0})
		siteHash.Write([]byte(file.Hash))
		siteHash.Write([]byte{0})
	}
	return cloudflareSnapshot{
		Files:       files,
		ContentHash: hex.EncodeToString(siteHash.Sum(nil)),
		FileCount:   len(files),
		TotalBytes:  total,
	}, nil
}

func checkCloudflareEligibility(snap cloudflareSnapshot) cloudflareEligibility {
	reasons := make([]string, 0)
	for _, file := range snap.Files {
		switch {
		case file.Path == accessFileName:
			reasons = append(reasons, accessFileName+" is not supported on Cloudflare Pages")
		case file.Path == "spot.js":
			reasons = append(reasons, "/spot.js depends on Spot's same-origin runtime")
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
	DeleteDomain(ctx context.Context, accountID, projectName, hostname string) error
	ListDNSRecords(ctx context.Context, zoneID, hostname string) ([]cloudflareDNSRecord, error)
	CreateDNSRecord(ctx context.Context, zoneID, hostname, target string) error
	DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error
	DeleteProject(ctx context.Context, accountID, projectName string) error
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
}

type CloudflarePublisher struct {
	cfg    cloudflareConfig
	repo   *CloudflarePublicationStore
	client cloudflareAPI
}

func (p *CloudflarePublisher) status() string {
	if p == nil {
		return cloudflareConfigDisabled
	}
	return p.cfg.Status
}

func (p *CloudflarePublisher) publish(ctx context.Context, site string, snap cloudflareSnapshot) (cloudflarePublication, error) {
	if p == nil || !p.cfg.Enabled() || p.repo == nil || p.client == nil {
		return cloudflarePublication{}, errors.New("cloudflare publishing is not configured")
	}
	projectName := p.cfg.ProjectName(site)
	hostname := p.cfg.Hostname(site)
	publication, err := p.repo.Get(ctx, site)
	if err != nil {
		return cloudflarePublication{}, err
	}
	if publication == nil {
		existing, err := p.client.GetProject(ctx, p.cfg.AccountID, projectName)
		if err == nil && existing != nil {
			return cloudflarePublication{}, fmt.Errorf("Cloudflare Pages project %q already exists but is not managed by Spot", projectName)
		}
		if err != nil && !errors.Is(err, errCloudflareNotFound) {
			return cloudflarePublication{}, err
		}
		if err := p.client.CreateProject(ctx, p.cfg.AccountID, projectName); err != nil {
			if errors.Is(err, errCloudflareConflict) {
				return cloudflarePublication{}, fmt.Errorf("Cloudflare Pages project %q already exists but is not managed by Spot", projectName)
			}
			return cloudflarePublication{}, err
		}
		if err := p.repo.Upsert(ctx, cloudflarePublication{
			Site:        site,
			ProjectName: projectName,
			Hostname:    hostname,
			ContentHash: snap.ContentHash,
			FileCount:   snap.FileCount,
			TotalBytes:  snap.TotalBytes,
			Status:      "creating",
		}); err != nil {
			return cloudflarePublication{}, err
		}
	}

	hashes := make([]string, 0, len(snap.Files))
	for _, file := range snap.Files {
		hashes = append(hashes, file.Hash)
	}
	uploadToken, err := p.client.GetUploadToken(ctx, p.cfg.AccountID, projectName)
	if err != nil {
		_ = p.recordFailure(ctx, site, projectName, hostname, snap, err)
		return cloudflarePublication{}, err
	}
	missing, err := p.client.CheckMissing(ctx, uploadToken, hashes)
	if err != nil {
		_ = p.recordFailure(ctx, site, projectName, hostname, snap, err)
		return cloudflarePublication{}, err
	}
	missingSet := make(map[string]struct{}, len(missing))
	for _, hash := range missing {
		missingSet[hash] = struct{}{}
	}
	for _, batch := range cloudflareAssetBatches(snap.Files, missingSet) {
		if err := p.client.UploadAssets(ctx, uploadToken, batch); err != nil {
			_ = p.recordFailure(ctx, site, projectName, hostname, snap, err)
			return cloudflarePublication{}, err
		}
	}
	if err := p.client.UpsertHashes(ctx, uploadToken, hashes); err != nil {
		_ = p.recordFailure(ctx, site, projectName, hostname, snap, err)
		return cloudflarePublication{}, err
	}
	manifest := make(map[string]string, len(snap.Files))
	for _, file := range snap.Files {
		manifest["/"+file.Path] = file.Hash
	}
	deployment, err := p.client.CreateDeployment(ctx, p.cfg.AccountID, projectName, manifest)
	if err != nil {
		_ = p.recordFailure(ctx, site, projectName, hostname, snap, err)
		return cloudflarePublication{}, err
	}
	if err := p.client.AddDomain(ctx, p.cfg.AccountID, projectName, hostname); err != nil && !errors.Is(err, errCloudflareConflict) {
		_ = p.recordFailure(ctx, site, projectName, hostname, snap, err)
		return cloudflarePublication{}, err
	}
	target := projectName + ".pages.dev"
	if err := p.ensureDNS(ctx, hostname, target); err != nil {
		_ = p.recordFailure(ctx, site, projectName, hostname, snap, err)
		return cloudflarePublication{}, err
	}

	next := cloudflarePublication{
		Site:          site,
		ProjectName:   projectName,
		Hostname:      hostname,
		DeploymentID:  deployment.ID,
		DeploymentURL: deployment.URL,
		ContentHash:   snap.ContentHash,
		FileCount:     snap.FileCount,
		TotalBytes:    snap.TotalBytes,
		Status:        "published",
	}
	if err := p.repo.Upsert(ctx, next); err != nil {
		return cloudflarePublication{}, err
	}
	stored, err := p.repo.Get(ctx, site)
	if err != nil {
		return cloudflarePublication{}, err
	}
	if stored != nil {
		return *stored, nil
	}
	return next, nil
}

func cloudflareAssetBatches(files []cloudflareSiteFile, missing map[string]struct{}) [][]cloudflareSiteFile {
	var batches [][]cloudflareSiteFile
	var batch []cloudflareSiteFile
	var size int
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
		if len(batch) >= maxCloudflareAssetBatchFiles || size+len(file.Data) > maxCloudflareAssetBatchSize {
			flush()
		}
		batch = append(batch, file)
		size += len(file.Data)
	}
	flush()
	return batches
}

func (p *CloudflarePublisher) ensureDNS(ctx context.Context, hostname, target string) error {
	records, err := p.client.ListDNSRecords(ctx, p.cfg.ZoneID, hostname)
	if err != nil {
		return err
	}
	for _, record := range records {
		if strings.EqualFold(record.Type, "CNAME") &&
			strings.EqualFold(strings.TrimSuffix(record.Name, "."), hostname) &&
			strings.EqualFold(strings.TrimSuffix(record.Content, "."), target) {
			return nil
		}
	}
	if len(records) > 0 {
		return fmt.Errorf("Cloudflare DNS already has a conflicting record for %s", hostname)
	}
	return p.client.CreateDNSRecord(ctx, p.cfg.ZoneID, hostname, target)
}

func (p *CloudflarePublisher) recordFailure(ctx context.Context, site, projectName, hostname string, snap cloudflareSnapshot, cause error) error {
	if p.repo == nil {
		return nil
	}
	return p.repo.Upsert(ctx, cloudflarePublication{
		Site:        site,
		ProjectName: projectName,
		Hostname:    hostname,
		ContentHash: snap.ContentHash,
		FileCount:   snap.FileCount,
		TotalBytes:  snap.TotalBytes,
		Status:      "failed",
		LastError:   cause.Error(),
	})
}

func (p *CloudflarePublisher) unpublish(ctx context.Context, site string) error {
	if p == nil || !p.cfg.Enabled() || p.repo == nil || p.client == nil {
		return errors.New("cloudflare publishing is not configured")
	}
	pub, err := p.repo.Get(ctx, site)
	if err != nil {
		return err
	}
	if pub == nil {
		return ErrSiteNotFound
	}
	target := pub.ProjectName + ".pages.dev"
	records, err := p.client.ListDNSRecords(ctx, p.cfg.ZoneID, pub.Hostname)
	if err != nil {
		return err
	}
	for _, record := range records {
		if strings.EqualFold(record.Type, "CNAME") &&
			strings.EqualFold(strings.TrimSuffix(record.Name, "."), pub.Hostname) &&
			strings.EqualFold(strings.TrimSuffix(record.Content, "."), target) {
			if err := p.client.DeleteDNSRecord(ctx, p.cfg.ZoneID, record.ID); err != nil && !errors.Is(err, errCloudflareNotFound) {
				return err
			}
		}
	}
	if err := p.client.DeleteDomain(ctx, p.cfg.AccountID, pub.ProjectName, pub.Hostname); err != nil && !errors.Is(err, errCloudflareNotFound) {
		return err
	}
	if err := p.client.DeleteProject(ctx, p.cfg.AccountID, pub.ProjectName); err != nil && !errors.Is(err, errCloudflareNotFound) {
		return err
	}
	return p.repo.Delete(ctx, site)
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
		return fmt.Errorf("cloudflare API %s %s returned %d: %s", method, path, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var envelope cloudflareResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode cloudflare API response: %w", err)
	}
	if !envelope.Success {
		if len(envelope.Errors) > 0 {
			return fmt.Errorf("cloudflare API %s %s: %s", method, path, envelope.Errors[0].Message)
		}
		return fmt.Errorf("cloudflare API %s %s failed", method, path)
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
	return c.do(ctx, http.MethodPost, "/accounts/"+url.PathEscape(accountID)+"/pages/projects/"+url.PathEscape(projectName)+"/domains", "", body, "application/json", nil)
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

func (c *CloudflareClient) CreateDNSRecord(ctx context.Context, zoneID, hostname, target string) error {
	body, _ := json.Marshal(map[string]any{
		"type":    "CNAME",
		"name":    hostname,
		"content": target,
	})
	return c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", "", body, "application/json", nil)
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
