package application

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestInspectionProjectsTrustedFeedbackWithoutRawBody(t *testing.T) {
	secret := "Authorization: Bearer not-for-inspect"
	now := time.Now().UTC()
	result := projectInspection(RunInspection{TrustedFeedback: []TrustedReviewFeedbackRecord{{RunID: "run", TrustedReviewFeedback: domain.TrustedReviewFeedback{PRNumber: 1, PRDatabaseID: 2, PRNodeID: "PR", ReviewDatabaseID: 3, ReviewNodeID: "REVIEW", ThreadNodeID: "THREAD", RootCommentDatabaseID: 4, RootCommentNodeID: "COMMENT", Author: domain.ActorIdentity{DatabaseID: 5, NodeID: "USER", Login: "ifan0927", Type: "User"}, OriginalReviewHeadSHA: strings.Repeat("a", 40), Body: secret, BodyDigest: domain.TrustedReviewFeedbackDigest(secret), SourceAt: now, ObservedAt: now, Lifecycle: domain.TrustedReviewFeedbackObserved}}}, FeedbackConflicts: []TrustedReviewFeedbackConflict{{RootCommentNodeID: "COMMENT", ObservedDigest: "digest", ReasonCode: "immutable_authority_conflict", ObservedAt: now}}})
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), secret) || !result.TrustedFeedback[0].TrustedAuthor || !result.FeedbackConflicts[0].OperatorAttention {
		t.Fatalf("unsafe projection: %s", raw)
	}
}

func TestSanitizeRepositoryPathRejectsLegacyUnsafeFeedbackPaths(t *testing.T) {
	for _, unsafe := range []string{".", "..", "../escape", "/absolute", "~/.secret", "dir\\file", "contains\x00nul", " padded "} {
		if got := sanitizeRepositoryPath(unsafe); got != "" {
			t.Fatalf("unsafe path %q projected as %q", unsafe, got)
		}
	}
	if got := sanitizeRepositoryPath("internal/example.go"); got != "internal/example.go" {
		t.Fatalf("safe repository path=%q", got)
	}

	secret := "legacy raw feedback body"
	now := time.Now().UTC()
	result := projectInspection(RunInspection{TrustedFeedback: []TrustedReviewFeedbackRecord{{TrustedReviewFeedback: domain.TrustedReviewFeedback{Path: ".", Body: secret, BodyDigest: domain.TrustedReviewFeedbackDigest(secret), SourceAt: now, ObservedAt: now}}}})
	if len(result.TrustedFeedback) != 1 || result.TrustedFeedback[0].Path != "" {
		t.Fatalf("legacy unsafe feedback path leaked: %+v", result.TrustedFeedback)
	}
}
