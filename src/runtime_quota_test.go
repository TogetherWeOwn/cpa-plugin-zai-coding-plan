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
	if err := runtime.pollOnce(context.Background(), item.Identity, item.key, runtime.snapshot.Generation, 0); err != nil {
		t.Fatal(err)
	}
	view, _ := runtime.quotaView(item.Identity)
	if view.Source != "authoritative" || view.FiveHour.ConsumedMicrocredits != 6_000*creditScale {
		t.Fatalf("authoritative view = %#v", view)
	}
	if err := runtime.pollOnce(context.Background(), item.Identity, item.key, runtime.snapshot.Generation, 0); err == nil {
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
	defer func() { runtimeState = pluginRuntime{} }()
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	testRuntime := quotaTestRuntime(t, now, []account{item})
	runtimeState = pluginRuntime{clock: testRuntime.clock, snapshot: testRuntime.snapshot}
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

func TestRuntimePersistenceFailureDoesNotMarkReplacementGeneration(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	first := account{Identity: accountIdentity("persist-first"), Name: "first", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	second := account{Identity: accountIdentity("persist-second"), Name: "second", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{first})
	runtime.snapshot.Generation = 1
	oldStore := runtime.snapshot.Store
	runtime.mu.Lock()
	oldState := runtime.persistenceSnapshotLocked()
	runtime.mu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	runtime.persistWrite = func(*secureStore, persistedState) error {
		close(started)
		<-release
		return errors.New("disk unavailable")
	}
	done := make(chan error, 1)
	go func() { done <- runtime.persistState(oldStore, oldState) }()
	<-started
	replacementStore, err := newSecureStore(filepath.Join(t.TempDir(), "replacement-auth"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.snapshot = &runtimeSnapshot{
		Config:       runtime.snapshot.Config,
		BaseAccounts: []account{second},
		Accounts:     []account{second},
		Store:        replacementStore,
		Quota:        map[string]accountQuotaState{second.Identity: {CompleteSince: now}},
		byAuthID:     map[string]string{},
		Generation:   2,
	}
	runtime.mu.Unlock()
	close(release)
	if err := <-done; err == nil {
		t.Fatal("old-generation persistence failure unexpectedly succeeded")
	}
	runtime.mu.RLock()
	state := runtime.snapshot.Quota[second.Identity]
	runtime.mu.RUnlock()
	if state.PersistenceWarning || state.DeliveryWarning {
		t.Fatalf("old-generation failure marked replacement account: %#v", state)
	}
}

func TestRuntimeShutdownPersistsWarningAfterAdmittedUsageFailure(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("shutdown-warning"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	runtime := quotaTestRuntime(t, now, []account{item})
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	runtime.persistWrite = func(store *secureStore, state persistedState) error {
		calls++
		if calls == 1 {
			close(started)
			<-release
			return errors.New("disk unavailable")
		}
		return store.saveState(state)
	}
	usageDone := make(chan error, 1)
	go func() {
		usageDone <- runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 1}})
	}()
	<-started
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.shutdown() }()
	close(release)
	if err := <-usageDone; err == nil {
		t.Fatal("usage persistence failure unexpectedly succeeded")
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	reloadedStore, err := newSecureStore(filepath.Dir(runtime.snapshot.Store.dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloadedStore.close() }()
	persisted, err := reloadedStore.loadStateAt(now)
	if err != nil {
		t.Fatal(err)
	}
	state := persisted.Accounts[item.Identity]
	if !state.PersistenceWarning || !state.DeliveryWarning {
		t.Fatalf("shutdown overwrote warning-bearing state: %#v", state)
	}
}

func TestRuntimeStatePersistenceRejectsStaleSnapshot(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("persist-order"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
	runtime := quotaTestRuntime(t, now, []account{item})
	var writes []persistedState
	runtime.persistWrite = func(_ *secureStore, state persistedState) error {
		writes = append(writes, state)
		return nil
	}
	older := persistedState{Version: 1, Accounts: map[string]accountQuotaState{item.Identity: {Events: []creditEvent{{At: now, Microcredits: creditScale, Model: "glm-5.3"}}}}, Generation: 1}
	newer := persistedState{Version: 1, Accounts: map[string]accountQuotaState{item.Identity: {Events: []creditEvent{{At: now, Microcredits: creditScale, Model: "glm-5.3"}, {At: now.Add(time.Second), Microcredits: creditScale, Model: "glm-5.3"}}}}, Generation: 2}
	if err := runtime.persistState(runtime.snapshot.Store, newer); err != nil {
		t.Fatal(err)
	}
	if err := runtime.persistState(runtime.snapshot.Store, older); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0].Generation != newer.Generation || len(writes[0].Accounts[item.Identity].Events) != 2 {
		t.Fatalf("stale snapshot replaced newer state: %#v", writes)
	}
}

func TestRuntimePeriodicPollUsesConfiguredQuotaTimeout(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("periodic-timeout"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.snapshot.Config.QuotaTimeout = 5 * time.Second
	var remaining time.Duration
	runtime.httpClient = roundTripDoer(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Fatal("periodic poll request has no deadline")
		}
		remaining = time.Until(deadline)
		return quotaHTTPResponse(http.StatusBadGateway, quotaFixtureKey), nil
	})

	if err := runtime.pollOnce(context.Background(), item.Identity, item.key, runtime.snapshot.Generation, runtime.snapshot.Config.QuotaTimeout); err == nil {
		t.Fatal("periodic poll unexpectedly succeeded")
	}
	if remaining < 4*time.Second || remaining > 5*time.Second {
		t.Fatalf("periodic poll deadline remaining = %s, want configured timeout %s", remaining, runtime.snapshot.Config.QuotaTimeout)
	}
}

func TestRuntimePollOnceRejectsCancelledOldGenerationResponse(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("stale-generation"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.snapshot.Generation = 1
	started := make(chan struct{})
	release := make(chan struct{})
	runtime.httpClient = roundTripDoer(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return quotaHTTPResponse(200, quotaFixture("pro", []string{
			quotaLimitFixture(3, 5, 12_000, 6_000, 6_000, now.Add(time.Hour).UnixMilli()),
			quotaLimitFixture(6, 1, 60_000, 18_000, 42_000, now.Add(24*time.Hour).UnixMilli()),
		})), nil
	})
	var persisted int
	runtime.persistWrite = func(*secureStore, persistedState) error {
		persisted++
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	pollDone := make(chan error, 1)
	go func() { pollDone <- runtime.pollOnce(ctx, item.Identity, item.key, 1, 0) }()
	<-started
	cancel()
	runtime.mu.Lock()
	replacement := cloneRuntimeSnapshot(runtime.snapshot)
	replacement.Generation = 2
	replacementState := replacement.Quota[item.Identity]
	replacementState.LastPollError = "replacement"
	replacement.Quota[item.Identity] = replacementState
	runtime.snapshot = replacement
	runtime.mu.Unlock()
	close(release)

	if err := <-pollDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("old-generation poll error = %v, want context canceled", err)
	}
	state := runtime.snapshot.Quota[item.Identity]
	if state.Authoritative != nil || state.LastPollAttempt != (time.Time{}) || state.LastPollError != "replacement" {
		t.Fatalf("old-generation response mutated replacement snapshot: %#v", state)
	}
	if persisted != 0 {
		t.Fatalf("old-generation response persisted %d snapshots", persisted)
	}
}

func TestRuntimeForcedRefreshCapturesGenerationAndAccountsAtomically(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	first := account{Identity: accountIdentity("refresh-first"), Name: "first", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: "first-key"}
	second := account{Identity: accountIdentity("refresh-second"), Name: "second", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: "second-key"}
	runtime := quotaTestRuntime(t, now, []account{first})
	runtime.snapshot.Generation = 1
	refresh, leader, err := runtime.beginRefresh()
	if err != nil || !leader {
		t.Fatalf("begin refresh: leader=%t err=%v", leader, err)
	}
	defer runtime.refreshWorkers.Done()

	runtime.mu.Lock()
	replacement := cloneRuntimeSnapshot(runtime.snapshot)
	replacement.Generation = 2
	replacement.Accounts = []account{second}
	replacement.Quota = map[string]accountQuotaState{second.Identity: {CompleteSince: now}}
	runtime.snapshot = replacement
	runtime.mu.Unlock()

	var requestedKey string
	runtime.httpClient = roundTripDoer(func(request *http.Request) (*http.Response, error) {
		requestedKey = strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		return quotaHTTPResponse(http.StatusBadGateway, quotaFixtureKey), nil
	})
	if err := runtime.runRefresh(context.Background(), refresh); err == nil {
		t.Fatal("stale refresh unexpectedly succeeded")
	}
	if requestedKey != first.key {
		t.Fatalf("refresh mixed generation 1 with account key %q, want %q", requestedKey, first.key)
	}
}

func TestRuntimeForcedRefreshRejectsReconfiguredGeneration(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("refresh-generation"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.snapshot.Generation = 1
	started := make(chan struct{})
	release := make(chan struct{})
	runtime.httpClient = roundTripDoer(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return quotaHTTPResponse(200, quotaFixture("pro", []string{
			quotaLimitFixture(3, 5, 12_000, 6_000, 6_000, now.Add(time.Hour).UnixMilli()),
			quotaLimitFixture(6, 1, 60_000, 18_000, 42_000, now.Add(24*time.Hour).UnixMilli()),
		})), nil
	})
	var persisted int
	runtime.persistWrite = func(*secureStore, persistedState) error {
		persisted++
		return nil
	}

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- runtime.forceRefresh(context.Background()) }()
	<-started
	runtime.mu.Lock()
	replacement := cloneRuntimeSnapshot(runtime.snapshot)
	replacement.Generation = 2
	replacementState := replacement.Quota[item.Identity]
	replacementState.LastPollError = "replacement"
	replacement.Quota[item.Identity] = replacementState
	runtime.snapshot = replacement
	runtime.mu.Unlock()
	close(release)

	if err := <-refreshDone; err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("stale forced refresh error = %v, want context canceled", err)
	}
	state := runtime.snapshot.Quota[item.Identity]
	if state.Authoritative != nil || state.LastPollAttempt != (time.Time{}) || state.LastPollError != "replacement" {
		t.Fatalf("stale forced refresh mutated replacement snapshot: %#v", state)
	}
	if persisted != 0 {
		t.Fatalf("stale forced refresh persisted %d snapshots", persisted)
	}
}

