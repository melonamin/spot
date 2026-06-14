package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrDeployForbidden = errors.New("deploy forbidden")
	ErrSiteNotFound    = errors.New("site not found")
)

type DeployAuthorizer interface {
	AuthorizeDeploy(ctx context.Context, site string, actor Identity) (DeployAuthorization, error)
	RecordDeploy(ctx context.Context, event DeployAuditEvent) error
}

type DeployAuthorization struct {
	Action string
}

type DeployAuditEvent struct {
	Site       string
	Actor      Identity
	Action     string
	Status     string
	Message    string
	FileCount  int
	TotalBytes int64
}

type SiteRegistry struct {
	db     *sql.DB
	admins *AccessPolicy
}

type SiteRecord struct {
	Name        string
	OwnerEmail  string
	OwnerPeerIP string
	OwnerName   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func NewSiteRegistry(db *sql.DB, admins *AccessPolicy) *SiteRegistry {
	return &SiteRegistry{db: db, admins: admins}
}

// AuthorizeDeploy claims a new site for its first deployer, then only
// allows that owner or configured platform admins to replace it.
func (r *SiteRegistry) AuthorizeDeploy(ctx context.Context, site string, actor Identity) (DeployAuthorization, error) {
	if actorKey(actor) == "" {
		return DeployAuthorization{}, ErrDeployForbidden
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return DeployAuthorization{}, fmt.Errorf("begin deploy auth: %w", err)
	}
	defer tx.Rollback()

	record, err := r.readSiteForUpdate(ctx, tx, site)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx,
			r.insertSiteSQL(),
			site, strings.ToLower(actor.Email), actor.PeerIP, actor.Name,
		); err != nil {
			return DeployAuthorization{}, fmt.Errorf("claim site %s: %w", site, err)
		}
		if err := tx.Commit(); err != nil {
			return DeployAuthorization{}, fmt.Errorf("commit site claim: %w", err)
		}
		return DeployAuthorization{Action: "create"}, nil
	}
	if err != nil {
		return DeployAuthorization{}, err
	}

	if !record.OwnedBy(actor) && !allowsAdmin(r.admins, actor) {
		return DeployAuthorization{}, ErrDeployForbidden
	}
	if _, err := tx.ExecContext(ctx,
		r.touchSiteSQL(), site,
	); err != nil {
		return DeployAuthorization{}, fmt.Errorf("touch site %s: %w", site, err)
	}
	if err := tx.Commit(); err != nil {
		return DeployAuthorization{}, fmt.Errorf("commit deploy auth: %w", err)
	}
	return DeployAuthorization{Action: "update"}, nil
}

func (r *SiteRegistry) readSiteForUpdate(ctx context.Context, tx *sql.Tx, site string) (SiteRecord, error) {
	var record SiteRecord
	err := tx.QueryRowContext(ctx, r.readSiteSQL(), site).
		Scan(&record.Name, &record.OwnerEmail, &record.OwnerPeerIP, &record.OwnerName, &record.CreatedAt, &record.UpdatedAt)
	return record, err
}

