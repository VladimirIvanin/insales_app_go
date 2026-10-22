// Command server is the entry point for the InSales partner application.
package main

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ivaninvladimir/insales_app_go/internal/config"
	"github.com/ivaninvladimir/insales_app_go/internal/httpapp"
	"github.com/ivaninvladimir/insales_app_go/internal/lifecycle"
	"github.com/ivaninvladimir/insales_app_go/internal/storage"
)

func main() {
	// Set up structured logging
	logLevel := &slog.LevelVar{}
	opts := &slog.HandlerOptions{Level: logLevel}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// Parse log level
	switch cfg.LogLevel {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "info":
		logLevel.Set(slog.LevelInfo)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	}

	// Decode encryption key
	key, err := hex.DecodeString(cfg.EncryptionKey)
	if err != nil {
		slog.Error("invalid ENCRYPTION_KEY format", "error", err)
		os.Exit(1)
	}

	// Create database directory if needed
	dbDir := cfg.DBPath
	if idx := strings.LastIndex(dbDir, "/"); idx >= 0 {
		dir := dbDir[:idx]
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error("cannot create db directory", "dir", dir, "error", err)
			os.Exit(1)
		}
	}

	// Open storage
	store, err := storage.New(cfg.DBPath, key)
	if err != nil {
		slog.Error("cannot open database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	// Create lifecycle service
	lc := lifecycle.NewService(store, cfg.AppID, cfg.AppSecret)

	// Create HTTP application
	app, err := httpapp.New(cfg, lc, store)
	if err != nil {
		slog.Error("cannot create application", "error", err)
		os.Exit(1)
	}

	// Start server in background
	go func() {
		if err := app.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	slog.Info("application started", "bind", cfg.BindHost, "port", cfg.Port)

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	slog.Info("shutting down", "signal", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := app.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}

	slog.Info("shutdown complete")
}
