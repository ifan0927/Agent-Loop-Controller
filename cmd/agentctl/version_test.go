package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/buildidentity"
)

func TestVersionCommandPreservesPlainOutputAndProvidesStrictJSON(t *testing.T) {
	plain, err := captureConfigOutput(func() error { return versionCommand(nil) })
	if err != nil || strings.TrimSpace(plain) != version {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
	structured, err := captureConfigOutput(func() error { return versionCommand([]string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var got buildidentity.Info
	decoder := json.NewDecoder(strings.NewReader(structured))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil || got.BuildIdentity != currentBuild.BuildIdentity || got.SupportedControllerSchemaVersion != currentBuild.SupportedControllerSchemaVersion {
		t.Fatalf("structured=%s got=%+v err=%v", structured, got, err)
	}
	if err := versionCommand([]string{"--yaml"}); err == nil {
		t.Fatal("unknown version format was accepted")
	}
}
