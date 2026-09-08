package main

import (
	"encoding/json"
	"net/http"

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

// managementStatusPath is served under the host's /v0/management/ prefix
// once the management_api capability is registered.
const managementStatusPath = "/plugins/zai-coding-plan/status"

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

// schedulerPick always declines in the scaffold. A Handled=false response
// makes the host fall back to its native scheduler, matching the
// architecture contract: CPA's native scheduling stays in control while
// all accounts are healthy.
func schedulerPick(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}

// usageHandle acknowledges a usage record. Quota accounting arrives in a
// later slice; the host only logs when the call errors.
func usageHandle(_ []byte) ([]byte, error) {
	return okEnvelope(struct{}{})
}

// managementHandle serves the registered management routes. Requests for
// any other path are rejected so misrouted calls surface in host logs.
func managementHandle(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if req.Path != managementStatusPath {
		return nil, &envelopeError{Code: "not_found", Message: "unknown management route"}
	}
	return okEnvelope(map[string]any{
		"plugin":  pluginID,
		"status":  "registered",
		"version": pluginVersion,
	})
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
