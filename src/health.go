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
	if state.CapacityExhausted && (state.CapacityResetAt.IsZero() || state.CapacityResetAt.After(now)) {
		reason := state.CapacitySource
		if reason == "" {
			reason = "quota threshold reached"
		}
		return accountHealth{Status: healthExhausted, Reason: reason, ResetAt: state.CapacityResetAt}
	}
	return accountHealth{Status: healthHealthy}
}

func boundedHealthReason(reason string) string {
	return boundedText(reason, 96)
}
