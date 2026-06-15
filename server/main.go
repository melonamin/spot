// spot-api is the shared backend for all Spot sites. It provides the
// document store, and resolves visitor identity from the configured mesh.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type config struct {
	Port                 string
	StorageMode          string
	DataDir              string
	SQLitePath           string
	SpotDomain           string
	SitesDir             string
	AuthMode             string
	NetbirdAPIURL        string
	NetbirdAPIToken      string
	TailscaleAPIURL      string
	TailscaleAPIToken    string
	TailscaleOAuthID     string
	TailscaleOAuthSecret string
	TailscaleTailnet     string
	S3Endpoint           string
	S3AccessKey          string
	S3SecretKey          string
	UploadsBucket        string
	SitesBucket          string
	AnthropicAPIKey      string
	AnthropicBaseURL     string
	AIModel              string
	AIAllowedModels      []string
	AIAccess             string
	TrustedProxies       string
	AdminAllow           []string
	DevIdentityEmail     string
	DevIdentityName      string
	DevIdentityGroups    []string
	SingleUserEmail      string
	SingleUserName       string
	SingleUserGroups     []string
}

const (
	authModeAuto       = "auto"
	authModeSingleUser = "single-user"

	storageModeS3    = "s3"
	storageModeLocal = "local"
)

func loadConfig() (config, error) {
	return loadConfigFrom(os.Args[1:])
}

