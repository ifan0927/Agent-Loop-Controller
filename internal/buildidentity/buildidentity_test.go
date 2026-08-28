package buildidentity

import (
	"strings"
	"testing"
)

func TestCurrentBuildIdentityIsAllowlistedAndSchemaBound(t *testing.T) {
	first := Current("test-version", 41)
	second := Current("test-version", 42)
	if first.ProductVersion != "test-version" || first.SupportedControllerSchemaVersion != 41 {
		t.Fatalf("build info=%+v", first)
	}
	if !strings.HasPrefix(first.BuildIdentity, "sha256:") || len(first.BuildIdentity) != 71 {
		t.Fatalf("build identity=%q", first.BuildIdentity)
	}
	if first.BuildIdentity == second.BuildIdentity {
		t.Fatal("supported database schema did not bind build identity")
	}
}
