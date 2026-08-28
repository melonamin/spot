package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

var (
	ErrDeployForbidden                  = errors.New("deploy forbidden")
	ErrSiteNotFound                     = errors.New("site not found")
	ErrSiteNotActive                    = errors.New("site is not active")
	ErrExternalContentMutationActive    = errors.New("external content mutation already active")
	ErrExternalContentMutationLeaseLost = errors.New("external content mutation lease lost")
)

type SiteState string

const (
	SiteStateProvisioning SiteState = "provisioning"
	SiteStateActive       SiteState = "active"
	SiteStateDeleted      SiteState = "deleted"
)

type ManagementRole string

const (
	ManagementRoleOwner      ManagementRole = "owner"
	ManagementRoleAdmin      ManagementRole = "admin"
	ManagementRoleMaintainer ManagementRole = "maintainer"
)

type ManagementDecision struct {
	Allowed bool
	Role    ManagementRole
	State   SiteState
}

type PolicyTransition struct {
	Generation   int64
	PreviousHash string
	NextHash     string
}

type DeployAuthorizer interface {
	AuthorizeDeploy(ctx context.Context, site string, actor Identity) (DeployAuthorization, error)
	RecordDeploy(ctx context.Context, event DeployAuditEvent) error
}

type DeployAuthorization struct {
	Action            string
	AuthorizedAs      ManagementRole
	PreviousState     SiteState
	ContentGeneration int64
	ContentWasDirty   bool
}

type DeployAuditEvent struct {
	Site              string
	Actor             Identity
	Action            string
	Status            string
	Message           string
	FileCount         int
	TotalBytes        int64
	ContentHash       string
	AuthorizedAs      ManagementRole
	ContentGeneration int64
	AuthMethod        string
	PublisherKeyID    string
	PublisherName     string
}

type SiteRegistry struct {
	db             *sql.DB
	admins         *AccessPolicy
	policyResolver func(context.Context, string) (*AccessPolicy, error)
}

type siteMetadataUpdater interface {
	UpdateSiteMetadata(ctx context.Context, site string, meta SiteMetadata) error
}

type deploySiteMetadataUpdater interface {
	UpdateDeploySiteMetadata(ctx context.Context, site string, meta SiteMetadata) error
}

type siteMetadataReader interface {
	SiteMetadata(ctx context.Context, site string) (SiteMetadata, error)
}

type deployAuthorizationCanceler interface {
	CancelDeployAuthorization(ctx context.Context, site string, authz DeployAuthorization) error
}

type deployCompleter interface {
	CompleteDeploy(ctx context.Context, site string, authz DeployAuthorization) error
}

