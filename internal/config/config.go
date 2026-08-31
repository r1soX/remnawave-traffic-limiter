package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	PanelURL                    string
	APIToken                    string
	WebhookSecret               string
	BasicSquad                  string
	WhitelistSquad              string
	LimitNoticeSquad            string
	EnablePairedWhiteList       bool
	PairedWhiteListManageAll    bool
	PairedWhiteListAllowlist    map[string]struct{}
	PairedWhiteListControlToken string
	DiagnosticsToken            string
	SubscriptionGatewayEnabled  bool
	SubscriptionUpstreamURL     string
	DatabasePath                string
	LogLevelStr                 string
	Port                        string
	WebhookPath                 string
	APIPath                     string
	PollInterval                time.Duration
	HTTPTimeout                 time.Duration
}

func Load() (*Config, error) {
	cfg := &Config{
		PanelURL:                    getenv("REMNAWAVE_PANEL_URL", ""),
		APIToken:                    getenv("REMNAWAVE_API_TOKEN", ""),
		WebhookSecret:               getenv("WEBHOOK_SECRET", ""),
		BasicSquad:                  getenv("BASIC_SQUAD_UUID", ""),
		WhitelistSquad:              getenv("WHITELIST_SQUAD_UUID", ""),
		LimitNoticeSquad:            getenv("LIMIT_NOTICE_SQUAD_UUID", ""),
		EnablePairedWhiteList:       getenvBool("ENABLE_PAIRED_WHITELIST", false),
		PairedWhiteListManageAll:    getenvBool("PAIRED_WHITELIST_MANAGE_ALL", false),
		PairedWhiteListAllowlist:    stringSet(getenv("PAIRED_WHITELIST_USER_SHORT_UUIDS", "")),
		PairedWhiteListControlToken: strings.TrimSpace(getenv("PAIRED_WHITELIST_CONTROL_TOKEN", "")),
		DiagnosticsToken:            strings.TrimSpace(getenv("DIAGNOSTICS_TOKEN", "")),
		SubscriptionGatewayEnabled:  getenvBool("SUBSCRIPTION_GATEWAY_ENABLED", false),
		SubscriptionUpstreamURL:     strings.TrimRight(getenv("SUBSCRIPTION_UPSTREAM_URL", ""), "/"),
		DatabasePath:                getenv("DATABASE_PATH", "/data/state.sqlite"),
		LogLevelStr:                 getenv("LOG_LEVEL", "info"),
		Port:                        getenv("PORT", "8080"),
		WebhookPath:                 getenv("WEBHOOK_PATH", "/webhook"),
		APIPath:                     getenv("API_PATH", "/api"),
	}

	if cfg.PanelURL == "" || cfg.APIToken == "" || cfg.WebhookSecret == "" || cfg.DiagnosticsToken == "" {
		return nil, fmt.Errorf("missing required REMNAWAVE_PANEL_URL / REMNAWAVE_API_TOKEN / WEBHOOK_SECRET / DIAGNOSTICS_TOKEN")
	}
	if cfg.BasicSquad == "" || cfg.WhitelistSquad == "" {
		return nil, fmt.Errorf("missing required BASIC_SQUAD_UUID / WHITELIST_SQUAD_UUID")
	}
	if cfg.EnablePairedWhiteList && cfg.LimitNoticeSquad == "" {
		return nil, fmt.Errorf("LIMIT_NOTICE_SQUAD_UUID is required when ENABLE_PAIRED_WHITELIST=true")
	}
	if cfg.EnablePairedWhiteList && !cfg.PairedWhiteListManageAll && len(cfg.PairedWhiteListAllowlist) == 0 {
		return nil, fmt.Errorf("set PAIRED_WHITELIST_USER_SHORT_UUIDS for a pilot or PAIRED_WHITELIST_MANAGE_ALL=true")
	}
	if cfg.SubscriptionGatewayEnabled && cfg.SubscriptionUpstreamURL == "" {
		return nil, fmt.Errorf("SUBSCRIPTION_UPSTREAM_URL is required when SUBSCRIPTION_GATEWAY_ENABLED=true")
	}

	poll := getenv("POLL_INTERVAL", "45s")
	pollDur, err := time.ParseDuration(poll)
	if err != nil {
		return nil, fmt.Errorf("invalid POLL_INTERVAL: %w", err)
	}
	cfg.PollInterval = pollDur

	httpTimeout, err := time.ParseDuration(getenv("HTTP_TIMEOUT", "10s"))
	if err != nil {
		return nil, fmt.Errorf("invalid HTTP_TIMEOUT: %w", err)
	}
	cfg.HTTPTimeout = httpTimeout

	return cfg, nil
}

func (c *Config) PairingAllowed(shortUUID string) bool {
	if c == nil || !c.EnablePairedWhiteList {
		return false
	}
	if c.PairedWhiteListManageAll {
		return true
	}
	_, ok := c.PairedWhiteListAllowlist[strings.TrimSpace(shortUUID)]
	return ok
}

func (c *Config) LogLevel() slog.Level {
	switch c.LogLevelStr {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getenv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	default:
		return false
	}
}

func stringSet(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}
