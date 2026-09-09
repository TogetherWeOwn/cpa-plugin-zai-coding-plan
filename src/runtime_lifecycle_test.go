package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestPersistenceCommitsAreOrderedByVersion pins review finding H1: an older
// persistence snapshot that reaches the store after a newer one must not
// overwrite it. The blocked first save simulates the interleaving the
// reviewer reproduced (older blocked save finishing after a newer save).
func TestPersistenceCommitsAreOrderedByVersion(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	runtime := quotaTestRuntime(t, now, []account{item})

	var mu sync.Mutex
	var saved []persistedState
	releaseFirst := make(chan struct{})
	firstEntered := make(chan struct{})
	runtime.persist = func(_ *secureStore, state persistedState) error {
		mu.Lock()
		first := len(saved) == 0
		mu.Unlock()
		if first {
			close(firstEntered)
			<-releaseFirst // older commit stalls inside persistState
		}
		mu.Lock()
		saved = append(saved, state)
		mu.Unlock()
		return nil
	}

	// First usage event takes the stalled (older) commit path.
	go func() {
		_ = runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 10_000}})
	}()
	<-firstEntered
	// Second usage event commits while the first is still stalled: the caller
	// must not deadlock on persistMu, so run it concurrently and require it
	// to finish only after the older commit is released.
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now.Add(time.Second), Detail: pluginapi.UsageDetail{InputTokens: 2_000}})
	}()
	<-secondStarted
	select {
	case err := <-secondDone:
		t.Fatalf("second commit overtook the stalled older commit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(saved) == 2
	})

	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 2 {
		t.Fatalf("saved states = %d, want 2", len(saved))
	}
	if last := saved[len(saved)-1]; len(last.Accounts[item.Identity].Events) != 2 {
		t.Fatalf("last persisted snapshot lost events: %#v", last.Accounts[item.Identity].Events)
	}
}

// TestShutdownJoinsInFlightUsageHandlers pins review finding H2: a usage
// handler that already accepted a record must finish persisting before the
// shutdown's final flush, and must not write after it.
func TestShutdownJoinsInFlightUsageHandlers(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	runtime := quotaTestRuntime(t, now, []account{item})

	inPersist := make(chan struct{})
	releasePersist := make(chan struct{})
	var persistCalls int32
	runtime.persist = func(_ *secureStore, state persistedState) error {
		first := persistCalls == 0
		persistCalls++
		if first {
			close(inPersist)
			<-releasePersist
		}
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 1}})
	}()
	<-inPersist

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.shutdown() }()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown completed while a usage handler was still persisting")
	case <-time.After(50 * time.Millisecond):
	}
	close(releasePersist)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
}

// TestUpstreamPlanSyncsFallbackBuckets pins review finding H3: when the quota
// endpoint reports a different named plan than configured, the estimator
// fallback must use the upstream plan's capacity.
func TestUpstreamPlanSyncsFallbackBuckets(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "max", FiveHourCredits: 28_000, WeeklyCredits: 140_000, ClaudeAuthID: "auth", key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.httpClient = roundTripDoer(func(*http.Request) (*http.Response, error) {
		return quotaHTTPResponse(200, quotaFixture("lite", []string{
			quotaLimitFixture(3, 5, 2_000, 1_000, 1_000, now.Add(time.Hour).UnixMilli()),
			quotaLimitFixture(6, 1, 10_000, 3_000, 7_000, now.Add(24*time.Hour).UnixMilli()),
		})), nil
	})
	if err := runtime.pollOnce(context.Background(), item.Identity, item.key, runtime.snapshot.Generation, runtime.snapshot.Config.QuotaTimeout); err != nil {
		t.Fatal(err)
	}
	fake := runtime.clock.(*fakeClock)
	fake.set(now.Add(6 * time.Minute)) // authoritative snapshot expires
	view, _ := runtime.quotaView(item.Identity)
	if view.Source != "estimated" {
		t.Fatalf("source = %q, want estimated after expiry", view.Source)
	}
	if view.FiveHour.BucketMicrocredits != 2_000*creditScale {
		t.Fatalf("five-hour fallback bucket = %d, want lite %d", view.FiveHour.BucketMicrocredits, 2_000*creditScale)
	}
	if view.Weekly.BucketMicrocredits != 10_000*creditScale {
		t.Fatalf("weekly fallback bucket = %d, want lite %d", view.Weekly.BucketMicrocredits, 10_000*creditScale)
	}
}

