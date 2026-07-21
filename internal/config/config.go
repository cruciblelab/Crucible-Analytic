// Package config loads collector configuration from environment
// variables, with sane defaults for everything except the two values that
// have no safe default: where to send traffic and where to persist it.
package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds everything main.go needs to wire up the collector.
type Config struct {
	// ListenAddr is where the proxy accepts client connections.
	ListenAddr string
	// BackendAddr is the user's real site (host:port) traffic is proxied
	// to unmodified.
	BackendAddr string
	// DatabaseURL is a Postgres connection string for the TimescaleDB
	// instance flushes are written to.
	DatabaseURL string

	// FlushInterval is how often RateStore state is summarized and
	// written to TimescaleDB.
	FlushInterval time.Duration
	// WindowSize is the sliding-window width used for rate estimation.
	WindowSize time.Duration
	// IdleTTL is how long an IP can go without a request before its
	// state is dropped from memory.
	IdleTTL time.Duration
	// CleanupInterval is how often the idle-TTL sweep runs.
	CleanupInterval time.Duration
	// HandshakeTimeout bounds how long the proxy waits to see a complete
	// TLS ClientHello before giving up on fingerprinting and proxying
	// unfingerprinted.
	HandshakeTimeout time.Duration
	// DialTimeout bounds connecting to the backend.
	DialTimeout time.Duration
}

// Load reads configuration from environment variables and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:  getEnv("LISTEN_ADDR", ":8443"),
		BackendAddr: os.Getenv("BACKEND_ADDR"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
	}

	durations := []struct {
		key string
		dst *time.Duration
		def time.Duration
	}{
		{"FLUSH_INTERVAL", &cfg.FlushInterval, 10 * time.Second},
		{"WINDOW_SIZE", &cfg.WindowSize, 60 * time.Second},
		{"IDLE_TTL", &cfg.IdleTTL, 5 * time.Minute},
		{"CLEANUP_INTERVAL", &cfg.CleanupInterval, time.Minute},
		{"HANDSHAKE_TIMEOUT", &cfg.HandshakeTimeout, 5 * time.Second},
		{"DIAL_TIMEOUT", &cfg.DialTimeout, 10 * time.Second},
	}
	for _, d := range durations {
		v, err := getDurationEnv(d.key, d.def)
		if err != nil {
			return nil, err
		}
		*d.dst = v
	}

	if cfg.BackendAddr == "" {
		return nil, fmt.Errorf("config: BACKEND_ADDR is required (host:port of the site to proxy to)")
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required (postgres connection string for TimescaleDB)")
	}

	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getDurationEnv(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: invalid %s=%q: %w", key, v, err)
	}
	return d, nil
}
