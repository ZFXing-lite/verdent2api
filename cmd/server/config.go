package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config 服务配置（config.json）。
type Config struct {
	Listen     string            `json:"listen"`
	APIKey     string            `json:"api_key"`
	KeyFile    string            `json:"key_file"`
	StateFile  string            `json:"state_file"`
	Upstream   UpstreamConf      `json:"upstream"`
	Pool       PoolConf          `json:"pool"`
	Scheduler  SchedulerConf     `json:"scheduler"`
	ModelMap   map[string]string `json:"model_map"`
	ModelAlias map[string]string `json:"model_alias"`
	Features   FeaturesConf      `json:"features"`
}

type UpstreamConf struct {
	BaseURL              string        `json:"base_url"`
	TimeoutSeconds       time.Duration `json:"timeout_seconds"`
	HeaderTimeoutSeconds time.Duration `json:"header_timeout_seconds"`
	IdleTimeoutSeconds   time.Duration `json:"idle_timeout_seconds"`
	UserAgent            string        `json:"user_agent"`
}

type PoolConf struct {
	MaxInFlight    int           `json:"max_in_flight"`
	ErrThreshold   int           `json:"err_threshold"`
	ErrCooldown    time.Duration `json:"err_cooldown"`
	RateCooldown   time.Duration `json:"rate_cooldown"`
	CreditCooldown time.Duration `json:"credit_cooldown"`
	SelectJitterMS int           `json:"select_jitter_ms"`
}

type SchedulerConf struct {
	KeepaliveMinutes int  `json:"keepalive_minutes"`
	Enabled          bool `json:"enabled"`
}

type FeaturesConf struct {
	StripReasoning bool `json:"strip_reasoning"`
}

// loadConfig 从 path 读配置，应用缺省值，并支持环境变量覆盖敏感项。
func loadConfig(path string) (*Config, error) {
	cfg := &Config{
		Listen:    ":7866",
		KeyFile:   "./auths/verdent-keys.json",
		StateFile: "./data/state.json",
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

	// 环境变量覆盖（12-factor 友好，容器部署常用）。
	if v := strings.TrimSpace(os.Getenv("V2A_API_KEY")); v != "" {
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("V2A_LISTEN")); v != "" {
		cfg.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("V2A_KEY_FILE")); v != "" {
		cfg.KeyFile = v
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
	if c.KeyFile == "" {
		c.KeyFile = "./auths/verdent-keys.json"
	}
	if c.StateFile == "" {
		c.StateFile = "./data/state.json"
	}
	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = "https://api.verdent.ai"
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = 120
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if c.Pool.MaxInFlight <= 0 {
		c.Pool.MaxInFlight = 3
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
		c.Pool.CreditCooldown = 12 * time.Hour
	}
	if c.Pool.SelectJitterMS <= 0 {
		c.Pool.SelectJitterMS = 100
	}
	if c.Scheduler.KeepaliveMinutes <= 0 {
		c.Scheduler.KeepaliveMinutes = 30
	}
}
