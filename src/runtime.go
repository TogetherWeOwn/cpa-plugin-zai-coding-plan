package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	Config   pluginConfig
	Accounts []account
	Store    *secureStore
	Quota    map[string]accountQuotaState
	byAuthID map[string]string
}

type pluginRuntime struct {
	mu           sync.RWMutex
	snapshot     *runtimeSnapshot
	lastErr      error
	stopped      bool
	reconfigures sync.WaitGroup
	workers      sync.WaitGroup
	cancel       context.CancelFunc
	clock        clock
	httpClient   httpDoer
	endpoint     string
	persist      func(*secureStore, persistedState) error
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

	quota := make(map[string]accountQuotaState, len(accounts))
	byAuthID := make(map[string]string, len(accounts)*2)
	for i := range accounts {
		state := persisted.Accounts[accounts[i].Identity]
		if state.CompleteSince.IsZero() {
			state.CompleteSince = r.runtimeClock().Now()
		}
		state.compact(r.runtimeClock().Now(), cfg.StateRetention)
		quota[accounts[i].Identity] = state
		byAuthID[accounts[i].ClaudeAuthID] = accounts[i].Identity
		byAuthID[accounts[i].OpenAIAuthID] = accounts[i].Identity
	}
	staged := &runtimeSnapshot{Config: cfg, Accounts: accounts, Store: store, Quota: quota, byAuthID: byAuthID}

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		cancel()
		return fmt.Errorf("plugin is shutting down")
	}
	previousCancel := r.cancel
	r.cancel = cancel
	r.snapshot = staged
	r.lastErr = nil
	r.mu.Unlock()
	if previousCancel != nil {
		previousCancel()
	}
	r.startPollers(ctx, staged)
	return nil
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

func cloneRuntimeSnapshot(source *runtimeSnapshot) *runtimeSnapshot {
	copySnapshot := *source
	copySnapshot.Accounts = append([]account(nil), source.Accounts...)
	copySnapshot.byAuthID = make(map[string]string, len(source.byAuthID))
	for authID, identity := range source.byAuthID {
		copySnapshot.byAuthID[authID] = identity
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
	r.mu.Lock()
	if r.stopped || r.snapshot == nil {
		r.mu.Unlock()
		return fmt.Errorf("plugin is not configured")
	}
	identity := r.snapshot.byAuthID[record.AuthID]
	if identity == "" {
		r.mu.Unlock()
		return nil
	}
	state := r.snapshot.Quota[identity]
	if state.CompleteSince.IsZero() {
		state.CompleteSince = r.runtimeClock().Now()
	}
	if record.Failed && usageDetailEmpty(record.Detail) {
		state.DeliveryWarning = true
		r.snapshot.Quota[identity] = state
		snapshot := r.persistenceSnapshotLocked()
		store := r.snapshot.Store
		r.mu.Unlock()
		return r.persistState(store, snapshot)
	}
	dedupHash := usageDedupHash(record)
	if state.seenDedup(dedupHash) {
		r.snapshot.Quota[identity] = state
		snapshot := r.persistenceSnapshotLocked()
		store := r.snapshot.Store
		r.mu.Unlock()
		return r.persistState(store, snapshot)
	}
	estimate, err := estimateUsageCredits(record, usageTimestamp(record, r.runtimeClock().Now()))
	if err != nil {
		state.UnknownModelWarning = true
		state.DeliveryWarning = true
		r.snapshot.Quota[identity] = state
		snapshot := r.persistenceSnapshotLocked()
		store := r.snapshot.Store
		r.mu.Unlock()
		return r.persistState(store, snapshot)
	}
	state.addEvent(creditEvent{At: usageTimestamp(record, r.runtimeClock().Now()), Microcredits: estimate.Microcredits, Model: estimate.Model})
	state.compact(r.runtimeClock().Now(), r.snapshot.Config.StateRetention)
	r.snapshot.Quota[identity] = state
	snapshot := r.persistenceSnapshotLocked()
	store := r.snapshot.Store
	r.mu.Unlock()
	return r.persistState(store, snapshot)
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
		go r.pollAccount(ctx, item.Identity, item.key, snapshot.Config.QuotaRefresh)
	}
}

func (r *pluginRuntime) pollAccount(ctx context.Context, identity, key string, base time.Duration) {
	defer r.workers.Done()
	attempt := 0
	for {
		if err := r.pollOnce(ctx, identity, key); err != nil && errors.Is(err, context.Canceled) {
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

func (r *pluginRuntime) pollOnce(ctx context.Context, identity, key string) error {
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
	if r.stopped || r.snapshot == nil {
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
	}
	r.snapshot.Quota[identity] = state
	persisted := r.persistenceSnapshotLocked()
	store := r.snapshot.Store
	r.mu.Unlock()
	persistErr := r.persistState(store, persisted)
	if err != nil {
		return err
	}
	return persistErr
}

func (r *pluginRuntime) persistState(store *secureStore, state persistedState) error {
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
	return nil
}

func (r *pluginRuntime) persistenceSnapshotLocked() persistedState {
	state := persistedState{Version: 1, Accounts: make(map[string]accountQuotaState, len(r.snapshot.Quota))}
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
	r.workers.Wait()

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
