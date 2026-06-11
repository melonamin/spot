// quick-api is the shared backend for all Quick sites. It provides the
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
	QuickDomain     string
	SitesDir        string
	NetbirdAPIURL   string
	NetbirdAPIToken string
}

func loadConfig() (config, error) {
	cfg := config{
		Port:            envOr("PORT", "8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		QuickDomain:     os.Getenv("QUICK_DOMAIN"),
		SitesDir:        envOr("QUICK_SITES_DIR", "/srv/sites"),
		NetbirdAPIURL:   os.Getenv("NETBIRD_API_URL"),
		NetbirdAPIToken: os.Getenv("NETBIRD_API_TOKEN"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.QuickDomain == "" {
		return cfg, errors.New("QUICK_DOMAIN is required (e.g. quick.localhost)")
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

	srv := &Server{
		store:       &DocStore{db: db},
		resolver:    resolver,
		policies:    NewPolicyStore(cfg.SitesDir, 5*time.Second),
		quickDomain: cfg.QuickDomain,
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

	log.Printf("quick-api listening on :%s (domain %s)", cfg.Port, cfg.QuickDomain)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}
