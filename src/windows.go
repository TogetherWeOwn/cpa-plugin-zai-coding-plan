package main

import (
	"sort"
	"time"
)

const (
	fiveHourWindow = 5 * time.Hour
	weeklyWindow   = 7 * 24 * time.Hour
	maxDedupHashes = 4096
)

type creditEvent struct {
	At           time.Time `json:"at"`
	Microcredits int64     `json:"microcredits"`
	Model        string    `json:"model"`
}

type accountQuotaState struct {
	Authoritative        *quotaSnapshot `json:"authoritative,omitempty"`
	LastPollAttempt      time.Time      `json:"last_poll_attempt,omitempty"`
	LastPollError        string         `json:"last_poll_error,omitempty"`
	ConsecutiveFailures  int            `json:"consecutive_failures,omitempty"`
	Events               []creditEvent  `json:"events,omitempty"`
	DedupHashes          []string       `json:"dedup_hashes,omitempty"`
	CompleteSince        time.Time      `json:"complete_since,omitempty"`
	DeliveryWarning      bool           `json:"delivery_warning,omitempty"`
	PersistenceWarning   bool           `json:"persistence_warning,omitempty"`
	UnknownModelWarning  bool           `json:"unknown_model_warning,omitempty"`
	DedupCollisionWarn   bool           `json:"dedup_collision_warning,omitempty"`
	LastSourceTransition string         `json:"last_source_transition,omitempty"`
	LastDivergence       string         `json:"last_divergence,omitempty"`
}

type accountQuotaView struct {
	Source              string
	FiveHour            quotaWindow
	Weekly              quotaWindow
	ObservedAt          time.Time
	Stale               bool
	Warning             string
	CompleteSince       time.Time
	DeliveryWarning     bool
	PersistenceWarning  bool
	UnknownModelWarning bool
	DedupCollisionWarn  bool
}

func (state *accountQuotaState) addEvent(event creditEvent) {
	if event.Microcredits <= 0 {
		return
	}
	state.Events = append(state.Events, event)
}

func (state *accountQuotaState) compact(now time.Time, retention time.Duration) {
	cutoff := now.Add(-retention)
	first := sort.Search(len(state.Events), func(index int) bool {
		return state.Events[index].At.After(cutoff)
	})
	if first > 0 {
		state.Events = append([]creditEvent(nil), state.Events[first:]...)
	}
	if len(state.DedupHashes) > maxDedupHashes {
		state.DedupHashes = append([]string(nil), state.DedupHashes[len(state.DedupHashes)-maxDedupHashes:]...)
	}
}

func (state *accountQuotaState) seenDedup(hash string) bool {
	for _, existing := range state.DedupHashes {
		if existing == hash {
			state.DedupCollisionWarn = true
			return true
		}
	}
	state.DedupHashes = append(state.DedupHashes, hash)
	if len(state.DedupHashes) > maxDedupHashes {
		copy(state.DedupHashes, state.DedupHashes[len(state.DedupHashes)-maxDedupHashes:])
		state.DedupHashes = state.DedupHashes[:maxDedupHashes]
	}
	return false
}

func (state accountQuotaState) view(now time.Time, item account, cfg pluginConfig) accountQuotaView {
	fresh := state.Authoritative != nil && now.Sub(state.Authoritative.ObservedAt) <= cfg.AuthoritativeMaxAge
	if fresh {
		return accountQuotaView{
			Source:              "authoritative",
			FiveHour:            state.Authoritative.FiveHour,
			Weekly:              state.Authoritative.Weekly,
			ObservedAt:          state.Authoritative.ObservedAt,
			Warning:             boundedStatus(state.LastPollError),
			CompleteSince:       state.CompleteSince,
			DeliveryWarning:     state.DeliveryWarning,
			PersistenceWarning:  state.PersistenceWarning,
			UnknownModelWarning: state.UnknownModelWarning,
			DedupCollisionWarn:  state.DedupCollisionWarn,
		}
	}
	return accountQuotaView{
		Source:              "estimated",
		FiveHour:            estimatedWindow(state.Events, now, fiveHourWindow, item.FiveHourCredits*creditScale, cfg.ThresholdPercent),
		Weekly:              estimatedWindow(state.Events, now, weeklyWindow, item.WeeklyCredits*creditScale, cfg.ThresholdPercent),
		ObservedAt:          state.LastPollAttempt,
		Stale:               state.Authoritative != nil,
		Warning:             boundedStatus(state.LastPollError),
		CompleteSince:       state.CompleteSince,
		DeliveryWarning:     true,
		PersistenceWarning:  state.PersistenceWarning,
		UnknownModelWarning: state.UnknownModelWarning,
		DedupCollisionWarn:  state.DedupCollisionWarn,
	}
}

func estimatedWindow(events []creditEvent, now time.Time, duration time.Duration, bucket int64, thresholdPercent int) quotaWindow {
	cutoff := now.Add(-duration)
	active := make([]creditEvent, 0, len(events))
	var consumed int64
	for _, event := range events {
		if event.At.After(cutoff) && !event.At.After(now) {
			active = append(active, event)
			consumed = saturatingAdd(consumed, event.Microcredits)
		}
	}
	window := quotaWindow{ConsumedMicrocredits: consumed, BucketMicrocredits: bucket}
	if !atOrAboveThreshold(consumed, bucket, thresholdPercent) {
		return window
	}
	remaining := consumed
	for _, event := range active {
		remaining -= event.Microcredits
		if !atOrAboveThreshold(remaining, bucket, thresholdPercent) {
			window.ResetsAt = event.At.Add(duration)
			break
		}
	}
	return window
}

func atOrAboveThreshold(consumed, bucket int64, thresholdPercent int) bool {
	if bucket <= 0 {
		return true
	}
	threshold := bucket / 100 * int64(thresholdPercent)
	threshold += bucket % 100 * int64(thresholdPercent) / 100
	return consumed >= threshold
}

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > int64(^uint64(0)>>1)-right {
		return int64(^uint64(0) >> 1)
	}
	return left + right
}
