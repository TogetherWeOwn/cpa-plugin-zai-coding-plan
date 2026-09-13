// Package opencodego implements quota-aware scheduling for OpenCode Go
// credentials from the proxy-observable behavior captured in the first-party
// first-party observed-behavior fixture corpus.
package opencodego

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/providermodule"
)

const (
	providerID             = "opencode-go"
	defaultThreshold       = 97
	defaultMicroRetry      = 3
	defaultFailureCooldown = 10 * time.Minute
	maxFailureBody         = 16 << 10
	maxResetFuture         = 35 * 24 * time.Hour
)

type windowKind string

const (
	windowFiveHour windowKind = "five_hour"
	windowWeekly   windowKind = "weekly"
	windowMonthly  windowKind = "monthly"
)

type moduleConfig struct {
	Enabled          bool
	ThresholdPercent int
	Accounts         []accountConfig
}

type accountConfig struct {
	Name     string
	AuthIDs  []string
	Disabled bool
}

type rawModuleConfig struct {
	Enabled          *bool              `json:"enabled"`
	ThresholdPercent *int               `json:"threshold-percent"`
	Accounts         []rawAccountConfig `json:"accounts"`
}

type rawAccountConfig struct {
	Name         string   `json:"name"`
	AuthIDs      []string `json:"auth-ids"`
	ConnectionID string   `json:"connection-id"`
	Disabled     bool     `json:"disabled"`
}

type accountState struct {
	Name     string
	AuthIDs  []string
	Disabled bool
	Windows  map[windowKind]windowState
	cursor   uint64
}

type windowState struct {
	Utilization float64
	Exhausted   bool
	ResetAt     time.Time
	Source      string
}

type moduleState struct {
	Threshold int
	Accounts  map[string]*accountState
	ByAuthID  map[string]string
}

type Module struct {
	mu      sync.Mutex
	state   *moduleState
	lastErr string
	closed  bool
}

var _ providermodule.Module = (*Module)(nil)

func NewModule() *Module { return &Module{} }

func (m *Module) ID() string { return providerID }

func (m *Module) Recognize(candidate pluginapi.SchedulerAuthCandidate) bool {
	m.mu.Lock()
	state := m.state
	_, managed := stateAccountByAuthID(state, candidate.ID)
	m.mu.Unlock()
	if managed {
		return candidateClaimsOpenCodeGo(candidate) || !candidateClaimsOtherProvider(candidate)
	}
	return candidateClaimsOpenCodeGo(candidate)
}

func (m *Module) Reconfigure(_ context.Context, _ providermodule.HostConfig, providerConfig json.RawMessage) error {
	cfg, err := parseConfig(providerConfig)
	if err != nil {
		m.recordError(err)
		return err
	}
	staged, err := buildState(cfg)
	if err != nil {
		m.recordError(err)
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("opencode_go_closed: module is closed")
	}
	carryForwardState(staged, m.state)
	m.state = staged
	m.lastErr = ""
	return nil
}

func (m *Module) OwnedAuthIDs(context.Context) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return nil
	}
	ids := make([]string, 0, len(m.state.ByAuthID))
	for id := range m.state.ByAuthID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Module) Pick(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	now := requestTime(req)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.state == nil {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("opencode_go_not_configured", "OpenCode Go scheduler is not configured")
	}

	recognized := false
	healthyAccounts := make(map[string]*accountState)
	for _, candidate := range req.Candidates {
		name, managed := stateAccountByAuthID(m.state, candidate.ID)
		claims := candidateClaimsOpenCodeGo(candidate)
		if claims {
			recognized = true
			if !managed {
				return pluginapi.SchedulerPickResponse{}, newSchedulerError("opencode_go_unmanaged_candidate", "recognized OpenCode Go credential is absent from the validated account snapshot")
			}
		}
		if !managed {
			continue
		}
		if candidateClaimsOtherProvider(candidate) {
			continue
		}
		recognized = true
		account := m.state.Accounts[name]
		refreshWindows(account, now)
		if accountHealthy(account, m.state.Threshold, now) {
			healthyAccounts[name] = account
		}
	}
	if !recognized {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	if len(healthyAccounts) == 0 {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("opencode_go_no_capacity", "no healthy managed OpenCode Go capacity remains")
	}

	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidateClaimsOtherProvider(candidate) {
			continue
		}
		name, managed := stateAccountByAuthID(m.state, candidate.ID)
		if managed && healthyAccounts[name] != nil {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("opencode_go_no_capacity", "no healthy managed OpenCode Go capacity remains")
	}

	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	accountName, _ := stateAccountByAuthID(m.state, candidates[0].ID)
	account := m.state.Accounts[accountName]
	selected := candidates[int(account.cursor%uint64(len(candidates)))].ID
	account.cursor++
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected}, nil
}

