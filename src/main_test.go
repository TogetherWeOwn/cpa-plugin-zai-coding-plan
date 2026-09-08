package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginRegistration(t *testing.T) {
	registration := pluginRegistration()
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	if registration.Metadata.Name != pluginID {
		t.Fatalf("plugin name = %q, want %q", registration.Metadata.Name, pluginID)
	}
	// The pinned host rejects plugins that advertise no capability
	// (internal/pluginhost host.go validPlugin). Every advertised
	// capability must also have a handler in pluginCall.
	if registration.Capabilities != (capabilities{Scheduler: true, UsagePlugin: true, ManagementAPI: true}) {
		t.Fatalf("capabilities = %#v, want scheduler, usage_plugin, and management_api", registration.Capabilities)
	}
}

func TestSchedulerPickDeclines(t *testing.T) {
	raw, err := schedulerPick(nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var pick pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(envelope.Result, &pick); err != nil {
		t.Fatal(err)
	}
	if pick.Handled {
		t.Fatal("scaffold scheduler must decline so the host's native scheduler stays in control")
	}
	if pick.AuthID != "" || pick.DelegateBuiltin != "" {
		t.Fatalf("declined pick must not select an auth: %#v", pick)
	}
}

func TestManagementRegistrationRoutes(t *testing.T) {
	raw, err := okEnvelope(managementRegistration())
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var routes managementRoutes
	if err := json.Unmarshal(envelope.Result, &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes.Routes) != 1 {
		t.Fatalf("routes = %#v, want exactly one route", routes.Routes)
	}
	route := routes.Routes[0]
	if route.Method != "GET" || route.Path != managementStatusPath {
		t.Fatalf("route = %#v, want GET %s", route, managementStatusPath)
	}
}

func TestManagementHandleStatus(t *testing.T) {
	request, err := json.Marshal(pluginapi.ManagementRequest{
		Method: "GET",
		Path:   managementStatusPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := managementHandle(request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var status struct {
		Plugin  string `json:"plugin"`
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(envelope.Result, &status); err != nil {
		t.Fatal(err)
	}
	if status.Plugin != pluginID || status.Status != "registered" || status.Version != pluginVersion {
		t.Fatalf("status = %#v", status)
	}
}

func TestManagementHandleUnknownPath(t *testing.T) {
	request, err := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := managementHandle(request); err == nil {
		t.Fatal("managementHandle() error = nil, want not_found for unknown route")
	}
}

func TestUsageHandleAcknowledges(t *testing.T) {
	record, err := json.Marshal(pluginapi.UsageRecord{Provider: "zai", Model: "glm-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := usageHandle(record)
	if err != nil {
		t.Fatal(err)
	}
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("usageHandle() envelope = %#v, want ok", envelope)
	}
}

func TestEnvelopes(t *testing.T) {
	raw, err := okEnvelope(map[string]string{"status": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	var response envelope
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || len(response.Result) == 0 || response.Error != nil {
		t.Fatalf("unexpected success envelope: %#v", response)
	}

	if err := json.Unmarshal(errorEnvelope("test", "failure"), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "test" {
		t.Fatalf("unexpected error envelope: %#v", response)
	}
}
