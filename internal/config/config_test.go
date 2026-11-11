package config

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestFromEnvDefaults(t *testing.T) {
	for _, k := range []string{"LOG_LEVEL", "SYNC_HTTP_ADDR", "SYNC_POLL_INTERVAL", "SYNC_STATE"} {
		t.Setenv(k, "")
	}
	cfg := FromEnv()
	if cfg.LogLevel != "info" || cfg.HTTPAddr != ":8080" || cfg.PollInterval != time.Minute || cfg.StateBackend != "kv" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SYNC_POLL_INTERVAL", "90")
	t.Setenv("SYNC_RECONCILE_INTERVAL", "5m")
	cfg := FromEnv()
	if cfg.LogLevel != "debug" || cfg.PollInterval != 90*time.Second || cfg.ReconcileInterval != 5*time.Minute {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestServiceKey(t *testing.T) {
	c := Config{JikuKeyB64: base64.StdEncoding.EncodeToString([]byte(`{"k":1}`))}
	if b, err := c.ServiceKey(); err != nil || string(b) != `{"k":1}` {
		t.Fatalf("b64 key: %q %v", b, err)
	}
	if _, err := (Config{}).ServiceKey(); err == nil {
		t.Fatal("a missing key must fail")
	}
	if err := (Config{}).ValidateJiku(); err == nil {
		t.Fatal("empty bus config must fail validation")
	}
}