type SiteRecord struct {
	Name                       string
	OwnerEmail                 string
	OwnerPeerIP                string
	OwnerName                  string
	Title                      string
	Description                string
	Tags                       []string
	State                      SiteState
	PolicyTransitionGeneration int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

func NewSiteRegistry(db *sql.DB, admins *AccessPolicy) *SiteRegistry {
	return &SiteRegistry{db: db, admins: admins}
}

func (r *SiteRegistry) SetPolicyResolver(resolver func(context.Context, string) (*AccessPolicy, error)) {
	r.policyResolver = resolver
}

// AuthorizeDeploy atomically claims a missing site and reserves a content
// generation for an authorized lifecycle transition. Stored maintainers are
// consulted only for active sites; provisional claims and tombstones remain
// recoverable solely by their immutable owner or a platform admin.
func (r *SiteRegistry) AuthorizeDeploy(ctx context.Context, site string, actor Identity) (DeployAuthorization, error) {
	return r.authorizeDeploy(ctx, site, actor, "")
}

func (r *SiteRegistry) AuthorizePublishingKeyDeploy(ctx context.Context, site string, actor Identity, keyID string) (DeployAuthorization, error) {
	if keyID == "" {
		return DeployAuthorization{}, errPublishingKeyInvalid
	}
	return r.authorizeDeploy(ctx, site, actor, keyID)
}

func (r *SiteRegistry) authorizeDeploy(ctx context.Context, site string, actor Identity, publishingKeyID string) (DeployAuthorization, error) {
	if actorKey(actor) == "" {
		return DeployAuthorization{}, ErrDeployForbidden
	}

	// Policy resolution may require remote object storage. Keep it outside the
	// SQLite transaction, then verify the authorization snapshot after opening
	// the transaction before reserving a generation.
	for attempt := 0; attempt < 4; attempt++ {
		snapshot, snapshotErr := r.readDeployAuthorizationRecord(ctx, r.db, site)
		var role ManagementRole
		if snapshotErr == nil {
			if publishingKeyID != "" {
				if snapshot.OwnedBy(actor) {
					role = ManagementRoleOwner
				}
			} else {
				var err error
				role, err = r.managementRole(ctx, snapshot.SiteRecord, actor)
				if err != nil {
					return DeployAuthorization{}, err
				}
			}
		} else if !errors.Is(snapshotErr, sql.ErrNoRows) {
			return DeployAuthorization{}, snapshotErr
		}

		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return DeployAuthorization{}, fmt.Errorf("begin deploy auth: %w", err)
		}
		if publishingKeyID != "" {
			var prefix, ownerEmail, ownerPeerIP string
			err := tx.QueryRowContext(ctx, `SELECT site_prefix, owner_email, owner_peer_ip
				FROM publishing_keys
				WHERE id = ? AND revoked_at IS NULL AND secret_hash IS NOT NULL`, publishingKeyID).Scan(
				&prefix, &ownerEmail, &ownerPeerIP)
			if errors.Is(err, sql.ErrNoRows) {
				tx.Rollback()
				return DeployAuthorization{}, errPublishingKeyInvalid
			}
			if err != nil {
				tx.Rollback()
				return DeployAuthorization{}, fmt.Errorf("revalidate publishing key: %w", err)
			}
			ownerMatches := ownerEmail != "" && actor.Email != "" && strings.EqualFold(ownerEmail, actor.Email)
			if ownerEmail == "" {
				ownerMatches = ownerPeerIP != "" && actor.PeerIP == ownerPeerIP
			}
			if !ownerMatches || !strings.HasPrefix(site, prefix) || len(site) == len(prefix) {
				tx.Rollback()
				return DeployAuthorization{}, ErrDeployForbidden
			}
		}
		current, currentErr := r.readDeployAuthorizationRecord(ctx, tx, site)
		if errors.Is(snapshotErr, sql.ErrNoRows) && errors.Is(currentErr, sql.ErrNoRows) {
			claimState := SiteStateProvisioning
			if r.policyResolver == nil {
				claimState = SiteStateActive
			}
			var generation int64
			if err := tx.QueryRowContext(ctx, insertSiteSQL,
				site, strings.ToLower(actor.Email), actor.PeerIP, ownerNameForIdentity(actor), claimState,
			).Scan(&generation); err != nil {
				tx.Rollback()
				return DeployAuthorization{}, fmt.Errorf("claim site %s: %w", site, err)
			}
			if err := tx.Commit(); err != nil {
				return DeployAuthorization{}, fmt.Errorf("commit site claim: %w", err)
			}
			return DeployAuthorization{Action: "create", AuthorizedAs: ManagementRoleOwner, PreviousState: claimState, ContentGeneration: generation}, nil
		}
		if currentErr != nil && !errors.Is(currentErr, sql.ErrNoRows) {
			tx.Rollback()
			return DeployAuthorization{}, currentErr
		}
		if snapshotErr != nil || currentErr != nil || !snapshot.sameAuthorizationState(current) {
			tx.Rollback()
			continue
		}
		if role == "" || (snapshot.State != SiteStateActive && role == ManagementRoleMaintainer) {
			tx.Rollback()
			return DeployAuthorization{}, ErrDeployForbidden
		}
		action := "update"
		targetState := SiteStateActive
		if snapshot.State == SiteStateDeleted {
			action = "recreate"
			targetState = SiteStateProvisioning
		} else if snapshot.State == SiteStateProvisioning {
			targetState = SiteStateProvisioning
		}
		ownerName := ""
		if role == ManagementRoleOwner {
			ownerName = ownerNameForIdentity(actor)
		}
		var generation int64
		if err := tx.QueryRowContext(ctx, touchSiteSQL,
			string(targetState), ownerName, site, snapshot.ContentGeneration,
		).Scan(&generation); err != nil {
			tx.Rollback()
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return DeployAuthorization{}, fmt.Errorf("touch site %s: %w", site, err)
		}
		if err := tx.Commit(); err != nil {
			return DeployAuthorization{}, fmt.Errorf("commit deploy auth: %w", err)
		}
		return DeployAuthorization{
			Action: action, AuthorizedAs: role, PreviousState: snapshot.State,
			ContentGeneration: generation, ContentWasDirty: snapshot.ContentDirty,
		}, nil
	}
	return DeployAuthorization{}, fmt.Errorf("authorize deploy for %s: site changed concurrently", site)
}

type deployAuthorizationRecord struct {
	SiteRecord
	ContentDirty           bool
	ContentGeneration      int64
	ExternalMutationActive bool
}

func (r deployAuthorizationRecord) sameAuthorizationState(other deployAuthorizationRecord) bool {
	return r.Name == other.Name && r.OwnerEmail == other.OwnerEmail && r.OwnerPeerIP == other.OwnerPeerIP &&
		r.State == other.State && r.PolicyTransitionGeneration == other.PolicyTransitionGeneration &&
		r.ContentDirty == other.ContentDirty && r.ContentGeneration == other.ContentGeneration &&
		r.ExternalMutationActive == other.ExternalMutationActive
}

