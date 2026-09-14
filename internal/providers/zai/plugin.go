package zai

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/providermodule"
)

const pluginID = "zai-coding-plan"

// pluginVersion is stamped at build time via -ldflags -X main.pluginVersion in
// src/, mirrored here by coordinator.PluginVersion so the registered plugin
// metadata and this module's own status body never disagree on version.
var pluginVersion = "0.0.0-dev"

// runtimeState is this module's own runtime instance, driven by zaiModule
// (see module.go) as the coordinator's provider-module boundary.
var runtimeState = &pluginRuntime{}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *envelopeError) Error() string { return e.Code + ": " + e.Message }

// Coded implements providermodule.Coded so errors raised inside this package
// (e.g. by newSchedulerError) carry their wire code, message, and HTTP status
// across the coordinator boundary unchanged, rather than being downgraded to
// a generic error by a package-local type assertion on the other side.
func (e *envelopeError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: e.Code, Message: e.Message, Retryable: e.Retryable, HTTPStatus: e.HTTPStatus}
}

func newSchedulerError(code, message string) error {
	return &envelopeError{Code: code, Message: message, Retryable: false}
}

// usageHandle consumes a lossy best-effort usage observation. Persistence
// failures are surfaced in estimator integrity state, but the response remains
// successful because CPA discards usage-plugin RPC errors.
func usageHandle(request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(request, &record); err != nil {
		return nil, fmt.Errorf("decode usage record")
	}
	_ = runtimeState.handleUsage(record)
	return okEnvelope(struct{}{})
}

// managementStatusBody is the redacted JSON served at the status route.
type managementStatusBody struct {
	Plugin          string                    `json:"plugin"`
	Status          string                    `json:"status"`
	Version         string                    `json:"version"`
	GeneratedAt     time.Time                 `json:"generated_at"`
	ValidationError string                    `json:"validation_error,omitempty"`
	Accounts        []managementAccountStatus `json:"accounts,omitempty"`
}

type managementAccountStatus struct {
	Name                   string     `json:"name"`
	KeySuffix              string     `json:"key_suffix"`
	Plan                   string     `json:"plan"`
	FiveHourUtilization    float64    `json:"five_hour_utilization"`
	WeeklyUtilization      float64    `json:"weekly_utilization"`
	FiveHourResetsAt       *time.Time `json:"five_hour_resets_at"`
	WeeklyResetsAt         *time.Time `json:"weekly_resets_at"`
	QuotaSource            string     `json:"quota_source"`
	QuotaObservedAt        time.Time  `json:"quota_observed_at,omitempty"`
	QuotaAgeSeconds        int64      `json:"quota_age_seconds"`
	QuotaStale             bool       `json:"quota_stale"`
	QuotaError             string     `json:"quota_error,omitempty"`
	FiveHourError          string     `json:"five_hour_error,omitempty"`
	Offpeak                bool       `json:"offpeak"`
	Health                 string     `json:"health"`
	EstimatorCompleteSince time.Time  `json:"estimator_complete_since,omitempty"`
	DeliveryWarning        bool       `json:"delivery_warning"`
	PersistenceWarning     bool       `json:"persistence_warning"`
	UnknownModelWarning    bool       `json:"unknown_model_warning"`
	HeuristicDedupWarning  bool       `json:"heuristic_dedup_warning"`
	DedupMode              string     `json:"dedup_mode"`
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}
