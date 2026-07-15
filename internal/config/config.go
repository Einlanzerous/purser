// Package config loads Purser's configuration from environment variables,
// following the construct-server house convention: 12-factor env vars, a
// <SERVICE>_-prefixed namespace, and a DATABASE_URL fallback. There are no
// config files.
package config

import (
	"log"
	"os"
	"strconv"
)

// Config is Purser's fully-resolved configuration.
type Config struct {
	Addr        string // HTTP listen address (PURSER_ADDR)
	DatabaseURL string // Postgres DSN (PURSER_DATABASE_URL | DATABASE_URL)
	APIToken    string // bearer token protecting the HTTP API (PURSER_API_TOKEN)

	Switchyard SwitchyardConfig
	Cloudflare CloudflareConfig
	Lyceum     LyceumConfig
	SMTP       SMTPConfig
}

// LyceumConfig configures the Lyceum connector.
type LyceumConfig struct {
	BaseURL    string // PURSER_LYCEUM_BASE_URL (internal API base)
	OwnerToken string // PURSER_LYCEUM_OWNER_TOKEN (owner session token, lyc_…)
	AppURL     string // PURSER_LYCEUM_URL (public app URL for the block; optional)
}

// Configured reports whether the Lyceum connector can run.
func (c LyceumConfig) Configured() bool { return c.BaseURL != "" && c.OwnerToken != "" }

// SwitchyardConfig configures the Switchyard connector.
type SwitchyardConfig struct {
	BaseURL  string // PURSER_SWITCHYARD_BASE_URL (internal API base)
	Token    string // PURSER_SWITCHYARD_TOKEN (admin sw_ token)
	LoginURL string // PURSER_SWITCHYARD_URL (public login URL for the block)
}

// Configured reports whether the Switchyard connector can run.
func (c SwitchyardConfig) Configured() bool { return c.BaseURL != "" && c.Token != "" }

// CloudflareConfig configures the Cloudflare Access connector.
type CloudflareConfig struct {
	APIToken   string // PURSER_CF_API_TOKEN
	AccountID  string // PURSER_CF_ACCOUNT_ID
	GroupID    string // PURSER_CF_ACCESS_GROUP_ID
	GroupName  string // PURSER_CF_ACCESS_GROUP_NAME
	TeamDomain string // PURSER_CF_TEAM_DOMAIN
	AppsNote   string // PURSER_CF_APPS_NOTE
}

// SMTPConfig configures email delivery.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      string
}

// Configured reports whether email delivery is available.
func (c SMTPConfig) Configured() bool { return c.Host != "" && c.From != "" }

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		Addr:        envOr("PURSER_ADDR", ":4006"),
		DatabaseURL: firstEnv([]string{"PURSER_DATABASE_URL", "DATABASE_URL"}, defaultDatabaseURL),
		APIToken:    os.Getenv("PURSER_API_TOKEN"),
		Switchyard: SwitchyardConfig{
			BaseURL:  envOr("PURSER_SWITCHYARD_BASE_URL", "http://switchyard:4002"),
			Token:    os.Getenv("PURSER_SWITCHYARD_TOKEN"),
			LoginURL: envOr("PURSER_SWITCHYARD_URL", "https://switchyard.zerogravity.industries"),
		},
		Cloudflare: CloudflareConfig{
			APIToken:   os.Getenv("PURSER_CF_API_TOKEN"),
			AccountID:  os.Getenv("PURSER_CF_ACCOUNT_ID"),
			GroupID:    os.Getenv("PURSER_CF_ACCESS_GROUP_ID"),
			GroupName:  envOr("PURSER_CF_ACCESS_GROUP_NAME", "zerogravity-members"),
			TeamDomain: envOr("PURSER_CF_TEAM_DOMAIN", "zero-gravity-industries.cloudflareaccess.com"),
			AppsNote:   envOr("PURSER_CF_APPS_NOTE", "Switchyard and the other tunneled Construct apps"),
		},
		Lyceum: LyceumConfig{
			BaseURL:    envOr("PURSER_LYCEUM_BASE_URL", "http://lyceum:4005"),
			OwnerToken: os.Getenv("PURSER_LYCEUM_OWNER_TOKEN"),
			AppURL:     os.Getenv("PURSER_LYCEUM_URL"),
		},
		SMTP: SMTPConfig{
			Host:     os.Getenv("PURSER_SMTP_HOST"),
			Port:     envOrInt("PURSER_SMTP_PORT", 587),
			Username: os.Getenv("PURSER_SMTP_USERNAME"),
			Password: os.Getenv("PURSER_SMTP_PASSWORD"),
			From:     os.Getenv("PURSER_SMTP_FROM"),
			TLS:      envOr("PURSER_SMTP_TLS", "starttls"),
		},
	}
}

// defaultDatabaseURL points at the purser database on the shared construct-server
// Postgres. The password is supplied out of band via DATABASE_URL / PGPASSWORD.
const defaultDatabaseURL = "postgres://purser_user@localhost:5432/purser?sslmode=disable"

func firstEnv(keys []string, def string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("config: %s=%q is not an integer; using %d", key, os.Getenv(key), def)
	}
	return def
}
