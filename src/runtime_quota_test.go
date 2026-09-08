package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	sleeps   []time.Duration
	releases chan struct{}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Sleep(ctx context.Context, delay time.Duration) error {
	clock.mu.Lock()
	clock.sleeps = append(clock.sleeps, delay)
	clock.mu.Unlock()
	if clock.releases == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-clock.releases:
		clock.mu.Lock()
		clock.now = clock.now.Add(delay)
		clock.mu.Unlock()
		return nil
	}
}

func (clock *fakeClock) set(now time.Time) {
	clock.mu.Lock()
	clock.now = now
	clock.mu.Unlock()
}

func TestRuntimeUsageAccountingIsolationAndDedupIntegrity(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	first := account{Identity: accountIdentity("first"), Name: "first", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "first-auth"}
	second := account{Identity: accountIdentity("second"), Name: "second", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "second-auth"}
	runtime := quotaTestRuntime(t, now, []account{first, second})
	record := pluginapi.UsageRecord{AuthID: first.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 10_000}}
	if err := runtime.handleUsage(record); err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleUsage(record); err != nil {
		t.Fatal(err)
	}
	firstView, _ := runtime.quotaView(first.Identity)
	secondView, _ := runtime.quotaView(second.Identity)
	if firstView.FiveHour.ConsumedMicrocredits != 3_450_000 || secondView.FiveHour.ConsumedMicrocredits != 0 {
		t.Fatalf("isolated views = %#v / %#v", firstView, secondView)
	}
	if !firstView.DedupCollisionWarn || !firstView.DeliveryWarning || firstView.Source != "estimated" {
		t.Fatalf("integrity view = %#v", firstView)
	}
}

func TestRuntimeUnknownModelAndPersistenceFailureSurfaceIntegrity(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.persist = func(*secureStore, persistedState) error { return errors.New("disk unavailable") }
	if err := runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "unknown", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 1}}); err == nil {
		t.Fatal("persistence failure was not returned internally")
	}
	view, _ := runtime.quotaView(item.Identity)
	if !view.UnknownModelWarning || !view.PersistenceWarning || !view.DeliveryWarning {
		t.Fatalf("integrity view = %#v", view)
	}
}

func TestAuthoritativeQuotaReplacesEstimateAndFailureRetainsLastGood(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth", key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	if err := runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 10_000}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runtime.httpClient = roundTripDoer(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return quotaHTTPResponse(200, quotaFixture("pro", []string{
				quotaLimitFixture(3, 5, 12_000, 6_000, 6_000, now.Add(time.Hour).UnixMilli()),
				quotaLimitFixture(6, 1, 60_000, 18_000, 42_000, now.Add(24*time.Hour).UnixMilli()),
			})), nil
		}
		return quotaHTTPResponse(503, quotaFixtureKey), nil
	})
	if err := runtime.pollOnce(context.Background(), item.Identity, item.key); err != nil {
		t.Fatal(err)
	}
	view, _ := runtime.quotaView(item.Identity)
	if view.Source != "authoritative" || view.FiveHour.ConsumedMicrocredits != 6_000*creditScale {
		t.Fatalf("authoritative view = %#v", view)
	}
	if err := runtime.pollOnce(context.Background(), item.Identity, item.key); err == nil {
		t.Fatal("non-200 poll succeeded")
	}
	view, _ = runtime.quotaView(item.Identity)
	if view.Source != "authoritative" || view.FiveHour.ConsumedMicrocredits != 6_000*creditScale || view.Warning == "" {
		t.Fatalf("last good snapshot erased: %#v", view)
	}
	fake := runtime.clock.(*fakeClock)
	fake.set(now.Add(6 * time.Minute))
	view, _ = runtime.quotaView(item.Identity)
	if view.Source != "estimated" || !view.Stale || view.FiveHour.ConsumedMicrocredits != 3_450_000 {
		t.Fatalf("fallback view = %#v", view)
	}
}

