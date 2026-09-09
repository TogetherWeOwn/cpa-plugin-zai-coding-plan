package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var schedulerNow = time.Date(2026, time.September, 9, 1, 0, 0, 0, time.UTC)

func TestUsageFailureImpairsBothPairedCredentials(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})

	for _, authID := range []string{account.ClaudeAuthID, account.OpenAIAuthID} {
		_, err := runtime.pick(schedulerRequest(authID))
		assertSchedulerError(t, err, "zai_no_capacity")
	}
	health, ok := runtime.health(account.Identity)
	if !ok || health.Status != healthSuspended || !health.ResetAt.Equal(schedulerNow.Add(defaultSuspend)) {
		t.Fatalf("health = %#v, ok = %v", health, ok)
	}
}

func TestRepeatedFailuresExtendNeverShortenBlocks(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"3600"}}})
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"60"}}})
	health, _ := runtime.health(account.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(time.Hour)) {
		t.Fatalf("rate-limit reset = %v, want %v", health.ResetAt, schedulerNow.Add(time.Hour))
	}

	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"7200"}}})
	health, _ = runtime.health(account.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(2 * time.Hour)) {
		t.Fatalf("extended reset = %v, want %v", health.ResetAt, schedulerNow.Add(2*time.Hour))
	}
}

func TestResetHintsAreBoundedAndUseAuthoritativeThenFallback(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	generation := runtime.snapshot.Generation
	if !runtime.updateCapacity(generation, account.Identity, capacityUpdate{ResetAt: schedulerNow.Add(3 * time.Hour), Source: "authoritative quota"}) {
		t.Fatal("capacity update rejected")
	}
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: `{"retry_after":999999999999}`}, ResponseHeaders: http.Header{"Retry-After": []string{"999999999999"}}})
	health, _ := runtime.health(account.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(3*time.Hour)) || health.Reason != "authoritative quota reset" {
		t.Fatalf("authoritative reset not chosen: %#v", health)
	}

	other := schedulerAccount("two", "claude-two", "openai-two")
	runtime = schedulerTestRuntime(schedulerNow, other)
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: other.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: strings.Repeat("x", maxFailureBodyBytes+1)}})
	health, _ = runtime.health(other.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(defaultFallback)) || health.Reason != "conservative rate-limit cooldown" {
		t.Fatalf("fallback reset = %#v", health)
	}
}

func TestRetryAfterMillisecondsUsesRelativeDuration(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: `{"retry_after_ms":1800000}`}})
	health, _ := runtime.health(account.Identity)
	if !health.ResetAt.Equal(schedulerNow.Add(30 * time.Minute)) {
		t.Fatalf("rate-limit reset = %v, want %v", health.ResetAt, schedulerNow.Add(30*time.Minute))
	}
}

func TestAuthSuspensionPrecedesExhaustionAndBothRecover(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: http.Header{"Retry-After": []string{"7200"}}})
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusForbidden}})
	health, _ := runtime.health(account.Identity)
	if health.Status != healthSuspended {
		t.Fatalf("health = %#v, want suspension precedence", health)
	}

	runtime.now = func() time.Time { return schedulerNow.Add(defaultSuspend + time.Second) }
	health, _ = runtime.health(account.Identity)
	if health.Status != healthExhausted {
		t.Fatalf("health = %#v, want remaining exhaustion", health)
	}
	runtime.now = func() time.Time { return schedulerNow.Add(2*time.Hour + time.Second) }
	health, _ = runtime.health(account.Identity)
	if health.Status != healthHealthy {
		t.Fatalf("health = %#v, want recovered", health)
	}
}

func TestCapacityGenerationBoundaryAndRecovery(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	oldGeneration := runtime.snapshot.Generation
	staged := schedulerSnapshot(account)
	if err := runtime.commitSnapshot(staged); err != nil {
		t.Fatal(err)
	}
	if runtime.updateCapacity(oldGeneration, account.Identity, capacityUpdate{Exhausted: true, ResetAt: schedulerNow.Add(time.Hour), Source: "stale"}) {
		t.Fatal("stale generation mutated replacement snapshot")
	}
	if !runtime.updateCapacity(runtime.snapshot.Generation, account.Identity, capacityUpdate{Exhausted: true, ResetAt: schedulerNow.Add(time.Hour), Source: "authoritative"}) {
		t.Fatal("current generation update rejected")
	}
	if health, _ := runtime.health(account.Identity); health.Status != healthExhausted {
		t.Fatalf("health = %#v, want exhausted", health)
	}
	if !runtime.updateCapacity(runtime.snapshot.Generation, account.Identity, capacityUpdate{Exhausted: false, Source: "authoritative"}) {
		t.Fatal("recovery update rejected")
	}
	if health, _ := runtime.health(account.Identity); health.Status != healthHealthy {
		t.Fatalf("health = %#v, want healthy", health)
	}
}