func TestRuntimeShutdownCancelsAndJoinsForcedRefresh(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("refresh-shutdown"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	started := make(chan struct{})
	runtime.httpClient = roundTripDoer(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- runtime.forceRefresh(context.Background()) }()
	<-started
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.shutdown() }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel and join forced refresh")
	}
	select {
	case err := <-refreshDone:
		if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Fatalf("refresh error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forced refresh did not finish during shutdown")
	}
}

func TestRuntimeConcurrentReconfigureAndShutdownReturns(t *testing.T) {
	for range 100 {
		now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
		item := account{Identity: accountIdentity("reconfigure-shutdown"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000}
		runtime := quotaTestRuntime(t, now, []account{item})
		runtime.snapshot.Generation = 1
		runtime.reconfigures.Add(1)
		reconfigureDone := make(chan error, 1)
		go func() {
			defer runtime.reconfigures.Done()
			staged := cloneRuntimeSnapshot(runtime.snapshot)
			reconfigureDone <- runtime.commitSnapshot(staged)
		}()
		shutdownDone := make(chan error, 1)
		go func() { shutdownDone <- runtime.shutdown() }()
		for name, done := range map[string]<-chan error{"reconfigure": reconfigureDone, "shutdown": shutdownDone} {
			select {
			case err := <-done:
				if name == "reconfigure" && err != nil && !strings.Contains(err.Error(), "shutting down") {
					t.Fatalf("reconfigure error = %v", err)
				}
				if name == "shutdown" && err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatalf("concurrent %s did not return", name)
			}
		}
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
			Config:       pluginConfig{QuotaRefresh: 2 * time.Minute, AuthoritativeMaxAge: 5 * time.Minute, ThresholdPercent: 97, StateRetention: 8 * 24 * time.Hour},
			BaseAccounts: append([]account(nil), accounts...),
			Accounts:     accounts,
			Store:        store,
			Quota:        quota,
			byAuthID:     byAuthID,
		},
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
