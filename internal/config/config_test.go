package config

import "testing"

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("SYNC_HTTP_ADDR", "")
	cfg := FromEnv()
	if cfg.LogLevel != "info" || cfg.HTTPAddr != ":8080" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SYNC_HTTP_ADDR", ":9090")
	cfg := FromEnv()
	if cfg.LogLevel != "debug" || cfg.HTTPAddr != ":9090" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}
