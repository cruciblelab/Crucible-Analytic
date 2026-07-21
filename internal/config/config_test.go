package config

import (
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("BACKEND_ADDR", "127.0.0.1:8080")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("FLUSH_INTERVAL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ListenAddr != ":8443" {
		t.Errorf("ListenAddr = %q, want :8443", cfg.ListenAddr)
	}
	if cfg.FlushInterval != 10*time.Second {
		t.Errorf("FlushInterval = %v, want 10s", cfg.FlushInterval)
	}
	if cfg.WindowSize != 60*time.Second {
		t.Errorf("WindowSize = %v, want 60s", cfg.WindowSize)
	}
	if cfg.IdleTTL != 5*time.Minute {
		t.Errorf("IdleTTL = %v, want 5m", cfg.IdleTTL)
	}
	if cfg.CleanupInterval != time.Minute {
		t.Errorf("CleanupInterval = %v, want 1m", cfg.CleanupInterval)
	}
	if cfg.HandshakeTimeout != 5*time.Second {
		t.Errorf("HandshakeTimeout = %v, want 5s", cfg.HandshakeTimeout)
	}
	if cfg.DialTimeout != 10*time.Second {
		t.Errorf("DialTimeout = %v, want 10s", cfg.DialTimeout)
	}
}

func TestLoad_MissingBackendAddr(t *testing.T) {
	t.Setenv("BACKEND_ADDR", "")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	if _, err := Load(); err == nil {
		t.Error("Load() error = nil, want error for missing BACKEND_ADDR")
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	t.Setenv("BACKEND_ADDR", "127.0.0.1:8080")
	t.Setenv("DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Error("Load() error = nil, want error for missing DATABASE_URL")
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	t.Setenv("BACKEND_ADDR", "127.0.0.1:8080")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("FLUSH_INTERVAL", "not-a-duration")

	if _, err := Load(); err == nil {
		t.Error("Load() error = nil, want error for invalid FLUSH_INTERVAL")
	}
}

func TestLoad_CustomValues(t *testing.T) {
	t.Setenv("BACKEND_ADDR", "127.0.0.1:8080")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("LISTEN_ADDR", ":9443")
	t.Setenv("FLUSH_INTERVAL", "5s")
	t.Setenv("WINDOW_SIZE", "30s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ListenAddr != ":9443" {
		t.Errorf("ListenAddr = %q, want :9443", cfg.ListenAddr)
	}
	if cfg.FlushInterval != 5*time.Second {
		t.Errorf("FlushInterval = %v, want 5s", cfg.FlushInterval)
	}
	if cfg.WindowSize != 30*time.Second {
		t.Errorf("WindowSize = %v, want 30s", cfg.WindowSize)
	}
}
