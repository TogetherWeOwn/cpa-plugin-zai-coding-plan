package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginID = "zai-coding-plan"

// pluginVersion is stamped at build time with -ldflags.
var pluginVersion = "0.0.0-dev"

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *envelopeError) Error() string { return e.Code + ": " + e.Message }

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// capabilities mirrors the rpcCapabilities wire schema of CLIProxyAPI's
// internal/pluginhost. Every true field must be backed by a handler in
// pluginCall: the host rejects registrations that advertise no capability
// (internal/pluginhost/host.go validPlugin) and warns on every advertised
// method that fails to answer.
type capabilities struct {
	Scheduler     bool `json:"scheduler"`
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           "TogetherWeOwn",
			GitHubRepository: "https://github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan",
		},
		// Advertised per docs/ARCHITECTURE.md: quota-aware scheduler,
		// usage accounting feed, and management status endpoints. The
		// scheduler declines every pick until quota state exists, which
		// keeps the host's native scheduler in control.
		Capabilities: capabilities{
			Scheduler:     true,
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}

// schedulerPick always declines in the scaffold. A Handled=false response
// makes the host fall back to its native scheduler, matching the
// architecture contract: CPA's native scheduling stays in control while
// all accounts are healthy.
func schedulerPick(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
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

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
