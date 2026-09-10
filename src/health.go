package main

import "time"

const (
	healthHealthy   = "healthy"
	healthExhausted = "exhausted"
	healthSuspended = "suspended"
	healthDisabled  = "disabled"
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
	return accountHealth{Status: healthHealthy}
}

func boundedHealthReason(reason string) string {
	return boundedText(reason, 96)
}