func TestSchedulerHealthyDelegatesBuiltinRoundRobin(t *testing.T) {
	accounts := []account{
		schedulerAccount("one", "claude-one", "openai-one"),
		schedulerAccount("two", "claude-two", "openai-two"),
	}
	runtime := schedulerTestRuntime(schedulerNow, accounts...)
	response, err := runtime.pick(schedulerRequest("claude-one", "claude-two"))
	if err != nil || !response.Handled || response.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin || response.AuthID != "" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
}

func TestSchedulerDegradedExcludesSiblingsAndRoundRobinsHealthy(t *testing.T) {
	impaired := schedulerAccount("impaired", "claude-bad", "openai-bad")
	first := schedulerAccount("first", "claude-one", "openai-one")
	second := schedulerAccount("second", "claude-two", "openai-two")
	runtime := schedulerTestRuntime(schedulerNow, impaired, first, second)
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: impaired.OpenAIAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	req := schedulerRequest(impaired.ClaudeAuthID, impaired.OpenAIAuthID, first.ClaudeAuthID, second.ClaudeAuthID)

	one, err := runtime.pick(req)
	if err != nil {
		t.Fatal(err)
	}
	two, err := runtime.pick(req)
	if err != nil {
		t.Fatal(err)
	}
	three, err := runtime.pick(req)
	if err != nil {
		t.Fatal(err)
	}
	if one.AuthID != first.ClaudeAuthID || two.AuthID != second.ClaudeAuthID || three.AuthID != first.ClaudeAuthID {
		t.Fatalf("round robin = %q, %q, %q", one.AuthID, two.AuthID, three.AuthID)
	}
}

func TestSchedulerStickinessSurvivesUntilCandidateIsImpaired(t *testing.T) {
	bad := schedulerAccount("bad", "claude-bad", "openai-bad")
	first := schedulerAccount("first", "claude-first", "openai-first")
	second := schedulerAccount("second", "claude-second", "openai-second")
	sticky := schedulerAccount("sticky", "claude-sticky", "openai-sticky")
	runtime := schedulerTestRuntime(schedulerNow, bad, first, second, sticky)
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: bad.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusForbidden}})
	req := schedulerRequest(bad.ClaudeAuthID, first.ClaudeAuthID, second.ClaudeAuthID, sticky.ClaudeAuthID)
	req.Options.Headers = map[string][]string{"session_id": []string{"s6"}}

	one, err := runtime.pick(req)
	if err != nil || one.AuthID != sticky.ClaudeAuthID {
		t.Fatalf("initial sticky pick = %#v, err = %v", one, err)
	}
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: first.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	two, err := runtime.pick(req)
	if err != nil || two.AuthID != sticky.ClaudeAuthID {
		t.Fatalf("unrelated impairment changed sticky pick = %#v, err = %v", two, err)
	}
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: sticky.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	three, err := runtime.pick(req)
	if err != nil || three.AuthID == sticky.ClaudeAuthID || three.AuthID == bad.ClaudeAuthID || three.AuthID == first.ClaudeAuthID {
		t.Fatalf("failover pick = %#v, err = %v", three, err)
	}
}

func TestSchedulerUnmanagedAndAllImpairedPaths(t *testing.T) {
	account := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, account)
	response, err := runtime.pick(schedulerRequest("unmanaged"))
	if err != nil || response.Handled {
		t.Fatalf("unmanaged response = %#v, err = %v", response, err)
	}

	_, err = runtime.pick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "unknown-zai", Attributes: map[string]string{"base_url": zaiAnthropicBaseURL}}}})
	assertSchedulerError(t, err, "zai_unmanaged_candidate")

	runtime.handleUsage(pluginapi.UsageRecord{AuthID: account.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests}})
	response, err = runtime.pick(schedulerRequest(account.ClaudeAuthID, "unmanaged"))
	if response.Handled {
		t.Fatalf("hard error returned handled response: %#v", response)
	}
	assertSchedulerError(t, err, "zai_no_capacity")
}

