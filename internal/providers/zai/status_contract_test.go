package zai

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This test-only consumer models the documented acceptance boundary. It does
// not write telemetry, map to host auth IDs, or call a provider.
type consumedCooldown struct {
	Active bool
	Until  time.Time
	Reason string
}

func consumeStatusCooldowns(raw []byte, known map[string]bool, now time.Time, maxAge time.Duration) (map[string]consumedCooldown, error) {
	var body struct {
		Plugin      string          `json:"plugin"`
		Status      string          `json:"status"`
		GeneratedAt time.Time       `json:"generated_at"`
		Accounts    json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("invalid status JSON")
	}
	if body.Plugin != "zai-coding-plan" || body.Status != "registered" || body.GeneratedAt.IsZero() || body.GeneratedAt.After(now) || maxAge <= 0 || now.Sub(body.GeneratedAt) > maxAge {
		return nil, fmt.Errorf("unsupported or stale status")
	}
	var rows []struct {
		Identity string `json:"identity"`
		Cooldown *struct {
			Active *bool           `json:"active"`
			Until  json.RawMessage `json:"until"`
			Reason *string         `json:"reason"`
			Source string          `json:"source"`
		} `json:"cooldown"`
	}
	if err := json.Unmarshal(body.Accounts, &rows); err != nil || rows == nil {
		return nil, fmt.Errorf("missing or invalid accounts")
	}
	result := make(map[string]consumedCooldown)
	seen := make(map[string]bool)
	for _, row := range rows {
		identity, err := hex.DecodeString(row.Identity)
		if err != nil || len(identity) != 32 || row.Identity != strings.ToLower(row.Identity) || seen[row.Identity] {
			return nil, fmt.Errorf("invalid or duplicate identity")
		}
		seen[row.Identity] = true
		c := row.Cooldown
		if c == nil || c.Active == nil || c.Reason == nil || len(c.Until) == 0 || c.Source != "zai_runtime_health_v1" {
			return nil, fmt.Errorf("missing or unsupported cooldown")
		}
		var until *time.Time
		if err := json.Unmarshal(c.Until, &until); err != nil {
			return nil, fmt.Errorf("invalid cooldown deadline")
		}
		value := consumedCooldown{Active: *c.Active, Reason: *c.Reason}
		if value.Active {
			if until == nil || !until.After(body.GeneratedAt) {
				return nil, fmt.Errorf("invalid active cooldown")
			}
			switch value.Reason {
			case "retry_after", "reset_header", "reset_body", "quota_reset_fallback", "configured_fallback", "upstream_rate_limit":
			default:
				return nil, fmt.Errorf("unsupported cooldown reason")
			}
			value.Until = *until
			if !until.After(now) {
				continue // expired evidence is dropped, not converted into release evidence
			}
		} else if until != nil || value.Reason != "" {
			return nil, fmt.Errorf("contradictory inactive cooldown")
		}
		if known[row.Identity] {
			result[row.Identity] = value
		}
	}
	return result, nil
}

func statusContractFixture(t *testing.T) (*pluginRuntime, []account, time.Time) {
	t.Helper()
	first := exactPairFixture("test-only-status-alpha")
	second := exactPairFixture("test-only-status-beta")
	first.ClaudeKeys = append(first.ClaudeKeys, second.ClaudeKeys...)
	first.OpenAICompatibility[0].APIKeyEntries = append(first.OpenAICompatibility[0].APIKeyEntries, second.OpenAICompatibility[0].APIKeyEntries...)
	accounts, err := discoverAccounts(first, pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	r := quotaTestRuntime(t, now, accounts)
	r.snapshot.Config.FallbackCooldown = defaultFallback
	r.snapshot.Config.SuspendDuration = defaultSuspend
	return r, accounts, now
}

func serializedModuleStatus(t *testing.T, r *pluginRuntime) []byte {
	t.Helper()
	m := &zaiModule{runtime: r}
	raw, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementStatusPath})
	if response.StatusCode != http.StatusOK || string(response.Body) != string(raw) {
		t.Fatal("module and management route serialized different status")
	}
	return raw
}