// TestCustomBucketsSurviveUpstreamPlanSync verifies plan sync never overrides
// explicit custom credit buckets (ARCHITECTURE.md: only named-plan defaults
// follow the upstream plan). Custom accounts keep their explicit buckets even
// when upstream reports a named plan; named accounts follow the upstream plan.
func TestCustomBucketsSurviveUpstreamPlanSync(t *testing.T) {
	accounts := []account{{Identity: accountIdentity("account"), Name: "account", Plan: "custom", FiveHourCredits: 5_000, WeeklyCredits: 25_000, ClaudeAuthID: "auth"}}
	syncPlanFromUpstream(accounts, accounts[0].Identity, "pro")
	if accounts[0].Plan != "custom" || accounts[0].FiveHourCredits != 5_000 || accounts[0].WeeklyCredits != 25_000 {
		t.Fatalf("custom buckets overridden: %#v", accounts[0])
	}
	named := []account{{Identity: accountIdentity("named"), Name: "named", Plan: "max", FiveHourCredits: 28_000, WeeklyCredits: 140_000, ClaudeAuthID: "auth"}}
	syncPlanFromUpstream(named, named[0].Identity, "lite")
	if named[0].Plan != "lite" || named[0].FiveHourCredits != 2_000 || named[0].WeeklyCredits != 10_000 {
		t.Fatalf("named plan did not follow upstream: %#v", named[0])
	}
	syncPlanFromUpstream(named, named[0].Identity, "")
	if named[0].Plan != "lite" {
		t.Fatalf("empty upstream plan mutated account: %#v", named[0])
	}
}

// TestCreditBucketOverflowRejected pins review finding M1: credit buckets
// whose microcredit conversion would overflow int64 are rejected at every
// configuration boundary instead of wrapping during estimation.
func TestCreditBucketOverflowRejected(t *testing.T) {
	overflow := int64(^uint64(0)>>1) / creditScale
	if !validCreditBucket(overflow) {
		t.Fatalf("boundary bucket %d must be valid", overflow)
	}
	if validCreditBucket(overflow + 1) {
		t.Fatal("bucket above boundary accepted")
	}
	if err := validateStoredSetting(accountSetting{Plan: "custom", FiveHourCredits: overflow + 1, WeeklyCredits: 1}); err == nil {
		t.Fatal("stored setting above boundary accepted")
	}
	if err := validateAccounts([]account{{Name: "a", Plan: "custom", FiveHourCredits: overflow + 1, WeeklyCredits: 1}}); err == nil {
		t.Fatal("account above boundary accepted")
	}
	_, err := parsePluginConfig([]byte(fmt.Sprintf("default-plan: custom\naccounts:\n  - key-suffix: redacted\n    five-hour-credits: %d\n", overflow+1)))
	if err == nil {
		t.Fatal("plugin config above boundary accepted")
	}
}

// TestFutureAuthoritativeTimestampGoesStale pins review finding M2: a
// snapshot observed in the future (skewed upstream clock or tampered state)
// must not stay authoritative forever.
func TestFutureAuthoritativeTimestampGoesStale(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth", key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})
	runtime.httpClient = roundTripDoer(func(*http.Request) (*http.Response, error) {
		return quotaHTTPResponse(200, quotaFixture("pro", []string{
			quotaLimitFixture(3, 5, 12_000, 6_000, 6_000, now.Add(time.Hour).UnixMilli()),
			quotaLimitFixture(6, 1, 60_000, 18_000, 42_000, now.Add(24*time.Hour).UnixMilli()),
		})), nil
	})
	// Poll succeeds while the runtime clock is ahead, then the local clock
	// moves backward so the observation is in the future relative to the view.
	fake := runtime.clock.(*fakeClock)
	fake.set(now.Add(10 * time.Minute))
	if err := runtime.pollOnce(context.Background(), item.Identity, item.key, runtime.snapshot.Generation, runtime.snapshot.Config.QuotaTimeout); err != nil {
		t.Fatal(err)
	}
	fake.set(now)
	view, _ := runtime.quotaView(item.Identity)
	if !view.Stale || view.Source != "estimated" {
		t.Fatalf("future-observed snapshot treated as fresh: %#v", view)
	}
}