func (r *SiteRegistry) readDeployAuthorizationRecord(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, site string) (deployAuthorizationRecord, error) {
	var record deployAuthorizationRecord
	err := q.QueryRowContext(ctx, `SELECT name, owner_email, owner_peer_ip, owner_name, state,
		policy_transition_generation, content_dirty, content_generation, content_external_mutation
		FROM sites WHERE name = ?`, site).Scan(
		&record.Name, &record.OwnerEmail, &record.OwnerPeerIP, &record.OwnerName, &record.State,
		&record.PolicyTransitionGeneration, &record.ContentDirty, &record.ContentGeneration, &record.ExternalMutationActive,
	)
	if err != nil {
		return deployAuthorizationRecord{}, err
	}
	return record, nil
}

func (r *SiteRegistry) CancelDeployAuthorization(ctx context.Context, site string, authz DeployAuthorization) error {
	if authz.Action == "create" {
		_, err := r.db.ExecContext(ctx, `DELETE FROM sites
			WHERE name = ? AND content_generation = ?`, site, authz.ContentGeneration)
		return err
	}
	state := authz.PreviousState
	if state == "" {
		state = SiteStateActive
	}
	_, err := r.db.ExecContext(ctx, `UPDATE sites SET content_dirty = ?, state = ?
		WHERE name = ? AND content_generation = ? AND content_external_mutation = 0`,
		authz.ContentWasDirty, state, site, authz.ContentGeneration)
	if err != nil {
		return fmt.Errorf("cancel deploy authorization for %s: %w", site, err)
	}
	return nil
}

// DeleteClaim removes a site's registry row. handleDeploy calls it when a
// site's first deploy claims the name but its storage write then fails,
// so a failed create does not permanently claim the name. It never runs
// for a redeploy, so an existing site's claim is left intact.
func (r *SiteRegistry) DeleteClaim(ctx context.Context, site string) error {
	if _, err := r.db.ExecContext(ctx, deleteSiteSQL, site); err != nil {
		return fmt.Errorf("delete site claim %s: %w", site, err)
	}
	return nil
}

func (r *SiteRegistry) readSiteForUpdate(ctx context.Context, tx *sql.Tx, site string) (SiteRecord, error) {
	var record SiteRecord
	err := scanSiteRecord(tx.QueryRowContext(ctx, readSiteSQL, site), &record)
	return record, err
}

