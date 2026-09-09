package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }
func (realClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type runtimeSnapshot struct {
	Config     pluginConfig
	Accounts   []account
	Store      *secureStore
	Quota      map[string]accountQuotaState
	Health     map[string]accountHealthState
	Routing    routingState
	byAuthID   map[string]string
	byIdentity map[string]account
	Generation uint64
}

type pluginRuntime struct {
	mu           sync.RWMutex
	snapshot     *runtimeSnapshot
	lastErr      error
	stopped      bool
	reconfigures sync.WaitGroup
	workers      sync.WaitGroup
	usage        sync.WaitGroup
	cancel       context.CancelFunc
	clock        clock
	httpClient   httpDoer
	endpoint     string
	persist      func(*secureStore, persistedState) error
	pollersMu    sync.Mutex
	persistMu    sync.Mutex
	persistNext  atomic.Uint64
	persisted    atomic.Uint64
	now          func() time.Time
}

func (r *pluginRuntime) reconfigure(rawConfig []byte) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("plugin is shutting down")
	}
	r.reconfigures.Add(1)
	r.mu.Unlock()
	defer r.reconfigures.Done()

	cfg, err := parsePluginConfig(rawConfig)
	if err != nil {
		return r.recordError(err)
	}
	configPath, err := resolveCPAConfigPath(cfg.CPAConfigPath)
	if err != nil {
		return r.recordError(err)
	}
	cpa, err := loadCPAConfig(configPath)
	if err != nil {
		return r.recordError(err)
	}
	providerKeys := cpaProviderKeys(cpa)
	authDir, err := resolveAuthDir(cpa.AuthDir)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	store, err := newSecureStore(authDir)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	accounts, err := discoverAccounts(cpa, cfg)
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	settings, err := store.loadSettings()
	if err != nil {
		return r.recordError(err, providerKeys...)
	}
	if err = applyStoredSettings(accounts, settings); err != nil {
		return r.recordError(err, providerKeys...)
	}
	persisted, err := store.loadState()
	if err != nil {
		return r.recordError(err, providerKeys...)
	}

	// Build the staged quota view before taking the publish lock. The swap
	// below runs under r.mu, and handleUsage serializes on the same mutex
	// with a generation check, so usage accepted while staging either lands
	// in the old snapshot first (and is re-read from persisted state only if
	// it was durably saved) or lands in the new snapshot after the swap —
	// never silently dropped by an overwrite.
	quota := make(map[string]accountQuotaState, len(accounts))
	health := make(map[string]accountHealthState, len(accounts))
	byAuthID := make(map[string]string, len(accounts)*2)
	byIdentity := make(map[string]account, len(accounts))
	for i := range accounts {
		state := persisted.Accounts[accounts[i].Identity]
		if state.CompleteSince.IsZero() {
			state.CompleteSince = r.runtimeClock().Now()
		}
		state.compact(r.runtimeClock().Now(), cfg.StateRetention)
		quota[accounts[i].Identity] = state
		health[accounts[i].Identity] = accountHealthState{}
		if state.Authoritative != nil {
			syncPlanFromUpstream(accounts, accounts[i].Identity, state.Authoritative.Plan)
		}
		byAuthID[accounts[i].ClaudeAuthID] = accounts[i].Identity
		byAuthID[accounts[i].OpenAIAuthID] = accounts[i].Identity
		byIdentity[accounts[i].Identity] = accounts[i]
	}
	staged := &runtimeSnapshot{
		Config: cfg, Accounts: accounts, Store: store, Quota: quota, Health: health,
		byAuthID: byAuthID, byIdentity: byIdentity,
	}
	return r.commitSnapshot(staged)
}

func (r *pluginRuntime) recordError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	redacted := redactError(err, secrets...)
	r.mu.Lock()
	if !r.stopped {
		r.lastErr = redacted
	}
	r.mu.Unlock()
	return redacted
}

func (r *pluginRuntime) current() (*runtimeSnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		if r.lastErr != nil {
			return nil, r.lastErr
		}
		return nil, fmt.Errorf("plugin is not configured")
	}
	return cloneRuntimeSnapshot(r.snapshot), nil
}

func sameSecureStore(left, right *secureStore) bool {
	return left != nil && right != nil && left.dir == right.dir
}

