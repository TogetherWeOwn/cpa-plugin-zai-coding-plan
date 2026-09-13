package zai

import (
	"testing"

	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/providermodule"
)

var _ providermodule.Module = (*zaiModule)(nil)

func TestNewModuleID(t *testing.T) {
	module := NewModule()
	if module.ID() != "zai" {
		t.Fatalf("ID() = %q, want %q", module.ID(), "zai")
	}
}
