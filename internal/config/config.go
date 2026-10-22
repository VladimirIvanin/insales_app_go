package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	AppID         string
	AppSecret     string
	BaseURL       string
	AllowedShops  []string
	DBPath        string
	BindHost      string
	Port          int
	EncryptionKey string
	LogLevel      string
}

func Load() (*Config, error) {
	appID := os.Getenv("INSALES_APP_ID")
	appSecret := os.Getenv("INSALES_APP_SECRET")
	baseURL := os.Getenv("APP_BASE_URL")
	encKey := os.Getenv("ENCRYPTION_KEY")

	if appID == "" {
		return nil, errors.New("INSALES_APP_ID is required")
	}
	if appSecret == "" {
		return nil, errors.New("INSALES_APP_SECRET is required")
	}
	if baseURL == "" {
		return nil, errors.New("APP_BASE_URL is required")
	}
	if encKey == "" {
		return nil, errors.New("ENCRYPTION_KEY is required (32 bytes hex for AES-256)")
	}
	if len(encKey) != 64 {
		return nil, fmt.Errorf("ENCRYPTION_KEY must be 32 bytes hex (got %d hex chars)", len(encKey))
	}

	allowedRaw := os.Getenv("ALLOWED_SHOPS")
	var allowed []string
	if allowedRaw != "" {
		for _, s := range strings.Split(allowedRaw, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				allowed = append(allowed, s)
			}
		}
	}

	dbPath := os.Getenv("APP_DB")
	if dbPath == "" {
		dbPath = "runtime/app.sqlite3"
	}

	bindHost := os.Getenv("BIND_HOST")
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}

	portStr := os.Getenv("PORT")
	port := 8765
	if portStr != "" {
		var err error
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid PORT: %w", err)
		}
	}

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	return &Config{
		AppID:         appID,
		AppSecret:     appSecret,
		BaseURL:       strings.TrimRight(baseURL, "/"),
		AllowedShops:  allowed,
		DBPath:        dbPath,
		BindHost:      bindHost,
		Port:          port,
		EncryptionKey: encKey,
		LogLevel:      logLevel,
	}, nil
}
