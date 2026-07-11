package application

import (
	"context"
	"strings"
	"testing"
)

func TestLegacyRunWithoutModelEvidenceFailsClosed(t *testing.T) {
	controller := &LocalController{}
	err := controller.execute(context.Background(), Run{ID: "legacy-run"}, nil)
	if err == nil || !strings.Contains(err.Error(), "missing or unsupported implementation model evidence") {
		t.Fatalf("error=%v", err)
	}
	if err := validateRunModelPolicy(Run{ImplementationModel: "gpt-5.6-terra"}); err == nil || !strings.Contains(err.Error(), "review model evidence") {
		t.Fatalf("legacy approval model error=%v", err)
	}
}
