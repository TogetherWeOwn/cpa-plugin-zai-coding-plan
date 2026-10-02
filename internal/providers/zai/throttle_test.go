package zai

import (
	"errors"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// laneQuarantinePhrase is the model-selection router's lane-exhaustion match
// (plugins/model-selection/src/lane-capacity/run-failure.ts). A request-rate
// throttle must never produce it, or a few seconds of backoff evacuates the
// whole Z.ai lane for fifteen minutes.
var laneQuarantinePhrase = regexp.MustCompile(`(?i)no healthy managed .{0,60}capacity remains`)

const requestRateBody = `{"error":{"code":"1302","message":"Rate limit reached for requests"}}`

func requestRate429(authID, body string, headers http.Header) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{AuthID: authID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests, Body: body}, ResponseHeaders: headers}
}

func assertRateLimitedError(t *testing.T, err error, wantSeconds string) {
	t.Helper()
	var schedulerErr *envelopeError
	if !errors.As(err, &schedulerErr) || schedulerErr.Code != "zai_rate_limited" || !schedulerErr.Retryable || schedulerErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("error = %T %v, want retryable 429 zai_rate_limited", err, err)
	}
	if laneQuarantinePhrase.MatchString(schedulerErr.Message) {
		t.Fatalf("rate-limited message %q matches the router's lane-quarantine phrase", schedulerErr.Message)
	}
	if want := "retry in " + wantSeconds; !regexp.MustCompile(regexp.QuoteMeta(want) + `$`).MatchString(schedulerErr.Message) {
		t.Fatalf("message = %q, want suffix %q", schedulerErr.Message, want)
	}
}

func TestSingleRequestRate429RoutesToHealthySibling(t *testing.T) {
	limited := schedulerAccount("limited", "claude-limited", "openai-limited")
	sibling := schedulerAccount("sibling", "claude-sibling", "openai-sibling")
	runtime := schedulerTestRuntime(schedulerNow, limited, sibling)
	mustHandleUsage(t, runtime, requestRate429(limited.ClaudeAuthID, requestRateBody, nil))

	health, _ := runtime.health(limited.Identity)
	if health.Status != healthThrottled || !health.ResetAt.Equal(schedulerNow.Add(requestRateBackoffBase)) || health.Reason != "request-rate backoff" {
		t.Fatalf("limited health = %#v, want throttled for %v", health, requestRateBackoffBase)
	}
	if health, _ := runtime.health(sibling.Identity); health.Status != healthHealthy && health.Status != "" {
		t.Fatalf("sibling health = %#v, want healthy", health)
	}
	for range 3 {
		response, err := runtime.pick(schedulerRequest(limited.ClaudeAuthID, sibling.ClaudeAuthID))
		if err != nil || !response.Handled || response.AuthID != sibling.ClaudeAuthID {
			t.Fatalf("pick = %#v, err = %v, want the healthy sibling", response, err)
		}
	}
}

func TestAllAccountsRequestRateLimitedIsRetryable429NotLaneLoss(t *testing.T) {
	first := schedulerAccount("first", "claude-first", "openai-first")
	second := schedulerAccount("second", "claude-second", "openai-second")
	runtime := schedulerTestRuntime(schedulerNow, first, second)
	mustHandleUsage(t, runtime, requestRate429(first.ClaudeAuthID, requestRateBody, nil))
	mustHandleUsage(t, runtime, requestRate429(second.OpenAIAuthID, requestRateBody, http.Header{"Retry-After": []string{"3"}}))

	response, err := runtime.pick(schedulerRequest(first.ClaudeAuthID, second.ClaudeAuthID))
	if response.Handled {
		t.Fatalf("rate-limited pick returned a handled response: %#v", response)
	}
	// The earliest sibling deadline wins: the 3s hint, not the 5s backoff.
	assertRateLimitedError(t, err, "3s")

	// A single-account lane (the live zai-lane-1 shape) behaves the same.
	single := schedulerAccount("only", "claude-only", "openai-only")
	runtime = schedulerTestRuntime(schedulerNow, single)
	mustHandleUsage(t, runtime, requestRate429(single.ClaudeAuthID, requestRateBody, nil))
	_, err = runtime.pick(schedulerRequest(single.ClaudeAuthID))
	assertRateLimitedError(t, err, "5s")
}