func loadConfigFrom(args []string) (config, error) {
	cfg := config{
		Port:                 envOr("PORT", "8080"),
		StorageMode:          normalizeStorageMode(os.Getenv("SPOT_STORAGE_MODE")),
		DataDir:              envOr("SPOT_DATA_DIR", "./data"),
		SQLitePath:           os.Getenv("SPOT_SQLITE_PATH"),
		SpotDomain:           os.Getenv("SPOT_DOMAIN"),
		SitesDir:             os.Getenv("SPOT_SITES_DIR"),
		AuthMode:             normalizeAuthMode(os.Getenv("SPOT_AUTH_MODE")),
		NetbirdAPIURL:        os.Getenv("NETBIRD_API_URL"),
		NetbirdAPIToken:      os.Getenv("NETBIRD_API_TOKEN"),
		TailscaleAPIURL:      os.Getenv("TAILSCALE_API_URL"),
		TailscaleAPIToken:    os.Getenv("TAILSCALE_API_TOKEN"),
		TailscaleOAuthID:     os.Getenv("TAILSCALE_OAUTH_CLIENT_ID"),
		TailscaleOAuthSecret: os.Getenv("TAILSCALE_OAUTH_CLIENT_SECRET"),
		TailscaleTailnet:     envOr("TAILSCALE_TAILNET", "-"),
		S3Endpoint:           os.Getenv("SPOT_S3_ENDPOINT"),
		S3AccessKey:          os.Getenv("SPOT_S3_ACCESS_KEY"),
		S3SecretKey:          os.Getenv("SPOT_S3_SECRET_KEY"),
		UploadsBucket:        envOr("SPOT_UPLOADS_BUCKET", "spot-uploads"),
		SitesBucket:          envOr("SPOT_SITES_BUCKET", "spot-sites"),
		AnthropicAPIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicBaseURL:     os.Getenv("ANTHROPIC_BASE_URL"),
		AIModel:              os.Getenv("SPOT_AI_MODEL"),
		AIAllowedModels:      splitList(os.Getenv("SPOT_AI_ALLOWED_MODELS")),
		AIAccess:             envOr("SPOT_AI_ACCESS", aiAccessOwners),
		TrustedProxies:       envOr("SPOT_TRUSTED_PROXIES", "127.0.0.1/32,::1/128"),
		AdminAllow: append(
			splitList(os.Getenv("SPOT_ADMIN_EMAILS")),
			splitList(os.Getenv("SPOT_ADMIN_GROUPS"))...,
		),
		DevIdentityEmail:  os.Getenv("SPOT_DEV_IDENTITY_EMAIL"),
		DevIdentityName:   envOr("SPOT_DEV_IDENTITY_NAME", "Spot Dev"),
		DevIdentityGroups: splitList(os.Getenv("SPOT_DEV_IDENTITY_GROUPS")),
		SingleUserEmail:   envOr("SPOT_SINGLE_USER_EMAIL", "owner@spot.local"),
		SingleUserName:    envOr("SPOT_SINGLE_USER_NAME", "Spot Owner"),
		SingleUserGroups:  splitList(os.Getenv("SPOT_SINGLE_USER_GROUPS")),
	}
	if err := applyCLIFlags(&cfg, args); err != nil {
		return cfg, err
	}
	if cfg.SQLitePath == "" {
		cfg.SQLitePath = cfg.DataDir + "/spot.db"
	}
	if cfg.StorageMode == storageModeLocal {
		cfg.SitesDir = envOr("SPOT_SITES_DIR", cfg.DataDir+"/sites")
	}
	if cfg.SpotDomain == "" {
		return cfg, errors.New("SPOT_DOMAIN is required (e.g. spot.localhost)")
	}
	if cfg.AIAccess != aiAccessOwners && cfg.AIAccess != aiAccessVisitors {
		return cfg, fmt.Errorf("SPOT_AI_ACCESS must be %q or %q", aiAccessOwners, aiAccessVisitors)
	}
	if err := validateDeploymentSafety(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyCLIFlags(cfg *config, args []string) error {
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("spot-api", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.StorageMode, "storage", cfg.StorageMode, "storage mode: s3 or local")
	fs.StringVar(&cfg.AuthMode, "auth", cfg.AuthMode, "auth mode: auto or single-user")
	fs.StringVar(&cfg.SpotDomain, "domain", cfg.SpotDomain, "apex domain for Spot")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "data directory for SQLite and local storage")
	fs.StringVar(&cfg.SQLitePath, "sqlite", cfg.SQLitePath, "SQLite database path")
	fs.StringVar(&cfg.Port, "listen", cfg.Port, "listen port or address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg.StorageMode = normalizeStorageMode(cfg.StorageMode)
	cfg.AuthMode = normalizeAuthMode(cfg.AuthMode)
	return nil
}

func listenAddr(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ":8080"
	}
	if strings.Contains(listen, ":") {
		return listen
	}
	return ":" + listen
}

func validateDeploymentSafety(cfg config) error {
	mode := normalizeAuthMode(cfg.AuthMode)
	if mode != authModeAuto && mode != authModeSingleUser {
		return fmt.Errorf("SPOT_AUTH_MODE must be %q or %q", authModeAuto, authModeSingleUser)
	}
	storageMode := normalizeStorageMode(cfg.StorageMode)
	if storageMode != storageModeS3 && storageMode != storageModeLocal {
		return fmt.Errorf("SPOT_STORAGE_MODE must be %q or %q", storageModeS3, storageModeLocal)
	}
	if storageMode == storageModeS3 && strings.TrimSpace(cfg.S3Endpoint) == "" {
		return errors.New("SPOT_S3_ENDPOINT is required when SPOT_STORAGE_MODE=s3")
	}
	netbird := netbirdConfigured(cfg)
	tailscale := tailscaleConfigured(cfg)
	if netbird && tailscale {
		return errors.New("configure exactly one mesh identity provider: NETBIRD_* or TAILSCALE_*")
	}
	if cfg.TailscaleAPIToken != "" && tailscaleOAuthConfigured(cfg) {
		return errors.New("configure either TAILSCALE_API_TOKEN or TAILSCALE_OAUTH_CLIENT_ID/TAILSCALE_OAUTH_CLIENT_SECRET")
	}
	if tailscaleOAuthConfigured(cfg) && (cfg.TailscaleOAuthID == "" || cfg.TailscaleOAuthSecret == "") {
		return errors.New("Tailscale OAuth requires TAILSCALE_OAUTH_CLIENT_ID and TAILSCALE_OAUTH_CLIENT_SECRET")
	}
	if mode == authModeSingleUser {
		if netbird || tailscale {
			return errors.New("SPOT_AUTH_MODE=single-user cannot be combined with NETBIRD_* or TAILSCALE_*")
		}
		if strings.TrimSpace(cfg.SingleUserEmail) == "" {
			return errors.New("SPOT_SINGLE_USER_EMAIL is required when SPOT_AUTH_MODE=single-user")
		}
		return nil
	}
	shared := !localSpotDomain(cfg.SpotDomain) || netbird || tailscale
	if !shared {
		return nil
	}
	if cfg.DevIdentityEmail != "" && !localSpotDomain(cfg.SpotDomain) {
		return errors.New("SPOT_DEV_IDENTITY_EMAIL is only allowed for .localhost deployments")
	}
	if netbird && (cfg.NetbirdAPIURL == "" || cfg.NetbirdAPIToken == "") {
		return errors.New("NetBird deployments require NETBIRD_API_URL and NETBIRD_API_TOKEN")
	}
	if !netbird && !tailscale {
		return errors.New("shared deployments require NETBIRD_API_URL/NETBIRD_API_TOKEN, TAILSCALE_API_TOKEN, or TAILSCALE_OAUTH_CLIENT_ID/TAILSCALE_OAUTH_CLIENT_SECRET")
	}
	if storageMode == storageModeS3 && cfg.S3Endpoint != "" && (cfg.S3AccessKey == "rustfsadmin" || cfg.S3SecretKey == "rustfsadmin") {
		return errors.New("shared deployments must replace the default RustFS credentials")
	}
	return nil
}

func normalizeAuthMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return authModeAuto
	}
	return mode
}

func normalizeStorageMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return storageModeS3
	}
	if mode == "appliance" {
		return storageModeLocal
	}
	if mode == "server" {
		return storageModeS3
	}
	return mode
}

func netbirdConfigured(cfg config) bool {
	return cfg.NetbirdAPIURL != "" || cfg.NetbirdAPIToken != ""
}

func tailscaleConfigured(cfg config) bool {
	return cfg.TailscaleAPIToken != "" || tailscaleOAuthConfigured(cfg)
}

func tailscaleOAuthConfigured(cfg config) bool {
	return cfg.TailscaleOAuthID != "" || cfg.TailscaleOAuthSecret != ""
}

