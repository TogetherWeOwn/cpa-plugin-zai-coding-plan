package main

import (
	"encoding/json"
	"net/http"
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
	if len(routes.Routes) != 4 {
		t.Fatalf("routes = %#v, want four routes", routes.Routes)
	}
	if route := routes.Routes[0]; route.Method != "GET" || route.Path != managementStatusPath {
		t.Fatalf("route = %#v, want GET %s", route, managementStatusPath)
	}
}

func TestManagementHandleStatus(t *testing.T) {
	// The host forwards the full request path (internal/pluginhost
	// management.go ServeManagementHTTP passes r.URL.Path verbatim).
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
	// The host decodes the result as pluginapi.ManagementResponse, whose
	// fields carry no JSON tags and whose Body is base64-encoded bytes.
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(envelope.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", resp.StatusCode)
	}
	if got := resp.Headers.Get("Content-Type"); got != managementContentType {
		t.Fatalf("content type = %q, want %q", got, managementContentType)
	}
	var status managementStatusBody
	if err := json.Unmarshal(resp.Body, &status); err != nil {
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
	raw, err := managementHandle(request)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped envelope
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(wrapped.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
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