func (r *SiteRegistry) RecordDeploy(ctx context.Context, event DeployAuditEvent) error {
	rawGroups, err := json.Marshal(event.Actor.Groups)
	if err != nil {
		return fmt.Errorf("encode deploy audit groups: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin deploy audit for %s: %w", event.Site, err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		insertDeployAuditSQL,
		event.Site,
		strings.ToLower(event.Actor.Email),
		event.Actor.PeerIP,
		event.Actor.Name,
		string(rawGroups),
		event.Action,
		event.Status,
		event.FileCount,
		event.TotalBytes,
		event.ContentHash,
		event.AuthorizedAs,
		event.AuthMethod,
		event.PublisherKeyID,
		event.PublisherName,
		event.Message,
	)
	if err != nil {
		return fmt.Errorf("record deploy audit for %s: %w", event.Site, err)
	}
	// Store the success audit and clear the dirty marker atomically so a failed
	// audit write never makes an older content hash appear current. Legacy
	// callers with no role still use this write as their activation signal.
	if event.Status == "success" &&
		(event.Action == "create" || event.Action == "recreate" || event.Action == "update") && event.ContentGeneration > 0 {
		state := SiteStateActive
		if event.AuthorizedAs != "" {
			state = ""
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sites SET
			state = CASE WHEN ? = '' THEN state ELSE ? END,
			content_dirty = 0
			WHERE name = ? AND content_generation = ? AND content_external_mutation = 0
			  AND policy_transition_generation = 0`,
			state, state, event.Site, event.ContentGeneration); err != nil {
			return fmt.Errorf("mark deployed content current for %s: %w", event.Site, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit deploy audit for %s: %w", event.Site, err)
	}
	return nil
}

func (r *SiteRegistry) CompleteDeploy(ctx context.Context, site string, authz DeployAuthorization) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sites SET state = 'active'
		WHERE name = ? AND content_generation = ? AND content_external_mutation = 0
		  AND policy_transition_generation = 0
		  AND state IN ('active', 'provisioning')`, site, authz.ContentGeneration)
	if err != nil {
		return fmt.Errorf("complete deploy for %s: %w", site, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count completed deploy for %s: %w", site, err)
	}
	if updated != 1 {
		return fmt.Errorf("complete deploy for %s: lifecycle or generation changed", site)
	}
	return nil
}

func (r *SiteRegistry) BeginPolicyTransition(ctx context.Context, site string, generation int64, previousHash, nextHash string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sites SET
		policy_transition_generation = ?, policy_previous_hash = ?, policy_next_hash = ?
		WHERE name = ? AND content_generation = ? AND policy_transition_generation = 0
		  AND (content_external_mutation = 0 OR content_external_mutation_owner LIKE 'delete:%')`,
		generation, previousHash, nextHash, site, generation)
	if err != nil {
		return fmt.Errorf("begin policy transition for %s: %w", site, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count policy transition for %s: %w", site, err)
	}
	if updated != 1 {
		return fmt.Errorf("begin policy transition for %s: generation changed or transition pending", site)
	}
	return nil
}

func (r *SiteRegistry) ClearPolicyTransition(ctx context.Context, site string, generation int64) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sites SET
		policy_transition_generation = 0, policy_previous_hash = '', policy_next_hash = ''
		WHERE name = ? AND content_generation = ? AND policy_transition_generation = ?`, site, generation, generation)
	if err != nil {
		return fmt.Errorf("clear policy transition for %s: %w", site, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count cleared policy transition for %s: %w", site, err)
	}
	if updated != 1 {
		return fmt.Errorf("clear policy transition for %s: generation changed", site)
	}
	return nil
}

// ClearPolicyTransitionForPublishingKey clears a recoverable policy fence only
// while the named credential is active and the site is owned by that key's
// creator. Keeping those checks in the UPDATE prevents a revoked or admin-owned
// key from using ambient owner/admin recovery authority on a foreign site.
func (r *SiteRegistry) ClearPolicyTransitionForPublishingKey(ctx context.Context, site string, generation int64, actor Identity, keyID string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sites SET
		policy_transition_generation = 0, policy_previous_hash = '', policy_next_hash = ''
		WHERE name = ? AND policy_transition_generation = ? AND content_generation = ?
		  AND EXISTS (
			SELECT 1 FROM publishing_keys pk
			WHERE pk.id = ? AND pk.revoked_at IS NULL AND pk.secret_hash IS NOT NULL
			  AND substr(sites.name, 1, length(pk.site_prefix)) = pk.site_prefix
			  AND length(sites.name) > length(pk.site_prefix)
			  AND ((pk.owner_email <> '' AND pk.owner_email = ? AND sites.owner_email = pk.owner_email)
			    OR (pk.owner_email = '' AND pk.owner_peer_ip <> '' AND pk.owner_peer_ip = ?
			      AND sites.owner_email = '' AND sites.owner_peer_ip = pk.owner_peer_ip))
		  )`, site, generation, generation, keyID, strings.ToLower(actor.Email), actor.PeerIP)
	if err != nil {
		return fmt.Errorf("clear publishing-key policy transition: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count publishing-key policy transition clear: %w", err)
	}
	if updated != 1 {
		return ErrDeployForbidden
	}
	return nil
}

func (r *SiteRegistry) HasPendingPolicyTransition(ctx context.Context, site string) (bool, error) {
	var pending bool
	if err := r.db.QueryRowContext(ctx, `SELECT policy_transition_generation <> 0 FROM sites WHERE name = ?`, site).Scan(&pending); errors.Is(err, sql.ErrNoRows) {
		return false, ErrSiteNotFound
	} else if err != nil {
		return false, fmt.Errorf("read policy transition for %s: %w", site, err)
	}
	return pending, nil
}

func (r *SiteRegistry) PendingPolicyTransition(ctx context.Context, site string) (PolicyTransition, error) {
	var transition PolicyTransition
	err := r.db.QueryRowContext(ctx, `SELECT policy_transition_generation, policy_previous_hash, policy_next_hash
		FROM sites WHERE name = ?`, site).Scan(&transition.Generation, &transition.PreviousHash, &transition.NextHash)
	if errors.Is(err, sql.ErrNoRows) {
		return PolicyTransition{}, ErrSiteNotFound
	}
	if err != nil {
		return PolicyTransition{}, fmt.Errorf("read policy transition for %s: %w", site, err)
	}
	return transition, nil
}

// OwnedSite is a site row joined with the size of its last successful
// deploy, for the platform's "my spots" page.
type OwnedSite struct {
	SiteRecord
	FileCount            int
	TotalBytes           int64
	ContentHash          string
	ContentHashUncertain bool
	LastDeployAt         string
	LastDeployAuthMethod string
	LastDeployPublisher  string
}

type ManageableSite struct {
	OwnedSite
	ManagementRole ManagementRole
}

func (r *SiteRegistry) SitesManageableBy(ctx context.Context, actor Identity) ([]ManageableSite, error) {
	rows, err := r.db.QueryContext(ctx, manageableSitesSQL)
	if err != nil {
		return nil, fmt.Errorf("list manageable site candidates: %w", err)
	}
	defer rows.Close()
	var candidates []OwnedSite
	for rows.Next() {
		var site OwnedSite
		var rawTags string
		if err := rows.Scan(&site.Name, &site.OwnerEmail, &site.OwnerPeerIP, &site.OwnerName,
			&site.Title, &site.Description, &rawTags, &site.State, &site.PolicyTransitionGeneration, &site.CreatedAt, &site.UpdatedAt,
			&site.FileCount, &site.TotalBytes, &site.ContentHash, &site.ContentHashUncertain,
			&site.LastDeployAt, &site.LastDeployAuthMethod, &site.LastDeployPublisher); err != nil {
			return nil, fmt.Errorf("scan manageable site candidate: %w", err)
		}
		site.Tags = decodeSiteTags(rawTags)
		candidates = append(candidates, site)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list manageable site candidates: %w", err)
	}

	resolved := make([]*ManageableSite, len(candidates))
	workers := 8
	if len(candidates) < workers {
		workers = len(candidates)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				candidate := candidates[index]
				role, err := r.managementRole(ctx, candidate.SiteRecord, actor)
				if err != nil {
					log.Printf("manageable sites: omit %s: %v", candidate.Name, err)
				} else if role != "" {
					resolved[index] = &ManageableSite{OwnedSite: candidate, ManagementRole: role}
				}
			}
		}()
	}
	for index := range candidates {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	manageable := make([]ManageableSite, 0, len(candidates))
	for _, site := range resolved {
		if site != nil {
			manageable = append(manageable, *site)
		}
	}
	return manageable, nil
}

// SitesOwnedBy returns the sites the actor owns, most recently updated
// first. Ownership mirrors SiteRecord.OwnedBy: the owner email when the
// site has one, the claiming peer IP otherwise.
func (r *SiteRegistry) SitesOwnedBy(ctx context.Context, actor Identity) ([]OwnedSite, error) {
	rows, err := r.db.QueryContext(ctx,
		sitesOwnedBySQL,
		strings.ToLower(actor.Email), actor.PeerIP)
	if err != nil {
		return nil, fmt.Errorf("list owned sites: %w", err)
	}
	defer rows.Close()

	var owned []OwnedSite
	for rows.Next() {
		var site OwnedSite
		var rawTags string
		if err := rows.Scan(&site.Name, &site.OwnerEmail, &site.OwnerPeerIP, &site.OwnerName,
			&site.Title, &site.Description, &rawTags, &site.State, &site.PolicyTransitionGeneration, &site.CreatedAt, &site.UpdatedAt,
			&site.FileCount, &site.TotalBytes, &site.ContentHash, &site.ContentHashUncertain,
			&site.LastDeployAt, &site.LastDeployAuthMethod, &site.LastDeployPublisher); err != nil {
			return nil, fmt.Errorf("scan owned site: %w", err)
		}
		site.Tags = decodeSiteTags(rawTags)
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
		`SELECT name, owner_email, owner_peer_ip, owner_name, title, description, tags, state, policy_transition_generation, created_at, updated_at
		 FROM sites
		 WHERE state = 'active' AND policy_transition_generation = 0
		 ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	defer rows.Close()

	var sites []SiteRecord
	for rows.Next() {
		var site SiteRecord
		if err := scanSiteRecord(rows, &site); err != nil {
			return nil, fmt.Errorf("scan site: %w", err)
		}
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	return sites, nil
}

func (r *SiteRegistry) ManagementDecision(ctx context.Context, site string, actor Identity) (ManagementDecision, error) {
	var record SiteRecord
	err := scanSiteRecord(r.db.QueryRowContext(ctx, readSiteSQL, site), &record)
	if errors.Is(err, sql.ErrNoRows) {
		return ManagementDecision{}, ErrSiteNotFound
	}
	if err != nil {
		return ManagementDecision{}, fmt.Errorf("read site %s: %w", site, err)
	}
	role, err := r.managementRole(ctx, record, actor)
	if err != nil {
		return ManagementDecision{State: record.State}, err
	}
	return ManagementDecision{Allowed: role != "", Role: role, State: record.State}, nil
}

func (r *SiteRegistry) CanManageSite(ctx context.Context, site string, actor Identity) (bool, error) {
	decision, err := r.ManagementDecision(ctx, site, actor)
	return decision.Allowed && (decision.State == SiteStateActive || r.policyResolver == nil), err
}

func (r *SiteRegistry) managementRole(ctx context.Context, record SiteRecord, actor Identity) (ManagementRole, error) {
	if record.OwnedBy(actor) {
		return ManagementRoleOwner, nil
	}
	if allowsAdmin(r.admins, actor) {
		return ManagementRoleAdmin, nil
	}
	if record.State != SiteStateActive || r.policyResolver == nil {
		return "", nil
	}
	if record.PolicyTransitionGeneration != 0 {
		return "", errors.New("stored management policy transition is unresolved")
	}
	policy, err := r.policyResolver(ctx, record.Name)
	if err != nil {
		return "", fmt.Errorf("resolve management policy for %s: %w", record.Name, err)
	}
	if policy != nil && policy.AllowsMaintainer(actor) {
		return ManagementRoleMaintainer, nil
	}
	return "", nil
}

// MarkSiteContentDirty durably records that storage may change before the
// first mutation starts. A later successful deploy audit clears the marker.
func (r *SiteRegistry) MarkSiteContentDirty(ctx context.Context, site string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sites
		SET content_dirty = 1, content_generation = content_generation + 1
		WHERE name = ? AND state = 'active'`, site)
	if err != nil {
		return fmt.Errorf("mark site content dirty for %s: %w", site, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count dirty site update for %s: %w", site, err)
	}
	if updated != 1 {
		return ErrSiteNotFound
	}
	return nil
}

func (r *SiteRegistry) BeginExternalContentMutation(ctx context.Context, site string) (string, error) {
	var owner string
	err := r.db.QueryRowContext(ctx, `UPDATE sites SET
		content_dirty = 1,
		content_generation = content_generation + 1,
		content_external_mutation = 1,
		content_external_mutation_started_at = unixepoch(),
		content_external_mutation_owner = lower(hex(randomblob(16)))
		WHERE name = ? AND state = 'active' AND content_external_mutation = 0
		  AND policy_transition_generation = 0
		RETURNING content_external_mutation_owner`, site).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if checkErr := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sites WHERE name = ?)`, site).Scan(&exists); checkErr != nil {
			return "", checkErr
		}
		if !exists {
			return "", ErrSiteNotFound
		}
		var state SiteState
		if stateErr := r.db.QueryRowContext(ctx, `SELECT state FROM sites WHERE name = ?`, site).Scan(&state); stateErr == nil && state != SiteStateActive {
			return "", ErrSiteNotActive
		}
		return "", ErrExternalContentMutationActive
	}
	if err != nil {
		return "", fmt.Errorf("begin external content mutation for %s: %w", site, err)
	}
	return owner, nil
}

func (r *SiteRegistry) EndExternalContentMutation(ctx context.Context, site, owner string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sites SET
		content_dirty = 1,
		content_generation = content_generation + 1,
		content_external_mutation = 0,
		content_external_mutation_started_at = 0,
		content_external_mutation_owner = ''
		WHERE name = ? AND content_external_mutation = 1
		  AND content_external_mutation_owner = ?`, site, owner)
	if err != nil {
		return fmt.Errorf("end external content mutation for %s: %w", site, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count external content mutation update for %s: %w", site, err)
	}
	if updated != 1 {
		return ErrExternalContentMutationLeaseLost
	}
	return nil
}

func (r *SiteRegistry) RecoverStaleExternalContentMutation(ctx context.Context, site string, staleBefore time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE sites SET
		content_dirty = 1,
		content_generation = content_generation + 1,
		content_external_mutation = 0,
		content_external_mutation_started_at = 0,
		content_external_mutation_owner = ''
		WHERE name = ? AND content_external_mutation <> 0
		  AND content_external_mutation_started_at > 0
		  AND content_external_mutation_started_at < ?`, site, staleBefore.Unix())
	if err != nil {
		return fmt.Errorf("recover stale external content mutation for %s: %w", site, err)
	}
	return nil
}

// DeleteSite removes a site's registry row after purge succeeds. A
// failed purge leaves the site claimed so its owner can retry.
func (r *SiteRegistry) DeleteSite(ctx context.Context, site string, actor Identity, purge func(context.Context) error) (retErr error) {
	var record SiteRecord
	err := scanSiteRecord(r.db.QueryRowContext(ctx, readSiteSQL, site), &record)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSiteNotFound
	}
	if err != nil {
		return fmt.Errorf("read site %s: %w", site, err)
	}
	role, err := r.managementRole(ctx, record, actor)
	if err != nil {
		return err
	}
	if role == "" {
		return ErrDeployForbidden
	}
	if record.State == SiteStateProvisioning {
		return ErrSiteNotActive
	}
	if record.State == SiteStateDeleted {
		if role == ManagementRoleMaintainer {
			return ErrDeployForbidden
		}
		if _, err := r.db.ExecContext(ctx, deleteSiteSQL, site); err != nil {
			return fmt.Errorf("release deleted site %s: %w", site, err)
		}
		return nil
	}
	var reservation string
	err = r.db.QueryRowContext(ctx, `UPDATE sites SET
		content_external_mutation = 1,
		content_external_mutation_started_at = unixepoch(),
		content_external_mutation_owner = 'delete:' || lower(hex(randomblob(16)))
		WHERE name = ? AND state = 'active' AND content_external_mutation = 0
		  AND policy_transition_generation = 0
		RETURNING content_external_mutation_owner`, site).Scan(&reservation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExternalContentMutationActive
	}
	if err != nil {
		return fmt.Errorf("reserve site deletion for %s: %w", site, err)
	}
	deletionComplete := false
	defer func() {
		if deletionComplete {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := r.db.ExecContext(cleanupCtx, `UPDATE sites SET
			content_external_mutation = 0,
			content_external_mutation_started_at = 0,
			content_external_mutation_owner = ''
			WHERE name = ? AND content_external_mutation = 1
			  AND content_external_mutation_owner = ?`, site, reservation)
		if err == nil {
			var updated int64
			updated, err = result.RowsAffected()
			if err == nil && updated != 1 {
				err = ErrExternalContentMutationLeaseLost
			}
		}
		if err != nil && retErr == nil {
			retErr = fmt.Errorf("release site deletion reservation for %s: %w", site, err)
		}
	}()
	if purge != nil {
		if err := purge(ctx); err != nil {
			return fmt.Errorf("purge site %s: %w", site, err)
		}
	}
	if role == ManagementRoleMaintainer {
		_, err := r.db.ExecContext(ctx, `UPDATE sites SET state = 'deleted', title = '', description = '', tags = '[]',
			content_dirty = 0, content_external_mutation = 0,
			content_external_mutation_started_at = 0, content_external_mutation_owner = '',
			updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now')
			WHERE name = ? AND state = 'active'`, site)
		if err != nil {
			return fmt.Errorf("tombstone site %s: %w", site, err)
		}
		deletionComplete = true
		return nil
	}
	if _, err := r.db.ExecContext(ctx, deleteSiteSQL, site); err != nil {
		return fmt.Errorf("delete site %s: %w", site, err)
	}
	deletionComplete = true
	return nil
}

func (r *SiteRegistry) UpdateSiteMetadata(ctx context.Context, site string, meta SiteMetadata) error {
	res, err := r.db.ExecContext(ctx, updateSiteMetadataSQL,
		meta.Title, meta.Description, encodeSiteTags(meta.Tags), site)
	if err != nil {
		return fmt.Errorf("update site metadata for %s: %w", site, err)
	}
	rows, err := res.RowsAffected()
	if err == nil && rows == 0 {
		return ErrSiteNotFound
	}
	return nil
}

func (r *SiteRegistry) UpdateDeploySiteMetadata(ctx context.Context, site string, meta SiteMetadata) error {
	res, err := r.db.ExecContext(ctx, `UPDATE sites SET title = ?, description = ?, tags = ?
		WHERE name = ? AND state IN ('active', 'provisioning')`,
		meta.Title, meta.Description, encodeSiteTags(meta.Tags), site)
	if err != nil {
		return fmt.Errorf("update deploying site metadata for %s: %w", site, err)
	}
	rows, err := res.RowsAffected()
	if err == nil && rows == 0 {
		return ErrSiteNotFound
	}
	return nil
}

func (r *SiteRegistry) SiteMetadata(ctx context.Context, site string) (SiteMetadata, error) {
	var record SiteRecord
	err := scanSiteRecord(r.db.QueryRowContext(ctx, readSiteSQL, site), &record)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteMetadata{}, ErrSiteNotFound
	}
	if err != nil {
		return SiteMetadata{}, fmt.Errorf("read site metadata for %s: %w", site, err)
	}
	return SiteMetadata{
		Title:       record.Title,
		Description: record.Description,
		Tags:        cloneSiteTags(record.Tags),
	}, nil
}

