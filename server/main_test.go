package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateDeploymentSafetyAllowsLocalDefaults(t *testing.T) {
	cfg := config{
		SpotDomain:  "spot.localhost",
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
	if err := validateDeploymentSafety(cfg); err != nil {
		t.Fatalf("shared hardened config rejected: %v", err)
	}
}

func TestValidateDeploymentSafetyAcceptsTailscaleSharedConfig(t *testing.T) {
	cfg := config{
		SpotDomain:        "spot.example.com",
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

func TestValidateDeploymentSafetyAcceptsLocalStorageWithoutS3(t *testing.T) {
	cfg := config{
		SpotDomain:      "spot.home.arpa",
		StorageMode:     storageModeLocal,
		AuthMode:        authModeSingleUser,
		SingleUserEmail: "owner@spot.local",
	}
	if err := validateDeploymentSafety(cfg); err != nil {
		t.Fatalf("local storage config rejected: %v", err)
	}
}

func TestValidateDeploymentSafetyIgnoresDefaultS3CredsInLocalStorage(t *testing.T) {
	cfg := config{
		SpotDomain:      "spot.example.com",
		StorageMode:     storageModeLocal,
		S3Endpoint:      "rustfs:9000",
		S3AccessKey:     "rustfsadmin",
		S3SecretKey:     "rustfsadmin",
		NetbirdAPIURL:   "https://netbird.example.com",
		NetbirdAPIToken: "token",
	}
	if err := validateDeploymentSafety(cfg); err != nil {
		t.Fatalf("local storage with inert S3 defaults rejected: %v", err)
	}
}

func TestValidateDeploymentSafetyRequiresS3EndpointInS3Mode(t *testing.T) {
	cfg := config{
		SpotDomain: "spot.localhost",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "SPOT_S3_ENDPOINT") {
		t.Fatalf("missing S3 endpoint error = %v, want SPOT_S3_ENDPOINT rejection", err)
	}
}

func TestLoadConfigFromCLIFlags(t *testing.T) {
	t.Setenv("NETBIRD_API_URL", "")
	t.Setenv("NETBIRD_API_TOKEN", "")
	t.Setenv("TAILSCALE_API_URL", "")
	t.Setenv("TAILSCALE_API_TOKEN", "")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	cfg, err := loadConfigFrom([]string{
		"serve",
		"--storage", "local",
		"--auth", "single-user",
		"--domain", "spot.home.arpa",
		"--data-dir", "/var/lib/spot",
		"--listen", ":9090",
	})
	if err != nil {
		t.Fatalf("load config from flags: %v", err)
	}
	if cfg.StorageMode != storageModeLocal || cfg.AuthMode != authModeSingleUser {
		t.Fatalf("mode/auth = %q/%q, want local/single-user", cfg.StorageMode, cfg.AuthMode)
	}
	if cfg.SpotDomain != "spot.home.arpa" || cfg.DataDir != "/var/lib/spot" || cfg.SQLitePath != "/var/lib/spot/spot.db" || cfg.Port != ":9090" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadConfigFromCLIFlagsRejectsUnexpectedArgs(t *testing.T) {
	t.Setenv("SPOT_DOMAIN", "spot.localhost")
	t.Setenv("SPOT_STORAGE_MODE", "local")
	if _, err := loadConfigFrom([]string{"extra"}); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("unexpected arg error = %v, want rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsUnknownStorageMode(t *testing.T) {
	cfg := config{
		SpotDomain:  "spot.localhost",
		StorageMode: "tiny",
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "SPOT_STORAGE_MODE") {
		t.Fatalf("unknown storage mode error = %v, want SPOT_STORAGE_MODE rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsUnknownAuthMode(t *testing.T) {
	cfg := config{
		SpotDomain:  "spot.localhost",
		AuthMode:    "none",
		StorageMode: storageModeLocal,
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "SPOT_AUTH_MODE") {
		t.Fatalf("unknown auth mode error = %v, want SPOT_AUTH_MODE rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsSingleUserWithMeshProvider(t *testing.T) {
	cfg := config{
		SpotDomain:      "spot.home.arpa",
		StorageMode:     storageModeLocal,
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
		StorageMode: storageModeLocal,
		AuthMode:    authModeSingleUser,
	}
	if err := validateDeploymentSafety(cfg); err == nil || !strings.Contains(err.Error(), "SPOT_SINGLE_USER_EMAIL") {
		t.Fatalf("single-user missing email error = %v, want rejection", err)
	}
}

func TestValidateDeploymentSafetyRejectsBothProviders(t *testing.T) {
	cfg := config{
		SpotDomain:        "spot.example.com",
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