func TestUsageHandleAcknowledgesLossyPersistenceFailure(t *testing.T) {
	previous := runtimeState
	defer func() { runtimeState = previous }()
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	runtimeState = quotaTestRuntime(t, now, []account{item})
	runtimeState.persist = func(*secureStore, persistedState) error { return errors.New("disk unavailable") }
	raw, err := json.Marshal(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 1}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := usageHandle(raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(response, &envelope); err != nil || !envelope.OK {
		t.Fatalf("usage response = %s, err = %v", response, err)
	}
	view, _ := runtimeState.quotaView(item.Identity)
	if !view.PersistenceWarning {
		t.Fatalf("persistence warning missing: %#v", view)
	}
}

func TestRuntimeShutdownCancelsAndJoinsPollWorkers(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.clock = clock
	started := make(chan struct{})
	runtime.httpClient = roundTripDoer(func(request *http.Request) (*http.Response, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	runtime.cancel = cancel
	runtime.startPollers(ctx, runtime.snapshot)
	<-started
	done := make(chan error, 1)
	go func() { done <- runtime.shutdown() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join cancelled poll worker")
	}
}

func TestPersistedStateAndManagementStatusNeverContainKey(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity(quotaFixtureKey), Name: "account", KeySuffix: "redacted", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth", key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	if err := runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	runtime.mu.RLock()
	persisted, err := json.Marshal(runtime.persistenceSnapshotLocked())
	runtime.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	status, err := json.Marshal(runtime.managementStatus("registered"))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{persisted, status} {
		if strings.Contains(string(raw), quotaFixtureKey) {
			t.Fatalf("serialized output leaked key: %s", raw)
		}
	}
}

func quotaTestRuntime(t *testing.T, now time.Time, accounts []account) *pluginRuntime {
	t.Helper()
	store, err := newSecureStore(filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	quota := make(map[string]accountQuotaState, len(accounts))
	byAuthID := make(map[string]string, len(accounts)*2)
	for _, item := range accounts {
		quota[item.Identity] = accountQuotaState{CompleteSince: now}
		byAuthID[item.ClaudeAuthID] = item.Identity
		byAuthID[item.OpenAIAuthID] = item.Identity
	}
	return &pluginRuntime{
		clock: &fakeClock{now: now},
		snapshot: &runtimeSnapshot{
			Config:   pluginConfig{QuotaRefresh: 2 * time.Minute, AuthoritativeMaxAge: 5 * time.Minute, ThresholdPercent: 97, StateRetention: 8 * 24 * time.Hour},
			Accounts: accounts,
			Store:    store,
			Quota:    quota,
			byAuthID: byAuthID,
		},
	}
}

func TestUsageArrivingOutOfOrderStaysPersistable(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	runtime := quotaTestRuntime(t, now, []account{item})
	later := pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 10_000}}
	earlier := pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now.Add(-time.Minute), Detail: pluginapi.UsageDetail{InputTokens: 2_000}}
	if err := runtime.handleUsage(later); err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleUsage(earlier); err != nil {
		t.Fatalf("late-arriving record broke persistence: %v", err)
	}
	state := runtime.snapshot.Quota[item.Identity]
	if len(state.Events) != 2 || state.Events[0].At.After(state.Events[1].At) {
		t.Fatalf("events not chronological: %#v", state.Events)
	}
	if err := validatePersistedState(persistedState{Version: 1, Accounts: map[string]accountQuotaState{item.Identity: state}}); err != nil {
		t.Fatalf("out-of-order delivery poisoned persisted state: %v", err)
	}
}

func TestFailedUsageWithoutCountersAddsNoEstimate(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	runtime := quotaTestRuntime(t, now, []account{item})
	if err := runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: quotaFixtureKey}}); err != nil {
		t.Fatal(err)
	}
	view, _ := runtime.quotaView(item.Identity)
	if view.FiveHour.ConsumedMicrocredits != 0 || !view.DeliveryWarning {
		t.Fatalf("failed usage view = %#v", view)
	}
	statePath := filepath.Join(runtime.snapshot.Store.dir, "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), quotaFixtureKey) {
		t.Fatalf("failure body leaked to persistence: %s", raw)
	}
}
