package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type ambiguousPolicyStore struct {
	SiteStorage
	commit          bool
	unreadableAfter bool
	attempted       bool
}

type clearFailingRegistry struct {
	*SiteRegistry
}

func (r *clearFailingRegistry) ClearPolicyTransition(context.Context, string, int64) error {
	return errors.New("database response lost while clearing policy transition")
}

func (s *ambiguousPolicyStore) Put(ctx context.Context, site, path, contentType string, data []byte) error {
	if s.commit {
		if err := s.SiteStorage.Put(ctx, site, path, contentType, data); err != nil {
			return err
		}
	}
	s.attempted = true
	return errors.New("response lost after policy put")
}

func (s *ambiguousPolicyStore) Open(ctx context.Context, site, path string) (io.ReadCloser, SiteFileInfo, error) {
	if s.attempted && s.unreadableAfter {
		return nil, SiteFileInfo{}, errors.New("policy read-back unavailable")
	}
	return s.SiteStorage.Open(ctx, site, path)
}

func TestSiteMaintainerLifecycleAndRecovery(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	policy := &AccessPolicy{Maintainers: []string{"maintainer@example.com", "team-estimation"}}
	registry.SetPolicyResolver(func(context.Context, string) (*AccessPolicy, error) {
		return cloneAccessPolicy(policy), nil
	})

	owner := Identity{Email: "owner@example.com", Name: "Owner"}
	maintainer := Identity{Email: "maintainer@example.com", Name: "Maintainer"}
	groupMaintainer := Identity{Email: "group@example.com", Groups: []string{"TEAM-ESTIMATION"}}
	stranger := Identity{Email: "stranger@example.com"}

	create, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := registry.ManagementDecision(ctx, "demo", maintainer); err != nil || decision.Allowed {
		t.Fatalf("provisioning maintainer decision = %+v, %v; want denied", decision, err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", create, newDeployVersion()); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []Identity{maintainer, groupMaintainer} {
		decision, err := registry.ManagementDecision(ctx, "demo", actor)
		if err != nil || !decision.Allowed || decision.Role != ManagementRoleMaintainer || decision.State != SiteStateActive {
			t.Fatalf("maintainer decision for %+v = %+v, %v", actor, decision, err)
		}
	}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", stranger); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("stranger deploy = %v, want forbidden", err)
	}

	update, err := registry.AuthorizeDeploy(ctx, "demo", maintainer)
	if err != nil || update.AuthorizedAs != ManagementRoleMaintainer {
		t.Fatalf("maintainer deploy = %+v, %v", update, err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", update, newDeployVersion()); err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(ctx, DeployAuditEvent{
		Site: "demo", Actor: maintainer, Action: "update", Status: "success",
		AuthorizedAs: update.AuthorizedAs, ContentGeneration: update.ContentGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	var authorizedAs string
	if err := db.QueryRowContext(ctx, `SELECT authorized_as FROM site_deploy_audit ORDER BY id DESC LIMIT 1`).Scan(&authorizedAs); err != nil {
		t.Fatal(err)
	}
	if authorizedAs != "maintainer" {
		t.Fatalf("authorized_as = %q, want maintainer", authorizedAs)
	}
	policy = &AccessPolicy{}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", maintainer); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("self-removed maintainer deploy = %v, want forbidden", err)
	}
	policy.Maintainers = []string{"maintainer@example.com"}

	manageable, err := registry.SitesManageableBy(ctx, maintainer)
	if err != nil || len(manageable) != 1 || manageable[0].ManagementRole != ManagementRoleMaintainer {
		t.Fatalf("manageable sites = %+v, %v", manageable, err)
	}
	if err := registry.DeleteSite(ctx, "demo", maintainer, nil); err != nil {
		t.Fatal(err)
	}
	if state, err := registry.SiteState(ctx, "demo"); err != nil || state != SiteStateDeleted {
		t.Fatalf("state after maintainer delete = %q, %v", state, err)
	}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", maintainer); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("maintainer tombstone recreate = %v, want forbidden", err)
	}
	recreate, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil || recreate.Action != "recreate" || recreate.AuthorizedAs != ManagementRoleOwner {
		t.Fatalf("owner recreate = %+v, %v", recreate, err)
	}
}