func cloneRuntimeSnapshot(source *runtimeSnapshot) *runtimeSnapshot {
	copySnapshot := *source
	copySnapshot.Accounts = append([]account(nil), source.Accounts...)
	copySnapshot.byAuthID = make(map[string]string, len(source.byAuthID))
	for authID, identity := range source.byAuthID {
		copySnapshot.byAuthID[authID] = identity
	}
	copySnapshot.byIdentity = make(map[string]account, len(source.byIdentity))
	for identity, item := range source.byIdentity {
		copySnapshot.byIdentity[identity] = item
	}
	copySnapshot.Health = make(map[string]accountHealthState, len(source.Health))
	for identity, state := range source.Health {
		copySnapshot.Health[identity] = state
	}
	copySnapshot.Routing = routingState{cursors: make(map[string]uint64, len(source.Routing.cursors))}
	for scope, cursor := range source.Routing.cursors {
		copySnapshot.Routing.cursors[scope] = cursor
	}
	copySnapshot.Quota = make(map[string]accountQuotaState, len(source.Quota))
	for identity, state := range source.Quota {
		state.Events = append([]creditEvent(nil), state.Events...)
		state.DedupHashes = append([]string(nil), state.DedupHashes...)
		if state.Authoritative != nil {
			copyQuota := *state.Authoritative
			state.Authoritative = &copyQuota
		}
		copySnapshot.Quota[identity] = state
	}
	return &copySnapshot
}

// hasSnapshot is called from the linux/cgo ABI boundary, which is excluded
// from the default non-CGO lint build.
//
//nolint:unused
func (r *pluginRuntime) hasSnapshot() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot != nil
}

func (r *pluginRuntime) validationStatus() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lastErr == nil {
		return ""
	}
	return boundedStatus(r.lastErr.Error())
}

func (r *pluginRuntime) handleUsage(record pluginapi.UsageRecord) error {
	now := r.runtimeNow()
	resetAt, resetReason, hasResetHint := parseRateLimitHint(record, now)

	r.mu.Lock()
	if r.stopped || r.snapshot == nil {
		r.mu.Unlock()
		return fmt.Errorf("plugin is not configured")
	}
	// Add while holding the same lock shutdown uses to close admission. Once
	// stopped is set no new handler can increment this group, so Wait cannot
	// race an Add from zero.
	r.usage.Add(1)
	defer r.usage.Done()
	identity := r.snapshot.byAuthID[strings.TrimSpace(record.AuthID)]
	if identity == "" {
		r.mu.Unlock()
		return nil
	}

	health := r.snapshot.Health[identity]
	if record.Failed {
		switch record.Failure.StatusCode {
		case 401, 403:
			health.suspendUntil(now.Add(r.snapshot.Config.SuspendDuration))
		case 429:
			if !hasResetHint {
				resetAt, resetReason = rateLimitReset(now, health, r.snapshot.Config.FallbackCooldown)
			}
			health.exhaustUntil(resetAt, resetReason)
		}
		r.snapshot.Health[identity] = health
	}

	state := r.snapshot.Quota[identity]
	if state.CompleteSince.IsZero() {
		state.CompleteSince = now
	}
	if record.Failed && usageDetailEmpty(record.Detail) {
		state.DeliveryWarning = true
	} else if hash := usageDedupHash(record); state.seenDedup(hash) {
		// Heuristic dedup hit: keep the warning but add no event.
	} else if estimate, err := estimateUsageCredits(record, usageTimestamp(record, now)); err != nil {
		state.UnknownModelWarning = true
		state.DeliveryWarning = true
	} else {
		at := usageTimestamp(record, now)
		state.addEvent(creditEvent{At: at, Microcredits: estimate.Microcredits, Model: estimate.Model})
		state.compact(now, r.snapshot.Config.StateRetention)
	}
	r.snapshot.Quota[identity] = state
	persisted := r.persistenceSnapshotLocked()
	store := r.snapshot.Store
	r.mu.Unlock()
	return r.persistState(store, persisted)
}

// updateCapacity is the in-memory adapter used by quota polling. Results are
// generation-bound so a late poll cannot mutate a replacement snapshot.
func (r *pluginRuntime) updateCapacity(generation uint64, identity string, update capacityUpdate) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.snapshot == nil || r.snapshot.Generation != generation {
		return false
	}
	state, exists := r.snapshot.Health[identity]
	if !exists {
		return false
	}
	state.CapacityExhausted = update.Exhausted
	state.CapacityResetAt = update.ResetAt.UTC()
	state.CapacitySource = boundedHealthReason(update.Source)
	r.snapshot.Health[identity] = state
	return true
}