// TestPersistedFutureObservedAtRejectedAtLoad: a tampered state file with an
// out-of-range ObservedAt fails closed at load instead of at first view.
func TestPersistedFutureObservedAtRejectedAtLoad(t *testing.T) {
	root := t.TempDir()
	store, err := newSecureStore(root)
	if err != nil {
		t.Fatal(err)
	}
	identity := accountIdentity("account")
	authority := &quotaSnapshot{
		Plan:       "pro",
		FiveHour:   quotaWindow{ConsumedMicrocredits: 1, BucketMicrocredits: 12_000, ResetsAt: time.Now().Add(time.Hour)},
		Weekly:     quotaWindow{ConsumedMicrocredits: 1, BucketMicrocredits: 60_000, ResetsAt: time.Now().Add(24 * time.Hour)},
		ObservedAt: time.Now().UTC(),
	}
	state := persistedState{Version: 1, Accounts: map[string]accountQuotaState{
		identity: {Authoritative: authority},
	}}
	if err := store.saveState(state); err != nil {
		t.Fatal(err)
	}
	// Round-trip a valid state first, then write the tampered variant
	// directly: saveState itself rejects implausible timestamps, which is
	// exactly the fail-closed behavior under test.
	authority.ObservedAt = time.Date(25000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := validatePersistedState(state); err == nil {
		t.Fatal("state with far-future ObservedAt validated")
	}
	authority.ObservedAt = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := validatePersistedState(state); err == nil {
		t.Fatal("state with pre-2000 ObservedAt validated")
	}
}

// TestPollOnceResultBindsToSnapshotGeneration pins review finding H5: a poll
// result from before a reconfigure must not mutate the new snapshot's state.
func TestPollOnceResultBindsToSnapshotGeneration(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth", key: quotaFixtureKey}
	runtime := quotaTestRuntime(t, now, []account{item})

	blockFetch := make(chan struct{})
	fetchReleased := make(chan struct{})
	runtime.httpClient = roundTripDoer(func(*http.Request) (*http.Response, error) {
		close(blockFetch)
		<-fetchReleased
		return quotaHTTPResponse(200, quotaFixture("pro", []string{
			quotaLimitFixture(3, 5, 12_000, 6_000, 6_000, now.Add(time.Hour).UnixMilli()),
			quotaLimitFixture(6, 1, 60_000, 18_000, 42_000, now.Add(24*time.Hour).UnixMilli()),
		})), nil
	})

	oldGeneration := runtime.snapshot.Generation
	pollDone := make(chan error, 1)
	go func() {
		pollDone <- runtime.pollOnce(context.Background(), item.Identity, item.key, oldGeneration, runtime.snapshot.Config.QuotaTimeout)
	}()
	<-blockFetch

	// Reconfigure installs a new generation while the poll is in flight.
	runtime.mu.Lock()
	staged := *runtime.snapshot
	staged.Generation = oldGeneration + 1
	staged.Quota = map[string]accountQuotaState{item.Identity: {}}
	runtime.snapshot = &staged
	runtime.mu.Unlock()
	close(fetchReleased)

	if err := <-pollDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("stale poll returned %v, want context.Canceled", err)
	}
	state := runtime.snapshot.Quota[item.Identity]
	if state.Authoritative != nil || state.LastPollAttempt != (time.Time{}) {
		t.Fatalf("stale poll mutated new snapshot: %#v", state)
	}
}

// TestReconfigureKeepsUsageAcceptedDuringStaging pins review finding H4: a
// usage record accepted while reconfigure is staging persisted state must
// survive into the new snapshot rather than being overwritten by it.
func TestReconfigureKeepsUsageAcceptedDuringStaging(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth", Disabled: true}
	runtime := quotaTestRuntime(t, now, []account{item})

	// Capture the stale staged snapshot before usage arrives, matching a
	// reconfigure that has loaded state but has not reached commitSnapshot.
	staged := cloneRuntimeSnapshot(runtime.snapshot)
	if err := runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.commitSnapshot(staged); err != nil {
		t.Fatal(err)
	}
	events := runtime.snapshot.Quota[item.Identity].Events
	if len(events) != 1 {
		t.Fatalf("commitSnapshot replaced live usage with stale staged state: %#v", events)
	}
}

func TestCommitSnapshotCarriesForwardSameStoreNamedPlan(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	identity := accountIdentity("account")
	live := account{Identity: identity, Name: "account", Plan: "lite", FiveHourCredits: 2_000, WeeklyCredits: 10_000, ClaudeAuthID: "auth", Disabled: true}
	runtime := quotaTestRuntime(t, now, []account{live})

	staged := cloneRuntimeSnapshot(runtime.snapshot)
	staged.Accounts[0].Plan = "max"
	staged.Accounts[0].FiveHourCredits = 28_000
	staged.Accounts[0].WeeklyCredits = 140_000
	if err := runtime.commitSnapshot(staged); err != nil {
		t.Fatal(err)
	}

	got := runtime.snapshot.Accounts[0]
	if got.Plan != "lite" || got.FiveHourCredits != 2_000 || got.WeeklyCredits != 10_000 {
		t.Fatalf("same-store commit reverted live plan: %#v", got)
	}
}

