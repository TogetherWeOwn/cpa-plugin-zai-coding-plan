package coordinator

import "testing"

// TestParseCoordinatorConfigAcceptsHostInjectedFields pins the actual host
// contract: pluginhost's normalizedConfigNode (both v7.2.67 and v7.2.157)
// always injects enabled/priority scalars into config_yaml before the plugin
// sees it -- it never strips them. KnownFields(true) must declare and ignore
// these fields or every real reconfigure call fails with "field enabled not
// found in type coordinator.coordinatorConfig".
func TestParseCoordinatorConfigAcceptsHostInjectedFields(t *testing.T) {
	raw := []byte("cpa-config-path: /tmp/config.yaml\nproviders:\n  zai:\n    default-plan: pro\nenabled: true\npriority: 1000\n")
	cfg, err := parseCoordinatorConfig(raw)
	if err != nil {
		t.Fatalf("parseCoordinatorConfig() error = %v, want host-injected enabled/priority to be accepted", err)
	}
	if cfg.CPAConfigPath != "/tmp/config.yaml" {
		t.Fatalf("CPAConfigPath = %q, want /tmp/config.yaml", cfg.CPAConfigPath)
	}
	if _, ok := cfg.Providers["zai"]; !ok {
		t.Fatal("providers.zai missing after parsing host-injected config")
	}
}

// TestParseCoordinatorConfigWithoutHostFields covers the shape used in
// hand-written test fixtures across this package: no enabled/priority at
// all, which must remain valid too.
func TestParseCoordinatorConfigWithoutHostFields(t *testing.T) {
	raw := []byte("cpa-config-path: /tmp/config.yaml\nproviders:\n  zai:\n    default-plan: pro\n")
	if _, err := parseCoordinatorConfig(raw); err != nil {
		t.Fatalf("parseCoordinatorConfig() error = %v, want fixture shape without host fields to remain valid", err)
	}
}
