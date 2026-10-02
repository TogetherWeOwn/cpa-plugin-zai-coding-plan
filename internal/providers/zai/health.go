package zai

import "time"

const (
	healthHealthy   = "healthy"
	healthExhausted = "exhausted"
	healthSuspended = "suspended"
	healthDisabled  = "disabled"
	// healthThrottled is a short per-account request-rate backoff. It is
	// impaired for scheduling like exhausted, but pick reports it as a
	// retryable 429 rather than a lane-wide capacity loss.
	healthThrottled = "throttled"
)

type capacityUpdate struct {
	Exhausted bool
	ResetAt   time.Time
	Source    string
}

type accountHealthState struct {
	SuspendedUntil    time.Time
	ExhaustedUntil    time.Time
	ExhaustedReason   string
	CapacityExhausted bool
	CapacityResetAt   time.Time
	CapacitySource    string
	// ThrottledUntil, ThrottleStreak and ThrottleReason track the bounded
	// request-rate backoff (failures.go). They are deliberately in-memory only:
	// a throttle is at most requestRateBackoffCap long, so a restart that
	// forgets it costs one more upstream 429, not a stale block.
	ThrottledUntil time.Time
	ThrottleStreak int
	ThrottleReason string
}

type accountHealth struct {
	Status  string
	Reason  string
	ResetAt time.Time
}

func (state *accountHealthState) suspendUntil(resetAt time.Time) {
	if resetAt.After(state.SuspendedUntil) {
		state.SuspendedUntil = resetAt.UTC()
	}
}

func (state *accountHealthState) exhaustUntil(resetAt time.Time, reason string) {
	if resetAt.After(state.ExhaustedUntil) {
		state.ExhaustedUntil = resetAt.UTC()
		state.ExhaustedReason = boundedHealthReason(reason)
	}
}

// throttle applies one request-rate backoff step and reports whether the
// account should instead take the conservative exhaustion path. A 429 that
// lands while a throttle is already active belongs to the same burst (requests
// dispatched before the first 429 was seen), so it can extend the deadline to
// a hint but never advances the streak.
func (state *accountHealthState) throttle(now, hintedAt time.Time, fallback time.Duration) bool {
	if state.ThrottledUntil.After(now) {
		if hintedAt.After(state.ThrottledUntil) {
			state.ThrottledUntil = hintedAt.UTC()
		}
		return false
	}
	if state.ThrottleStreak > 0 && now.Sub(state.ThrottledUntil) >= requestRateStreakDecay {
		state.ThrottleStreak = 0
	}
	if state.ThrottleStreak >= requestRateMaxThrottles {
		state.clearThrottle()
		return true
	}
	state.ThrottleStreak++
	delay := requestRateBackoff(state.ThrottleStreak, fallback)
	until := now.Add(delay)
	reason := "request-rate backoff"
	if hintedAt.After(now) {
		until = hintedAt
		reason = "request-rate retry hint"
	}
	state.ThrottledUntil = until.UTC()
	state.ThrottleReason = reason
	return false
}

func (state *accountHealthState) clearThrottle() {
	state.ThrottledUntil = time.Time{}
	state.ThrottleStreak = 0
	state.ThrottleReason = ""
}

func (state accountHealthState) persisted(now time.Time) (persistedHealthState, bool) {
	persisted := persistedHealthState{}
	if state.SuspendedUntil.After(now) {
		persisted.SuspendedUntil = state.SuspendedUntil.UTC()
	}
	if state.ExhaustedUntil.After(now) {
		persisted.ExhaustedUntil = state.ExhaustedUntil.UTC()
		persisted.ExhaustedReason = boundedHealthReason(state.ExhaustedReason)
	}
	return persisted, !persisted.SuspendedUntil.IsZero() || !persisted.ExhaustedUntil.IsZero()
}

func restorePersistedHealth(state persistedHealthState, now time.Time) accountHealthState {
	health := accountHealthState{}
	if state.SuspendedUntil.After(now) {
		health.SuspendedUntil = state.SuspendedUntil.UTC()
	}
	if state.ExhaustedUntil.After(now) {
		health.ExhaustedUntil = state.ExhaustedUntil.UTC()
		health.ExhaustedReason = boundedHealthReason(state.ExhaustedReason)
	}
	return health
}

func quotaCapacityHealth(state accountHealthState, view accountQuotaView, threshold int) accountHealthState {
	fiveHourExhausted := atOrAboveThreshold(view.FiveHour.ConsumedMicrocredits, view.FiveHour.BucketMicrocredits, threshold)
	weeklyExhausted := atOrAboveThreshold(view.Weekly.ConsumedMicrocredits, view.Weekly.BucketMicrocredits, threshold)
	state.CapacityExhausted = fiveHourExhausted || weeklyExhausted
	state.CapacityResetAt = time.Time{}
	state.CapacitySource = boundedHealthReason(view.Source)
	if state.CapacityExhausted {
		if fiveHourExhausted {
			state.CapacityResetAt = view.FiveHour.ResetsAt.UTC()
		}
		if weeklyExhausted && (state.CapacityResetAt.IsZero() || view.Weekly.ResetsAt.After(state.CapacityResetAt)) {
			state.CapacityResetAt = view.Weekly.ResetsAt.UTC()
		}
		state.CapacitySource = boundedHealthReason(view.Source + " quota threshold")
	}
	return state
}

func (state accountHealthState) assess(item account, now time.Time) accountHealth {
	now = now.UTC()
	if item.Disabled {
		return accountHealth{Status: healthDisabled, Reason: "administratively disabled"}
	}
	if state.SuspendedUntil.After(now) {
		return accountHealth{Status: healthSuspended, Reason: "upstream authentication failure", ResetAt: state.SuspendedUntil}
	}
	if state.ExhaustedUntil.After(now) {
		return accountHealth{Status: healthExhausted, Reason: state.ExhaustedReason, ResetAt: state.ExhaustedUntil}
	}
	if state.CapacityExhausted {
		reason := state.CapacitySource
		if reason == "" {
			reason = "quota threshold reached"
		}
		resetAt := state.CapacityResetAt
		if !resetAt.After(now) {
			resetAt = time.Time{}
		}
		return accountHealth{Status: healthExhausted, Reason: reason, ResetAt: resetAt}
	}
	if state.ThrottledUntil.After(now) {
		return accountHealth{Status: healthThrottled, Reason: state.ThrottleReason, ResetAt: state.ThrottledUntil}
	}
	return accountHealth{Status: healthHealthy}
}

func boundedHealthReason(reason string) string {
	return boundedText(reason, 96)
}
