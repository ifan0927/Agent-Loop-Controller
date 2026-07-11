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
}