func (r *pluginRuntime) health(identity string) (accountHealth, bool) {
	now := r.runtimeNow()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		return accountHealth{}, false
	}
	state, exists := r.snapshot.Health[identity]
	if !exists {
		return accountHealth{}, false
	}
	return state.assess(r.snapshot.byIdentity[identity], now), true
}

func (r *pluginRuntime) runtimeNow() time.Time {
	if r.now != nil {
		return r.now().UTC()
	}
	return r.runtimeClock().Now()
}

func usageDetailEmpty(detail pluginapi.UsageDetail) bool {
	return detail.InputTokens == 0 && detail.OutputTokens == 0 && detail.CacheReadTokens == 0 && detail.CacheCreationTokens == 0
}

func usageTimestamp(record pluginapi.UsageRecord, fallback time.Time) time.Time {
	if record.RequestedAt.IsZero() {
		return fallback.UTC()
	}
	return record.RequestedAt.UTC()
}

func (r *pluginRuntime) startPollers(ctx context.Context, snapshot *runtimeSnapshot) {
	for i := range snapshot.Accounts {
		item := snapshot.Accounts[i]
		if item.Disabled {
			continue
		}
		r.workers.Add(1)
		go r.pollAccount(ctx, snapshot.Generation, item.Identity, item.key, snapshot.Config.QuotaRefresh)
	}
}

// commitSnapshot is the lifecycle boundary shared with the management slice:
// publish one complete generation, cancel and join the prior pollers, then
// start workers from an immutable clone of the committed snapshot.
func (r *pluginRuntime) commitSnapshot(staged *runtimeSnapshot) error {
	r.pollersMu.Lock()
	defer r.pollersMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		cancel()
		return fmt.Errorf("plugin is shutting down")
	}
	previousCancel := r.cancel
	if r.snapshot != nil {
		staged.Generation = r.snapshot.Generation + 1
		if sameSecureStore(r.snapshot.Store, staged.Store) {
			for identity, state := range r.snapshot.Quota {
				if _, exists := staged.Quota[identity]; exists {
					staged.Quota[identity] = state
				}
			}
			for identity, state := range r.snapshot.Health {
				if _, exists := staged.Health[identity]; exists {
					staged.Health[identity] = state
				}
			}
			carryForwardNamedPlans(r.snapshot, staged)
		}
	} else {
		staged.Generation = 1
	}
	staged.Routing = routingState{}
	staged.Routing.initialize()
	r.cancel = cancel
	r.snapshot = staged
	r.lastErr = nil
	pollerSnapshot := cloneRuntimeSnapshot(staged)
	r.mu.Unlock()

	if previousCancel != nil {
		previousCancel()
		r.workers.Wait()
	}
	r.startPollers(ctx, pollerSnapshot)
	return nil
}

func (r *pluginRuntime) pollAccount(ctx context.Context, generation uint64, identity, key string, base time.Duration) {
	defer r.workers.Done()
	attempt := 0
	for {
		if err := r.pollOnce(ctx, generation, identity, key); err != nil && errors.Is(err, context.Canceled) {
			return
		}
		attempt++
		delay := jitteredPollInterval(base, identity, attempt)
		if failures := r.pollFailureCount(identity); failures > 0 {
			delay = pollBackoff(delay, failures)
		}
		if err := r.runtimeClock().Sleep(ctx, delay); err != nil {
			return
		}
	}
}

