// Package config reads the connector's configuration from the environment. See .env.dist.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the process configuration.
type Config struct {
	// LogLevel is debug, info, warn or error.
	LogLevel string
	// HTTPAddr serves /healthz, /readyz and the GitHub webhook receiver.
	HTTPAddr string

	// Jiku bus (the Jiku connector SDK names).
	JikuServers   string
	JikuInstance  string
	JikuCreds     string
	JikuIssuer    string
	JikuProjectID string // Zitadel project id: it puts the roles in the token
	JikuKeyFile   string
	JikuKeyB64    string
	JikuTimeout   time.Duration

	// GitHub.
	// GitHubToken is the default credential: the one of projects that name none.
	GitHubToken string
	// The connector's GitHub App, for `app` credentials.
	GitHubAppID      string
	GitHubAppKeyFile string
	GitHubAppKeyB64  string
	GitHubAPIURL     string
	WebhookSecret    string

	// Sync.
	MappingFile       string
	PollInterval      time.Duration
	ReconcileInterval time.Duration
	WriteInterval     time.Duration
	ConfigReload      time.Duration
	EventsDurable     string
	// VerifyAfterReconcile logs a verification of every project after each reconciliation.
	VerifyAfterReconcile bool
	// StateBackend is "kv" (Jiku's KV, the default) or "memory" (tests and trials: nothing
	// survives a restart).
	StateBackend string
}

// FromEnv reads the configuration, with defaults for anything unset.
func FromEnv() Config {
	return Config{
		LogLevel: getenv("LOG_LEVEL", "info"),
		HTTPAddr: getenv("SYNC_HTTP_ADDR", ":8080"),

		JikuServers:   getenv("JIKU_SERVERS", "nats://localhost:4222"),
		JikuInstance:  getenv("JIKU_INSTANCE", "dev"),
		JikuCreds:     os.Getenv("JIKU_CREDS"),
		JikuIssuer:    getenv("JIKU_ISSUER", "https://id.grava.io"),
		JikuProjectID: os.Getenv("JIKU_PROJECT_ID"),
		JikuKeyFile:   os.Getenv("JIKU_KEY_FILE"),
		JikuKeyB64:    os.Getenv("JIKU_KEY_B64"),
		JikuTimeout:   duration("JIKU_TIMEOUT", 15*time.Second),

		GitHubToken:      os.Getenv("GITHUB_TOKEN"),
		GitHubAppID:      os.Getenv("GITHUB_APP_ID"),
		GitHubAppKeyFile: os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"),
		GitHubAppKeyB64:  os.Getenv("GITHUB_APP_PRIVATE_KEY_B64"),
		GitHubAPIURL:     getenv("GITHUB_API_URL", "https://api.github.com"),
		WebhookSecret:    os.Getenv("GITHUB_WEBHOOK_SECRET"),

		MappingFile:       getenv("SYNC_CONFIG", "config.yaml"),
		PollInterval:      duration("SYNC_POLL_INTERVAL", time.Minute),
		ReconcileInterval: duration("SYNC_RECONCILE_INTERVAL", 15*time.Minute),
		WriteInterval:     duration("SYNC_WRITE_INTERVAL", time.Second),
		ConfigReload:      duration("SYNC_CONFIG_RELOAD", 30*time.Second),
		EventsDurable:     os.Getenv("SYNC_EVENTS_DURABLE"),

		VerifyAfterReconcile: getenv("SYNC_VERIFY", "true") != "false",
		StateBackend:         getenv("SYNC_STATE", "kv"),
	}
}

// ServiceKey returns the machine user's JSON key, from JIKU_KEY_B64 or JIKU_KEY_FILE.
func (c Config) ServiceKey() ([]byte, error) {
	if c.JikuKeyB64 != "" {
		b, err := base64.StdEncoding.DecodeString(c.JikuKeyB64)
		if err != nil {
			return nil, fmt.Errorf("JIKU_KEY_B64 is not base64: %w", err)
		}
		return b, nil
	}
	if c.JikuKeyFile != "" {
		return os.ReadFile(c.JikuKeyFile)
	}
	return nil, errors.New("the machine user key is missing: set JIKU_KEY_FILE or JIKU_KEY_B64")
}

// ValidateJiku checks what connecting to the bus needs.
func (c Config) ValidateJiku() error {
	var errs []error
	if c.JikuCreds == "" {
		errs = append(errs, errors.New("JIKU_CREDS (sentinel creds) is required"))
	}
	if c.JikuProjectID == "" {
		errs = append(errs, errors.New("JIKU_PROJECT_ID (the Zitadel project that puts the roles in the token) is required"))
	}
	if c.JikuKeyFile == "" && c.JikuKeyB64 == "" {
		errs = append(errs, errors.New("JIKU_KEY_FILE or JIKU_KEY_B64 is required"))
	}
	return errors.Join(errs...)
}

// AppKey returns the GitHub App's private key (nil when no App is configured).
func (c Config) AppKey() ([]byte, error) {
	if c.GitHubAppKeyB64 != "" {
		b, err := base64.StdEncoding.DecodeString(c.GitHubAppKeyB64)
		if err != nil {
			return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_B64 is not base64: %w", err)
		}
		return b, nil
	}
	if c.GitHubAppKeyFile != "" {
		return os.ReadFile(c.GitHubAppKeyFile)
	}
	return nil, nil
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// duration reads a Go duration ("90s", "15m"); a bare number is seconds.
func duration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(s) * time.Second
	}
	return fallback
}
