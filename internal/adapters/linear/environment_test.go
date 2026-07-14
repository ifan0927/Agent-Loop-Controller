package linear

import (
	"context"
	"testing"
)

func TestEnvironmentCredentialSource(t *testing.T) {
	t.Setenv("IFAN_LOOP_TEST_LINEAR_TOKEN", "token-value")
	source := EnvironmentCredentialSource{Variable: "IFAN_LOOP_TEST_LINEAR_TOKEN"}
	value, err := source.Resolve(context.Background(), EnvironmentCredentialSourceRef)
	if err != nil || value != "token-value" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	if _, err := (EnvironmentCredentialSource{Variable: "IFAN_LOOP_MISSING_LINEAR_TOKEN"}).Resolve(context.Background(), EnvironmentCredentialSourceRef); err == nil {
		t.Fatal("missing token was accepted")
	}
}
