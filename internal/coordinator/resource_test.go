package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeStatusModule is a fakeModule that returns a fixed Status() payload, so
// tests can drive the resource dashboard with realistic per-provider JSON
// without any real provider logic.
type fakeStatusModule struct {
	fakeModule
	status json.RawMessage
}

func (m *fakeStatusModule) Status(context.Context) (json.RawMessage, error) {
	return m.status, nil
}

func TestResourceStatusResponseRendersAccountsWithoutSecrets(t *testing.T) {
	fiveHourReset := time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)
	weeklyReset := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	zaiStatus, err := json.Marshal(map[string]any{
		"accounts": []map[string]any{
			{
				"identity":              "test-only-private-account-pseudonym",
				"cooldown":              map[string]any{"active": true, "until": fiveHourReset, "reason": "retry_after", "source": "zai_runtime_health_v1"},
				"name":                  "zai-pro-1",
				"five_hour_utilization": 0.42,
				"weekly_utilization":    0.10,
				"five_hour_resets_at":   fiveHourReset,
				"weekly_resets_at":      weeklyReset,
				"quota_source":          "quota_api",
				"quota_stale":           false,
				"health":                "healthy",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal zai status: %v", err)
	}

	monthlyReset := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	utilization := 0.75
	opencodeStatus, err := json.Marshal(map[string]any{
		"accounts": []map[string]any{
			{
				"name": "go-1",
				"windows": map[string]any{
					"monthly": map[string]any{
						"known":         true,
						"utilization":   utilization,
						"exhausted":     false,
						"resets_at":     monthlyReset,
						"source":        "dashboard usage poll",
						"authoritative": true,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal opencode-go status: %v", err)
	}

	zai := &fakeStatusModule{fakeModule: fakeModule{id: "zai"}, status: zaiStatus}
	opencodeGo := &fakeStatusModule{fakeModule: fakeModule{id: "opencode-go"}, status: opencodeStatus}
	c := New(zai, opencodeGo)
	c.now = func() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) }

	resp := c.handleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   resourcePluginBasePath + resourceStatusPath,
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if resp.Headers.Get("Content-Type") != resourceContentType {
		t.Fatalf("Content-Type = %q", resp.Headers.Get("Content-Type"))
	}

	body := string(resp.Body)
	for _, required := range []string{"Subscription Quota", "zai-pro-1", "go-1", "42%", "75%", "quota_api", "authoritative"} {
		if !strings.Contains(body, required) {
			t.Fatalf("resource page missing %q; body = %s", required, body)
		}
	}
	for _, forbidden := range []string{"management", "Bearer", "Authorization", "dashboard-api-key", "key\"", "secret", "test-only-private-account-pseudonym", "retry_after", "zai_runtime_health_v1"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Fatalf("resource page contains forbidden %q", forbidden)
		}
	}

	csp := resp.Headers.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "style-src 'unsafe-inline'", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("content security policy %q missing %q", csp, directive)
		}
	}
	if strings.Contains(csp, "frame-ancestors") {
		t.Fatalf("content security policy %q blocks documented cross-origin Management Center embedding", csp)
	}
	if resp.Headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", resp.Headers.Get("Referrer-Policy"))
	}
	if resp.Headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", resp.Headers.Get("X-Content-Type-Options"))
	}
	if resp.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Headers.Get("Cache-Control"))
	}
}

func TestResourceStatusResponseHandlesLookalikeResourcePath(t *testing.T) {
	c := New(&fakeModule{id: "alpha"})
	resp := c.handleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/attacker" + resourcePluginBasePath + resourceStatusPath,
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("lookalike resource path status = %d, want 404", resp.StatusCode)
	}
}

func TestResourceStatusResponseRendersEmptyStateWhenNoAccountsConfigured(t *testing.T) {
	c := New(&fakeModule{id: "alpha"})
	resp := c.handleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   resourceStatusPath,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "No accounts are configured yet.") {
		t.Fatalf("resource page missing empty state: %s", resp.Body)
	}
}
