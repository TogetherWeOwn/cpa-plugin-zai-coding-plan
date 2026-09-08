package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestPluginRegistration(t *testing.T) {
	registration := pluginRegistration()
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	if registration.Metadata.Name != pluginID {
		t.Fatalf("plugin name = %q, want %q", registration.Metadata.Name, pluginID)
	}
	if registration.Capabilities != (capabilities{}) {
		t.Fatal("scaffold must not advertise unimplemented capabilities")
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
