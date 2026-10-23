package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/config"
	"github.com/ivaninvladimir/insales_app_go/internal/httpapp"
	"github.com/ivaninvladimir/insales_app_go/internal/insales"
	"github.com/ivaninvladimir/insales_app_go/internal/lifecycle"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "serve":
		return cmdServe()
	case "doctor":
		return cmdDoctor()
	case "db":
		sub := ""
		if len(args) > 1 {
			sub = args[1]
		}
		switch sub {
		case "upgrade":
			return cmdUpgrade()
		case "status":
			return cmdStatus()
		default:
			fmt.Fprintln(os.Stderr, "usage: server db (upgrade|status)")
			return 1
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nusage: server [serve|doctor|db upgrade|db status]\n", args[0])
		return 1
	}
}

func cmdServe() int {
	logLevel := &slog.LevelVar{}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		return 1
	}
	switch cfg.LogLevel {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}

	store, err := openUpgraded(cfg)
	if err != nil {
		slog.Error("cannot open database", "error", err)
		return 1
	}
	defer store.Close()

	lc := lifecycle.NewService(store, cfg.AppID, cfg.AppSecret)
	app, err := httpapp.New(cfg, lc, store)
	if err != nil {
		slog.Error("cannot create application", "error", err)
		return 1
	}

	errCh := make(chan error, 1)
	go func() {
		if err := app.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	slog.Info("application started", "bind", cfg.BindHost, "port", cfg.Port)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-quit:
		slog.Info("shutting down", "signal", sig)
	case err := <-errCh:
		slog.Error("server error", "error", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
		return 1
	}
	slog.Info("shutdown complete")
	return 0
}

func cmdUpgrade() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		return 1
	}
	store, err := openUpgraded(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "upgrade error: %v\n", err)
		return 1
	}
	defer store.Close()
	fmt.Printf("database upgraded: %s\n", cfg.DBPath)
	return 0
}

func cmdStatus() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		return 1
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		fmt.Printf("database file is missing: %s\n", cfg.DBPath)
		for _, m := range storage.Migrations {
			fmt.Printf("  [ ] v%d: %s\n", m.Version, m.Name)
		}
		return 1
	}
	store, err := openStore(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status error: %v\n", err)
		return 1
	}
	defer store.Close()
	applied, err := store.AppliedVersions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "status error: %v\n", err)
		return 1
	}
	have := map[int]bool{}
	current := 0
	for _, v := range applied {
		have[v] = true
		if v > current {
			current = v
		}
	}
	fmt.Printf("current schema version: %d\n", current)
	for _, m := range storage.Migrations {
		mark := " "
		if have[m.Version] {
			mark = "x"
		}
		fmt.Printf("  [%s] v%d: %s\n", mark, m.Version, m.Name)
	}
	return 0
}

func cmdDoctor() int {
	fmt.Println("doctor")
	issues := 0
	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("  x configuration: %v\n", err)
		return 1
	}
	fmt.Printf("  + INSALES_APP_ID = %s\n", cfg.AppID)
	fmt.Printf("  + INSALES_APP_SECRET = ***\n")
	fmt.Printf("  + APP_BASE_URL = %s\n", cfg.BaseURL)
	fmt.Printf("  + ALLOWED_SHOPS = %s\n", strings.Join(cfg.AllowedShops, ","))
	fmt.Printf("  + ENCRYPTION_KEY = ***\n")

	if _, err := os.Stat(cfg.DBPath); err != nil {
		fmt.Printf("  x database file is missing: %s\n", cfg.DBPath)
		return 1
	}
	store, err := openStore(cfg)
	if err != nil {
		fmt.Printf("  x database: %v\n", err)
		return 1
	}
	defer store.Close()
	applied, err := store.AppliedVersions()
	if err != nil {
		fmt.Printf("  x schema: %v\n", err)
		return 1
	}
	have := map[int]bool{}
	for _, v := range applied {
		have[v] = true
	}
	missing := false
	for _, m := range storage.Migrations {
		if !have[m.Version] {
			missing = true
			fmt.Printf("  x migration v%d is not applied: %s\n", m.Version, m.Name)
		}
	}
	if missing {
		issues++
	} else {
		fmt.Printf("  + schema version %d\n", storage.Migrations[len(storage.Migrations)-1].Version)
	}

	stores, err := store.ListStores()
	if err != nil {
		fmt.Printf("  x stores: %v\n", err)
		return 1
	}
	if len(stores) == 0 {
		fmt.Println("  - no active connections")
	}
	for _, st := range stores {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client := insales.NewClient("https://"+st.Shop, cfg.AppID, st.Password)
		account, callErr := client.GetAccount(ctx)
		cancel()
		if callErr != nil {
			fmt.Printf("  x shop %s: %v\n", st.Shop, callErr)
			issues++
			continue
		}
		fmt.Printf("  + shop %s: %s (%d)", st.Shop, account.Title, account.ID)
		if usage := client.UsageLimit(); usage != "" {
			fmt.Printf(" usage=%s", usage)
		}
		fmt.Println()
	}
	if issues > 0 {
		fmt.Printf("problems: %d\n", issues)
		return 1
	}
	fmt.Println("ok")
	return 0
}

func openUpgraded(cfg *config.Config) (*storage.DB, error) {
	if err := ensureDBDir(cfg.DBPath); err != nil {
		return nil, err
	}
	store, err := openStore(cfg)
	if err != nil {
		return nil, err
	}
	if err := store.Upgrade(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func openStore(cfg *config.Config) (*storage.DB, error) {
	key, err := hex.DecodeString(cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("invalid ENCRYPTION_KEY format: %w", err)
	}
	return storage.Open(cfg.DBPath, key)
}

func ensureDBDir(dbPath string) error {
	dir := filepath.Dir(dbPath)
	if dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("cannot create db directory: %w", err)
	}
	return nil
}