func (m *Module) HandleUsage(_ context.Context, record pluginapi.UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.state == nil {
		return nil
	}
	name, managed := stateAccountByAuthID(m.state, record.AuthID)
	if !managed || !record.Failed {
		return nil
	}
	account := m.state.Accounts[name]
	now := record.RequestedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	if isMicrothrottle(record) {
		return nil
	}
	window, ok := quotaWindow(record.Failure.Body)
	if !ok || record.Failure.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	resetAt, ok := retryAfterReset(record.ResponseHeaders, now)
	if !ok {
		resetAt, ok = bodyReset(record.Failure.Body, now)
	}
	if !ok {
		resetAt = now.Add(defaultFailureCooldown)
	}
	current := account.Windows[window]
	if resetAt.After(current.ResetAt) {
		current.ResetAt = resetAt
	}
	current.Exhausted = true
	current.Utilization = 1
	current.Source = "proxy-observed " + string(window) + " quota response"
	account.Windows[window] = current
	return nil
}

func (m *Module) Status(context.Context) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type statusWindow struct {
		Utilization float64    `json:"utilization"`
		Exhausted   bool       `json:"exhausted"`
		ResetAt     *time.Time `json:"resets_at,omitempty"`
		Source      string     `json:"source,omitempty"`
	}
	type statusAccount struct {
		Name     string                      `json:"name"`
		Disabled bool                        `json:"disabled,omitempty"`
		Windows  map[windowKind]statusWindow `json:"windows"`
	}
	status := struct {
		Provider        string          `json:"provider"`
		Status          string          `json:"status"`
		ValidationError string          `json:"validation_error,omitempty"`
		Accounts        []statusAccount `json:"accounts,omitempty"`
		ObservationGaps []string        `json:"observation_gaps"`
	}{
		Provider:        providerID,
		Status:          "registered",
		ValidationError: m.lastErr,
		ObservationGaps: []string{"five-hour and weekly enforcement are generalized from the observed monthly 429 shape", "authenticated per-account dashboard payload was unavailable"},
	}
	if m.lastErr != "" {
		status.Status = "reconfigure_rejected"
	}
	if m.state != nil {
		now := time.Now().UTC()
		names := make([]string, 0, len(m.state.Accounts))
		for name := range m.state.Accounts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			account := m.state.Accounts[name]
			refreshWindows(account, now)
			item := statusAccount{Name: account.Name, Disabled: account.Disabled, Windows: make(map[windowKind]statusWindow, 3)}
			for _, kind := range []windowKind{windowFiveHour, windowWeekly, windowMonthly} {
				window := account.Windows[kind]
				var resetAt *time.Time
				if !window.ResetAt.IsZero() {
					value := window.ResetAt.UTC()
					resetAt = &value
				}
				item.Windows[kind] = statusWindow{Utilization: window.Utilization, Exhausted: window.Exhausted, ResetAt: resetAt, Source: window.Source}
			}
			status.Accounts = append(status.Accounts, item)
		}
	}
	return json.Marshal(status)
}

func (m *Module) Close(context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.state = nil
	m.mu.Unlock()
	return nil
}

func (m *Module) recordError(err error) {
	m.mu.Lock()
	m.lastErr = bounded(err.Error(), 256)
	m.mu.Unlock()
}

