package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config 网关配置。凭证是 PKCE 登录得到的账号 token，不再是 team API key。
type Config struct {
	Listen      string        `json:"listen"`
	APIKey      string        `json:"api_key"`
	AuthFile    string        `json:"auth_file"`
	StateFile   string        `json:"state_file"`
	CatalogFile string        `json:"catalog_file"`
	FreeOnly    bool          `json:"free_only"`
	Upstream    UpstreamConf  `json:"upstream"`
	Pool        PoolConf      `json:"pool"`
}

type UpstreamConf struct {
	BaseURL              string        `json:"base_url"`
	TimeoutSeconds       time.Duration `json:"timeout_seconds"`
	HeaderTimeoutSeconds time.Duration `json:"header_timeout_seconds"`
	IdleTimeoutSeconds   time.Duration `json:"idle_timeout_seconds"`
}

type PoolConf struct {
	ErrThreshold   int           `json:"err_threshold"`
	ErrCooldown    time.Duration `json:"err_cooldown"`
	RateCooldown   time.Duration `json:"rate_cooldown"`
	CreditCooldown time.Duration `json:"credit_cooldown"`
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{
		Listen:   ":7866",
		AuthFile: "./auths/accounts.json",
		StateFile: "./data/state.json",
		FreeOnly: true,
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("config file not found: %s", path)
			}
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	cfg.applyDefaults()
	if v := strings.TrimSpace(os.Getenv("V2A_API_KEY")); v != "" {
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("V2A_LISTEN")); v != "" {
		cfg.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("V2A_AUTH_FILE")); v != "" {
		cfg.AuthFile = v
	}
	if v := strings.TrimSpace(os.Getenv("V2A_STATE_FILE")); v != "" {
		cfg.StateFile = v
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":7866"
	}
	if c.AuthFile == "" {
		c.AuthFile = "./auths/accounts.json"
	}
	if c.StateFile == "" {
		c.StateFile = "./data/state.json"
	}
	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = "https://llm-proxy.verdent.ai"
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 180
	}
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = 120
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if c.Pool.ErrThreshold <= 0 {
		c.Pool.ErrThreshold = 5
	}
	if c.Pool.ErrCooldown <= 0 {
		c.Pool.ErrCooldown = 10 * time.Minute
	}
	if c.Pool.RateCooldown <= 0 {
		c.Pool.RateCooldown = 60 * time.Second
	}
	if c.Pool.CreditCooldown <= 0 {
		c.Pool.CreditCooldown = time.Hour
	}
}
