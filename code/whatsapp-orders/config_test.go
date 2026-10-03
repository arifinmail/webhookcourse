package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `
products:
  - Dimsum ayam (isi 10) - Rp 35.000
filter:
  groups: [PO Oktober]
  max_age: 6h
batching:
  quiet_period: 90s
  max_wait: 10m
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Batching.QuietPeriod != 90*time.Second || cfg.Batching.MaxWait != 10*time.Minute || cfg.Filter.MaxAge != 6*time.Hour {
		t.Errorf("durations not read: %+v %+v", cfg.Batching, cfg.Filter)
	}
	if !cfg.Filter.PrivateChats || cfg.Claude.Model != "claude-opus-5-5" || cfg.Claude.Effort != "medium" || cfg.DataDir != "store" {
		t.Errorf("defaults lost: %+v", cfg)
	}
	if len(cfg.Products) != 1 || cfg.Filter.Groups[0] != "PO Oktober" {
		t.Errorf("lists not read: %+v", cfg)
	}
}

func TestLoadConfigRejectsMistakes(t *testing.T) {
	cases := map[string]string{
		"effort":         "claude:\n  effort: extreme\n",
		"max_wait":       "batching:\n  quiet_period: 5m\n  max_wait: 1m\n",
		"output.secret":  "output:\n  webhook_url: https://example.com/orders\n",
		"copy config.ex": "", // missing file, see below
	}
	for want, yaml := range cases {
		path := writeConfig(t, yaml)
		if yaml == "" {
			path = filepath.Join(t.TempDir(), "missing.yaml")
		}
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %q: err = %v, want it to mention %q", yaml, err, want)
		}
	}
}

func TestLoadConfigSecretFromEnvironment(t *testing.T) {
	t.Setenv("ORDER_WEBHOOK_SECRET", "from-env")
	cfg, err := LoadConfig(writeConfig(t, "output:\n  webhook_url: https://example.com/orders\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Output.Secret != "from-env" {
		t.Errorf("secret = %q", cfg.Output.Secret)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := LoadConfig("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Products) == 0 || len(cfg.Filter.SkipPhrases) == 0 || cfg.Batching.QuietPeriod != 2*time.Minute {
		t.Errorf("example config not read as expected: %+v", cfg)
	}
}