func parseConfig(raw json.RawMessage) (moduleConfig, error) {
	cfg := moduleConfig{Enabled: true, ThresholdPercent: defaultThreshold}
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return cfg, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var input rawModuleConfig
	if err := decoder.Decode(&input); err != nil {
		return moduleConfig{}, fmt.Errorf("opencode_go_invalid_config: decode provider config: %w", err)
	}
	if input.Enabled != nil {
		cfg.Enabled = *input.Enabled
	}
	if input.ThresholdPercent != nil {
		cfg.ThresholdPercent = *input.ThresholdPercent
	}
	if cfg.ThresholdPercent < 1 || cfg.ThresholdPercent > 100 {
		return moduleConfig{}, fmt.Errorf("opencode_go_invalid_config: threshold-percent must be between 1 and 100")
	}
	for i, rawAccount := range input.Accounts {
		name := strings.TrimSpace(rawAccount.Name)
		if name == "" {
			return moduleConfig{}, fmt.Errorf("opencode_go_invalid_config: accounts[%d].name is required", i)
		}
		ids := append([]string(nil), rawAccount.AuthIDs...)
		if connectionID := strings.TrimSpace(rawAccount.ConnectionID); connectionID != "" {
			ids = append(ids, connectionID)
		}
		ids = normalizeIDs(ids)
		if len(ids) == 0 {
			return moduleConfig{}, fmt.Errorf("opencode_go_invalid_config: accounts[%d] requires auth-ids or connection-id", i)
		}
		cfg.Accounts = append(cfg.Accounts, accountConfig{Name: name, AuthIDs: ids, Disabled: rawAccount.Disabled})
	}
	return cfg, nil
}

func buildState(cfg moduleConfig) (*moduleState, error) {
	state := &moduleState{Threshold: cfg.ThresholdPercent, Accounts: make(map[string]*accountState), ByAuthID: make(map[string]string)}
	if !cfg.Enabled {
		return state, nil
	}
	seenNames := make(map[string]struct{}, len(cfg.Accounts))
	for _, item := range cfg.Accounts {
		key := strings.ToLower(item.Name)
		if _, exists := seenNames[key]; exists {
			return nil, fmt.Errorf("opencode_go_invalid_config: duplicate account name %q", item.Name)
		}
		seenNames[key] = struct{}{}
		account := &accountState{Name: item.Name, AuthIDs: append([]string(nil), item.AuthIDs...), Disabled: item.Disabled, Windows: map[windowKind]windowState{windowFiveHour: {}, windowWeekly: {}, windowMonthly: {}}}
		for _, authID := range item.AuthIDs {
			if owner := state.ByAuthID[authID]; owner != "" {
				return nil, fmt.Errorf("opencode_go_invalid_config: auth id %q belongs to both %q and %q", authID, owner, item.Name)
			}
			state.ByAuthID[authID] = item.Name
		}
		state.Accounts[item.Name] = account
	}
	return state, nil
}

func carryForwardState(staged, previous *moduleState) {
	if staged == nil || previous == nil {
		return
	}
	for name, account := range staged.Accounts {
		old := previous.Accounts[name]
		if old == nil || !sameIDs(account.AuthIDs, old.AuthIDs) {
			continue
		}
		account.cursor = old.cursor
		for _, kind := range []windowKind{windowFiveHour, windowWeekly, windowMonthly} {
			account.Windows[kind] = old.Windows[kind]
		}
	}
}

func sameIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func normalizeIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stateAccountByAuthID(state *moduleState, authID string) (string, bool) {
	if state == nil {
		return "", false
	}
	name := state.ByAuthID[strings.TrimSpace(authID)]
	return name, name != ""
}

func candidateClaimsOpenCodeGo(candidate pluginapi.SchedulerAuthCandidate) bool {
	for _, value := range []string{candidate.Provider, candidate.Attributes["provider_key"], candidate.Attributes["compat_name"], candidate.Attributes["provider"]} {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == providerID || normalized == "openai-compatible-"+providerID {
			return true
		}
	}
	return false
}

func candidateClaimsOtherProvider(candidate pluginapi.SchedulerAuthCandidate) bool {
	for _, value := range []string{candidate.Provider, candidate.Attributes["provider_key"], candidate.Attributes["compat_name"], candidate.Attributes["provider"]} {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" {
			continue
		}
		if normalized != providerID && normalized != "openai-compatible-"+providerID {
			return true
		}
	}
	return false
}

func requestTime(req pluginapi.SchedulerPickRequest) time.Time {
	if req.Options.Metadata != nil {
		switch value := req.Options.Metadata["now"].(type) {
		case time.Time:
			return value.UTC()
		case string:
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed.UTC()
			}
		}
	}
	return time.Now().UTC()
}