func TestSiteOwnerNameUsesNetBirdPeerName(t *testing.T) {
	ctx := context.Background()

	t.Run("human name takes precedence", func(t *testing.T) {
		db := openTestDB(t)
		registry := NewSiteRegistry(db, nil)
		owner := Identity{
			Email: "owner@example.com", Name: "Site Owner",
			PeerIP: "100.64.0.7", PeerName: "owner-laptop",
		}

		if _, err := registry.AuthorizeDeploy(ctx, "human-site", owner); err != nil {
			t.Fatal(err)
		}
		var ownerName string
		if err := db.QueryRowContext(ctx, `SELECT owner_name FROM sites WHERE name = 'human-site'`).Scan(&ownerName); err != nil {
			t.Fatal(err)
		}
		if ownerName != "Site Owner" {
			t.Fatalf("owner_name = %q, want Site Owner", ownerName)
		}
	})

	t.Run("new server-owned site", func(t *testing.T) {
		db := openTestDB(t)
		registry := NewSiteRegistry(db, nil)
		owner := Identity{PeerIP: "100.64.0.9", PeerName: "gitlab-runner"}

		if _, err := registry.AuthorizeDeploy(ctx, "new-site", owner); err != nil {
			t.Fatal(err)
		}
		var ownerName string
		if err := db.QueryRowContext(ctx, `SELECT owner_name FROM sites WHERE name = 'new-site'`).Scan(&ownerName); err != nil {
			t.Fatal(err)
		}
		if ownerName != "gitlab-runner" {
			t.Fatalf("owner_name = %q, want gitlab-runner", ownerName)
		}
	})

	t.Run("existing server-owned site", func(t *testing.T) {
		db := openTestDB(t)
		registry := NewSiteRegistry(db, nil)
		legacyOwner := Identity{PeerIP: "100.64.0.9"}
		create, err := registry.AuthorizeDeploy(ctx, "existing-site", legacyOwner)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.CompleteDeploy(ctx, "existing-site", create, newDeployVersion()); err != nil {
			t.Fatal(err)
		}

		owner := Identity{PeerIP: "100.64.0.9", PeerName: "gitlab-runner"}
		if _, err := registry.AuthorizeDeploy(ctx, "existing-site", owner); err != nil {
			t.Fatal(err)
		}
		var ownerName string
		if err := db.QueryRowContext(ctx, `SELECT owner_name FROM sites WHERE name = 'existing-site'`).Scan(&ownerName); err != nil {
			t.Fatal(err)
		}
		if ownerName != "gitlab-runner" {
			t.Fatalf("owner_name = %q, want gitlab-runner", ownerName)
		}
	})

	t.Run("maintainer cannot replace missing owner name", func(t *testing.T) {
		db := openTestDB(t)
		registry := NewSiteRegistry(db, nil)
		registry.SetPolicyResolver(func(context.Context, string) (*AccessPolicy, error) {
			return &AccessPolicy{Maintainers: []string{"maintainer@example.com"}}, nil
		})
		owner := Identity{Email: "owner@example.com"}
		create, err := registry.AuthorizeDeploy(ctx, "maintained-site", owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.CompleteDeploy(ctx, "maintained-site", create, newDeployVersion()); err != nil {
			t.Fatal(err)
		}
		maintainer := Identity{Email: "maintainer@example.com", Name: "Maintainer"}
		if _, err := registry.AuthorizeDeploy(ctx, "maintained-site", maintainer); err != nil {
			t.Fatal(err)
		}
		var ownerName string
		if err := db.QueryRowContext(ctx, `SELECT owner_name FROM sites WHERE name = 'maintained-site'`).Scan(&ownerName); err != nil {
			t.Fatal(err)
		}
		if ownerName != "" {
			t.Fatalf("owner_name = %q, want blank", ownerName)
		}
	})
}

func TestAllSitesExcludesPendingPolicyTransitions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "owner@example.com"}
	for _, name := range []string{"stable", "fenced"} {
		if _, err := registry.AuthorizeDeploy(ctx, name, owner); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE sites SET policy_transition_generation = content_generation WHERE name = 'fenced'`); err != nil {
		t.Fatal(err)
	}

	sites, err := registry.AllSites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || sites[0].Name != "stable" {
		t.Fatalf("listed sites = %+v, want only stable", sites)
	}
}

func TestMeCapabilitiesFailClosedForSiteLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "owner@example.com"}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", owner); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		aiAccess: aiAccessVisitors, ai: NewAIProxy("test-key", "", "", nil, nil),
		slackAccess: slackAccessVisitors, slack: NewSlackProxy("test-token", ""),
		sites: newTestSiteStore(t), siteAdmin: registry,
		resolver: NewStaticResolver(owner.Email, "Owner", nil), spotDomain: "spot.localhost",
	}
	get := func() meResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleMe(rec, httptest.NewRequest(http.MethodGet, "http://demo.spot.localhost/api/me", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
		}
		var body meResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if body := get(); !body.AIAllowed || !body.SlackAllowed {
		t.Fatalf("active capabilities = ai:%v slack:%v, want both true", body.AIAllowed, body.SlackAllowed)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sites SET policy_transition_generation = content_generation WHERE name = 'demo'`); err != nil {
		t.Fatal(err)
	}
	if body := get(); body.AIAllowed || body.SlackAllowed {
		t.Fatalf("fenced capabilities = ai:%v slack:%v, want both false", body.AIAllowed, body.SlackAllowed)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sites SET policy_transition_generation = 0, state = 'deleted' WHERE name = 'demo'`); err != nil {
		t.Fatal(err)
	}
	if body := get(); body.AIAllowed || body.SlackAllowed {
		t.Fatalf("deleted capabilities = ai:%v slack:%v, want both false", body.AIAllowed, body.SlackAllowed)
	}
}

func TestManageableSitesAPIIncludesMaintainerAttribution(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	root := t.TempDir()
	writeSiteFile(t, root, "demo", accessFileName, `{"maintainers":["maintainer@example.com"]}`)
	sites, err := NewLocalSiteStore(root)
	if err != nil {
		t.Fatal(err)
	}
	policies := NewPolicyStore(root, time.Minute)
	registry := NewSiteRegistry(db, nil)
	registry.SetPolicyResolver(func(ctx context.Context, site string) (*AccessPolicy, error) {
		return policies.For(site)
	})
	owner := Identity{Email: "owner@example.com", Name: "CI Runner"}
	authz, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", authz, newDeployVersion()); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		siteAdmin: registry, siteManager: registry, sites: sites, policies: policies,
		resolver:   NewStaticResolver("maintainer@example.com", "Maintainer", nil),
		spotDomain: "spot.localhost", trustedProxies: testTrustedProxies(t),
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/manageable"))
	if rec.Code != http.StatusOK {
		t.Fatalf("manageable = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Sites []ownedSiteJSON `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sites) != 1 || body.Sites[0].ManagementRole != "maintainer" || body.Sites[0].Owner != "CI Runner" || body.Sites[0].State != SiteStateActive {
		t.Fatalf("manageable response = %+v", body.Sites)
	}
}

