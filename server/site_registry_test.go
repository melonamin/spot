package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func readSiteTouch(t *testing.T, db *sql.DB, site string) (updatedAt string, generation int64, state SiteState) {
	t.Helper()
	if err := db.QueryRow(`SELECT CAST(updated_at AS TEXT), content_generation, state FROM sites WHERE name = ?`, site).Scan(
		&updatedAt, &generation, &state); err != nil {
		t.Fatal(err)
	}
	return updatedAt, generation, state
}

// A cancelled authorization puts back the row it touched: a refused create
// releases its claim, and a refused recreate leaves the tombstone with its
// previous updated_at and generation.
func TestCancelDeployAuthorizationRestoresTheTouch(t *testing.T) {
	ctx := context.Background()
	owner := Identity{Email: "owner@example.com", Name: "Owner"}
	registry := newAdminRegistry(t, nil)
	noPurge := func(context.Context) error { return nil }

	authz, err := registry.AuthorizeDeploy(ctx, "fresh", owner)
	if err != nil || authz.Action != "create" {
		t.Fatalf("authorize create = %+v %v", authz, err)
	}
	if err := registry.CancelDeployAuthorization(ctx, "fresh", authz); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SiteState(ctx, "fresh"); !errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("cancelled create left a claim: %v", err)
	}

	authz, err = registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", authz); err != nil {
		t.Fatal(err)
	}
	if err := registry.DeleteSite(ctx, "demo", owner, noPurge); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.db.Exec(`UPDATE sites SET updated_at = '2026-01-01 00:00:00.000' WHERE name = 'demo'`); err != nil {
		t.Fatal(err)
	}
	beforeAt, beforeGen, beforeState := readSiteTouch(t, registry.db, "demo")
	if beforeState != SiteStateDeleted {
		t.Fatalf("state = %s, want a tombstone", beforeState)
	}
	authz, err = registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil || authz.Action != "recreate" {
		t.Fatalf("authorize recreate = %+v %v", authz, err)
	}
	if at, gen, _ := readSiteTouch(t, registry.db, "demo"); at == beforeAt || gen != beforeGen+1 {
		t.Fatalf("authorization did not touch the row: %s %d", at, gen)
	}
	if err := registry.CancelDeployAuthorization(ctx, "demo", authz); err != nil {
		t.Fatal(err)
	}
	if at, gen, state := readSiteTouch(t, registry.db, "demo"); at != beforeAt || gen != beforeGen || state != SiteStateDeleted {
		t.Fatalf("cancelled recreate = %s %d %s, want %s %d %s", at, gen, state, beforeAt, beforeGen, SiteStateDeleted)
	}
}

// A policy fence opened under the reserved generation is cleared only while
// the row still carries that generation, so cancelling must not rewind it.
func TestCancelDeployAuthorizationKeepsTheGenerationOfAnOpenPolicyFence(t *testing.T) {
	ctx := context.Background()
	owner := Identity{Email: "owner@example.com", Name: "Owner"}
	registry := newAdminRegistry(t, nil)
	authz, err := registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CompleteDeploy(ctx, "demo", authz); err != nil {
		t.Fatal(err)
	}
	authz, err = registry.AuthorizeDeploy(ctx, "demo", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.BeginPolicyTransition(ctx, "demo", authz.ContentGeneration, "a", "b"); err != nil {
		t.Fatal(err)
	}
	touchedAt, _, _ := readSiteTouch(t, registry.db, "demo")
	if err := registry.CancelDeployAuthorization(ctx, "demo", authz); err != nil {
		t.Fatal(err)
	}
	if at, gen, _ := readSiteTouch(t, registry.db, "demo"); at != touchedAt || gen != authz.ContentGeneration {
		t.Fatalf("cancel under an open fence = %s %d, want %s %d", at, gen, touchedAt, authz.ContentGeneration)
	}
	if err := registry.ClearPolicyTransition(ctx, "demo", authz.ContentGeneration); err != nil {
		t.Fatalf("fence no longer clearable: %v", err)
	}
}
