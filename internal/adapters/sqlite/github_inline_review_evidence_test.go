package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestGenericGitHubEvidenceStoreCannotSerializeInlineReviewBodies(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const body = "Authorization: Bearer inline-review-body"
	handoff := domain.InlineReviewBodyHandoff{Comments: []domain.InlineReviewBody{{ThreadNodeID: "THREAD", CommentNodeID: "COMMENT", Body: body, BodyDigest: domain.TrustedReviewFeedbackDigest(body)}}}
	if err := handoff.Validate(); err != nil {
		t.Fatal(err)
	}
	createFeedbackRun(t, store, "generic-evidence-run")
	evidence := domain.GitHubReadEvidence{ReviewThreads: []domain.GitHubReviewThread{{NodeID: "THREAD", Comments: []domain.GitHubReviewComment{{NodeID: "COMMENT", BodyDigest: handoff.Comments[0].BodyDigest}}}}}
	if err := store.SaveGitHubEvidence(context.Background(), "generic-evidence-run", evidence); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(context.Background(), "generic-evidence-run")
	if err != nil || inspection.GitHubEvidence == nil {
		t.Fatalf("generic evidence was not persisted: %v", err)
	}
	raw, err := json.Marshal(inspection.GitHubEvidence)
	if err != nil || strings.Contains(string(raw), body) || strings.Contains(string(raw), "Authorization: Bearer") {
		t.Fatalf("raw inline body leaked through generic evidence store: %v", err)
	}
}