func (r *SiteRegistry) SiteState(ctx context.Context, site string) (SiteState, error) {
	var state SiteState
	if err := r.db.QueryRowContext(ctx, `SELECT state FROM sites WHERE name = ?`, site).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return "", ErrSiteNotFound
	} else if err != nil {
		return "", fmt.Errorf("read site state for %s: %w", site, err)
	}
	switch state {
	case SiteStateProvisioning, SiteStateActive, SiteStateDeleted:
		return state, nil
	default:
		return "", fmt.Errorf("invalid site lifecycle state %q", state)
	}
}

func (r *SiteRegistry) SiteContentGeneration(ctx context.Context, site string) (int64, error) {
	var generation int64
	if err := r.db.QueryRowContext(ctx, `SELECT content_generation FROM sites WHERE name = ?`, site).Scan(&generation); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSiteNotFound
	} else if err != nil {
		return 0, fmt.Errorf("read content generation for %s: %w", site, err)
	}
	return generation, nil
}

type siteRecordScanner interface {
	Scan(dest ...any) error
}

func scanSiteRecord(row siteRecordScanner, record *SiteRecord) error {
	var rawTags string
	if err := row.Scan(&record.Name, &record.OwnerEmail, &record.OwnerPeerIP, &record.OwnerName,
		&record.Title, &record.Description, &rawTags, &record.State, &record.PolicyTransitionGeneration, &record.CreatedAt, &record.UpdatedAt); err != nil {
		return err
	}
	record.Tags = decodeSiteTags(rawTags)
	switch record.State {
	case SiteStateProvisioning, SiteStateActive, SiteStateDeleted:
	default:
		return fmt.Errorf("invalid site lifecycle state %q", record.State)
	}
	return nil
}

