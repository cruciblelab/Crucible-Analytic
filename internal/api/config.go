package api

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
)

// Config is the analytics API's own TOML config, deliberately separate
// from the collector's: the two run as separate processes, on different
// ports, and - importantly - should use different database credentials.
// The collector needs to write; this only ever reads, so its DSN should
// name a read-only Postgres role (see the README).
type Config struct {
	ListenAddr string `toml:"listen_addr"`
	// TimescaleDSN should point at a read-only role. Nothing here can
	// enforce that (the database does), but every query this process
	// issues is a SELECT.
	TimescaleDSN string `toml:"timescale_dsn"`
	Tokens       []struct {
		Name   string   `toml:"name"`
		SHA256 string   `toml:"sha256"`
		Sites  []string `toml:"sites"`
	} `toml:"tokens"`
	// BotDataPath points at the known-bot fingerprint file, so the API
	// can label a JA4 in its responses. Optional: without it the
	// fingerprints are still reported, just unlabelled.
	//
	// The same file the collector reads. This project redistributes no
	// copy of that dataset - see internal/botdata.
	BotDataPath string `toml:"bot_data_path"`

	Logging logging.Config `toml:"logging"`
}

// LoadConfig reads and validates the API config at path.
func LoadConfig(path string) (Config, error) {
	cfg := Config{ListenAddr: "127.0.0.1:8082"} // loopback default: exposing this publicly should be a deliberate edit
	// 8082 and not 8080: the collector proxies to the customer's own site
	// on 8080, and two defaults on one port meant an untouched pair of
	// example configs pointed the traffic proxy at this API.
	if _, err := os.Stat(path); err != nil {
		return Config{}, fmt.Errorf("api: config file %s: %w", path, err)
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("api: parse %s: %w", path, err)
	}
	if cfg.TimescaleDSN == "" {
		return Config{}, fmt.Errorf("api: timescale_dsn is required (read-only postgres connection string)")
	}
	if len(cfg.Tokens) == 0 {
		return Config{}, fmt.Errorf("api: at least one [[tokens]] entry is required")
	}
	return cfg, nil
}

// TokenList converts the decoded config tokens into the form
// NewAuthenticator takes.
func (c Config) TokenList() []Token {
	tokens := make([]Token, 0, len(c.Tokens))
	for _, t := range c.Tokens {
		tokens = append(tokens, Token{Name: t.Name, SHA256: t.SHA256, Sites: t.Sites})
	}
	return tokens
}