func TestHardFailureAlongsideThrottleStillReportsRateLimitedButAllHardIsNoCapacity(t *testing.T) {
	throttled := schedulerAccount("throttled", "claude-throttled", "openai-throttled")
	suspended := schedulerAccount("suspended", "claude-suspended", "openai-suspended")
	runtime := schedulerTestRuntime(schedulerNow, throttled, suspended)
	mustHandleUsage(t, runtime, requestRate429(throttled.ClaudeAuthID, requestRateBody, nil))
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: suspended.ClaudeAuthID, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: http.StatusUnauthorized}})
	_, err := runtime.pick(schedulerRequest(throttled.ClaudeAuthID, suspended.ClaudeAuthID))
	assertRateLimitedError(t, err, "5s")

	// Without the throttled account every candidate is hard-impaired: the
	// existing lane-loss error is unchanged.
	_, err = runtime.pick(schedulerRequest(suspended.ClaudeAuthID))
	assertSchedulerError(t, err, "zai_no_capacity")

	exhausted := schedulerAccount("exhausted", "claude-exhausted", "openai-exhausted")
	runtime = schedulerTestRuntime(schedulerNow, exhausted)
	mustHandleUsage(t, runtime, requestRate429(exhausted.ClaudeAuthID, `{"error":{"code":"1308","message":"Usage limit reached"}}`, nil))
	_, err = runtime.pick(schedulerRequest(exhausted.ClaudeAuthID))
	assertSchedulerError(t, err, "zai_no_capacity")
}

func TestThrottledAccountRecoversAfterBackoff(t *testing.T) {
	first := schedulerAccount("first", "claude-first", "openai-first")
	second := schedulerAccount("second", "claude-second", "openai-second")
	runtime := schedulerTestRuntime(schedulerNow, first, second)
	mustHandleUsage(t, runtime, requestRate429(first.ClaudeAuthID, requestRateBody, nil))
	mustHandleUsage(t, runtime, requestRate429(second.ClaudeAuthID, requestRateBody, nil))
	req := schedulerRequest(first.ClaudeAuthID, second.ClaudeAuthID)
	if _, err := runtime.pick(req); err == nil {
		t.Fatal("pick during backoff succeeded")
	}

	runtime.now = func() time.Time { return schedulerNow.Add(requestRateBackoffBase) }
	response, err := runtime.pick(req)
	if err != nil || !response.Handled || response.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin {
		t.Fatalf("pick after backoff = %#v, err = %v, want builtin round robin", response, err)
	}
	for _, item := range []account{first, second} {
		if health, _ := runtime.health(item.Identity); health.Status != healthHealthy {
			t.Fatalf("%s health after backoff = %#v", item.Name, health)
		}
	}
}

func TestRequestRateBackoffLadderEscalatesWhenSustained(t *testing.T) {
	item := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, item)
	now := schedulerNow
	for step, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute} {
		runtime.now = func() time.Time { return now }
		mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
		health, _ := runtime.health(item.Identity)
		if health.Status != healthThrottled || !health.ResetAt.Equal(now.Add(want)) {
			t.Fatalf("step %d health = %#v, want throttled for %v", step+1, health, want)
		}
		now = health.ResetAt
	}
	runtime.now = func() time.Time { return now }
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	health, _ := runtime.health(item.Identity)
	if health.Status != healthExhausted || !health.ResetAt.Equal(now.Add(defaultFallback)) || health.Reason != "conservative rate-limit cooldown" {
		t.Fatalf("sustained 429 health = %#v, want the conservative fallback", health)
	}
	_, err := runtime.pick(schedulerRequest(item.ClaudeAuthID))
	assertSchedulerError(t, err, "zai_no_capacity")
	if state := runtime.snapshot.Health[item.Identity]; state.ThrottleStreak != 0 || !state.ThrottledUntil.IsZero() {
		t.Fatalf("escalation left throttle state behind: %#v", state)
	}
}