func (r *pluginRuntime) pollOnce(ctx context.Context, generation uint64, identity, key string) error {
	now := r.runtimeClock().Now()
	attemptCtx, cancel := context.WithTimeout(ctx, defaultQuotaTimeout)
	defer cancel()
	snapshot, err := fetchQuota(attemptCtx, r.quotaClient(), r.quotaEndpoint(), key, now)
	// A cancelled parent (shutdown or reconfigure) is not a poll failure; an
	// attemptCtx timeout with a live parent is.
	if err != nil && ctx.Err() != nil {
		return context.Canceled
	}
	r.mu.Lock()
	// Bind the result to the snapshot generation the poll was launched for:
	// a reconfigure (or shutdown) that replaced the snapshot in between must
	// not have this result mutate the new configuration's state.
	if r.stopped || r.snapshot == nil || r.snapshot.Generation != generation {
		r.mu.Unlock()
		return context.Canceled
	}
	state, exists := r.snapshot.Quota[identity]
	if !exists {
		r.mu.Unlock()
		return nil
	}
	state.LastPollAttempt = now
	if err != nil {
		state.ConsecutiveFailures++
		state.LastPollError = boundedStatus(err.Error())
	} else {
		if state.Authoritative == nil {
			state.LastSourceTransition = "estimated_to_authoritative"
		} else {
			view := state.view(now, accountByIdentity(r.snapshot.Accounts, identity), r.snapshot.Config)
			state.LastDivergence = formatQuotaDivergence(view, snapshot)
		}
		state.Authoritative = &snapshot
		state.LastPollError = ""
		state.ConsecutiveFailures = 0
		// Keep the fallback buckets aligned with the plan the upstream
		// actually reports (ARCHITECTURE.md: "server-reported plan level
		// selects the normal Lite/Pro/Max defaults"): a configured Max
		// account that upstream reports as Lite must fall back to Lite
		// capacity during an outage, not the stale configured buckets.
		syncPlanFromUpstream(r.snapshot.Accounts, identity, snapshot.Plan)
	}
	r.snapshot.Quota[identity] = state
	view := state.view(now, accountByIdentity(r.snapshot.Accounts, identity), r.snapshot.Config)
	health := r.snapshot.Health[identity]
	health.CapacityExhausted = atOrAboveThreshold(view.FiveHour.ConsumedMicrocredits, view.FiveHour.BucketMicrocredits, r.snapshot.Config.ThresholdPercent) ||
		atOrAboveThreshold(view.Weekly.ConsumedMicrocredits, view.Weekly.BucketMicrocredits, r.snapshot.Config.ThresholdPercent)
	if health.CapacityExhausted {
		health.CapacityResetAt = earliestReset(view.FiveHour.ResetsAt, view.Weekly.ResetsAt)
		health.CapacitySource = view.Source + " quota threshold"
	} else {
		health.CapacityResetAt = time.Time{}
		health.CapacitySource = view.Source
	}
	r.snapshot.Health[identity] = health
	persisted := r.persistenceSnapshotLocked()
	store := r.snapshot.Store
	r.mu.Unlock()
	persistErr := r.persistState(store, persisted)
	if err != nil {
		return err
	}
	return persistErr
}

// syncPlanFromUpstream aligns an account's plan and fallback buckets with a
// plan reported by the quota endpoint. Explicit custom credit buckets are
// never overridden — only named-plan defaults follow the upstream plan.
func syncPlanFromUpstream(accounts []account, identity, plan string) {
	if plan == "" {
		return
	}
	buckets, known := planBuckets[plan]
	if !known {
		return
	}
	for i := range accounts {
		if accounts[i].Identity == identity {
			// Explicit account or stored-setting choices take precedence over
			// upstream-derived named-plan defaults, including persisted snapshots
			// loaded during reconfigure.
			if accounts[i].Plan == "custom" || accounts[i].planExplicit || accounts[i].fiveHourCreditsExplicit || accounts[i].weeklyCreditsExplicit {
				return
			}
			if accounts[i].Plan != plan {
				accounts[i].Plan = plan
				accounts[i].FiveHourCredits = buckets.FiveHour
				accounts[i].WeeklyCredits = buckets.Weekly
			}
			return
		}
	}
}

// carryForwardNamedPlans preserves an upstream-discovered named plan across a
// same-store reconfigure that staged before the poll completed. Explicit plan
// or bucket overrides on either side remain authoritative and are never
// mistaken for upstream-derived state.
func carryForwardNamedPlans(live, staged *runtimeSnapshot) {
	liveAccounts := make(map[string]account, len(live.Accounts))
	for _, item := range live.Accounts {
		liveAccounts[item.Identity] = item
	}
	for i := range staged.Accounts {
		liveAccount, exists := liveAccounts[staged.Accounts[i].Identity]
		if !exists || liveAccount.Plan == "custom" || liveAccount.planExplicit || liveAccount.fiveHourCreditsExplicit || liveAccount.weeklyCreditsExplicit {
			continue
		}
		if staged.Accounts[i].planExplicit || staged.Accounts[i].fiveHourCreditsExplicit || staged.Accounts[i].weeklyCreditsExplicit {
			continue
		}
		buckets, known := planBuckets[liveAccount.Plan]
		if !known {
			continue
		}
		staged.Accounts[i].Plan = liveAccount.Plan
		staged.Accounts[i].FiveHourCredits = buckets.FiveHour
		staged.Accounts[i].WeeklyCredits = buckets.Weekly
	}
}