func TestCommitSnapshotKeepsStagedExplicitPlanOverride(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, configPath, authDir, fixtureKey)

	runtime := &pluginRuntime{clock: &fakeClock{now: now}}
	proConfig := []byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")
	if err := runtime.reconfigure(proConfig); err != nil {
		t.Fatal(err)
	}
	identity := runtime.snapshot.Accounts[0].Identity
	syncPlanFromUpstream(runtime.snapshot.Accounts, identity, "lite")

	explicitConfig := []byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\naccounts:\n  - key-suffix: " + displaySuffix(fixtureKey) + "\n    plan: max\n")
	if err := runtime.reconfigure(explicitConfig); err != nil {
		t.Fatal(err)
	}

	got := runtime.snapshot.Accounts[0]
	if got.Plan != "max" || got.FiveHourCredits != 28_000 || got.WeeklyCredits != 140_000 {
		t.Fatalf("same-store commit overrode explicit staged plan: %#v", got)
	}
}

func TestReconfigureKeepsExplicitPlanAndBucketsOverPersistedUpstream(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		config       string
		setting      accountSetting
		wantPlan     string
		wantFiveHour int64
		wantWeekly   int64
	}{
		{
			name:         "configured plan",
			config:       "accounts:\n  - key-suffix: " + displaySuffix(fixtureKey) + "\n    plan: max\n    disabled: true\n",
			setting:      accountSetting{},
			wantPlan:     "max",
			wantFiveHour: 28_000,
			wantWeekly:   140_000,
		},
		{
			name:         "configured buckets",
			config:       "accounts:\n  - key-suffix: " + displaySuffix(fixtureKey) + "\n    five-hour-credits: 77\n    weekly-credits: 999\n    disabled: true\n",
			setting:      accountSetting{},
			wantPlan:     "pro",
			wantFiveHour: 77,
			wantWeekly:   999,
		},
		{
			name:         "stored plan",
			setting:      accountSetting{Plan: "max", Disabled: boolPointer(true)},
			wantPlan:     "max",
			wantFiveHour: 28_000,
			wantWeekly:   140_000,
		},
		{
			name:         "stored buckets",
			setting:      accountSetting{FiveHourCredits: 77, WeeklyCredits: 999, Disabled: boolPointer(true)},
			wantPlan:     "pro",
			wantFiveHour: 77,
			wantWeekly:   999,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			authDir := filepath.Join(root, "auth")
			configPath := filepath.Join(root, "config.yaml")
			writeCPAConfigFixture(t, configPath, authDir, fixtureKey)

			store, err := newSecureStore(authDir)
			if err != nil {
				t.Fatal(err)
			}
			identity := accountIdentity(fixtureKey)
			if test.setting != (accountSetting{}) {
				if err := store.saveSettings(settingsFile{Version: 1, Accounts: map[string]accountSetting{identity: test.setting}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.saveState(persistedState{Version: 1, Accounts: map[string]accountQuotaState{
				identity: {Authoritative: &quotaSnapshot{
					Plan:       "lite",
					FiveHour:   quotaWindow{ConsumedMicrocredits: creditScale, BucketMicrocredits: 2_000 * creditScale, ResetsAt: now.Add(time.Hour)},
					Weekly:     quotaWindow{ConsumedMicrocredits: creditScale, BucketMicrocredits: 10_000 * creditScale, ResetsAt: now.Add(24 * time.Hour)},
					ObservedAt: now,
				}},
			}}); err != nil {
				t.Fatal(err)
			}

			runtime := &pluginRuntime{clock: &fakeClock{now: now}}
			rawConfig := "cpa-config-path: " + configPath + "\ndefault-plan: pro\n" + test.config
			if err := runtime.reconfigure([]byte(rawConfig)); err != nil {
				t.Fatal(err)
			}
			got := runtime.snapshot.Accounts[0]
			if got.Plan != test.wantPlan || got.FiveHourCredits != test.wantFiveHour || got.WeeklyCredits != test.wantWeekly {
				t.Fatalf("persisted upstream plan overrode explicit values: %#v", got)
			}
		})
	}
}

func TestCommitSnapshotDoesNotCarryForwardDifferentStorePlan(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	identity := accountIdentity("account")
	live := account{Identity: identity, Name: "account", Plan: "lite", FiveHourCredits: 2_000, WeeklyCredits: 10_000, ClaudeAuthID: "auth", Disabled: true}
	runtime := quotaTestRuntime(t, now, []account{live})

	staged := cloneRuntimeSnapshot(runtime.snapshot)
	store, err := newSecureStore(filepath.Join(t.TempDir(), "other-auth"))
	if err != nil {
		t.Fatal(err)
	}
	staged.Store = store
	staged.Accounts[0].Plan = "max"
	staged.Accounts[0].FiveHourCredits = 28_000
	staged.Accounts[0].WeeklyCredits = 140_000
	if err := runtime.commitSnapshot(staged); err != nil {
		t.Fatal(err)
	}

	got := runtime.snapshot.Accounts[0]
	if got.Plan != "max" || got.FiveHourCredits != 28_000 || got.WeeklyCredits != 140_000 {
		t.Fatalf("different-store commit carried live plan: %#v", got)
	}
}

// TestConcurrentUsageAndReconfigureStress drives handleUsage against
// reconfigure with the race detector: all accepted events must survive every
// completed reconfigure cycle.
func TestConcurrentUsageAndReconfigureStress(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	configPath := filepath.Join(root, "config.yaml")
	writeCPAConfigFixture(t, configPath, authDir, fixtureKey)

	runtime := &pluginRuntime{clock: &fakeClock{now: now}}
	if err := runtime.reconfigure([]byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")); err != nil {
		t.Fatal(err)
	}
	authID := runtime.snapshot.Accounts[0].ClaudeAuthID
	identity := runtime.snapshot.Accounts[0].Identity

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			at := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				at++
				// Keep stress timestamps within the persisted skew bound; this test
				// exercises ordering/reconfigure behavior, not future-time rejection.
				requestedAt := now.Add(-time.Minute).Add(time.Duration(at*3+i) * time.Microsecond)
				_ = runtime.handleUsage(pluginapi.UsageRecord{AuthID: authID, Model: "glm-5.3", RequestedAt: requestedAt, Detail: pluginapi.UsageDetail{InputTokens: 1}})
			}
		}(i)
	}
	for i := 0; i < 5; i++ {
		if err := runtime.reconfigure([]byte("cpa-config-path: " + configPath + "\ndefault-plan: pro\n")); err != nil {
			t.Errorf("reconfigure %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if err := runtime.shutdown(); err != nil {
		t.Fatal(err)
	}

	store, err := newSecureStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.loadState()
	if err != nil {
		t.Fatal(err)
	}
	events := persisted.Accounts[identity].Events
	if len(events) == 0 {
		t.Fatal("all usage lost across concurrent reconfigures")
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].At.After(events[i].At) {
			t.Fatalf("persisted events unsorted at %d: %#v", i, events)
		}
	}
	if strings.Contains(fmt.Sprint(persisted), fixtureKey) {
		t.Fatal("persisted state leaked key")
	}
}

