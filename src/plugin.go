package main

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *envelopeError) Error() string { return e.Code + ": " + e.Message }

func (e *envelopeError) WireError() envelopeError { return *e }

func newSchedulerError(code, message string) error {
	return &envelopeError{Code: code, Message: message, Retryable: false}
}

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

// managementStatusPath is the full Management API path the host dispatches:
// ServeManagementHTTP forwards r.URL.Path verbatim (internal/pluginhost
// management.go), and normalizeManagementRoute resolves this declaration to
// the same routing key. Registration and the handler share one constant so
// the advertised route and the served route can never drift apart.
const managementStatusPath = "/v0/management/plugins/zai-coding-plan/status"

// managementContentType is served with every management response body.
const managementContentType = "application/json"

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

// managementRegistration declares the routes this plugin serves. Handler
// fields stay nil on the wire; the host injects its own dispatcher.
func managementRegistration() managementRoutes {
	return managementRoutes{
		Routes: []managementRoute{
			{
				Method:      http.MethodGet,
				Path:        managementStatusPath,
				Description: "Reports zai-coding-plan plugin status.",
			},
		},
	}
}

type managementRoutes struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

// schedulerPick explicitly delegates healthy traffic to CPA's built-in
// round-robin scheduler and takes over only while managed accounts are
// impaired. A hard scheduler error prevents fallback to known-bad capacity.
func schedulerPick(request []byte) ([]byte, error) {
	var pick pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(request, &pick); err != nil {
		return nil, fmt.Errorf("decode scheduler request")
	}
	response, err := runtimeState.pick(pick)
	if err != nil {
		return nil, err
	}
	return okEnvelope(response)
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

// managementHandle serves the registered management routes. The host
// forwards ManagementRequest as-is with the full request path, and decodes
// the envelope result as pluginapi.ManagementResponse (StatusCode, Headers,
// base64 Body). Requests for any other path are rejected so misrouted
// calls surface in host logs.
func managementHandle(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if req.Path != managementStatusPath {
		return nil, &envelopeError{Code: "not_found", Message: "unknown management route"}
	}
	status := "registered"
	if runtimeState.validationStatus() != "" {
		status = "reconfigure_rejected"
	}
	body, err := json.Marshal(runtimeState.managementStatus(status))
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{managementContentType}},
		Body:       body,
	})
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
	Name                   string    `json:"name"`
	KeySuffix              string    `json:"key_suffix"`
	Plan                   string    `json:"plan"`
	FiveHourUtilization    float64   `json:"five_hour_utilization"`
	WeeklyUtilization      float64   `json:"weekly_utilization"`
	FiveHourResetsAt       time.Time `json:"five_hour_resets_at,omitempty"`
	WeeklyResetsAt         time.Time `json:"weekly_resets_at,omitempty"`
	QuotaSource            string    `json:"quota_source"`
	QuotaObservedAt        time.Time `json:"quota_observed_at,omitempty"`
	QuotaAgeSeconds        int64     `json:"quota_age_seconds"`
	QuotaStale             bool      `json:"quota_stale"`
	QuotaError             string    `json:"quota_error,omitempty"`
	Offpeak                bool      `json:"offpeak"`
	Health                 string    `json:"health"`
	EstimatorCompleteSince time.Time `json:"estimator_complete_since,omitempty"`
	DeliveryWarning        bool      `json:"delivery_warning"`
	PersistenceWarning     bool      `json:"persistence_warning"`
	UnknownModelWarning    bool      `json:"unknown_model_warning"`
	HeuristicDedupWarning  bool      `json:"heuristic_dedup_warning"`
	DedupMode              string    `json:"dedup_mode"`
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

type wireError interface {
	error
	WireError() envelopeError
}

func errorEnvelopeFor(err error) []byte {
	if typed, ok := err.(wireError); ok {
		wire := typed.WireError()
		raw, _ := json.Marshal(envelope{OK: false, Error: &wire})
		return raw
	}
	return errorEnvelope("plugin_error", err.Error())
}
