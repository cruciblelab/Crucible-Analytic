// Package config loads collector configuration from a TOML file, with
// sane defaults for everything except the handful of values that have no
// safe default (where to send traffic, where to persist it, and - in full
// mode - the TLS certificate/key to terminate with).
package config

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

// Mode selects which proxy implementation main.go wires up.
type Mode string

const (
	// ModePassthrough is the default: a content-blind TCP/TLS proxy that
	// never terminates TLS (internal/proxy). Unaffected by this package's
	// full-mode-related fields.
	ModePassthrough Mode = "passthrough"
	// ModeFull terminates TLS and reverse-proxies HTTP (internal/fullproxy),
	// trading a larger trust boundary (it needs the backend's real
	// certificate/key) for real per-request visibility.
	ModeFull Mode = "full"
)

// Config holds everything main.go needs to wire up the collector, decoded
// from a TOML file. Durations are stored as plain seconds in the file
// (simplest to write by hand) and exposed as time.Duration via the
// accessor methods below.
type Config struct {
	Mode    Mode          `toml:"mode"`
	Network NetworkConfig `toml:"network"`
	TLS     TLSConfig     `toml:"tls"`
	Cache   CacheConfig   `toml:"cache"`
	Storage StorageConfig `toml:"storage"`
}

// NetworkConfig covers where the collector listens and what it proxies to.
// BackendAddr is a plain host:port in both modes: passthrough dials it
// directly over TCP, full mode treats it as a plaintext HTTP backend (the
// standard "TLS terminates at the edge, internal traffic is HTTP" setup -
// an HTTPS backend isn't supported yet, since that's a meaningfully
// different, not-yet-requested case, not an oversight).
type NetworkConfig struct {
	ListenAddr              string `toml:"listen_addr"`
	BackendAddr             string `toml:"backend_addr"`
	DialTimeoutSeconds      int    `toml:"dial_timeout_seconds"`
	HandshakeTimeoutSeconds int    `toml:"handshake_timeout_seconds"`
}

// DialTimeout bounds connecting to BackendAddr, in both modes.
func (n NetworkConfig) DialTimeout() time.Duration {
	return time.Duration(n.DialTimeoutSeconds) * time.Second
}

// HandshakeTimeout bounds how long passthrough mode waits to see a
// complete ClientHello before giving up on fingerprinting. Unused in full
// mode, where crypto/tls owns handshake timing.
func (n NetworkConfig) HandshakeTimeout() time.Duration {
	return time.Duration(n.HandshakeTimeoutSeconds) * time.Second
}

// TLSConfig is only consulted in full mode, to terminate TLS with the
// backend's real certificate/key.
type TLSConfig struct {
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

// CacheConfig tunes the in-memory RateStore's sliding window and eviction.
type CacheConfig struct {
	WindowSizeSeconds      int `toml:"window_size_seconds"`
	TTLSeconds             int `toml:"ttl_seconds"`
	CleanupIntervalSeconds int `toml:"cleanup_interval_seconds"`
}

// WindowSize is the sliding-window width used for rate estimation.
func (c CacheConfig) WindowSize() time.Duration {
	return time.Duration(c.WindowSizeSeconds) * time.Second
}

// TTL is how long an IP can go without a request before its state is
// dropped from memory.
func (c CacheConfig) TTL() time.Duration {
	return time.Duration(c.TTLSeconds) * time.Second
}

// CleanupInterval is how often the idle-TTL sweep runs.
func (c CacheConfig) CleanupInterval() time.Duration {
	return time.Duration(c.CleanupIntervalSeconds) * time.Second
}

// StorageConfig configures the periodic flush to TimescaleDB.
type StorageConfig struct {
	TimescaleDSN         string `toml:"timescale_dsn"`
	FlushIntervalSeconds int    `toml:"flush_interval_seconds"`
}

// FlushInterval is how often RateStore state is summarized and written to
// TimescaleDB.
func (s StorageConfig) FlushInterval() time.Duration {
	return time.Duration(s.FlushIntervalSeconds) * time.Second
}

func defaults() Config {
	return Config{
		Mode: ModePassthrough,
		Network: NetworkConfig{
			ListenAddr:              ":8443",
			DialTimeoutSeconds:      10,
			HandshakeTimeoutSeconds: 5,
		},
		Cache: CacheConfig{
			WindowSizeSeconds:      60,
			TTLSeconds:             300,
			CleanupIntervalSeconds: 60,
		},
		Storage: StorageConfig{
			FlushIntervalSeconds: 10,
		},
	}
}

// Load reads and validates configuration from the TOML file at path.
// Fields absent from the file keep their defaults - see defaults() - so a
// minimal file only needs to set what actually differs.
func Load(path string) (*Config, error) {
	cfg := defaults()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	switch c.Mode {
	case "":
		c.Mode = ModePassthrough
	case ModePassthrough, ModeFull:
	default:
		return fmt.Errorf("config: invalid mode %q (want %q or %q)", c.Mode, ModePassthrough, ModeFull)
	}

	if c.Network.BackendAddr == "" {
		return fmt.Errorf("config: network.backend_addr is required (host:port of the site to proxy to)")
	}
	if c.Storage.TimescaleDSN == "" {
		return fmt.Errorf("config: storage.timescale_dsn is required (postgres connection string for TimescaleDB)")
	}
	if c.Mode == ModeFull && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return fmt.Errorf("config: tls.cert_file and tls.key_file are required when mode = %q", ModeFull)
	}

	return nil
}
