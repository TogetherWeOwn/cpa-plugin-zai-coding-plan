package main

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPublishedCreditFormulaVectors(t *testing.T) {
	peak := time.Date(2026, time.September, 7, 7, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		model  string
		detail pluginapi.UsageDetail
		want   int64
	}{
		{name: "glm-5.3", model: "glm-5.3", detail: pluginapi.UsageDetail{InputTokens: 10_000, CacheReadTokens: 10_000, OutputTokens: 10_000}, want: 32_600_000},
		{name: "glm-5.3 flash", model: "glm-5.3-flash", detail: pluginapi.UsageDetail{InputTokens: 10_000, CacheReadTokens: 10_000, OutputTokens: 10_000}, want: 10_860_000},
		{name: "cache creation normal input", model: "glm-5.3", detail: pluginapi.UsageDetail{CacheCreationTokens: 10_000}, want: 6_900_000},
		{name: "generic cached ignored", model: "glm-5.3", detail: pluginapi.UsageDetail{CachedTokens: 999_999}, want: 0},
		{name: "all counters disjoint", model: "glm-5.3", detail: pluginapi.UsageDetail{InputTokens: 1, CacheReadTokens: 1, CacheCreationTokens: 1, OutputTokens: 1}, want: 3_950},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate, err := estimateUsageCredits(pluginapi.UsageRecord{Model: tt.model, Detail: tt.detail}, peak)
			if err != nil || estimate.Microcredits != tt.want {
				t.Fatalf("estimate = %#v, err = %v, want %d", estimate, err, tt.want)
			}
		})
	}
}

func TestCreditRoundingBoundariesAndOffpeakMultiplier(t *testing.T) {
	peak := time.Date(2026, time.September, 7, 6, 0, 0, 0, time.UTC)
	offpeak := time.Date(2026, time.September, 7, 10, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{Model: "glm-5.3-flash", Detail: pluginapi.UsageDetail{CacheReadTokens: 1}}
	peakEstimate, err := estimateUsageCredits(record, peak)
	if err != nil || peakEstimate.Microcredits != 56 || peakEstimate.Offpeak {
		t.Fatalf("peak estimate = %#v, err = %v", peakEstimate, err)
	}
	offpeakEstimate, err := estimateUsageCredits(record, offpeak)
	if err != nil || offpeakEstimate.Microcredits != 28 || !offpeakEstimate.Offpeak {
		t.Fatalf("offpeak estimate = %#v, err = %v", offpeakEstimate, err)
	}
	record.Detail.CacheReadTokens = 3
	offpeakEstimate, err = estimateUsageCredits(record, offpeak)
	if err != nil || offpeakEstimate.Microcredits != 84 {
		t.Fatalf("fixed-point half rounding = %#v, err = %v", offpeakEstimate, err)
	}
}

func TestCreditFormulaLargeCountsAndOverflow(t *testing.T) {
	peak := time.Date(2026, time.September, 7, 7, 0, 0, 0, time.UTC)
	estimate, err := estimateUsageCredits(pluginapi.UsageRecord{Model: "glm-5.3", Detail: pluginapi.UsageDetail{InputTokens: 1_000_000_000_000}}, peak)
	if err != nil || estimate.Microcredits != 690_000_000_000_000 {
		t.Fatalf("large estimate = %#v, err = %v", estimate, err)
	}
	_, err = estimateUsageCredits(pluginapi.UsageRecord{Model: "glm-5.3", Detail: pluginapi.UsageDetail{OutputTokens: math.MaxInt64}}, peak)
	if err == nil {
		t.Fatal("overflowing estimate succeeded")
	}
}

func TestCreditModelAliasesAndUnknownModels(t *testing.T) {
	peak := time.Date(2026, time.September, 7, 7, 0, 0, 0, time.UTC)
	for _, record := range []pluginapi.UsageRecord{
		{Model: "zai/glm-5.3", Detail: pluginapi.UsageDetail{InputTokens: 1}},
		{Model: "GLM-5.3-Flash", Detail: pluginapi.UsageDetail{InputTokens: 1}},
		{Model: "unknown", Alias: "opencode-go/glm-5.3", Detail: pluginapi.UsageDetail{InputTokens: 1}},
	} {
		if _, err := estimateUsageCredits(record, peak); err != nil {
			t.Fatalf("alias record %#v: %v", record, err)
		}
	}
	if _, err := estimateUsageCredits(pluginapi.UsageRecord{Model: "glm-future"}, peak); err == nil {
		t.Fatal("unknown model was priced")
	}
}

func TestUTCPeakBoundaries(t *testing.T) {
	tests := []struct {
		at      time.Time
		offpeak bool
	}{
		{at: time.Date(2026, time.September, 7, 5, 59, 59, 0, time.UTC), offpeak: true},
		{at: time.Date(2026, time.September, 7, 6, 0, 0, 0, time.UTC), offpeak: false},
		{at: time.Date(2026, time.September, 7, 9, 59, 59, 0, time.UTC), offpeak: false},
		{at: time.Date(2026, time.September, 7, 10, 0, 0, 0, time.UTC), offpeak: true},
		{at: time.Date(2026, time.September, 12, 8, 0, 0, 0, time.UTC), offpeak: true},
	}
	for _, tt := range tests {
		if got := isOffpeak(tt.at); got != tt.offpeak {
			t.Fatalf("isOffpeak(%s) = %v", tt.at, got)
		}
	}
}

func TestRollingWindowUsesOpenLeftBoundaryAndEarliestThresholdReset(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	events := []creditEvent{
		{At: now.Add(-fiveHourWindow), Microcredits: 900, Model: "glm-5.3"},
		{At: now.Add(-4 * time.Hour), Microcredits: 400, Model: "glm-5.3"},
		{At: now.Add(-3 * time.Hour), Microcredits: 400, Model: "glm-5.3"},
		{At: now.Add(-2 * time.Hour), Microcredits: 400, Model: "glm-5.3"},
	}
	window := estimatedWindow(events, now, fiveHourWindow, 1_000, 97)
	if window.ConsumedMicrocredits != 1_200 {
		t.Fatalf("consumed = %d, want 1200", window.ConsumedMicrocredits)
	}
	if want := now.Add(-4 * time.Hour).Add(fiveHourWindow); !window.ResetsAt.Equal(want) {
		t.Fatalf("reset = %s, want %s", window.ResetsAt, want)
	}
}