func TestSchedulerPickNeverCallsExternalDependencies(t *testing.T) {
	bad := schedulerAccount("bad", "claude-bad", "openai-bad")
	good := schedulerAccount("good", "claude-good", "openai-good")
	runtime := schedulerTestRuntime(schedulerNow, bad, good)
	runtime.handleUsage(pluginapi.UsageRecord{AuthID: bad.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})

	done := make(chan error, 1)
	go func() {
		_, err := runtime.pick(schedulerRequest(bad.ClaudeAuthID, good.ClaudeAuthID))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler critical section blocked on an external dependency")
	}
}

func TestValidateSchedulerDeploymentFailsClosed(t *testing.T) {
	enabled := true
	disabled := false
	tests := []struct {
		name    string
		plugins cpaPluginsProjection
	}{
		{name: "plugins disabled", plugins: cpaPluginsProjection{}},
		{name: "plugin disabled", plugins: cpaPluginsProjection{Enabled: true, Configs: map[string]cpaPluginProjection{pluginID: {Enabled: &disabled, Priority: requiredPluginPriority}}}},
		{name: "wrong priority", plugins: cpaPluginsProjection{Enabled: true, Configs: map[string]cpaPluginProjection{pluginID: {Enabled: &enabled, Priority: 999}}}},
		{name: "lower competitor", plugins: schedulerPlugins(&enabled, "zz-lower", 1)},
		{name: "higher competitor", plugins: schedulerPlugins(&enabled, "aa-higher", 2000)},
		{name: "equal competitor wins tie", plugins: schedulerPlugins(&enabled, "aa-equal", requiredPluginPriority)},
		{name: "equal competitor loses tie", plugins: schedulerPlugins(&enabled, "zz-equal", requiredPluginPriority)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateSchedulerDeployment(tt.plugins); err == nil {
				t.Fatal("non-exclusive scheduler deployment succeeded")
			}
		})
	}
	valid := cpaPluginsProjection{Enabled: true, Configs: map[string]cpaPluginProjection{pluginID: {Enabled: &enabled, Priority: requiredPluginPriority}, "disabled": {Enabled: &disabled, Priority: 5000}}}
	if err := validateSchedulerDeployment(valid); err != nil {
		t.Fatalf("valid exclusive deployment rejected: %v", err)
	}
}

func schedulerPlugins(enabled *bool, competitor string, priority int) cpaPluginsProjection {
	return cpaPluginsProjection{Enabled: true, Configs: map[string]cpaPluginProjection{
		pluginID:   {Enabled: enabled, Priority: requiredPluginPriority},
		competitor: {Enabled: enabled, Priority: priority},
	}}
}

func schedulerAccount(name, claudeID, openAIID string) account {
	return account{Identity: accountIdentity(name), Name: name, Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: claudeID, OpenAIAuthID: openAIID}
}

func schedulerSnapshot(accounts ...account) *runtimeSnapshot {
	return newRuntimeSnapshot(pluginConfig{SuspendDuration: defaultSuspend, FallbackCooldown: defaultFallback}, accounts, nil)
}

func schedulerTestRuntime(now time.Time, accounts ...account) *pluginRuntime {
	runtime := &pluginRuntime{now: func() time.Time { return now }}
	if err := runtime.commitSnapshot(schedulerSnapshot(accounts...)); err != nil {
		panic(err)
	}
	return runtime
}

func schedulerRequest(authIDs ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{Provider: "claude", Model: "glm-5.3"}
	for _, authID := range authIDs {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: authID})
	}
	return req
}

func assertSchedulerError(t *testing.T, err error, code string) {
	t.Helper()
	var schedulerErr *envelopeError
	if !errors.As(err, &schedulerErr) || schedulerErr.Code != code || schedulerErr.Retryable {
		t.Fatalf("error = %T %v, want non-retryable %s", err, err, code)
	}
}
