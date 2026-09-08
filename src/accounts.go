package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

const (
	zaiAnthropicBaseURL = "https://api.z.ai/api/anthropic"
	zaiOpenAIBaseURL    = "https://api.z.ai/api/coding/paas/v4"
	zaiCompatName       = "zai-coding-plan"
)

type account struct {
	Identity        string `json:"-"`
	Name            string `json:"name"`
	KeySuffix       string `json:"key_suffix"`
	Plan            string `json:"plan"`
	Disabled        bool   `json:"disabled,omitempty"`
	FiveHourCredits int64  `json:"five_hour_credits"`
	WeeklyCredits   int64  `json:"weekly_credits"`
	ClaudeAuthID    string `json:"claude_auth_id"`
	OpenAIAuthID    string `json:"openai_auth_id"`
	key             string
}

type pairCandidate struct {
	key              string
	claudeCount      int
	openAIEntryCount int
}

type stableIDGenerator struct {
	counters map[string]int
}

func newStableIDGenerator() *stableIDGenerator {
	return &stableIDGenerator{counters: make(map[string]int)}
}

func (g *stableIDGenerator) next(kind string, parts ...string) string {
	short := stableAuthID(kind, parts...)[:12]
	key := kind + ":" + short
	index := g.counters[key]
	g.counters[key] = index + 1
	if index > 0 {
		short = fmt.Sprintf("%s-%d", short, index)
	}
	return kind + ":" + short
}

// stableAuthID deliberately reproduces CLIProxyAPI's v7.2 non-security
// interoperability identifier. The host contract requires these exact bytes.
func stableAuthID(kind string, parts ...string) string {
	encoded := make([]byte, 0, len(kind)+len(parts)*16)
	encoded = append(encoded, kind...)
	for _, part := range parts {
		encoded = append(encoded, 0)
		encoded = append(encoded, strings.TrimSpace(part)...)
	}
	return sha256Hex(encoded)
}

// sha256Hex is retained solely for the upstream stable auth-ID contract.
//
//go:noinline
func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value) // codeql[go/weak-sensitive-data-hashing] Upstream non-security ID contract.
	return hex.EncodeToString(digest[:])
}