func TestStatusContractMultiAccountReorderAndRedaction(t *testing.T) {
	r, accounts, now := statusContractFixture(t)
	first, second := accounts[0], accounts[1]
	mustHandleUsage(t, r, pluginapi.UsageRecord{
		AuthID: first.ClaudeAuthID, Failed: true,
		Failure:         pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: first.key},
		ResponseHeaders: http.Header{"Retry-After": []string{"120"}},
	})
	known := map[string]bool{first.Identity: true, second.Identity: true}
	raw := serializedModuleStatus(t, r)
	got, err := consumeStatusCooldowns(raw, known, now, time.Minute)
	if err != nil || len(got) != 2 || !got[first.Identity].Active || got[second.Identity].Active || got[first.Identity].Reason != "retry_after" || !got[first.Identity].Until.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("cooldown projection = %#v, error = %v", got, err)
	}
	for _, a := range accounts {
		for _, forbidden := range []string{a.key, a.KeySuffix, a.ClaudeAuthID, a.OpenAIAuthID} {
			if forbidden != "" && strings.Contains(string(raw), forbidden) {
				t.Fatal("status exposed credential material or host auth identity")
			}
		}
	}
	r.snapshot.Accounts = []account{second, first}
	for i := range r.snapshot.Accounts {
		r.snapshot.Accounts[i].Name = "renamed-display"
	}
	reordered, err := consumeStatusCooldowns(serializedModuleStatus(t, r), known, now, time.Minute)
	if err != nil || !reflect.DeepEqual(got, reordered) {
		t.Fatalf("reorder or rename changed identity join: %v", err)
	}
	unknown, err := consumeStatusCooldowns(raw, map[string]bool{second.Identity: true}, now, time.Minute)
	if err != nil || len(unknown) != 1 || unknown[second.Identity].Active {
		t.Fatalf("unknown identity was not dropped: %#v, %v", unknown, err)
	}
	// A single account is still unknown without an explicit identity match.
	r.snapshot.Accounts = []account{first}
	unknown, err = consumeStatusCooldowns(serializedModuleStatus(t, r), map[string]bool{}, now, time.Minute)
	if err != nil || len(unknown) != 0 {
		t.Fatalf("singleton fallback must not exist: %#v, %v", unknown, err)
	}
}

func TestStatusContractExpiryReleaseAndOtherHealth(t *testing.T) {
	for _, release := range []string{"deadline", "unblock"} {
		t.Run(release, func(t *testing.T) {
			r, accounts, now := statusContractFixture(t)
			item := accounts[0]
			known := map[string]bool{item.Identity: true}
			mustHandleUsage(t, r, pluginapi.UsageRecord{AuthID: item.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"60"}}})
			old := serializedModuleStatus(t, r)
			if release == "deadline" {
				now = now.Add(time.Minute)
				r.clock = &fakeClock{now: now}
				dropped, err := consumeStatusCooldowns(old, known, now, 2*time.Minute)
				if err != nil || len(dropped) != 0 {
					t.Fatal("elapsed evidence must be dropped, not synthesized as release")
				}
			} else if err := r.unblock(""); err != nil {
				t.Fatal(err)
			}
			got, err := consumeStatusCooldowns(serializedModuleStatus(t, r), known, now, time.Minute)
			value, present := got[item.Identity]
			if err != nil || !present || value.Active || !value.Until.IsZero() || value.Reason != "" {
				t.Fatalf("fresh no-active projection = %#v, %v", got, err)
			}
		})
	}
	for _, kind := range []string{"suspension", "quota", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			r, accounts, now := statusContractFixture(t)
			item := accounts[0]
			switch kind {
			case "suspension":
				mustHandleUsage(t, r, pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusForbidden}})
			case "quota":
				state := r.snapshot.Quota[item.Identity]
				state.Authoritative = &quotaSnapshot{ObservedAt: now, Weekly: quotaWindow{ConsumedMicrocredits: 60_000 * creditScale, BucketMicrocredits: 60_000 * creditScale, ResetsAt: now.Add(time.Hour)}}
				r.snapshot.Quota[item.Identity] = state
			case "disabled":
				r.snapshot.Accounts[0].Disabled = true
			}
			got, err := consumeStatusCooldowns(serializedModuleStatus(t, r), map[string]bool{item.Identity: true}, now, time.Minute)
			if err != nil || got[item.Identity].Active {
				t.Fatalf("%s must not manufacture a cooldown: %#v, %v", kind, got, err)
			}
		})
	}
}

