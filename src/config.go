package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"gopkg.in/yaml.v3"
)

const (
	defaultCPAConfigPath       = "config.yaml"
	requiredPluginPriority     = 1000
	defaultQuotaRefresh        = 2 * time.Minute
	defaultAuthoritativeMaxAge = 5 * time.Minute
	defaultThreshold           = 97
	defaultSuspend             = 30 * time.Minute
	defaultFallback            = 10 * time.Minute
	defaultStateRetention      = 8 * 24 * time.Hour
)

var planBuckets = map[string]creditBuckets{
	"lite": {FiveHour: 2_000, Weekly: 10_000},
	"pro":  {FiveHour: 12_000, Weekly: 60_000},
	"max":  {FiveHour: 28_000, Weekly: 140_000},
}

type creditBuckets struct {
	FiveHour int64
	Weekly   int64
}

type pluginConfig struct {
	CPAConfigPath       string
	QuotaRefresh        time.Duration
	AuthoritativeMaxAge time.Duration
	ThresholdPercent    int
	SuspendDuration     time.Duration
	FallbackCooldown    time.Duration
	StateRetention      time.Duration
	DefaultPlan         string
	Accounts            []accountOverride
}

type accountOverride struct {
	KeySuffix       string
	Name            string
	Plan            string
	Disabled        bool
	FiveHourCredits int64
	WeeklyCredits   int64
}

type rawPluginConfig struct {
	Enabled             bool                 `yaml:"enabled"`
	Priority            int                  `yaml:"priority"`
	CPAConfigPath       string               `yaml:"cpa-config-path"`
	QuotaRefresh        string               `yaml:"quota-refresh-interval"`
	AuthoritativeMaxAge string               `yaml:"authoritative-max-age"`
	ThresholdPercent    *int                 `yaml:"threshold-percent"`
	SuspendDuration     string               `yaml:"suspend-duration"`
	FallbackCooldown    string               `yaml:"fallback-cooldown"`
	StateRetention      string               `yaml:"state-retention"`
	DefaultPlan         string               `yaml:"default-plan"`
	Accounts            []rawAccountOverride `yaml:"accounts"`
}

type rawAccountOverride struct {
	KeySuffix       string `yaml:"key-suffix"`
	Name            string `yaml:"name"`
	Plan            string `yaml:"plan"`
	Disabled        bool   `yaml:"disabled"`
	FiveHourCredits *int64 `yaml:"five-hour-credits"`
	WeeklyCredits   *int64 `yaml:"weekly-credits"`
}

type cpaConfigProjection struct {
	AuthDir             string                          `yaml:"auth-dir"`
	ClaudeKeys          []sdkconfig.ClaudeKey           `yaml:"claude-api-key"`
	OpenAICompatibility []sdkconfig.OpenAICompatibility `yaml:"openai-compatibility"`
	Plugins             cpaPluginsProjection            `yaml:"plugins"`
}

type cpaPluginsProjection struct {
	Enabled bool                           `yaml:"enabled"`
	Configs map[string]cpaPluginProjection `yaml:"configs"`
}

type cpaPluginProjection struct {
	Enabled  *bool `yaml:"enabled"`
	Priority int   `yaml:"priority"`
}

func parsePluginConfig(raw []byte) (pluginConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var input rawPluginConfig
	if err := decoder.Decode(&input); err != nil {
		return pluginConfig{}, fmt.Errorf("decode plugin config: %w", err)
	}

	cfg := pluginConfig{
		CPAConfigPath:       strings.TrimSpace(input.CPAConfigPath),
		QuotaRefresh:        defaultQuotaRefresh,
		AuthoritativeMaxAge: defaultAuthoritativeMaxAge,
		ThresholdPercent:    defaultThreshold,
		SuspendDuration:     defaultSuspend,
		FallbackCooldown:    defaultFallback,
		StateRetention:      defaultStateRetention,
		DefaultPlan:         normalizePlan(input.DefaultPlan),
	}
	if cfg.CPAConfigPath == "" {
		cfg.CPAConfigPath = defaultCPAConfigPath
	}
	if input.ThresholdPercent != nil {
		cfg.ThresholdPercent = *input.ThresholdPercent
	}
	if cfg.ThresholdPercent < 1 || cfg.ThresholdPercent > 100 {
		return pluginConfig{}, fmt.Errorf("threshold-percent must be between 1 and 100")
	}

	var err error
	if cfg.QuotaRefresh, err = parsePositiveDuration("quota-refresh-interval", input.QuotaRefresh, defaultQuotaRefresh); err != nil {
		return pluginConfig{}, err
	}
	if cfg.QuotaRefresh < time.Minute || cfg.QuotaRefresh > 3*time.Minute {
		return pluginConfig{}, fmt.Errorf("quota-refresh-interval must be between one and three minutes")
	}
	if cfg.AuthoritativeMaxAge, err = parsePositiveDuration("authoritative-max-age", input.AuthoritativeMaxAge, defaultAuthoritativeMaxAge); err != nil {
		return pluginConfig{}, err
	}
	if cfg.AuthoritativeMaxAge <= maxPollInterval(cfg.QuotaRefresh) {
		return pluginConfig{}, fmt.Errorf("authoritative-max-age must exceed the maximum poll interval")
	}
	if cfg.SuspendDuration, err = parsePositiveDuration("suspend-duration", input.SuspendDuration, defaultSuspend); err != nil {
		return pluginConfig{}, err
	}
	if cfg.FallbackCooldown, err = parsePositiveDuration("fallback-cooldown", input.FallbackCooldown, defaultFallback); err != nil {
		return pluginConfig{}, err
	}
	if cfg.StateRetention, err = parsePositiveDuration("state-retention", input.StateRetention, defaultStateRetention); err != nil {
		return pluginConfig{}, err
	}
	if cfg.StateRetention <= 7*24*time.Hour {
		return pluginConfig{}, fmt.Errorf("state-retention must exceed one week")
	}
	if input.DefaultPlan != "" && (cfg.DefaultPlan == "" || cfg.DefaultPlan == "custom") {
		return pluginConfig{}, fmt.Errorf("default-plan must be lite, pro, or max")
	}

	seenNames := make(map[string]struct{}, len(input.Accounts))
	cfg.Accounts = make([]accountOverride, 0, len(input.Accounts))
	for i, rawAccount := range input.Accounts {
		override, errOverride := validateAccountOverride(rawAccount, cfg.DefaultPlan)
		if errOverride != nil {
			return pluginConfig{}, fmt.Errorf("accounts[%d]: %w", i, errOverride)
		}
		if override.Name != "" {
			nameKey := strings.ToLower(override.Name)
			if _, exists := seenNames[nameKey]; exists {
				return pluginConfig{}, fmt.Errorf("accounts[%d]: duplicate name %q", i, override.Name)
			}
			seenNames[nameKey] = struct{}{}
		}
		cfg.Accounts = append(cfg.Accounts, override)
	}
	return cfg, nil
}

