// spot-api is the shared backend for all Spot sites. It provides the
// document store, and resolves visitor identity from the NetBird mesh.
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schemaSQL string

type config struct {
	Port            string
	DatabaseURL     string
	SpotDomain      string
	SitesDir        string
	NetbirdAPIURL   string
	NetbirdAPIToken string
	S3Endpoint      string
	S3AccessKey     string
	S3SecretKey     string
	UploadsBucket   string
	AnthropicAPIKey string
}

func loadConfig() (config, error) {
	cfg := config{
		Port:            envOr("PORT", "8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		SpotDomain:      os.Getenv("SPOT_DOMAIN"),
		SitesDir:        envOr("SPOT_SITES_DIR", "/srv/sites"),
		NetbirdAPIURL:   os.Getenv("NETBIRD_API_URL"),
		NetbirdAPIToken: os.Getenv("NETBIRD_API_TOKEN"),
		S3Endpoint:      os.Getenv("SPOT_S3_ENDPOINT"),
		S3AccessKey:     os.Getenv("SPOT_S3_ACCESS_KEY"),
		S3SecretKey:     os.Getenv("SPOT_S3_SECRET_KEY"),
		UploadsBucket:   envOr("SPOT_UPLOADS_BUCKET", "spot-uploads"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.SpotDomain == "" {
		return cfg, errors.New("SPOT_DOMAIN is required (e.g. spot.localhost)")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func openDB(ctx context.Context, url string) (*sql.DB, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = db.PingContext(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("database not reachable after 30s: %w", err)
		}
		time.Sleep(time.Second)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return db, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	db, err := openDB(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	var resolver *NetbirdResolver
	if cfg.NetbirdAPIURL != "" && cfg.NetbirdAPIToken != "" {
		resolver = NewNetbirdResolver(cfg.NetbirdAPIURL, cfg.NetbirdAPIToken, 30*time.Second)
		log.Printf("identity: resolving via NetBird API at %s", cfg.NetbirdAPIURL)
	} else {
		log.Printf("identity: NETBIRD_API_URL/NETBIRD_API_TOKEN not set, /api/me will return 503")
	}

	store := &DocStore{db: db}
	hub := NewHub()
	listener := &Listener{dsn: cfg.DatabaseURL, store: store, hub: hub}
	go listener.Run(ctx)

	var files *FileStore
	if cfg.S3Endpoint != "" {
		files, err = NewFileStore(cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.UploadsBucket)
		if err != nil {
			log.Fatalf("file store: %v", err)
		}
		log.Printf("files: storing uploads in %s/%s", cfg.S3Endpoint, cfg.UploadsBucket)
	} else {
		log.Printf("files: SPOT_S3_ENDPOINT not set, /api/files will return 503")
	}

	var ai *AIProxy
	if cfg.AnthropicAPIKey != "" {
		ai = NewAIProxy(cfg.AnthropicAPIKey)
		log.Printf("ai: proxying to the Claude API")
	} else {
		log.Printf("ai: ANTHROPIC_API_KEY not set, /api/ai/chat will return 503")
	}

	srv := &Server{
		store:      store,
		resolver:   resolver,
		policies:   NewPolicyStore(cfg.SitesDir, 5*time.Second),
		hub:        hub,
		files:      files,
		ai:         ai,
		spotDomain: cfg.SpotDomain,
	}

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("spot-api listening on :%s (domain %s)", cfg.Port, cfg.SpotDomain)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}