func TestStatusContractReasonAllowlist(t *testing.T) {
	cases := map[string]string{
		"retry-after header":                 "retry_after",
		"x-ratelimit-reset header":           "reset_header",
		"x-rate-limit-reset header":          "reset_header",
		"ratelimit-reset header":             "reset_header",
		"rate-limit response body":           "reset_body",
		"authoritative quota reset":          "quota_reset_fallback",
		"conservative rate-limit cooldown":   "configured_fallback",
		"test-only-untrusted-persisted-text": "upstream_rate_limit",
	}
	for reason, want := range cases {
		t.Run(want+reason, func(t *testing.T) {
			r, accounts, now := statusContractFixture(t)
			item := accounts[0]
			r.snapshot.Health[item.Identity] = accountHealthState{ExhaustedUntil: now.Add(time.Minute), ExhaustedReason: reason}
			raw := serializedModuleStatus(t, r)
			got, err := consumeStatusCooldowns(raw, map[string]bool{item.Identity: true}, now, time.Minute)
			if err != nil || got[item.Identity].Reason != want || strings.Contains(string(raw), reason) {
				t.Fatalf("closed-vocabulary reason = %#v, %v", got, err)
			}
		})
	}
}

func TestStatusContractConsumerRefusesIncompleteOrStaleData(t *testing.T) {
	r, accounts, now := statusContractFixture(t)
	r.snapshot.Health[accounts[0].Identity] = accountHealthState{ExhaustedUntil: now.Add(10 * time.Minute), ExhaustedReason: "retry-after header"}
	raw := serializedModuleStatus(t, r)
	known := map[string]bool{accounts[0].Identity: true, accounts[1].Identity: true}
	cases := map[string]func(map[string]any, map[string]any, map[string]any){
		"missing generation":     func(b, a, c map[string]any) { delete(b, "generated_at") },
		"malformed generation":   func(b, a, c map[string]any) { b["generated_at"] = "yesterday" },
		"stale":                  func(b, a, c map[string]any) { b["generated_at"] = now.Add(-2 * time.Minute) },
		"future":                 func(b, a, c map[string]any) { b["generated_at"] = now.Add(time.Second) },
		"rejected config":        func(b, a, c map[string]any) { b["status"] = "reconfigure_rejected" },
		"wrong provider":         func(b, a, c map[string]any) { b["plugin"] = "opencode-go" },
		"missing accounts":       func(b, a, c map[string]any) { delete(b, "accounts") },
		"missing identity":       func(b, a, c map[string]any) { delete(a, "identity") },
		"malformed identity":     func(b, a, c map[string]any) { a["identity"] = "not-an-identity" },
		"duplicate identity":     func(b, a, c map[string]any) { b["accounts"] = []any{a, a} },
		"missing cooldown":       func(b, a, c map[string]any) { delete(a, "cooldown") },
		"missing active":         func(b, a, c map[string]any) { delete(c, "active") },
		"malformed active":       func(b, a, c map[string]any) { c["active"] = "true" },
		"missing until":          func(b, a, c map[string]any) { delete(c, "until") },
		"null active until":      func(b, a, c map[string]any) { c["until"] = nil },
		"malformed until":        func(b, a, c map[string]any) { c["until"] = "tomorrow" },
		"expired at generation":  func(b, a, c map[string]any) { c["until"] = now },
		"missing reason":         func(b, a, c map[string]any) { delete(c, "reason") },
		"untrusted reason":       func(b, a, c map[string]any) { c["reason"] = "test-only-arbitrary-text" },
		"missing provenance":     func(b, a, c map[string]any) { delete(c, "source") },
		"wrong provenance":       func(b, a, c map[string]any) { c["source"] = "quota_api" },
		"contradictory inactive": func(b, a, c map[string]any) { c["active"] = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var b map[string]any
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatal(err)
			}
			a := b["accounts"].([]any)[0].(map[string]any)
			mutate(b, a, a["cooldown"].(map[string]any))
			bad, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := consumeStatusCooldowns(bad, known, now, time.Minute); err == nil || got != nil {
				t.Fatalf("must refuse snapshot without partial writes: %#v, %v", got, err)
			}
		})
	}
	for _, bad := range [][]byte{nil, []byte("{"), []byte("null"), append(append([]byte{}, raw...), []byte("{}")...)} {
		if got, err := consumeStatusCooldowns(bad, known, now, time.Minute); err == nil || got != nil {
			t.Fatal("must refuse malformed document")
		}
	}
}