func discoverAccounts(cpa cpaConfigProjection, cfg pluginConfig) ([]account, error) {
	pairs := make(map[string]*pairCandidate)

	for i := range cpa.ClaudeKeys {
		entry := cpa.ClaudeKeys[i]
		key := strings.TrimSpace(entry.APIKey)
		_, recognized := recognizedBaseURL(entry.BaseURL, zaiAnthropicBaseURL)
		if !recognized {
			continue
		}
		if key == "" {
			return nil, fmt.Errorf("z.ai anthropic entry has no API key")
		}
		pair := pairs[key]
		if pair == nil {
			pair = &pairCandidate{key: key}
			pairs[key] = pair
		}
		pair.claudeCount++
	}

	for i := range cpa.OpenAICompatibility {
		compat := cpa.OpenAICompatibility[i]
		if strings.ToLower(strings.TrimSpace(compat.Name)) != zaiCompatName {
			continue
		}
		if compat.Disabled {
			return nil, fmt.Errorf("zai-coding-plan provider must be enabled")
		}
		if _, recognized := recognizedBaseURL(compat.BaseURL, zaiOpenAIBaseURL); !recognized {
			return nil, fmt.Errorf("zai-coding-plan provider has invalid base URL")
		}
		if len(compat.APIKeyEntries) == 0 {
			return nil, fmt.Errorf("zai-coding-plan provider has no API key entries")
		}
		for j := range compat.APIKeyEntries {
			entry := compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			if key == "" {
				return nil, fmt.Errorf("zai-coding-plan provider contains an empty API key entry")
			}
			pair := pairs[key]
			if pair == nil {
				pair = &pairCandidate{key: key}
				pairs[key] = pair
			}
			pair.openAIEntryCount++
		}
	}

	if len(pairs) == 0 {
		return nil, fmt.Errorf("no complete Z.ai account pairs found")
	}
	if err := validatePairs(pairs); err != nil {
		return nil, err
	}

	claudeIDs := make(map[string]string, len(pairs))
	idGen := newStableIDGenerator()
	for i := range cpa.ClaudeKeys {
		entry := cpa.ClaudeKeys[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		id := idGen.next(
			"claude:apikey",
			key,
			strings.TrimSpace(entry.BaseURL),
		)
		baseURL, err := normalizedBaseURL(entry.BaseURL)
		if err == nil {
			if pair := pairs[key]; pair != nil && pair.claudeCount == 1 && pair.openAIEntryCount == 1 && baseURL == zaiAnthropicBaseURL {
				claudeIDs[key] = id
			}
		}
	}

	openAIIDs := make(map[string]string, len(pairs))
	for i := range cpa.OpenAICompatibility {
		compat := cpa.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		providerName := strings.ToLower(strings.TrimSpace(compat.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		baseURL, baseRecognized := recognizedBaseURL(compat.BaseURL, zaiOpenAIBaseURL)
		for j := range compat.APIKeyEntries {
			entry := compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			if key == "" {
				continue
			}
			idKind := "openai-compatibility:" + providerName
			id := idGen.next(idKind, key, strings.TrimSpace(compat.BaseURL), strings.TrimSpace(entry.ProxyURL))
			if baseRecognized && providerName == zaiCompatName && baseURL == zaiOpenAIBaseURL {
				openAIIDs[key] = id
			}
		}
	}

	ordered := make([]account, 0, len(pairs))
	for i := range cpa.ClaudeKeys {
		entry := cpa.ClaudeKeys[i]
		key := strings.TrimSpace(entry.APIKey)
		pair := pairs[key]
		if pair == nil || pair.claudeCount != 1 || pair.openAIEntryCount != 1 {
			continue
		}
		item := account{
			Identity:     accountIdentity(key),
			KeySuffix:    displaySuffix(key),
			ClaudeAuthID: claudeIDs[key],
			OpenAIAuthID: openAIIDs[key],
			key:          key,
		}
		if item.ClaudeAuthID == "" || item.OpenAIAuthID == "" {
			return nil, fmt.Errorf("paired account %s has incomplete auth IDs", item.KeySuffix)
		}
		ordered = append(ordered, item)
	}

	if err := applyAccountOverrides(ordered, cfg); err != nil {
		return nil, err
	}
	return ordered, nil
}

func validatePairs(pairs map[string]*pairCandidate) error {
	for _, pair := range pairs {
		suffix := displaySuffix(pair.key)
		switch {
		case pair.claudeCount > 1:
			return fmt.Errorf("duplicate Z.ai Anthropic entry for key suffix %s", suffix)
		case pair.openAIEntryCount > 1:
			return fmt.Errorf("duplicate zai-coding-plan entry for key suffix %s", suffix)
		case pair.claudeCount == 0:
			return fmt.Errorf("missing Z.ai Anthropic sibling for key suffix %s", suffix)
		case pair.openAIEntryCount == 0:
			return fmt.Errorf("missing zai-coding-plan sibling for key suffix %s", suffix)
		}
	}
	return nil
}

func applyAccountOverrides(accounts []account, cfg pluginConfig) error {
	matched := make(map[int]int, len(cfg.Accounts))
	for accountIndex := range accounts {
		matches := make([]int, 0, 1)
		for overrideIndex := range cfg.Accounts {
			if strings.HasSuffix(accounts[accountIndex].key, cfg.Accounts[overrideIndex].KeySuffix) {
				matches = append(matches, overrideIndex)
			}
		}
		if len(matches) > 1 {
			return fmt.Errorf("account suffix %s matches multiple overrides", accounts[accountIndex].KeySuffix)
		}
		var override *accountOverride
		if len(matches) == 1 {
			index := matches[0]
			matched[index]++
			override = &cfg.Accounts[index]
		}
		if err := applyAccountOverride(&accounts[accountIndex], override, cfg.DefaultPlan, accountIndex); err != nil {
			return err
		}
	}
	for i := range cfg.Accounts {
		switch matched[i] {
		case 0:
			return fmt.Errorf("accounts[%d]: key-suffix matches no account", i)
		case 1:
		default:
			return fmt.Errorf("accounts[%d]: key-suffix matches multiple accounts", i)
		}
	}
	seenNames := make(map[string]struct{}, len(accounts))
	for i := range accounts {
		key := strings.ToLower(accounts[i].Name)
		if _, exists := seenNames[key]; exists {
			return fmt.Errorf("duplicate account name %q", accounts[i].Name)
		}
		seenNames[key] = struct{}{}
	}
	return nil
}

func applyAccountOverride(target *account, override *accountOverride, defaultPlan string, index int) error {
	plan := defaultPlan
	if override != nil && override.Plan != "" {
		plan = override.Plan
	}
	if plan == "" {
		return fmt.Errorf("account %s has no plan", target.KeySuffix)
	}
	buckets, knownPlan := planBuckets[plan]
	if plan == "custom" {
		knownPlan = true
	}
	if !knownPlan {
		return fmt.Errorf("account %s has invalid plan %q", target.KeySuffix, plan)
	}
	if override != nil {
		if override.Name != "" {
			target.Name = override.Name
		}
		target.Disabled = override.Disabled
		if override.FiveHourCredits > 0 {
			buckets.FiveHour = override.FiveHourCredits
		}
		if override.WeeklyCredits > 0 {
			buckets.Weekly = override.WeeklyCredits
		}
	}
	if !validCreditBucket(buckets.FiveHour) || !validCreditBucket(buckets.Weekly) {
		return fmt.Errorf("account %s has non-positive or out-of-range credit bucket", target.KeySuffix)
	}
	if target.Name == "" {
		target.Name = fmt.Sprintf("zai-%s-%d", plan, index+1)
	}
	target.Plan = plan
	target.FiveHourCredits = buckets.FiveHour
	target.WeeklyCredits = buckets.Weekly
	return nil
}

func recognizedBaseURL(raw, expected string) (string, bool) {
	normalized, err := normalizedBaseURL(raw)
	return normalized, err == nil && normalized == expected
}

func normalizedBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("base-url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("invalid base-url")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("base-url must not include query or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func accountIdentity(key string) string {
	mac := hmac.New(sha256.New, []byte(pluginID+":account-identity:v1"))
	_, _ = mac.Write([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(mac.Sum(nil))
}

func displaySuffix(key string) string {
	trimmed := strings.TrimSpace(key)
	const suffixLength = 6
	if len(trimmed) <= suffixLength {
		return "redacted"
	}
	return trimmed[len(trimmed)-suffixLength:]
}
