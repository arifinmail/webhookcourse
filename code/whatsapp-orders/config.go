package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is read from config.yaml; config.example.yaml explains every field.
type Config struct {
	BusinessInfo string       `yaml:"business_info"`
	Products     []string     `yaml:"products"`
	Filter       FilterConfig `yaml:"filter"`
	Batching     BatchConfig  `yaml:"batching"`
	Claude       ClaudeConfig `yaml:"claude"`
	Output       OutputConfig `yaml:"output"`
	Reply        ReplyConfig  `yaml:"reply"`
	DataDir      string       `yaml:"data_dir"`
}

type FilterConfig struct {
	PrivateChats  bool          `yaml:"private_chats"`
	Groups        []string      `yaml:"groups"`
	IgnoreNumbers []string      `yaml:"ignore_numbers"`
	OnlyNumbers   []string      `yaml:"only_numbers"`
	Keywords      []string      `yaml:"keywords"`
	SkipPhrases   []string      `yaml:"skip_phrases"`
	MinLength     int           `yaml:"min_length"`
	MaxAge        time.Duration `yaml:"max_age"`
}

type BatchConfig struct {
	QuietPeriod time.Duration `yaml:"quiet_period"`
	MaxWait     time.Duration `yaml:"max_wait"`
}

type ClaudeConfig struct {
	Model  string `yaml:"model"`
	Effort string `yaml:"effort"`
}

type OutputConfig struct {
	WebhookURL string `yaml:"webhook_url"`
	Secret     string `yaml:"secret"`
}

// ReplyConfig controls the confirmation messages sent to customers in private chats.
type ReplyConfig struct {
	Mode          string        `yaml:"mode"` // off, preview or send
	Confirm       string        `yaml:"confirm"`
	Missing       string        `yaml:"missing"`
	Thanks        string        `yaml:"thanks"`
	MinGap        time.Duration `yaml:"min_gap"`
	MaxPerHour    int           `yaml:"max_per_hour"`
	ConfirmWindow time.Duration `yaml:"confirm_window"`
}

func defaultConfig() Config {
	return Config{
		Filter:   FilterConfig{PrivateChats: true, MinLength: 2, MaxAge: 24 * time.Hour},
		Batching: BatchConfig{QuietPeriod: 2 * time.Minute, MaxWait: 15 * time.Minute},
		Claude:   ClaudeConfig{Model: "claude-opus-5-5", Effort: "medium"},
		Reply:    ReplyConfig{Mode: "off", MinGap: 20 * time.Second, MaxPerHour: 20, ConfirmWindow: 24 * time.Hour},
		DataDir:  "store",
	}
}

// LoadConfig reads the config file on top of the defaults. ORDER_WEBHOOK_URL and
// ORDER_WEBHOOK_SECRET override the output settings, so secrets can stay out of the file.
func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading config: %w (copy config.example.yaml to %s first)", err, path)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	if v := os.Getenv("ORDER_WEBHOOK_URL"); v != "" {
		cfg.Output.WebhookURL = v
	}
	if v := os.Getenv("ORDER_WEBHOOK_SECRET"); v != "" {
		cfg.Output.Secret = v
	}
	cfg.Output.WebhookURL = strings.TrimSpace(cfg.Output.WebhookURL)

	switch cfg.Claude.Effort {
	case "low", "medium", "high", "xhigh", "max", "":
	default:
		return cfg, fmt.Errorf("claude.effort must be low, medium, high, xhigh, max or empty, not %q", cfg.Claude.Effort)
	}
	if cfg.Batching.QuietPeriod <= 0 || cfg.Batching.MaxWait < cfg.Batching.QuietPeriod {
		return cfg, fmt.Errorf("batching.quiet_period must be above zero and batching.max_wait at least as long (got %s and %s)",
			cfg.Batching.QuietPeriod, cfg.Batching.MaxWait)
	}
	switch cfg.Reply.Mode {
	case "off":
	case "preview", "send":
		if strings.TrimSpace(cfg.Reply.Confirm) == "" {
			return cfg, fmt.Errorf("reply.mode is %s but reply.confirm is empty: write the confirmation message first", cfg.Reply.Mode)
		}
		if cfg.Reply.MinGap < 5*time.Second || cfg.Reply.MaxPerHour < 1 || cfg.Reply.ConfirmWindow <= 0 {
			return cfg, errors.New("reply.min_gap must be at least 5s, and reply.max_per_hour and reply.confirm_window above zero")
		}
	default:
		return cfg, fmt.Errorf("reply.mode must be off, preview or send, not %q", cfg.Reply.Mode)
	}
	if cfg.Output.WebhookURL != "" && cfg.Output.Secret == "" {
		return cfg, errors.New("output.secret is empty: set it (or ORDER_WEBHOOK_SECRET) so your system can check that orders really come from here")
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "store"
	}
	return cfg, nil
}