func validateAccountOverride(input rawAccountOverride, defaultPlan string) (accountOverride, error) {
	override := accountOverride{
		KeySuffix: strings.TrimSpace(input.KeySuffix),
		Name:      strings.TrimSpace(input.Name),
		Plan:      normalizePlan(input.Plan),
		Disabled:  input.Disabled,
	}
	if override.KeySuffix == "" {
		return accountOverride{}, fmt.Errorf("key-suffix is required")
	}
	if input.Plan != "" && override.Plan == "" {
		return accountOverride{}, fmt.Errorf("plan must be lite, pro, max, or custom")
	}
	if override.Plan == "" {
		override.Plan = defaultPlan
	}

	if input.FiveHourCredits != nil {
		override.FiveHourCredits = *input.FiveHourCredits
		if !validCreditBucket(override.FiveHourCredits) {
			return accountOverride{}, fmt.Errorf("five-hour-credits must be positive and at most %d", maxCreditBucket)
		}
	}
	if input.WeeklyCredits != nil {
		override.WeeklyCredits = *input.WeeklyCredits
		if !validCreditBucket(override.WeeklyCredits) {
			return accountOverride{}, fmt.Errorf("weekly-credits must be positive and at most %d", maxCreditBucket)
		}
	}

	if override.Plan == "custom" && (override.FiveHourCredits == 0 || override.WeeklyCredits == 0) {
		return accountOverride{}, fmt.Errorf("custom plan requires both credit buckets")
	}
	if override.Plan == "" && (override.FiveHourCredits == 0 || override.WeeklyCredits == 0) {
		return accountOverride{}, fmt.Errorf("plan or both credit buckets are required")
	}
	return override, nil
}

func normalizePlan(raw string) string {
	plan := strings.ToLower(strings.TrimSpace(raw))
	switch plan {
	case "lite", "pro", "max", "custom":
		return plan
	default:
		return ""
	}
}

func parsePositiveDuration(name, raw string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func loadCPAConfig(path string) (cpaConfigProjection, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cpaConfigProjection{}, fmt.Errorf("read CPA config: %w", err)
	}
	cfg, err := sdkconfig.ParseConfigBytes(data)
	if err != nil {
		return cpaConfigProjection{}, fmt.Errorf("load CPA config: invalid configuration")
	}
	plugins := cpaPluginsProjection{Enabled: cfg.Plugins.Enabled, Configs: make(map[string]cpaPluginProjection, len(cfg.Plugins.Configs))}
	for id, item := range cfg.Plugins.Configs {
		plugins.Configs[id] = cpaPluginProjection{Enabled: item.Enabled, Priority: item.Priority}
	}
	return cpaConfigProjection{
		AuthDir:             cfg.AuthDir,
		ClaudeKeys:          cfg.ClaudeKey,
		OpenAICompatibility: cfg.OpenAICompatibility,
		Plugins:             plugins,
	}, nil
}

func resolveCPAConfigPath(pluginConfigPath string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(pluginConfigPath))
	if clean == "." || clean == "" {
		return "", fmt.Errorf("cpa-config-path is required")
	}
	if filepath.IsAbs(clean) {
		return clean, nil
	}
	absolute, err := filepath.Abs(clean)
	if err != nil {
		return "", fmt.Errorf("resolve cpa-config-path: %w", err)
	}
	return absolute, nil
}

func resolveAuthDir(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = "~/.cli-proxy-api"
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve auth-dir: %w", err)
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	absolute, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return "", fmt.Errorf("resolve auth-dir: %w", err)
	}
	return absolute, nil
}