// persistState commits a persistence snapshot. Commits are serialized and
// versioned: an older generation that reaches the mutex after a newer
// generation's commit must never overwrite it (persistMu orders the write,
// the monotonic generation check rejects stale ones).
func (r *pluginRuntime) persistState(store *secureStore, state persistedState) error {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	if state.Generation != 0 && state.Generation <= r.persisted.Load() {
		return nil
	}
	persist := r.persist
	if persist == nil {
		persist = func(store *secureStore, state persistedState) error { return store.saveState(state) }
	}
	if err := persist(store, state); err != nil {
		r.mu.Lock()
		if r.snapshot != nil {
			for identity, accountState := range r.snapshot.Quota {
				accountState.PersistenceWarning = true
				accountState.DeliveryWarning = true
				r.snapshot.Quota[identity] = accountState
			}
		}
		r.mu.Unlock()
		return fmt.Errorf("persist quota state: %w", err)
	}
	if state.Generation != 0 {
		r.persisted.Store(state.Generation)
	}
	return nil
}

func (r *pluginRuntime) persistenceSnapshotLocked() persistedState {
	state := persistedState{Version: 1, Accounts: make(map[string]accountQuotaState, len(r.snapshot.Quota)), Generation: r.persistNext.Add(1)}
	for identity, accountState := range r.snapshot.Quota {
		accountState.Events = append([]creditEvent(nil), accountState.Events...)
		accountState.DedupHashes = append([]string(nil), accountState.DedupHashes...)
		if accountState.Authoritative != nil {
			copyQuota := *accountState.Authoritative
			accountState.Authoritative = &copyQuota
		}
		state.Accounts[identity] = accountState
	}
	return state
}

func (r *pluginRuntime) pollFailureCount(identity string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		return 0
	}
	return r.snapshot.Quota[identity].ConsecutiveFailures
}

func (r *pluginRuntime) quotaView(identity string) (accountQuotaView, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		return accountQuotaView{}, false
	}
	state, ok := r.snapshot.Quota[identity]
	if !ok {
		return accountQuotaView{}, false
	}
	return state.view(r.runtimeClock().Now(), accountByIdentity(r.snapshot.Accounts, identity), r.snapshot.Config), true
}

func (r *pluginRuntime) managementStatus(status string) managementStatusBody {
	now := r.runtimeClock().Now()
	r.mu.RLock()
	validationError := ""
	if r.lastErr != nil {
		validationError = boundedStatus(r.lastErr.Error())
	}
	result := managementStatusBody{
		Plugin:          pluginID,
		Status:          status,
		Version:         pluginVersion,
		GeneratedAt:     now,
		ValidationError: validationError,
	}
	defer r.mu.RUnlock()
	if r.snapshot == nil {
		return result
	}
	result.Accounts = make([]managementAccountStatus, 0, len(r.snapshot.Accounts))
	for _, item := range r.snapshot.Accounts {
		view := r.snapshot.Quota[item.Identity].view(now, item, r.snapshot.Config)
		accountStatus := managementAccountStatus{
			Name:                   item.Name,
			KeySuffix:              item.KeySuffix,
			Plan:                   item.Plan,
			FiveHourUtilization:    utilization(view.FiveHour),
			WeeklyUtilization:      utilization(view.Weekly),
			FiveHourResetsAt:       view.FiveHour.ResetsAt,
			WeeklyResetsAt:         view.Weekly.ResetsAt,
			QuotaSource:            view.Source,
			QuotaObservedAt:        view.ObservedAt,
			QuotaStale:             view.Stale,
			QuotaError:             view.Warning,
			Offpeak:                isOffpeak(now),
			Health:                 quotaHealth(item, view, r.snapshot.Config.ThresholdPercent),
			EstimatorCompleteSince: view.CompleteSince,
			DeliveryWarning:        view.DeliveryWarning,
			PersistenceWarning:     view.PersistenceWarning,
			UnknownModelWarning:    view.UnknownModelWarning,
			HeuristicDedupWarning:  view.DedupCollisionWarn,
			DedupMode:              "bounded_hash_heuristic",
		}
		if !view.ObservedAt.IsZero() {
			accountStatus.QuotaAgeSeconds = maxInt64(0, int64(now.Sub(view.ObservedAt)/time.Second))
		}
		result.Accounts = append(result.Accounts, accountStatus)
	}
	return result
}

func utilization(window quotaWindow) float64 {
	if window.BucketMicrocredits <= 0 {
		return 1
	}
	return float64(window.ConsumedMicrocredits) / float64(window.BucketMicrocredits)
}

