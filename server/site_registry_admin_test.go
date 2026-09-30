package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// newAdminRegistry builds a real SiteRegistry over an in-process SQLite
// database with the given platform-admin policy, mirroring the in-process
// database setup used elsewhere in the package's tests.
func newAdminRegistry(t *testing.T, admins *AccessPolicy) *SiteRegistry {
	t.Helper()
	ctx := context.Background()
	db, err := openSQLiteDB(ctx, filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatalf("open registry database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSiteRegistry(db, admins)
}

// TestPlatformAdminOverride covers allowsAdmin: with a non-nil admin
// policy a non-owner who matches admin by email or by group can deploy,
// manage, and delete a site they do not own, email matching is
// case-insensitive, and a non-owner non-admin is denied.
func TestPlatformAdminOverride(t *testing.T) {
	ctx := context.Background()
	owner := Identity{Email: "owner@example.com", Name: "Owner", PeerIP: "100.64.1.1"}
	admins := &AccessPolicy{Allow: []string{"admin@example.com", "platform-admins"}}

	adminByEmail := Identity{Email: "ADMIN@example.com", Name: "Admin", PeerIP: "100.64.2.1"}
	adminByGroup := Identity{Email: "ops@example.com", Name: "Ops", PeerIP: "100.64.2.2",
		Groups: []string{"platform-admins"}}
	stranger := Identity{Email: "stranger@example.com", Name: "Stranger", PeerIP: "100.64.3.1"}

	tests := []struct {
		name        string
		actor       Identity
		wantAllowed bool
	}{
		{"admin by email is case-insensitive", adminByEmail, true},
		{"admin by group", adminByGroup, true},
		{"non-owner non-admin denied", stranger, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newAdminRegistry(t, admins)

			const site = "admin-site"
			if _, err := registry.AuthorizeDeploy(ctx, site, owner); err != nil {
				t.Fatalf("owner claim: %v", err)
			}

			// Deploy: an admin may redeploy a site they do not own.
			authz, err := registry.AuthorizeDeploy(ctx, site, tt.actor)
			if tt.wantAllowed {
				if err != nil {
					t.Fatalf("admin deploy = %v, want allowed", err)
				}
				if authz.Action != "update" {
					t.Errorf("admin deploy action = %q, want update", authz.Action)
				}
			} else if !errors.Is(err, ErrDeployForbidden) {
				t.Fatalf("non-admin deploy = %v, want ErrDeployForbidden", err)
			}

			// Manage: CanManageSite mirrors the deploy decision.
			canManage, err := registry.CanManageSite(ctx, site, tt.actor)
			if err != nil {
				t.Fatalf("can manage: %v", err)
			}
			if canManage != tt.wantAllowed {
				t.Errorf("CanManageSite = %v, want %v", canManage, tt.wantAllowed)
			}

			// Delete: an admin may delete a site they do not own; a
			// non-admin non-owner is forbidden.
			purged := 0
			err = registry.DeleteSite(ctx, site, tt.actor, func(context.Context) error {
				purged++
				return nil
			})
			if tt.wantAllowed {
				if err != nil {
					t.Fatalf("admin delete = %v, want allowed", err)
				}
				if purged != 1 {
					t.Errorf("purge ran %d times, want 1", purged)
				}
			} else {
				if !errors.Is(err, ErrDeployForbidden) {
					t.Fatalf("non-admin delete = %v, want ErrDeployForbidden", err)
				}
				if purged != 0 {
					t.Errorf("purge ran %d times for forbidden delete, want 0", purged)
				}
			}
		})
	}
}

// TestNilAdminPolicyDeniesNonOwner confirms that without an admin policy
// allowsAdmin never grants a non-owner access, so the override is opt-in.
func TestNilAdminPolicyDeniesNonOwner(t *testing.T) {
	ctx := context.Background()
	registry := newAdminRegistry(t, nil)
	owner := Identity{Email: "owner@example.com", PeerIP: "100.64.1.1"}
	other := Identity{Email: "admin@example.com", PeerIP: "100.64.2.1",
		Groups: []string{"platform-admins"}}

	const site = "no-admin-site"
	if _, err := registry.AuthorizeDeploy(ctx, site, owner); err != nil {
		t.Fatalf("owner claim: %v", err)
	}
	if _, err := registry.AuthorizeDeploy(ctx, site, other); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("deploy with nil admin policy = %v, want ErrDeployForbidden", err)
	}
}

// TestDeletedSiteNameStaysWithItsOwner: a deleted site's name, and so its
// browser origin, never passes to another owner; only an admin can release it.
func TestDeletedSiteNameStaysWithItsOwner(t *testing.T) {
	ctx := context.Background()
	registry := newAdminRegistry(t, &AccessPolicy{Allow: []string{"admin@example.com"}})
	owner := Identity{Email: "owner@example.com", PeerIP: "100.64.1.1"}
	admin := Identity{Email: "admin@example.com", PeerIP: "100.64.2.1"}
	stranger := Identity{Email: "stranger@example.com", PeerIP: "100.64.3.1"}
	noPurge := func(context.Context) error { return nil }

	if _, err := registry.AuthorizeDeploy(ctx, "demo", owner); err != nil {
		t.Fatal(err)
	}
	if err := registry.DeleteSite(ctx, "demo", owner, noPurge); err != nil {
		t.Fatalf("owner delete = %v", err)
	}
	if state, err := registry.SiteState(ctx, "demo"); err != nil || state != SiteStateDeleted {
		t.Fatalf("state after owner delete = %q, %v; want a tombstone", state, err)
	}
	if _, err := registry.AuthorizeDeploy(ctx, "demo", stranger); !errors.Is(err, ErrDeployForbidden) {
		t.Fatalf("stranger claim of a deleted name = %v, want forbidden", err)
	}
	if err := registry.DeleteSite(ctx, "demo", owner, noPurge); !errors.Is(err, ErrSiteNameReleaseForbidden) {
		t.Fatalf("owner release = %v, want ErrSiteNameReleaseForbidden", err)
	}
	if err := registry.DeleteSite(ctx, "demo", admin, noPurge); err != nil {
		t.Fatalf("admin release = %v", err)
	}
	if authz, err := registry.AuthorizeDeploy(ctx, "demo", stranger); err != nil || authz.Action != "create" {
		t.Fatalf("claim after admin release = %+v, %v; want a new site", authz, err)
	}
}
