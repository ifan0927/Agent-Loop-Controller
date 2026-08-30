package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestCleanupSourceRecoveryCLIRequiresTypedApplyAuthority(t *testing.T) {
	for _, args := range [][]string{
		{"preview"},
		{"apply", "run", "--replacement-source", "/private/replacement"},
		{"unknown", "run", "--replacement-source", "/private/replacement"},
	} {
		if err := controllerRecoverCleanupSource(args); err == nil {
			t.Fatalf("args=%v accepted", args)
		}
	}
}

func TestCleanupSourceRecoveryPreviewProjectionContainsNoPrivateTopology(t *testing.T) {
	value := application.CleanupSourceRecoveryPreview{Eligible: true, PreviewDigest: strings.Repeat("a", 64), RequiredConfirmation: application.CleanupSourceRecoveryConfirmation, ResourceClasses: []string{"worktree"}, NextAction: application.CleanupSourceRecoveryNextAction}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(raw))
	for _, forbidden := range []string{"source_path", "replacement_source", "origin", "worktree_path", "inode", "url", "credential", "raw_git"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("preview leaked %s: %s", forbidden, text)
		}
	}
}