func quotaHealth(item account, view accountQuotaView, threshold int) string {
	if item.Disabled {
		return "disabled"
	}
	if atOrAboveThreshold(view.FiveHour.ConsumedMicrocredits, view.FiveHour.BucketMicrocredits, threshold) || atOrAboveThreshold(view.Weekly.ConsumedMicrocredits, view.Weekly.BucketMicrocredits, threshold) {
		return "exhausted"
	}
	return "healthy"
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func earliestReset(left, right time.Time) time.Time {
	if left.IsZero() {
		return right.UTC()
	}
	if right.IsZero() || left.Before(right) {
		return left.UTC()
	}
	return right.UTC()
}

func (r *pluginRuntime) runtimeClock() clock {
	if r.clock != nil {
		return r.clock
	}
	return realClock{}
}

func (r *pluginRuntime) quotaClient() httpDoer {
	if r.httpClient != nil {
		return r.httpClient
	}
	return newQuotaHTTPClient()
}

func (r *pluginRuntime) quotaEndpoint() string {
	if r.endpoint != "" {
		return r.endpoint
	}
	return quotaEndpoint
}

func (r *pluginRuntime) shutdown() error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.reconfigures.Wait()
	r.pollersMu.Lock()
	defer r.pollersMu.Unlock()
	r.workers.Wait()
	// In-flight usage handlers persist after dropping r.mu; join them so no
	// accepted record writes state after — or is lost from — this final flush.
	r.usage.Wait()

	r.mu.RLock()
	snapshot := r.snapshot
	var persisted persistedState
	if snapshot != nil {
		persisted = r.persistenceSnapshotLocked()
	}
	r.mu.RUnlock()
	if snapshot == nil || snapshot.Store == nil {
		return nil
	}
	persist := r.persist
	if persist == nil {
		persist = func(store *secureStore, state persistedState) error { return store.saveState(state) }
	}
	if err := persist(snapshot.Store, persisted); err != nil {
		return fmt.Errorf("persist quota state: %w", err)
	}
	return snapshot.Store.flush()
}

func maxPollInterval(base time.Duration) time.Duration {
	return base + base/2
}

func jitteredPollInterval(base time.Duration, identity string, attempt int) time.Duration {
	if base <= 0 {
		base = defaultQuotaRefresh
	}
	minimum := base / 2
	span := base
	var seed uint64
	if len(identity) >= 16 {
		seed = binary.LittleEndian.Uint64([]byte(identity[:8])) ^ binary.LittleEndian.Uint64([]byte(identity[8:16]))
	} else {
		for i := range identity {
			seed = seed*131 + uint64(identity[i])
		}
	}
	seed ^= uint64(attempt) * 0x9e3779b97f4a7c15
	return minimum + time.Duration(seed%uint64(span+1))
}

func pollBackoff(interval time.Duration, failures int) time.Duration {
	if failures <= 1 {
		return interval
	}
	shift := failures - 1
	if shift > 4 {
		shift = 4
	}
	delay := interval * time.Duration(1<<shift)
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}

func accountByIdentity(accounts []account, identity string) account {
	for _, item := range accounts {
		if item.Identity == identity {
			return item
		}
	}
	return account{}
}

func formatQuotaDivergence(view accountQuotaView, authoritative quotaSnapshot) string {
	return boundedStatus(fmt.Sprintf("five_hour_microcredits=%d weekly_microcredits=%d", authoritative.FiveHour.ConsumedMicrocredits-view.FiveHour.ConsumedMicrocredits, authoritative.Weekly.ConsumedMicrocredits-view.Weekly.ConsumedMicrocredits))
}

func cpaProviderKeys(cpa cpaConfigProjection) []string {
	keys := make([]string, 0, len(cpa.ClaudeKeys)+len(cpa.OpenAICompatibility))
	for i := range cpa.ClaudeKeys {
		keys = append(keys, cpa.ClaudeKeys[i].APIKey)
	}
	for i := range cpa.OpenAICompatibility {
		for j := range cpa.OpenAICompatibility[i].APIKeyEntries {
			keys = append(keys, cpa.OpenAICompatibility[i].APIKeyEntries[j].APIKey)
		}
	}
	return keys
}

func redactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return errors.New(message)
}

func boundedStatus(message string) string {
	const maxStatusLength = 240
	clean := strings.Join(strings.Fields(message), " ")
	if len(clean) > maxStatusLength {
		clean = clean[:maxStatusLength]
	}
	return clean
}