func (r *SiteRegistry) RecordDeploy(ctx context.Context, event DeployAuditEvent) error {
	rawGroups, err := json.Marshal(event.Actor.Groups)
	if err != nil {
		return fmt.Errorf("encode deploy audit groups: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		r.insertDeployAuditSQL(),
		event.Site,
		strings.ToLower(event.Actor.Email),
		event.Actor.PeerIP,
		event.Actor.Name,
		string(rawGroups),
		event.Action,
		event.Status,
		event.FileCount,
		event.TotalBytes,
		event.Message,
	)
	if err != nil {
		return fmt.Errorf("record deploy audit for %s: %w", event.Site, err)
	}
	return nil
}

// OwnedSite is a site row joined with the size of its last successful
// deploy, for the platform's "my spots" page.
type OwnedSite struct {
	SiteRecord
	FileCount  int
	TotalBytes int64
}

// SitesOwnedBy returns the sites the actor owns, most recently updated
// first. Ownership mirrors SiteRecord.OwnedBy: the owner email when the
// site has one, the claiming peer IP otherwise.
func (r *SiteRegistry) SitesOwnedBy(ctx context.Context, actor Identity) ([]OwnedSite, error) {
	rows, err := r.db.QueryContext(ctx,
		r.sitesOwnedBySQL(),
		strings.ToLower(actor.Email), actor.PeerIP)
	if err != nil {
		return nil, fmt.Errorf("list owned sites: %w", err)
	}
	defer rows.Close()

	var owned []OwnedSite
	for rows.Next() {
		var site OwnedSite
		if err := rows.Scan(&site.Name, &site.OwnerEmail, &site.OwnerPeerIP, &site.OwnerName,
			&site.CreatedAt, &site.UpdatedAt, &site.FileCount, &site.TotalBytes); err != nil {
			return nil, fmt.Errorf("scan owned site: %w", err)
		}
		owned = append(owned, site)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list owned sites: %w", err)
	}
	return owned, nil
}

// AllSites returns every registered site, most recently updated first.
// Callers filter out restricted sites before showing the list.
func (r *SiteRegistry) AllSites(ctx context.Context) ([]SiteRecord, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT name, owner_email, owner_peer_ip, owner_name, created_at, updated_at
		 FROM sites
		 ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	defer rows.Close()

	var sites []SiteRecord
	for rows.Next() {
		var site SiteRecord
		if err := rows.Scan(&site.Name, &site.OwnerEmail, &site.OwnerPeerIP, &site.OwnerName,
			&site.CreatedAt, &site.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan site: %w", err)
		}
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	return sites, nil
}

func (r *SiteRegistry) CanManageSite(ctx context.Context, site string, actor Identity) (bool, error) {
	var record SiteRecord
	err := r.db.QueryRowContext(ctx, r.readSitePlainSQL(), site).
		Scan(&record.Name, &record.OwnerEmail, &record.OwnerPeerIP, &record.OwnerName, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrSiteNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read site %s: %w", site, err)
	}
	return record.OwnedBy(actor) || allowsAdmin(r.admins, actor), nil
}

// DeleteSite removes a site's registry row after purge succeeds. A
// failed purge leaves the site claimed so its owner can retry.
func (r *SiteRegistry) DeleteSite(ctx context.Context, site string, actor Identity, purge func(context.Context) error) error {
	var record SiteRecord
	err := r.db.QueryRowContext(ctx, r.readSitePlainSQL(), site).
		Scan(&record.Name, &record.OwnerEmail, &record.OwnerPeerIP, &record.OwnerName, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSiteNotFound
	}
	if err != nil {
		return fmt.Errorf("read site %s: %w", site, err)
	}
	if !record.OwnedBy(actor) && !allowsAdmin(r.admins, actor) {
		return ErrDeployForbidden
	}
	if purge != nil {
		if err := purge(ctx); err != nil {
			return fmt.Errorf("purge site %s: %w", site, err)
		}
	}
	if _, err := r.db.ExecContext(ctx, r.deleteSiteSQL(), site); err != nil {
		return fmt.Errorf("delete site %s: %w", site, err)
	}
	return nil
}

func (r *SiteRegistry) insertSiteSQL() string {
	return `INSERT INTO sites (name, owner_email, owner_peer_ip, owner_name)
		VALUES (?, ?, ?, ?)`
}

func (r *SiteRegistry) touchSiteSQL() string {
	return `UPDATE sites SET updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now') WHERE name = ?`
}

func (r *SiteRegistry) readSiteSQL() string {
	return r.readSitePlainSQL()
}

func (r *SiteRegistry) readSitePlainSQL() string {
	return `SELECT name, owner_email, owner_peer_ip, owner_name, created_at, updated_at
		FROM sites
		WHERE name = ?`
}

func (r *SiteRegistry) insertDeployAuditSQL() string {
	return `INSERT INTO site_deploy_audit
		(site, actor_email, actor_peer_ip, actor_name, actor_groups,
		 action, status, file_count, total_bytes, message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
}

func (r *SiteRegistry) sitesOwnedBySQL() string {
	return `SELECT s.name, s.owner_email, s.owner_peer_ip, s.owner_name,
			s.created_at, s.updated_at,
			COALESCE((SELECT file_count FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), 0),
			COALESCE((SELECT total_bytes FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), 0)
		FROM sites s
		WHERE (s.owner_email <> '' AND s.owner_email = ?)
		   OR (s.owner_email = '' AND s.owner_peer_ip <> '' AND s.owner_peer_ip = ?)
		ORDER BY s.updated_at DESC`
}

func (r *SiteRegistry) deleteSiteSQL() string {
	return `DELETE FROM sites WHERE name = ?`
}

func (r SiteRecord) OwnedBy(actor Identity) bool {
	if r.OwnerEmail != "" {
		return actor.Email != "" && strings.EqualFold(r.OwnerEmail, actor.Email)
	}
	return r.OwnerPeerIP != "" && actor.PeerIP != "" && r.OwnerPeerIP == actor.PeerIP
}

func actorKey(actor Identity) string {
	if actor.Email != "" {
		return strings.ToLower(actor.Email)
	}
	return actor.PeerIP
}

func allowsAdmin(policy *AccessPolicy, actor Identity) bool {
	return policy != nil && policy.Allows(actor)
}