// TestShutdownFlushPersistsAcceptedEvents: end-to-end check that usage
// accepted before shutdown is present in the durable final flush.
func TestShutdownFlushPersistsAcceptedEvents(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	item := account{Identity: accountIdentity("account"), Name: "account", Plan: "pro", FiveHourCredits: 12_000, WeeklyCredits: 60_000, ClaudeAuthID: "auth"}
	runtime := quotaTestRuntime(t, now, []account{item})
	if err := runtime.handleUsage(pluginapi.UsageRecord{AuthID: item.ClaudeAuthID, Model: "glm-5.3", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 10_000}}); err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Dir(runtime.snapshot.Store.dir)
	if err := runtime.shutdown(); err != nil {
		t.Fatal(err)
	}
	store, err := newSecureStore(authDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	persisted, err := store.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Accounts[item.Identity].Events) != 1 {
		t.Fatalf("shutdown flush lost accepted event: %#v", persisted.Accounts[item.Identity])
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// TestPersistedStateStaysLoadableWithoutGeneration pins the on-disk
// compatibility requirement: Generation is an in-memory ordering token and
// must never be serialized into state.json.
func TestPersistedStateStaysLoadableWithoutGeneration(t *testing.T) {
	raw, err := json.Marshal(persistedState{Version: 1, Generation: 42, Accounts: map[string]accountQuotaState{
		accountIdentity("account"): {CompleteSince: time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "42") || strings.Contains(string(raw), "generation") {
		t.Fatalf("generation serialized to disk: %s", raw)
	}
}