const (
	insertSiteSQL = `INSERT INTO sites
		(name, owner_email, owner_peer_ip, owner_name, state, content_dirty, content_generation)
		VALUES (?, ?, ?, ?, ?, 1, 1)
		RETURNING content_generation`

	touchSiteSQL = `UPDATE sites SET
		updated_at = strftime('%Y-%m-%d %H:%M:%f', 'now'),
		state = ?,
		owner_name = CASE WHEN owner_name = '' THEN ? ELSE owner_name END,
		content_dirty = 1,
		content_generation = content_generation + 1
		WHERE name = ? AND content_generation = ? AND policy_transition_generation = 0
		RETURNING content_generation`

	readSiteSQL = `SELECT name, owner_email, owner_peer_ip, owner_name, title, description, tags, state, policy_transition_generation, created_at, updated_at
		FROM sites
		WHERE name = ?`

	insertDeployAuditSQL = `INSERT INTO site_deploy_audit
		(site, actor_email, actor_peer_ip, actor_name, actor_groups,
		 action, status, file_count, total_bytes, content_hash, authorized_as,
		 auth_method, publisher_key_id, publisher_name, message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sitesOwnedBySQL = `SELECT s.name, s.owner_email, s.owner_peer_ip, s.owner_name,
			s.title, s.description, s.tags, s.state, s.policy_transition_generation, s.created_at, s.updated_at,
			COALESCE((SELECT file_count FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), 0),
			COALESCE((SELECT total_bytes FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), 0),
			COALESCE((SELECT content_hash FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), ''),
			(s.content_dirty <> 0 OR COALESCE((SELECT status = 'failed' FROM site_deploy_audit
				WHERE site = s.name AND action IN ('create', 'update', 'delete')
				  AND status IN ('success', 'failed')
				ORDER BY created_at DESC, id DESC LIMIT 1), 0)),
			COALESCE((SELECT strftime('%Y-%m-%dT%H:%M:%fZ', created_at) FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), ''),
			COALESCE((SELECT auth_method FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), ''),
			COALESCE((SELECT publisher_name FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), '')
		FROM sites s
		WHERE s.state = 'active' AND ((s.owner_email <> '' AND s.owner_email = ?)
		   OR (s.owner_email = '' AND s.owner_peer_ip <> '' AND s.owner_peer_ip = ?))
		ORDER BY s.updated_at DESC`

	manageableSitesSQL = `SELECT s.name, s.owner_email, s.owner_peer_ip, s.owner_name,
			s.title, s.description, s.tags, s.state, s.policy_transition_generation, s.created_at, s.updated_at,
			CASE WHEN s.state = 'active' THEN COALESCE((SELECT file_count FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), 0) ELSE 0 END,
			CASE WHEN s.state = 'active' THEN COALESCE((SELECT total_bytes FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), 0) ELSE 0 END,
			CASE WHEN s.state = 'active' THEN COALESCE((SELECT content_hash FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), '') ELSE '' END,
			(s.state = 'active' AND (s.content_dirty <> 0 OR COALESCE((SELECT status = 'failed' FROM site_deploy_audit
				WHERE site = s.name AND action IN ('create', 'recreate', 'update', 'delete')
				  AND status IN ('success', 'failed')
				ORDER BY created_at DESC, id DESC LIMIT 1), 0))),
			CASE WHEN s.state = 'active' THEN COALESCE((SELECT strftime('%Y-%m-%dT%H:%M:%fZ', created_at) FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), '') ELSE '' END,
			CASE WHEN s.state = 'active' THEN COALESCE((SELECT auth_method FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), '') ELSE '' END,
			CASE WHEN s.state = 'active' THEN COALESCE((SELECT publisher_name FROM site_deploy_audit
				WHERE site = s.name AND status = 'success'
				ORDER BY created_at DESC, id DESC LIMIT 1), '') ELSE '' END
		FROM sites s
		WHERE s.state IN ('active', 'deleted')
		ORDER BY s.updated_at DESC`

	deleteSiteSQL = `DELETE FROM sites WHERE name = ?`

	updateSiteMetadataSQL = `UPDATE sites SET title = ?, description = ?, tags = ? WHERE name = ? AND state = 'active'`
)

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

func ownerNameForIdentity(actor Identity) string {
	if name := strings.TrimSpace(actor.Name); name != "" {
		return name
	}
	return strings.TrimSpace(actor.PeerName)
}

func allowsAdmin(policy *AccessPolicy, actor Identity) bool {
	return policy != nil && policy.Allows(actor)
}
