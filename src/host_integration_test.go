//go:build linux && cgo

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginhost"
)

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

// TestManagementRoutePathIsHostReachable pins the registered route to a
// path the pinned host accepts: normalizeManagementRoute drops empty
// paths and any path containing ':' or '*'.
func TestManagementRoutePathIsHostReachable(t *testing.T) {
	for _, route := range managementRegistration().Routes {
		path := strings.TrimSpace(route.Path)
		if path == "" || !strings.HasPrefix(path, "/") {
			t.Fatalf("route path %q must be an absolute path", route.Path)
		}
		if strings.ContainsAny(path, " \t\r\n:*") {
			t.Fatalf("route path %q contains characters the host rejects", route.Path)
		}
		if strings.TrimSpace(route.Method) == "" {
			t.Fatalf("route %q must declare an HTTP method", route.Path)
		}
	}
}
