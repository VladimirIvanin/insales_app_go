package config

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ivaninvladimir/insales_app_go/internal/insales"
)

// Config is the process configuration.
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

// Load reads .env without overriding existing variables, then reads the environment.
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, err
	}

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
	if _, err := hex.DecodeString(encKey); err != nil {
		return nil, fmt.Errorf("ENCRYPTION_KEY must be hex: %w", err)
	}

	allowed, err := normalizeAllowlist(os.Getenv("ALLOWED_SHOPS"))
	if err != nil {
		return nil, err
	}

	dbPath := os.Getenv("APP_DB")
	if dbPath == "" {
		dbPath = "runtime/app.sqlite3"
	}

	bindHost := os.Getenv("BIND_HOST")
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}

	port := 8765
	if portStr := os.Getenv("PORT"); portStr != "" {
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

func normalizeAllowlist(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("ALLOWED_SHOPS is required")
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		host, err := insales.CanonicalShop(part)
		if err != nil {
			return nil, fmt.Errorf("ALLOWED_SHOPS: %w", err)
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	if len(out) == 0 {
		return nil, errors.New("ALLOWED_SHOPS is required")
	}
	return out, nil
}

// loadDotEnv sets variables from path when they are not already in the environment.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open .env: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}
