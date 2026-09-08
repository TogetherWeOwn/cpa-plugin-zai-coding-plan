package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
)

func TestRegistry(t *testing.T) {
	raw, err := os.ReadFile("../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()

	registry, err := pluginstore.NewClient(server.Client(), server.URL).FetchRegistry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Plugins) != 1 || registry.Plugins[0].ID != pluginID {
		t.Fatalf("unexpected registry: %#v", registry.Plugins)
	}
}
