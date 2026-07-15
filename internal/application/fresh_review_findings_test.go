package application

import (
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func freshReviewFindingTestRun() Run {
	return Run{ID: "run", CandidateHead: "head"}
}

func TestNormalizeFreshReviewFindingsRejectsCountBeforePersistence(t *testing.T) {
	findings := make([]domain.ReviewFinding, 0, MaxNormalizedFindings+1)
	for index := 0; index <= MaxNormalizedFindings; index++ {
		findings = append(findings, domain.ReviewFinding{ID: string(rune('a' + index)), Severity: "medium", Title: "Finding", Body: "body"})
	}
	_, err := normalizeFreshReviewFindings(freshReviewFindingTestRun(), domain.ReviewOutcome{Verdict: domain.ReviewFindings, Summary: "findings", ReviewedHeadSHA: "head", Findings: findings})
	if err == nil {
		t.Fatal("unbounded fresh review finding count was normalized")
	}
}

func TestNormalizeFreshReviewFindingsRejectsAggregatePromptBeforePersistence(t *testing.T) {
	findings := make([]domain.ReviewFinding, 0, 5)
	for index := 0; index < 5; index++ {
		findings = append(findings, domain.ReviewFinding{ID: string(rune('a' + index)), Severity: "medium", Title: "Finding", Body: strings.Repeat("x", 15_000)})
	}
	_, err := normalizeFreshReviewFindings(freshReviewFindingTestRun(), domain.ReviewOutcome{Verdict: domain.ReviewFindings, Summary: "findings", ReviewedHeadSHA: "head", Findings: findings})
	if err == nil {
		t.Fatal("oversized fresh review prompt was normalized")
	}
}

func TestNormalizeFreshReviewFindingsRejectsInvalidItemBeforePersistence(t *testing.T) {
	line := 0
	_, err := normalizeFreshReviewFindings(freshReviewFindingTestRun(), domain.ReviewOutcome{
		Verdict:         domain.ReviewFindings,
		Summary:         "findings",
		ReviewedHeadSHA: "head",
		Findings: []domain.ReviewFinding{{
			ID: "finding-1", Severity: "unknown", Title: "Finding", Body: "body", Line: &line,
		}},
	})
	if err == nil {
		t.Fatal("invalid fresh review finding shape was normalized")
	}
}

func TestFreshReviewRepairEvidenceRejectsConflictingSourceIdentity(t *testing.T) {
	evidence := FreshReviewRepairEvidence{
		RunID: "run", AttemptID: 1, ReviewedHead: "head", OutcomePath: "/private/review.json", OutcomeHash: strings.Repeat("a", 64),
		Findings: []FindingRecord{{RunID: "run", Source: FreshReviewFindingSource, SourceID: "fresh-review:finding", HeadSHA: "head", Severity: "medium", Body: "body", BodyDigest: bytesHash([]byte("body"))}},
	}
	if err := evidence.Validate(); err != nil {
		t.Fatal(err)
	}
	duplicate := evidence
	duplicate.Findings = append(append([]FindingRecord(nil), evidence.Findings...), evidence.Findings[0])
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate fresh review source identity was accepted")
	}
}
