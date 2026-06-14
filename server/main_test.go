package main

import (
	"fmt"
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

func TestValidateDeploymentSafetyAcceptsTailscaleSharedConfig(t *testing.T) {
	cfg := config{
		SpotDomain:        "spot.example.com",
		DatabaseURL:       "postgres://spot:better@postgres:5432/spot?sslmode=disable",
		S3Endpoint:        "rustfs:9000",
		S3AccessKey:       "real-access",
		S3SecretKey:       "real-secret",
		TailscaleAPIToken: "token",
		TailscaleTailnet:  "-",
	}
	if err := validateDeploymentSafety(cfg); err != nil {
		t.Fatalf("tailscale shared config rejected: %v", err)
	}
}

func TestValidateDeploymentSafetyAcceptsTailscaleOAuthSharedConfig(t *testing.T) {
	cfg := config{
		SpotDomain:           "spot.example.com",
		DatabaseURL:          "postgres://spot:better@postgres:5432/spot?sslmode=disable",
		S3Endpoint:           "rustfs:9000",
		S3AccessKey:          "real-access",
		S3SecretKey:          "real-secret",
		TailscaleOAuthID:     "client-id",
		TailscaleOAuthSecret: "client-secret",
		TailscaleTailnet:     "-",
	}
	if err := validateDeploymentSafety(cfg); err != nil {
		t.Fatalf("tailscale oauth shared config rejected: %v", err)
	}
}

func TestValidateDeploymentSafetyAcceptsSingleUserNonLocalDefaults(t *testing.T) {
	cfg := config{
		SpotDomain:       "spot.home.arpa",
		DatabaseURL:      "postgres://spot:spot@postgres:5432/spot?sslmode=disable",
		S3Endpoint:       "rustfs:9000",
		S3AccessKey:      "rustfsadmin",
		S3SecretKey:      "rustfsadmin",
		AuthMode:         authModeSingleUser,
		SingleUserEmail:  "owner@spot.local",
		SingleUserName:   "Spot Owner",
		SingleUserGroups: []string{"family"},
	}
	if err := validateDeploymentSafety(cfg); err != nil {
		t.Fatalf("single-user homelab config rejected: %v", err)
	}
}

func TestValidateDeploymentSafetyRejectsUnknownAuthMode(t *testing.T) {
	cfg := config{
		SpotDomain:  "spot.localhost",
		DatabaseURL: "postgres://spot:spot@postgres:5432/spot?sslmode=disable",
		AuthMode:    "none",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "SPOT_AUTH_MODE") {
		t.Fatalf("unknown auth mode error = %v, want SPOT_AUTH_MODE rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsSingleUserWithMeshProvider(t *testing.T) {
	cfg := config{
		SpotDomain:      "spot.home.arpa",
		DatabaseURL:     "postgres://spot:spot@postgres:5432/spot?sslmode=disable",
		AuthMode:        authModeSingleUser,
		SingleUserEmail: "owner@spot.local",
		NetbirdAPIURL:   "https://netbird.example.com",
		NetbirdAPIToken: "token",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "single-user") {
		t.Fatalf("single-user mesh provider error = %v, want rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsSingleUserWithoutEmail(t *testing.T) {
	cfg := config{
		SpotDomain:  "spot.home.arpa",
		DatabaseURL: "postgres://spot:spot@postgres:5432/spot?sslmode=disable",
		AuthMode:    authModeSingleUser,
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "SPOT_SINGLE_USER_EMAIL") {
		t.Fatalf("single-user missing email error = %v, want rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsBothProviders(t *testing.T) {
	cfg := config{
		SpotDomain:        "spot.example.com",
		DatabaseURL:       "postgres://spot:better@postgres:5432/spot?sslmode=disable",
		S3Endpoint:        "rustfs:9000",
		S3AccessKey:       "real-access",
		S3SecretKey:       "real-secret",
		NetbirdAPIURL:     "https://netbird.example.com",
		NetbirdAPIToken:   "token",
		TailscaleAPIToken: "token",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "exactly one mesh identity provider") {
		t.Fatalf("both providers error = %v, want exactly-one rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsTailscaleTokenAndOAuth(t *testing.T) {
	cfg := config{
		SpotDomain:           "spot.example.com",
		DatabaseURL:          "postgres://spot:better@postgres:5432/spot?sslmode=disable",
		S3Endpoint:           "rustfs:9000",
		S3AccessKey:          "real-access",
		S3SecretKey:          "real-secret",
		TailscaleAPIToken:    "token",
		TailscaleOAuthID:     "client-id",
		TailscaleOAuthSecret: "client-secret",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "either TAILSCALE_API_TOKEN") {
		t.Fatalf("tailscale token+oauth error = %v, want rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsPartialTailscaleOAuth(t *testing.T) {
	cfg := config{
		SpotDomain:       "spot.example.com",
		DatabaseURL:      "postgres://spot:better@postgres:5432/spot?sslmode=disable",
		S3Endpoint:       "rustfs:9000",
		S3AccessKey:      "real-access",
		S3SecretKey:      "real-secret",
		TailscaleOAuthID: "client-id",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "TAILSCALE_OAUTH_CLIENT_ID and TAILSCALE_OAUTH_CLIENT_SECRET") {
		t.Fatalf("partial tailscale oauth error = %v, want rejection", err)
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

func TestValidateDeploymentSafetyRequiresProviderForNonLocalDomain(t *testing.T) {
	cfg := config{
		SpotDomain:  "spot.example.com",
		DatabaseURL: "postgres://spot:better@postgres:5432/spot?sslmode=disable",
		S3Endpoint:  "rustfs:9000",
		S3AccessKey: "real-access",
		S3SecretKey: "real-secret",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "TAILSCALE") {
		t.Fatalf("missing provider error = %v, want rejection naming provider options", err)
	}
}

func TestNewResolver(t *testing.T) {
	tests := []struct {
		name string
		cfg  config
		want string
	}{
		{
			name: "netbird",
			cfg:  config{NetbirdAPIURL: "https://netbird.example.com", NetbirdAPIToken: "token"},
			want: "*main.NetbirdResolver",
		},
		{
			name: "tailscale",
			cfg:  config{TailscaleAPIToken: "token", TailscaleTailnet: "-"},
			want: "*main.TailscaleResolver",
		},
		{
			name: "tailscale oauth",
			cfg:  config{TailscaleOAuthID: "client-id", TailscaleOAuthSecret: "client-secret", TailscaleTailnet: "-"},
			want: "*main.TailscaleResolver",
		},
		{
			name: "dev identity",
			cfg:  config{DevIdentityEmail: "dev@example.com", DevIdentityName: "Dev"},
			want: "*main.StaticResolver",
		},
		{
			name: "single user",
			cfg:  config{AuthMode: authModeSingleUser, SingleUserEmail: "owner@spot.local", SingleUserName: "Owner"},
			want: "*main.StaticResolver",
		},
		{
			name: "none",
			cfg:  config{},
			want: "<nil>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver, _ := newResolver(tt.cfg)
			if got := typeName(resolver); got != tt.want {
				t.Fatalf("newResolver type = %s, want %s", got, tt.want)
			}
		})
	}
}

func typeName(v any) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", v)
}
