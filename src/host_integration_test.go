//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginhost"

	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/abiclient"
)

// TestManagementRouteEndToEnd pins the full management dispatch contract the
// review flagged: the route declared by management.register must resolve to
// the same routing key the host dispatches, and management.handle must answer
// with pluginapi.ManagementResponse — not a bare status object — or the host
// decodes an empty HTTP response.
//
// The SDK host does not expose ServeManagementHTTP, so this test loads the
// compiled c-shared library through internal/abiclient (the host loader's C
// ABI) and replays the host's wire behavior byte-for-byte: the declared route
// normalizes to "/v0/management/plugins/zai-coding-plan/status",
// ServeManagementHTTP forwards that exact path in ManagementRequest, and the
// response is decoded as pluginapi.ManagementResponse.
func TestManagementRouteEndToEnd(t *testing.T) {
	binary, err := buildTestPlugin(t)
	if err != nil {
		t.Fatalf("build plugin: %v", err)
	}
	client, err := abiclient.Open(binary)
	if err != nil {
		t.Fatalf("open plugin: %v", err)
	}
	defer client.Close()

	// Host side of management.register: rpcManagementRegistrationResponse
	// decodes routes with untagged Method/Path fields.
	registerRaw, err := client.Call(pluginabi.MethodManagementRegister, []byte("{}"))
	if err != nil {
		t.Fatalf("management.register: %v", err)
	}
	registered := decodeEnvelopeResult[struct {
		Routes []pluginapi.ManagementRoute `json:"routes"`
	}](t, registerRaw, pluginabi.MethodManagementRegister)
	if len(registered.Routes) != 1 {
		t.Fatalf("management.register routes = %#v, want exactly one", registered.Routes)
	}
	route := registered.Routes[0]
	if !strings.EqualFold(route.Method, http.MethodGet) {
		t.Fatalf("route method = %q, want GET", route.Method)
	}

	// normalizeManagementRoute (identical in v7.2.67 and v7.2.151): a leading
	// /v0/management/ prefix is trimmed, then re-prefixed, yielding the
	// canonical key the host registers and dispatches.
	dispatchPath := normalizeManagementRouteForTest(t, route.Path)

	// Host side of management.handle: rpcManagementRequest embeds
	// ManagementRequest, whose Path is r.URL.Path — the full dispatch path.
	requestBody, err := json.Marshal(map[string]any{
		"method":           http.MethodGet,
		"path":             dispatchPath,
		"host_callback_id": "test-callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	handleRaw, err := client.Call(pluginabi.MethodManagementHandle, requestBody)
	if err != nil {
		t.Fatalf("management.handle: %v", err)
	}
	resp := decodeEnvelopeResult[pluginapi.ManagementResponse](t, handleRaw, pluginabi.MethodManagementHandle)

	// ServeManagementHTTP maps a zero status to 200 and writes Body verbatim.
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		t.Fatalf("management status = %d, want 200", status)
	}
	var body managementStatusBody
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("management body is not the status JSON: %v (raw %q)", err, resp.Body)
	}
	if body.Plugin != pluginID || body.Status != "registered" {
		t.Fatalf("management body = %#v", body)
	}
}

// normalizeManagementRouteForTest reproduces the host's route normalization
// (internal/pluginhost management.go): trim, force a leading slash, strip a
// redundant /v0/management prefix (keeping the leading slash), strip trailing
// slashes, then re-prefix.
func normalizeManagementRouteForTest(t *testing.T, declared string) string {
	t.Helper()
	const basePath = "/v0/management"
	path := strings.TrimSpace(declared)
	if path == "" {
		t.Fatal("declared route path is empty")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasPrefix(path, basePath+"/") {
		path = strings.TrimPrefix(path, basePath)
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		t.Fatal("declared route path normalizes to the management base path")
	}
	full := basePath + path
	if !strings.HasPrefix(full, basePath+"/") {
		t.Fatalf("normalized path %q is not under %s", full, basePath)
	}
	return full
}

// decodeEnvelopeResult unwraps a plugin envelope the way the host's
// callPlugin does (internal/pluginhost rpc_client.go): a false envelope is
// an error; a true envelope decodes Result into the target type.
func decodeEnvelopeResult[T any](t *testing.T, raw []byte, method string) T {
	t.Helper()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", method, err)
	}
	if !envelope.OK {
		t.Fatalf("plugin call %s failed: %+v", method, envelope.Error)
	}
	var out T
	if err := json.Unmarshal(envelope.Result, &out); err != nil {
		t.Fatalf("decode result %s: %v", method, err)
	}
	return out
}

// TestHostRegistersPlugin loads the compiled plugin through the pinned
// CLIProxyAPI plugin host (the same loader the server uses) and asserts
// the plugin survives registration: the host rejects plugins whose
// metadata is incomplete or that advertise no capability.
func TestHostRegistersPlugin(t *testing.T) {
	binary, err := buildTestPlugin(t)
	if err != nil {
		t.Fatalf("build plugin: %v", err)
	}

	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugins", "linux", "amd64")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	versioned := filepath.Join(pluginDir, pluginID+"-v0.0.0-test.so")
	if err := copyFile(versioned, binary); err != nil {
		t.Fatal(err)
	}

	enabled := true
	host := pluginhost.New()
	host.ApplyConfig(context.Background(), pluginhost.RuntimeConfig{
		Enabled: true,
		Dir:     filepath.Join(root, "plugins"),
		AuthDir: filepath.Join(root, "auth"),
		Configs: map[string]pluginhost.PluginInstanceConfig{
			pluginID: {Enabled: &enabled},
		},
	})
	defer host.ShutdownAll()

	registered := host.RegisteredPlugins()
	found := false
	for _, plugin := range registered {
		if plugin.ID != pluginID {
			continue
		}
		found = true
		if plugin.Metadata.Name != pluginID {
			t.Fatalf("registered name = %q, want %q", plugin.Metadata.Name, pluginID)
		}
		if strings.TrimSpace(plugin.Metadata.Version) == "" {
			t.Fatal("registered version is empty")
		}
	}
	if !found {
		t.Fatalf("plugin %s absent from host registration snapshot; plugins = %#v", pluginID, registered)
	}

	// The scheduler capability must decline rather than select, so the
	// host's native scheduler stays in control (docs/ARCHITECTURE.md).
	resp, handled, errPick := host.PickAuth(context.Background(), pluginapi.SchedulerPickRequest{})
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v", errPick)
	}
	if handled {
		t.Fatalf("PickAuth() handled = true, want scaffold scheduler to decline: %#v", resp)
	}

	if !host.HasScheduler() {
		t.Fatal("HasScheduler() = false, want the advertised scheduler capability to register")
	}
}

// buildTestPlugin compiles the plugin package into a c-shared library the
// same way the Makefile release build does. The module root is the
// repository root; the src package is imported by path from there.
func buildTestPlugin(t *testing.T) (string, error) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("go toolchain required: %w", err)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	repoRoot := filepath.Dir(dir)
	out := filepath.Join(t.TempDir(), pluginID+".so")
	cmd := exec.Command(goTool, "build", "-buildvcs=false", "-buildmode=c-shared",
		"-ldflags", "-X main.pluginVersion=0.0.0-test", "-o", out, "./src")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build: %v\n%s", err, output)
	}
	if _, err := os.Stat(out); err != nil {
		return "", err
	}
	return out, nil
}

func copyFile(dst, src string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o755)
}
