// Package config reads the connector's configuration from the environment. See .env.dist.
package config

import "os"

// Config is the process configuration. It grows with each milestone of the proof of concept.
type Config struct {
	// LogLevel is debug, info, warn or error.
	LogLevel string
	// HTTPAddr serves /healthz and /readyz (and, in v1, the GitHub webhook receiver).
	HTTPAddr string
}

// FromEnv reads the configuration, with defaults for anything unset.
func FromEnv() Config {
	return Config{
		LogLevel: getenv("LOG_LEVEL", "info"),
		HTTPAddr: getenv("SYNC_HTTP_ADDR", ":8080"),
	}
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