func TestRestrictiveStagingPolicyIsExplicit(t *testing.T) {
	staging := restrictiveStagingPolicy(&AccessPolicy{Maintainers: []string{"alice@example.com"}})
	raw, err := marshalRestrictiveStagingPolicy(staging)
	if err != nil {
		t.Fatal(err)
	}
	wantFields := []string{`"allow":[]`, `"download":false`, `"maintainers":["alice@example.com"]`}
	for _, field := range wantFields {
		if !bytes.Contains(raw, []byte(field)) {
			t.Fatalf("staging policy %s does not contain %s", raw, field)
		}
	}
}

func TestPolicyTransitionReadBackAndDurableFence(t *testing.T) {
	for _, tt := range []struct {
		name            string
		commit          bool
		unreadableAfter bool
		wantUnresolved  bool
	}{
		{name: "committed response loss resolves as success", commit: true},
		{name: "unreadable result remains fenced", unreadableAfter: true, wantUnresolved: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := openTestDB(t)
			registry := NewSiteRegistry(db, nil)
			registry.SetPolicyResolver(func(context.Context, string) (*AccessPolicy, error) {
				return &AccessPolicy{Maintainers: []string{"alice@example.com"}}, nil
			})
			owner := Identity{Email: "owner@example.com"}
			create, err := registry.AuthorizeDeploy(ctx, "demo", owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.CompleteDeploy(ctx, "demo", create, newDeployVersion()); err != nil {
				t.Fatal(err)
			}
			authz, err := registry.AuthorizeDeploy(ctx, "demo", owner)
			if err != nil {
				t.Fatal(err)
			}
			local, err := NewLocalSiteStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			store := &ambiguousPolicyStore{SiteStorage: local, commit: tt.commit, unreadableAfter: tt.unreadableAfter}
			srv := &Server{sites: store, deployAuth: registry, policies: NewPolicyStore("", time.Hour)}
			err = srv.commitPolicyObject(ctx, "demo", authz.ContentGeneration, []byte(`{"maintainers":["alice@example.com"]}`), false)
			if got := errors.Is(err, errPolicyTransitionUnresolved); got != tt.wantUnresolved {
				t.Fatalf("commitPolicyObject error = %v, unresolved=%v want %v", err, got, tt.wantUnresolved)
			}
			pending, err := registry.HasPendingPolicyTransition(ctx, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if pending != tt.wantUnresolved {
				t.Fatalf("pending fence = %v, want %v", pending, tt.wantUnresolved)
			}
			if tt.wantUnresolved {
				decision, decisionErr := registry.ManagementDecision(ctx, "demo", Identity{Email: "alice@example.com"})
				if decisionErr == nil || decision.Allowed {
					t.Fatalf("maintainer decision while fenced = %+v, %v", decision, decisionErr)
				}
				ownerDecision, ownerErr := registry.ManagementDecision(ctx, "demo", Identity{Email: "owner@example.com"})
				if ownerErr != nil || !ownerDecision.Allowed || ownerDecision.Role != ManagementRoleOwner {
					t.Fatalf("owner recovery decision = %+v, %v", ownerDecision, ownerErr)
				}
			}
		})
	}
}

func TestPolicyTransitionClearFailureRetainsProvisioningClaim(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	registry.SetPolicyResolver(func(context.Context, string) (*AccessPolicy, error) { return nil, nil })
	owner := Identity{Email: "owner@example.com"}
	authz, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	sites, err := NewLocalSiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites: sites, deployAuth: &clearFailingRegistry{SiteRegistry: registry},
		policies: NewPolicyStore("", time.Hour),
	}
	err = srv.commitPolicyObject(ctx, "demo", authz.ContentGeneration, []byte(`{"maintainers":["alice@example.com"]}`), false)
	if !errors.Is(err, errPolicyTransitionUnresolved) {
		t.Fatalf("commitPolicyObject = %v, want unresolved transition", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/deploy", nil)
	srv.failPolicyCommit(req, "demo", owner, authz, nil, nil, err, "could not store _access.json")
	if state, stateErr := registry.SiteState(ctx, "demo"); stateErr != nil || state != SiteStateProvisioning {
		t.Fatalf("site after unresolved create = %q, %v; want retained provisioning claim", state, stateErr)
	}
}

func TestPolicyTransitionsAndExternalMutationsAreMutuallyExclusive(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "owner@example.com"}
	create, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", create, newDeployVersion()); err != nil {
		t.Fatal(err)
	}
	generation, err := registry.SiteContentGeneration(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.BeginPolicyTransition(ctx, "demo", generation, absentPolicyHash, "sha256:next"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.BeginExternalContentMutation(ctx, "demo"); !errors.Is(err, ErrExternalContentMutationActive) {
		t.Fatalf("external mutation during policy transition = %v, want blocked", err)
	}
	if err := registry.ClearPolicyTransition(ctx, "demo", generation); err != nil {
		t.Fatal(err)
	}
	lease, err := registry.BeginExternalContentMutation(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	generation, err = registry.SiteContentGeneration(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.BeginPolicyTransition(ctx, "demo", generation, absentPolicyHash, "sha256:next"); err == nil {
		t.Fatal("policy transition started during external content mutation")
	}
	if err := registry.EndExternalContentMutation(ctx, "demo", lease); err != nil {
		t.Fatal(err)
	}
}

func TestMaintainerPolicyResolutionDoesNotHoldSQLiteConnection(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "owner@example.com"}
	create, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	registry.SetPolicyResolver(func(context.Context, string) (*AccessPolicy, error) {
		close(entered)
		<-release
		return &AccessPolicy{Maintainers: []string{"maintainer@example.com"}}, nil
	})
	result := make(chan error, 1)
	go func() {
		_, err := registry.AuthorizeDeploy(ctx, "demo", Identity{Email: "maintainer@example.com"})
		result <- err
	}()
	<-entered
	queryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var one int
	if err := db.QueryRowContext(queryCtx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("SQLite query while policy resolver blocked = %d, %v", one, err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("maintainer authorization = %v", err)
	}
	if create.ContentGeneration == 0 {
		t.Fatal("initial site claim did not reserve a generation")
	}
}

func TestDeleteReservationExcludesExternalContentMutation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "owner@example.com"}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", owner); err != nil {
		t.Fatal(err)
	}
	purgeStarted := make(chan struct{})
	releasePurge := make(chan struct{})
	deleteResult := make(chan error, 1)
	go func() {
		deleteResult <- registry.DeleteSite(ctx, "demo", owner, func(context.Context) error {
			close(purgeStarted)
			<-releasePurge
			return nil
		})
	}()
	<-purgeStarted
	if _, err := registry.BeginExternalContentMutation(ctx, "demo"); !errors.Is(err, ErrExternalContentMutationActive) {
		t.Fatalf("external mutation during delete = %v, want blocked", err)
	}
	close(releasePurge)
	if err := <-deleteResult; err != nil {
		t.Fatal(err)
	}

	if _, err := registry.AuthorizeDeploy(ctx, "leased", owner); err != nil {
		t.Fatal(err)
	}
	lease, err := registry.BeginExternalContentMutation(ctx, "leased")
	if err != nil {
		t.Fatal(err)
	}
	purged := false
	if err := registry.DeleteSite(ctx, "leased", owner, func(context.Context) error {
		purged = true
		return nil
	}); !errors.Is(err, ErrExternalContentMutationActive) {
		t.Fatalf("delete during external mutation = %v, want blocked", err)
	}
	if purged {
		t.Fatal("purge ran without acquiring deletion reservation")
	}
	if err := registry.EndExternalContentMutation(ctx, "leased", lease); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRemovalStagesVisitorCapabilityRevocation(t *testing.T) {
	for _, current := range []*AccessPolicy{{AI: aiAccessVisitors}, {Slack: slackAccessVisitors}} {
		if !policyNarrowsAccess(current, nil, false) {
			t.Fatalf("policy removal did not stage visitor capability revocation: %+v", current)
		}
	}
}

func TestDeployAuditAtomicallyClearsContentDirty(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	registry.SetPolicyResolver(func(context.Context, string) (*AccessPolicy, error) { return nil, nil })
	owner := Identity{Email: "owner@example.com"}
	authz, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", authz, newDeployVersion()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_deploy_audit BEFORE INSERT ON site_deploy_audit
		BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	event := DeployAuditEvent{
		Site: "demo", Actor: owner, Action: "create", Status: "success",
		AuthorizedAs: authz.AuthorizedAs, ContentGeneration: authz.ContentGeneration,
	}
	if err := registry.RecordDeploy(ctx, event); err == nil {
		t.Fatal("RecordDeploy succeeded while audit insert was forced to fail")
	}
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT content_dirty FROM sites WHERE name = 'demo'`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("failed audit made content appear current")
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_deploy_audit`); err != nil {
		t.Fatal(err)
	}
	if err := registry.RecordDeploy(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT content_dirty FROM sites WHERE name = 'demo'`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("successful audit did not mark content current")
	}
}

func TestSitePreviewHonorsLifecycleAndPolicyFence(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	registry.SetPolicyResolver(func(context.Context, string) (*AccessPolicy, error) { return nil, nil })
	sites, err := NewLocalSiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sites.Put(ctx, "demo", "_screenshot.png", "image/png", []byte("not-opened")); err != nil {
		t.Fatal(err)
	}
	owner := Identity{Email: "owner@example.com"}
	authz, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites: sites, siteAdmin: registry, spotDomain: "spot.localhost",
		trustedProxies: testTrustedProxies(t),
	}
	preview := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, sitesRequest(http.MethodGet, "/api/sites/demo/preview"))
		return rec
	}
	if rec := preview(); rec.Code != http.StatusNotFound {
		t.Fatalf("provisioning preview = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if err := registry.CompleteDeploy(ctx, "demo", authz, newDeployVersion()); err != nil {
		t.Fatal(err)
	}
	if err := registry.BeginPolicyTransition(ctx, "demo", authz.ContentGeneration, absentPolicyHash, "sha256:next"); err != nil {
		t.Fatal(err)
	}
	if rec := preview(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fenced preview = %d %s, want 503", rec.Code, rec.Body.String())
	}
}

func TestPublishedDeleteDoesNotStageRestrictivePolicy(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	root := t.TempDir()
	sites, err := NewLocalSiteStore(root)
	if err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"allow":["team@example.com"],"maintainers":["maintainer@example.com"]}`)
	if err := sites.Put(ctx, "demo", accessFileName, "application/json", original); err != nil {
		t.Fatal(err)
	}
	policies := NewPolicyStore(root, time.Hour)
	registry := NewSiteRegistry(db, nil)
	registry.SetPolicyResolver(func(ctx context.Context, site string) (*AccessPolicy, error) { return policies.For(site) })
	owner := Identity{Email: "owner@example.com"}
	create, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", create, newDeployVersion()); err != nil {
		t.Fatal(err)
	}
	publications := NewCloudflarePublicationStore(db)
	if err := publications.Upsert(ctx, cloudflarePublication{
		Site: "demo", ProjectName: "spot-demo", Hostname: "demo.example.com", Status: "published",
	}); err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites: sites, policies: policies, siteAdmin: registry, siteManager: registry, deployAuth: registry,
		resolver: NewStaticResolver(owner.Email, "Owner", nil), spotDomain: "spot.localhost",
		trustedProxies: testTrustedProxies(t), deployLimit: NewRateLimiter(1000, 1000),
		cloudflarePubs: publications,
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete published site = %d %s, want 409", rec.Code, rec.Body.String())
	}
	rc, _, err := sites.Open(ctx, "demo", accessFileName)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, original) {
		t.Fatalf("policy after rejected delete = %s, want %s", stored, original)
	}
	if pending, err := registry.HasPendingPolicyTransition(ctx, "demo"); err != nil || pending {
		t.Fatalf("policy fence after rejected delete = %v, %v", pending, err)
	}
}

func TestDeniedDeleteThroughManagementDecisionIsAudited(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	registry := NewSiteRegistry(db, nil)
	owner := Identity{Email: "owner@example.com"}
	create, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", create, newDeployVersion()); err != nil {
		t.Fatal(err)
	}
	sites, err := NewLocalSiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		sites: sites, siteAdmin: registry, siteManager: registry, deployAuth: registry,
		resolver:   NewStaticResolver("stranger@example.com", "Stranger", nil),
		spotDomain: "spot.localhost", trustedProxies: testTrustedProxies(t), deployLimit: NewRateLimiter(1000, 1000),
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, sitesRequest(http.MethodDelete, "/api/sites/demo"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied delete = %d %s, want 403", rec.Code, rec.Body.String())
	}
	var action, status string
	if err := db.QueryRowContext(ctx, `SELECT action, status FROM site_deploy_audit ORDER BY id DESC LIMIT 1`).Scan(&action, &status); err != nil {
		t.Fatal(err)
	}
	if action != "delete" || status != "denied" {
		t.Fatalf("denied delete audit = %q %q", action, status)
	}
}
