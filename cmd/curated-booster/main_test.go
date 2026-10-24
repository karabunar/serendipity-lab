// main_test.go directly tests main.go's curator wiring (avoiding the "typed nil"
// trap). The RunBoost tests in booster_test.go were only called with the Go literal
// `nil`, so they never exercised main.go's own
// buildCuratorClient() code path — this file closes that gap.
package main

import (
	"testing"

	"serendipity/internal/curated/grpcclient"
)

// TestBuildCuratorClient_NoEndpoint_ReturnsTrueNilInterface proves that when CURATOR_GRPC_ENDPOINT
// is undefined/empty (the default, the case where the agent service is not yet deployed)
// buildCuratorClient returns a REAL nil interface —
// i.e. the return value of type curatorClient gives true in a `== nil` comparison.
// This is a mandatory invariant for RunBoost's `curator != nil` gate to work correctly
// (see the buildCuratorClient comment in main.go).
func TestBuildCuratorClient_NoEndpoint_ReturnsTrueNilInterface(t *testing.T) {
	t.Setenv("CURATOR_GRPC_ENDPOINT", "")
	got := buildCuratorClient(testLogger())
	if got != nil {
		t.Errorf("expected true nil interface when endpoint unset, got non-nil: %#v", got)
	}
}

// TestBuildCuratorClient_DemonstratesTypedNilTrap is a documentation test:
// it reproduces the wrong (buggy) pattern — assigning a nil *grpcclient.CuratorClient directly
// to a curatorClient interface variable — and proves it REALLY behaves
// differently (the interface is non-nil). This way
// it will stay clear in the future why the real nil interface buildCuratorClient returns
// matters; if someone turns buildCuratorClient back into a `var curator
// *grpcclient.CuratorClient` declaration (concrete type),
// the second assert catches it because the comparison falls into the same trap.
func TestBuildCuratorClient_DemonstratesTypedNilTrap(t *testing.T) {
	var buggyPointer *grpcclient.CuratorClient      // nil concrete pointer
	var buggyInterface curatorClient = buggyPointer // typed-nil trap
	if buggyInterface == nil {
		t.Fatal("test setup invalid: the classic typed-nil trap was expected to reproduce here (an interface must be non-nil even when the underlying pointer is nil) — if this now fails, Go semantics changed or the interface definition changed incompatibly")
	}

	t.Setenv("CURATOR_GRPC_ENDPOINT", "")
	got := buildCuratorClient(testLogger())
	if got != nil {
		t.Errorf("buildCuratorClient must return a TRUE nil interface when disabled, got: %#v", got)
	}
}
