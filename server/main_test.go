package main

import (
	"strings"
	"testing"
)

func TestValidateDeploymentSafetyAllowsLocalDefaults(t *testing.T) {
	cfg := config{
		SpotDomain:  "spot.localhost",
		DatabaseURL: "postgres://spot:spot@postgres:5432/spot?sslmode=disable",
		S3Endpoint:  "rustfs:9000",
		S3AccessKey: "rustfsadmin",
		S3SecretKey: "rustfsadmin",
	}
	if err := validateDeploymentSafety(cfg); err != nil {
		t.Fatalf("local defaults rejected: %v", err)
	}
}

func TestValidateDeploymentSafetyRejectsSharedDefaults(t *testing.T) {
	cfg := config{
		SpotDomain:      "spot.example.com",
		DatabaseURL:     "postgres://spot:spot@postgres:5432/spot?sslmode=disable",
		S3Endpoint:      "rustfs:9000",
		S3AccessKey:     "rustfsadmin",
		S3SecretKey:     "rustfsadmin",
		NetbirdAPIURL:   "https://netbird.example.com",
		NetbirdAPIToken: "token",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "RustFS") {
		t.Fatalf("shared default RustFS creds error = %v, want RustFS rejection", err)
	}

	cfg.S3AccessKey = "real-access"
	cfg.S3SecretKey = "real-secret"
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "Postgres") {
		t.Fatalf("shared default Postgres password error = %v, want Postgres rejection", err)
	}

	cfg.DatabaseURL = "host=postgres user=spot password=spot dbname=spot sslmode=disable"
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "Postgres") {
		t.Fatalf("shared keyword/value Postgres password error = %v, want Postgres rejection", err)
	}

	cfg.DatabaseURL = "postgres://spot:better@postgres:5432/spot?sslmode=disable"
	if err := validateDeploymentSafety(cfg); err != nil {
		t.Fatalf("shared hardened config rejected: %v", err)
	}
}

func TestValidateDeploymentSafetyRejectsSharedDevIdentity(t *testing.T) {
	cfg := config{
		SpotDomain:       "spot.example.com",
		DatabaseURL:      "postgres://spot:better@postgres:5432/spot?sslmode=disable",
		S3Endpoint:       "rustfs:9000",
		S3AccessKey:      "real-access",
		S3SecretKey:      "real-secret",
		NetbirdAPIURL:    "https://netbird.example.com",
		NetbirdAPIToken:  "token",
		DevIdentityEmail: "dev@example.com",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "SPOT_DEV_IDENTITY_EMAIL") {
		t.Fatalf("shared dev identity error = %v, want rejection", err)
	}
}

func TestValidateDeploymentSafetyRequiresNetbirdForNonLocalDomain(t *testing.T) {
	cfg := config{
		SpotDomain:  "spot.example.com",
		DatabaseURL: "postgres://spot:better@postgres:5432/spot?sslmode=disable",
		S3Endpoint:  "rustfs:9000",
		S3AccessKey: "real-access",
		S3SecretKey: "real-secret",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "NETBIRD") {
		t.Fatalf("missing NetBird error = %v, want rejection", err)
	}
}
