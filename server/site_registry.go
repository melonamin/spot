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

var ErrDeployForbidden = errors.New("deploy forbidden")

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

	record, err := readSiteForUpdate(ctx, tx, site)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sites (name, owner_email, owner_peer_ip, owner_name)
			 VALUES ($1, $2, $3, $4)`,
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
		`UPDATE sites SET updated_at = now() WHERE name = $1`, site,
	); err != nil {
		return DeployAuthorization{}, fmt.Errorf("touch site %s: %w", site, err)
	}
	if err := tx.Commit(); err != nil {
		return DeployAuthorization{}, fmt.Errorf("commit deploy auth: %w", err)
	}
	return DeployAuthorization{Action: "update"}, nil
}

func readSiteForUpdate(ctx context.Context, tx *sql.Tx, site string) (SiteRecord, error) {
	var record SiteRecord
	err := tx.QueryRowContext(ctx,
		`SELECT name, owner_email, owner_peer_ip, owner_name, created_at, updated_at
		 FROM sites
		 WHERE name = $1
		 FOR UPDATE`,
		site,
	).Scan(&record.Name, &record.OwnerEmail, &record.OwnerPeerIP, &record.OwnerName, &record.CreatedAt, &record.UpdatedAt)
	return record, err
}

func (r *SiteRegistry) RecordDeploy(ctx context.Context, event DeployAuditEvent) error {
	rawGroups, err := json.Marshal(event.Actor.Groups)
	if err != nil {
		return fmt.Errorf("encode deploy audit groups: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO site_deploy_audit
		   (site, actor_email, actor_peer_ip, actor_name, actor_groups,
		    action, status, file_count, total_bytes, message)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10)`,
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