func TestBackoffNeverExceedsConfiguredFallback(t *testing.T) {
	for streak, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 5: time.Minute, 9: time.Minute} {
		if got := requestRateBackoff(streak, defaultFallback); got != want {
			t.Fatalf("backoff(%d) = %v, want %v", streak, got, want)
		}
	}
	if got := requestRateBackoff(4, 15*time.Second); got != 15*time.Second {
		t.Fatalf("backoff above a 15s fallback = %v", got)
	}
}

func TestInFlightBurstDoesNotAdvanceStreak(t *testing.T) {
	item := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, item)
	for range 8 {
		mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	}
	state := runtime.snapshot.Health[item.Identity]
	if state.ThrottleStreak != 1 || !state.ThrottledUntil.Equal(schedulerNow.Add(requestRateBackoffBase)) || !state.ExhaustedUntil.IsZero() {
		t.Fatalf("burst state = %#v, want one 5s step and no exhaustion", state)
	}
	// A later hint inside the active window extends it; an earlier one never shortens it.
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, http.Header{"Retry-After": []string{"30"}}))
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, http.Header{"Retry-After": []string{"2"}}))
	state = runtime.snapshot.Health[item.Identity]
	if state.ThrottleStreak != 1 || !state.ThrottledUntil.Equal(schedulerNow.Add(30*time.Second)) {
		t.Fatalf("hinted burst state = %#v", state)
	}
}

func TestSuccessOrQuietPeriodResetsStreak(t *testing.T) {
	item := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, item)
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	runtime.now = func() time.Time { return schedulerNow.Add(time.Second) }
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: item.ClaudeAuthID})
	if state := runtime.snapshot.Health[item.Identity]; state.ThrottleStreak != 1 {
		t.Fatalf("an in-flight success during the backoff must not end it: %#v", state)
	}
	runtime.now = func() time.Time { return schedulerNow.Add(requestRateBackoffBase) }
	mustHandleUsage(t, runtime, pluginapi.UsageRecord{AuthID: item.ClaudeAuthID})
	if state := runtime.snapshot.Health[item.Identity]; state.ThrottleStreak != 0 || !state.ThrottledUntil.IsZero() {
		t.Fatalf("success after the backoff left a streak: %#v", state)
	}

	now := schedulerNow.Add(time.Minute)
	runtime.now = func() time.Time { return now }
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	now = now.Add(requestRateBackoffBase)
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	if state := runtime.snapshot.Health[item.Identity]; state.ThrottleStreak != 2 {
		t.Fatalf("back-to-back streak = %#v", state)
	}
	now = runtime.snapshot.Health[item.Identity].ThrottledUntil.Add(requestRateStreakDecay)
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	if state := runtime.snapshot.Health[item.Identity]; state.ThrottleStreak != 1 || !state.ThrottledUntil.Equal(now.Add(requestRateBackoffBase)) {
		t.Fatalf("decayed streak = %#v, want a fresh first step", state)
	}
}