func localSpotDomain(domain string) bool {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	return domain == "localhost" || strings.HasSuffix(domain, ".localhost")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func newResolver(cfg config) (IdentityResolver, string) {
	if normalizeAuthMode(cfg.AuthMode) == authModeSingleUser {
		return NewStaticResolver(cfg.SingleUserEmail, cfg.SingleUserName, cfg.SingleUserGroups),
			fmt.Sprintf("using single-user identity %s", cfg.SingleUserEmail)
	}
	if cfg.NetbirdAPIURL != "" && cfg.NetbirdAPIToken != "" {
		return NewNetbirdResolver(cfg.NetbirdAPIURL, cfg.NetbirdAPIToken, 30*time.Second),
			fmt.Sprintf("resolving via NetBird API at %s", cfg.NetbirdAPIURL)
	}
	if cfg.TailscaleAPIToken != "" {
		return NewTailscaleResolver(cfg.TailscaleAPIURL, cfg.TailscaleAPIToken, cfg.TailscaleTailnet, 30*time.Second),
			"resolving via Tailscale API"
	}
	if cfg.TailscaleOAuthID != "" && cfg.TailscaleOAuthSecret != "" {
		return NewTailscaleOAuthResolver(cfg.TailscaleAPIURL, cfg.TailscaleOAuthID, cfg.TailscaleOAuthSecret, cfg.TailscaleTailnet, 30*time.Second),
			"resolving via Tailscale API with OAuth"
	}
	if cfg.DevIdentityEmail != "" {
		return NewStaticResolver(cfg.DevIdentityEmail, cfg.DevIdentityName, cfg.DevIdentityGroups),
			fmt.Sprintf("using explicit dev identity %s", cfg.DevIdentityEmail)
	}
	return nil, "NETBIRD_API_URL/NETBIRD_API_TOKEN, TAILSCALE_API_TOKEN, or TAILSCALE_OAUTH_CLIENT_ID/TAILSCALE_OAUTH_CLIENT_SECRET not set, /api/me will return 503"
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	var db *sql.DB
	db, err = openSQLiteDB(ctx, cfg.SQLitePath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	resolver, resolverLog := newResolver(cfg)
	log.Printf("identity: %s", resolverLog)

	hub := NewHub()
	store := &DocStore{db: db, hub: hub}

	var files FileStorage
	var sites SiteStorage
	if cfg.StorageMode == storageModeLocal {
		files, err = NewLocalFileStore(cfg.DataDir + "/uploads")
		if err != nil {
			log.Fatalf("file store: %v", err)
		}
		sites, err = NewLocalSiteStore(cfg.SitesDir)
		if err != nil {
			log.Fatalf("site store: %v", err)
		}
		log.Printf("files: storing uploads under %s/uploads", cfg.DataDir)
		log.Printf("deploys: storing sites under %s", cfg.SitesDir)
	} else {
		files, err = NewFileStore(cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.UploadsBucket)
		if err != nil {
			log.Fatalf("file store: %v", err)
		}
		log.Printf("files: storing uploads in %s/%s", cfg.S3Endpoint, cfg.UploadsBucket)
		sites, err = NewSiteStore(cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.SitesBucket)
		if err != nil {
			log.Fatalf("site store: %v", err)
		}
		log.Printf("deploys: storing sites in %s/%s", cfg.S3Endpoint, cfg.SitesBucket)
	}

	var ai *AIProxy
	if cfg.AnthropicAPIKey != "" {
		ai = NewAIProxyWithUpstream(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL, cfg.AIModel, cfg.AIAllowedModels)
		upstream := cfg.AnthropicBaseURL
		if upstream == "" {
			upstream = "the Claude API"
		}
		log.Printf("ai: proxying to %s (default model %s)", upstream, ai.model)
	} else {
		log.Printf("ai: ANTHROPIC_API_KEY not set, /api/ai/chat will return 503")
	}

	trustedProxies, err := NewTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		log.Fatalf("trusted proxies: %v", err)
	}
	var adminPolicy *AccessPolicy
	if len(cfg.AdminAllow) > 0 {
		adminPolicy = &AccessPolicy{Allow: cfg.AdminAllow}
	}

	registry := NewSiteRegistry(db, adminPolicy)
	var policies *PolicyStore
	if cfg.StorageMode == storageModeLocal {
		policies = NewPolicyStore(cfg.SitesDir, 5*time.Second)
	}
	srv := &Server{
		store:          store,
		resolver:       resolver,
		policies:       policies,
		hub:            hub,
		files:          files,
		sites:          sites,
		deployAuth:     registry,
		siteAdmin:      registry,
		siteManager:    registry,
		ai:             ai,
		aiAccess:       cfg.AIAccess,
		spotDomain:     cfg.SpotDomain,
		trustedProxies: trustedProxies,
		serveStatic:    true,
	}

	httpSrv := &http.Server{
		Addr:              listenAddr(cfg.Port),
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("spot-api listening on %s (domain %s, storage %s)", httpSrv.Addr, cfg.SpotDomain, cfg.StorageMode)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}
