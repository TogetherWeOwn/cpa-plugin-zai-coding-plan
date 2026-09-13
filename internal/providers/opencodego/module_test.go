package opencodego

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/providermodule"
)

func configuredModule(t *testing.T, threshold int) *Module {
	t.Helper()
	module := NewModule()
	raw := json.RawMessage(`{"threshold-percent":` + jsonNumber(threshold) + `,"accounts":[{"name":"go-a","auth-ids":["go-a-1"]},{"name":"go-b","auth-ids":["go-b-1"]}]}`)
	if err := module.Reconfigure(context.Background(), providermodule.HostConfig{}, raw); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	return module
}

func jsonNumber(value int) string { return strconv.Itoa(value) }

func TestModuleImplementsCurrentInterface(t *testing.T) {
	var module providermodule.Module = NewModule()
	if module.ID() != providerID {
		t.Fatalf("ID() = %q, want %q", module.ID(), providerID)
	}
}

func TestRecognizeOpenCodeButNeverClaimsUnqualifiedGLM(t *testing.T) {
	module := configuredModule(t, 97)
	if !module.Recognize(pluginapi.SchedulerAuthCandidate{Provider: providerID}) {
		t.Fatal("Recognize() = false for OpenCode Go candidate")
	}
	for _, candidate := range []pluginapi.SchedulerAuthCandidate{
		{Provider: "zai", Attributes: map[string]string{"model": "glm-5.3"}},
		{Attributes: map[string]string{"provider_key": "zai", "model_id": "glm-5.3"}},
		{Attributes: map[string]string{"compat_name": "zai-coding-plan", "requested_model": "GLM_5"}},
	} {
		if module.Recognize(candidate) {
			t.Fatalf("Recognize(%#v) = true, want false for non-OpenCode GLM", candidate)
		}
	}
}

func TestOpenCodeProviderIdentityAllowsObservedGLMFamily(t *testing.T) {
	module := configuredModule(t, 97)
	candidate := pluginapi.SchedulerAuthCandidate{ID: "go-a-1", Provider: providerID, Attributes: map[string]string{"model": "opencode-go/glm-5.3"}}
	if !module.Recognize(candidate) {
		t.Fatal("Recognize() = false for provider-qualified OpenCode Go GLM candidate")
	}
	if module.Recognize(pluginapi.SchedulerAuthCandidate{ID: "go-a-1", Provider: "zai", Attributes: map[string]string{"model": "glm-5.3"}}) {
		t.Fatal("Recognize() claimed a managed auth ID when the candidate explicitly belongs to Z.ai")
	}
	resp, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{Provider: providerID, Model: "glm-5.3", Candidates: []pluginapi.SchedulerAuthCandidate{candidate}})
	if err != nil || !resp.Handled || resp.AuthID != "go-a-1" {
		t.Fatalf("Pick(provider-qualified GLM) = %#v, %v", resp, err)
	}
}

func TestUnqualifiedGLMRequestIsNotClaimedByOpenCodeModule(t *testing.T) {
	module := configuredModule(t, 97)
	resp, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Model:      "glm-5.3",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "foreign-glm", Provider: "zai"}},
	})
	if err != nil || resp.Handled {
		t.Fatalf("Pick(unqualified GLM) = %#v, %v, want unhandled", resp, err)
	}
}

func TestRecognizedUnmanagedAndAllImpairedFailClosed(t *testing.T) {
	module := configuredModule(t, 97)
	_, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "missing", Provider: providerID}}})
	if err == nil || !strings.Contains(err.Error(), "opencode_go_unmanaged_candidate") {
		t.Fatalf("unmanaged Pick() error = %v", err)
	}

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for _, authID := range []string{"go-a-1", "go-b-1"} {
		record := monthlyLimitRecord(authID, now, 3600)
		if err := module.HandleUsage(context.Background(), record); err != nil {
			t.Fatalf("HandleUsage() error = %v", err)
		}
	}
	_, err = module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"now": now}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "go-a-1"}, {ID: "go-b-1"}},
	})
	if err == nil || !strings.Contains(err.Error(), "opencode_go_no_capacity") {
		t.Fatalf("all-impaired Pick() error = %v", err)
	}
}

