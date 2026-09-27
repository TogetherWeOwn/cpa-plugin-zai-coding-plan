// Package testfixture centralizes host CLIProxyAPI config fixtures shared
// across this repo's test suites, so a config-shape change (e.g. the
// subscription-pool plugin-id rename) happens in exactly one place.
package testfixture

import "os"

// zaiAnthropicBaseURL, zaiOpenAIBaseURL, and zaiCompatName duplicate the
// literals owned by the zai provider module. They are duplicated rather than
// imported to avoid a dependency cycle: the zai module's own tests import
// this package.
const (
	zaiAnthropicBaseURL = "https://api.z.ai/api/anthropic"
	zaiOpenAIBaseURL    = "https://api.z.ai/api/coding/paas/v4"
	zaiCompatName       = "zai-coding-plan"
)

// TB is the subset of testing.TB this package needs, so callers can pass
// *testing.T without this package importing "testing" test-only helpers.
type TB interface {
	Helper()
	Fatal(args ...any)
}

// WriteCPAConfigFixture writes a minimal host CLIProxyAPI config.yaml at
// path, pointing plugins.configs.subscription-pool at authDir and registering
// key as the sole Z.ai account credential.
func WriteCPAConfigFixture(t TB, path, authDir, key string) {
	t.Helper()
	raw := "auth-dir: " + authDir + "\n" +
		"plugins:\n" +
		"  enabled: true\n" +
		"  configs:\n" +
		"    subscription-pool:\n" +
		"      enabled: true\n" +
		"      priority: 1000\n" +
		"claude-api-key:\n" +
		"  - api-key: " + key + "\n" +
		"    base-url: " + zaiAnthropicBaseURL + "\n" +
		"openai-compatibility:\n" +
		"  - name: " + zaiCompatName + "\n" +
		"    base-url: " + zaiOpenAIBaseURL + "\n" +
		"    api-key-entries:\n" +
		"      - api-key: " + key + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}