func TestOnlyDocumentedRequestRateCodesThrottle(t *testing.T) {
	cases := map[string]bool{
		requestRateBody: true,
		`{"error":{"code":"1305","message":"The service may be temporarily overloaded"}}`: true,
		`{"error":{"code":1302,"message":"numeric code"}}`:                                true,
		`{"error":{"code":" 1302 ","message":"padded"}}`:                                  true,
		`{"error":{"code":"1308","message":"Usage limit reached"}}`:                       false,
		`{"error":{"code":"1310","message":"Weekly/Monthly Limit Exhausted"}}`:            false,
		`{"error":{"code":"1313","message":"Fair use"}}`:                                  false,
		`{"error":{"code":"1113","message":"Insufficient balance"}}`:                      false,
		`{"error":{"code":"9999","message":"unknown"}}`:                                   false,
		`{"code":"1302","message":"wrong shape"}`:                                         false,
		`{"error":{"code":true}}`:                                                         false,
		`{"error":{"code":"1302"}} {"trailing":true}`:                                     false,
		`Rate limit reached for requests`:                                                 false,
		``:                                                                                false,
	}
	for body, wantThrottle := range cases {
		item := schedulerAccount("one", "claude-one", "openai-one")
		runtime := schedulerTestRuntime(schedulerNow, item)
		mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, body, nil))
		health, _ := runtime.health(item.Identity)
		if wantThrottle && health.Status != healthThrottled {
			t.Fatalf("body %q health = %#v, want throttled", body, health)
		}
		if !wantThrottle && (health.Status != healthExhausted || !health.ResetAt.Equal(schedulerNow.Add(defaultFallback))) {
			t.Fatalf("body %q health = %#v, want the conservative fallback", body, health)
		}
	}
	if transientRateLimit(pluginapi.UsageRecord{Failure: pluginapi.UsageFailure{StatusCode: http.StatusServiceUnavailable, Body: requestRateBody}}) {
		t.Fatal("a non-429 status with a 1302 body is not a request-rate throttle")
	}
}

func TestRequestRateResetHintsShortThrottleLongExhaust(t *testing.T) {
	item := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, item)
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, http.Header{"Retry-After": []string{"45"}}))
	health, _ := runtime.health(item.Identity)
	if health.Status != healthThrottled || !health.ResetAt.Equal(schedulerNow.Add(45*time.Second)) || health.Reason != "request-rate retry hint" {
		t.Fatalf("short hint health = %#v", health)
	}

	runtime = schedulerTestRuntime(schedulerNow, item)
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, http.Header{"Retry-After": []string{"600"}}))
	health, _ = runtime.health(item.Identity)
	if health.Status != healthExhausted || !health.ResetAt.Equal(schedulerNow.Add(10*time.Minute)) || health.Reason != "retry-after header" {
		t.Fatalf("long hint health = %#v, want the hinted exhaustion", health)
	}
}

func TestRequestRateOnSpentAllowanceKeepsExhaustionPath(t *testing.T) {
	item := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, item)
	state := runtime.snapshot.Health[item.Identity]
	state.CapacityExhausted = true
	runtime.snapshot.Health[item.Identity] = state
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	if state := runtime.snapshot.Health[item.Identity]; state.ThrottleStreak != 0 || !state.ExhaustedUntil.After(schedulerNow) {
		t.Fatalf("spent-allowance 429 state = %#v, want exhaustion and no throttle", state)
	}
}

func TestUnblockClearsThrottle(t *testing.T) {
	runtime, accounts, _ := statusContractFixture(t)
	item := accounts[0]
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	if err := runtime.unblock(item.Name); err != nil {
		t.Fatal(err)
	}
	if state := runtime.snapshot.Health[item.Identity]; state.ThrottleStreak != 0 || !state.ThrottledUntil.IsZero() || state.ThrottleReason != "" {
		t.Fatalf("unblock left throttle state: %#v", state)
	}
	if health, _ := runtime.health(item.Identity); health.Status != healthHealthy {
		t.Fatalf("health after unblock = %#v", health)
	}
}

func TestManagementReportsThrottleWithoutCooldown(t *testing.T) {
	item := schedulerAccount("one", "claude-one", "openai-one")
	runtime := schedulerTestRuntime(schedulerNow, item)
	mustHandleUsage(t, runtime, requestRate429(item.ClaudeAuthID, requestRateBody, nil))
	status := runtime.managementStatus("registered")
	if len(status.Accounts) != 1 || status.Accounts[0].Health != healthThrottled || status.Accounts[0].Cooldown.Active {
		t.Fatalf("management status = %#v, want throttled health and no exported cooldown", status.Accounts)
	}
}
