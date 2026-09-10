package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const fixtureKey = "test-only-zai-key"

func TestParsePluginConfigDefaults(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("enabled: true\npriority: 10\ndefault-plan: pro\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPAConfigPath != defaultCPAConfigPath || cfg.QuotaRefresh != defaultQuotaRefresh || cfg.AuthoritativeMaxAge != defaultAuthoritativeMaxAge || cfg.ThresholdPercent != defaultThreshold {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.SuspendDuration != defaultSuspend || cfg.FallbackCooldown != defaultFallback || cfg.StateRetention != defaultStateRetention {
		t.Fatalf("unexpected durations: %#v", cfg)
	}
}

func TestParsePluginConfigQuotaCadenceAndFreshness(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("quota-refresh-interval: 1m\nauthoritative-max-age: 2m\ndefault-plan: pro\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuotaRefresh != time.Minute || cfg.AuthoritativeMaxAge != 2*time.Minute {
		t.Fatalf("quota config = %#v", cfg)
	}
	for _, raw := range []string{
		"quota-refresh-interval: 59s\nauthoritative-max-age: 5m\ndefault-plan: pro\n",
		"quota-refresh-interval: 181s\nauthoritative-max-age: 5m\ndefault-plan: pro\n",
		"quota-refresh-interval: 2m\nauthoritative-max-age: 3m\ndefault-plan: pro\n",
	} {
		if _, err := parsePluginConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid quota config succeeded: %q", raw)
		}
	}
}

func TestParsePluginConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unknown field", raw: "unknown: true\n", want: "field unknown not found"},
		{name: "threshold", raw: "threshold-percent: 0\n", want: "between 1 and 100"},
		{name: "duration", raw: "suspend-duration: 0s\n", want: "positive duration"},
		{name: "retention", raw: "state-retention: 7d\n", want: "positive duration"},
		{name: "invalid plan", raw: "default-plan: enterprise\n", want: "default-plan"},
		{name: "custom default plan", raw: "default-plan: custom\n", want: "default-plan"},
		{name: "missing suffix", raw: "default-plan: pro\naccounts:\n  - name: one\n", want: "key-suffix is required"},
		{name: "duplicate name", raw: "default-plan: pro\naccounts:\n  - key-suffix: one\n    name: Same\n  - key-suffix: two\n    name: same\n", want: "duplicate name"},
		{name: "custom buckets", raw: "accounts:\n  - key-suffix: one\n    plan: custom\n    five-hour-credits: 1\n", want: "requires both"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePluginConfig([]byte(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestPluginConfigRoundTripDoesNotExposeCPAKeys(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("default-plan: pro\naccounts:\n  - key-suffix: 31a7\n    name: primary\n"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fixtureKey) || strings.Contains(string(raw), "api-key") {
		t.Fatalf("plugin config serialization includes provider key material: %s", raw)
	}
}

func TestLoadCPAConfigDecodeErrorIsRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "openai-compatibility:\n  - name: zai-coding-plan\n    disabled: " + fixtureKey + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadCPAConfig(path)
	if err == nil {
		t.Fatal("malformed CPA config succeeded")
	}
	if strings.Contains(err.Error(), fixtureKey) || strings.Contains(err.Error(), fixtureKey[:7]) {
		t.Fatalf("decode error leaked provider key material: %v", err)
	}
}

func TestLoadCPAConfigProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "auth-dir: /auth\nclaude-api-key:\n  - api-key: " + fixtureKey + "\n    base-url: https://api.z.ai/api/anthropic\n    prefix: ' /zai/ '\n    headers:\n      ' X-Test ': ' value '\n      Empty: '   '\nopenai-compatibility:\n  - name: zai-coding-plan\n    base-url: https://api.z.ai/api/coding/paas/v4\n    api-key-entries:\n      - api-key: " + fixtureKey + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCPAConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthDir != "/auth" || len(cfg.ClaudeKeys) != 1 || len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("unexpected projection: %#v", cfg)
	}
	if got := cfg.ClaudeKeys[0].Prefix; got != "zai" {
		t.Fatalf("normalized prefix = %q, want zai", got)
	}
	if got, want := cfg.ClaudeKeys[0].Headers, map[string]string{"X-Test": "value"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized headers = %#v, want %#v", got, want)
	}
}
