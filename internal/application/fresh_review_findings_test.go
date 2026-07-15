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