func refreshWindows(account *accountState, now time.Time) {
	for _, kind := range []windowKind{windowFiveHour, windowWeekly, windowMonthly} {
		window := account.Windows[kind]
		if !window.ResetAt.IsZero() && !window.ResetAt.After(now) {
			window = windowState{}
			account.Windows[kind] = window
		}
	}
}

func accountHealthy(account *accountState, threshold int, now time.Time) bool {
	if account.Disabled {
		return false
	}
	limit := float64(threshold) / 100
	for _, window := range account.Windows {
		if window.Exhausted && (window.ResetAt.IsZero() || window.ResetAt.After(now)) {
			return false
		}
		if window.Utilization >= limit {
			return false
		}
	}
	return true
}

func quotaWindow(body string) (windowKind, bool) {
	if len(body) == 0 || len(body) > maxFailureBody {
		return "", false
	}
	text := strings.ToLower(body)
	switch {
	case strings.Contains(text, "five-hour usage limit reached"), strings.Contains(text, "5-hour usage limit reached"), strings.Contains(text, "five hour usage limit reached"):
		return windowFiveHour, true
	case strings.Contains(text, "weekly usage limit reached"):
		return windowWeekly, true
	case strings.Contains(text, "monthly usage limit reached"):
		return windowMonthly, true
	default:
		return "", false
	}
}

func isMicrothrottle(record pluginapi.UsageRecord) bool {
	if record.Failure.StatusCode != http.StatusForbidden || len(record.Failure.Body) > maxFailureBody {
		return false
	}
	text := strings.ToLower(record.Failure.Body)
	if !strings.Contains(text, "<!doctype html>") || !strings.Contains(text, "reset after ") {
		return false
	}
	seconds, ok := parseResetAfterSeconds(text)
	return ok && seconds >= 1 && seconds <= defaultMicroRetry
}

func retryAfterReset(headers http.Header, now time.Time) (time.Time, bool) {
	for name, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(name), "Retry-After") {
			continue
		}
		for _, raw := range values {
			value := strings.TrimSpace(raw)
			seconds, err := strconv.ParseInt(value, 10, 64)
			if err == nil && seconds > 0 && seconds <= int64(maxResetFuture/time.Second) {
				return now.Add(time.Duration(seconds) * time.Second), true
			}
			if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) && parsed.Before(now.Add(maxResetFuture)) {
				return parsed.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func bodyReset(body string, now time.Time) (time.Time, bool) {
	seconds, ok := parseResetAfterSeconds(strings.ToLower(body))
	if !ok || seconds <= 0 || seconds > int(maxResetFuture/time.Second) {
		return time.Time{}, false
	}
	return now.Add(time.Duration(seconds) * time.Second), true
}

func parseResetAfterSeconds(text string) (int, bool) {
	marker := "reset after "
	index := strings.LastIndex(text, marker)
	if index < 0 {
		return 0, false
	}
	text = text[index+len(marker):]
	if end := strings.IndexByte(text, ')'); end >= 0 {
		text = text[:end]
	}
	text = strings.TrimSpace(text)
	var total time.Duration
	var number int
	digits := false
	seen := false
	for _, r := range text {
		switch {
		case r >= '0' && r <= '9':
			number = number*10 + int(r-'0')
			digits = true
		case r == 'd' || r == 'h' || r == 'm' || r == 's':
			if !digits {
				return 0, false
			}
			seen = true
			switch r {
			case 'd':
				total += time.Duration(number) * 24 * time.Hour
			case 'h':
				total += time.Duration(number) * time.Hour
			case 'm':
				total += time.Duration(number) * time.Minute
			case 's':
				total += time.Duration(number) * time.Second
			}
			number = 0
			digits = false
		case r == ' ':
			if digits {
				return 0, false
			}
		default:
			return 0, false
		}
	}
	return int(total / time.Second), seen && !digits && total > 0
}

type schedulerError struct {
	code    string
	message string
}

func newSchedulerError(code, message string) error {
	return &schedulerError{code: code, message: message}
}
func (e *schedulerError) Error() string { return e.code + ": " + e.message }
func (e *schedulerError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: e.code, Message: e.message, Retryable: false}
}

func bounded(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return value[:limit-17] + "#" + hex.EncodeToString(digest[:8])
}