func TestReconfigureFailureRetainsPriorSnapshot(t *testing.T) {
	module := configuredModule(t, 97)
	err := module.Reconfigure(context.Background(), providermodule.HostConfig{}, json.RawMessage(`{"unknown":true}`))
	if err == nil {
		t.Fatal("invalid Reconfigure() = nil error")
	}
	if got := module.OwnedAuthIDs(context.Background()); len(got) != 2 || got[0] != "go-a-1" || got[1] != "go-b-1" {
		t.Fatalf("OwnedAuthIDs() after rejected config = %#v, want prior snapshot", got)
	}
	resp, pickErr := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "go-a-1"}}})
	if pickErr != nil || !resp.Handled || resp.AuthID != "go-a-1" {
		t.Fatalf("Pick() after rejected config = %#v, %v", resp, pickErr)
	}
}

func TestSuccessfulReconfigureCarriesForwardLiveWindowState(t *testing.T) {
	module := configuredModule(t, 97)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	if err := module.HandleUsage(context.Background(), monthlyLimitRecord("go-a-1", now, 3600)); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"threshold-percent":97,"accounts":[{"name":"go-a","auth-ids":["go-a-1"]},{"name":"go-b","auth-ids":["go-b-1"]}]}`)
	if err := module.Reconfigure(context.Background(), providermodule.HostConfig{}, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"now": now}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "go-a-1"}, {ID: "go-b-1"}},
	})
	if err != nil || resp.AuthID != "go-b-1" {
		t.Fatalf("Pick() after same-account reconfigure = %#v, %v", resp, err)
	}
}

func TestQuotaWindowWithoutResetUsesConservativeCooldown(t *testing.T) {
	module := configuredModule(t, 97)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		AuthID:      "go-a-1",
		RequestedAt: now,
		Failed:      true,
		Failure: pluginapi.UsageFailure{
			StatusCode: http.StatusTooManyRequests,
			Body:       `[opencode-go/qwen] [429]: Monthly usage limit reached.`,
		},
	}
	if err := module.HandleUsage(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	window := module.state.Accounts["go-a"].Windows[windowMonthly]
	module.mu.Unlock()
	if !window.Exhausted || !window.ResetAt.Equal(now.Add(defaultFailureCooldown)) {
		t.Fatalf("window = %#v, want conservative cooldown", window)
	}
}

func TestWindowSignalsImpairOnlyTheirGoverningWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		body   string
		window windowKind
	}{
		{"five-hour", `[opencode-go/qwen] [429]: Five-hour usage limit reached. Resets in 4hr 59min. To continue using this model now, enable us (reset after 4h 59m 1s)`, windowFiveHour},
		{"weekly", `[opencode-go/qwen] [429]: Weekly usage limit reached. Resets in 6d 2hr. To continue using this model now, enable us (reset after 6d 6h 1m 2s)`, windowWeekly},
		{"monthly", `[opencode-go/qwen] [429]: Monthly usage limit reached. Resets in 2hr 37min. To continue using this model now, enable us (reset after 2h 37m 57s)`, windowMonthly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			module := configuredModule(t, 97)
			record := pluginapi.UsageRecord{
				AuthID:          "go-a-1",
				RequestedAt:     now,
				Failed:          true,
				Failure:         pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: tc.body},
				ResponseHeaders: http.Header{"Retry-After": []string{"9477"}},
			}
			if err := module.HandleUsage(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			module.mu.Lock()
			account := module.state.Accounts["go-a"]
			for _, kind := range []windowKind{windowFiveHour, windowWeekly, windowMonthly} {
				got := account.Windows[kind]
				if kind == tc.window && (!got.Exhausted || !got.ResetAt.Equal(now.Add(9477*time.Second))) {
					t.Fatalf("window %s = %#v, want exhausted at Retry-After reset", kind, got)
				}
				if kind != tc.window && got.Exhausted {
					t.Fatalf("window %s was impaired by %s response", kind, tc.window)
				}
			}
			module.mu.Unlock()
		})
	}
}

func TestRetryAfterLookupIsCaseInsensitive(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	resetAt, ok := retryAfterReset(http.Header{"retry-after": []string{"9477"}}, now)
	if !ok || !resetAt.Equal(now.Add(9477*time.Second)) {
		t.Fatalf("retryAfterReset() = %v, %v", resetAt, ok)
	}
}

func TestBodyResetParsesDayHourMinuteSecondShape(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	resetAt, ok := bodyReset(`Weekly usage limit reached (reset after 6d 6h 1m 2s)`, now)
	if !ok || !resetAt.Equal(now.Add((6*24+6)*time.Hour+time.Minute+2*time.Second)) {
		t.Fatalf("bodyReset() = %v, %v", resetAt, ok)
	}
}

func TestMicrothrottleDoesNotFeedPacingState(t *testing.T) {
	module := configuredModule(t, 97)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		AuthID:      "go-a-1",
		RequestedAt: now,
		Failed:      true,
		Failure: pluginapi.UsageFailure{
			StatusCode: http.StatusForbidden,
			Body:       `[opencode-go/qwen] [403]: <!doctype html> (reset after 2s)`,
		},
	}
	if err := module.HandleUsage(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	defer module.mu.Unlock()
	for kind, window := range module.state.Accounts["go-a"].Windows {
		if window.Exhausted || window.Utilization != 0 || !window.ResetAt.IsZero() {
			t.Fatalf("microthrottle changed %s state: %#v", kind, window)
		}
	}
}

func TestDegradedPickExcludesExhaustedAccount(t *testing.T) {
	module := configuredModule(t, 97)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	if err := module.HandleUsage(context.Background(), monthlyLimitRecord("go-a-1", now, 3600)); err != nil {
		t.Fatal(err)
	}
	resp, err := module.Pick(context.Background(), pluginapi.SchedulerPickRequest{
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"now": now}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "go-a-1"}, {ID: "go-b-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "go-b-1" || resp.DelegateBuiltin != "" {
		t.Fatalf("Pick() = %#v, want healthy account go-b-1", resp)
	}
}

func TestStatusDeclaresObservationGaps(t *testing.T) {
	module := configuredModule(t, 97)
	raw, err := module.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "five-hour and weekly enforcement") || !strings.Contains(text, "dashboard payload was unavailable") {
		t.Fatalf("Status() omitted clean-room observation gaps: %s", raw)
	}
}

func TestStatusClearsExpiredWindows(t *testing.T) {
	module := configuredModule(t, 97)
	module.mu.Lock()
	module.state.Accounts["go-a"].Windows[windowMonthly] = windowState{
		Utilization: 1,
		Exhausted:   true,
		ResetAt:     time.Now().UTC().Add(-time.Minute),
	}
	module.mu.Unlock()
	if _, err := module.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	module.mu.Lock()
	window := module.state.Accounts["go-a"].Windows[windowMonthly]
	module.mu.Unlock()
	if window.Exhausted || window.Utilization != 0 || !window.ResetAt.IsZero() {
		t.Fatalf("Status() retained expired window: %#v", window)
	}
}

func monthlyLimitRecord(authID string, now time.Time, seconds int) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		AuthID:          authID,
		RequestedAt:     now,
		Failed:          true,
		Failure:         pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: `[opencode-go/qwen] [429]: Monthly usage limit reached. Resets in 1hr. To continue using this model now, enable us (reset after 1h)`},
		ResponseHeaders: http.Header{"Retry-After": []string{strconv.Itoa(seconds)}},
	}
}
