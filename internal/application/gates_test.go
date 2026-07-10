package application

import (
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestAuthorizePROpenRequiresPassForExactVerifiedHead(t *testing.T) {
	valid := PROpenEvidence{
		Review: domain.ReviewOutcome{
			Verdict:         domain.ReviewPass,
			Summary:         "No findings",
			ReviewedHeadSHA: "abc123",
		},
		CurrentHeadSHA:      "abc123",
		VerificationHeadSHA: "abc123",
	}
	if err := AuthorizePROpen(domain.StateFreshReview, valid); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*PROpenEvidence)
	}{
		{
			name: "findings verdict",
			mutate: func(e *PROpenEvidence) {
				e.Review.Verdict = domain.ReviewFindings
				e.Review.Findings = []domain.ReviewFinding{{
					ID: "finding-1", Severity: "high", Title: "Risk", Body: "Fix it",
				}}
			},
		},
		{name: "review head mismatch", mutate: func(e *PROpenEvidence) { e.Review.ReviewedHeadSHA = "old" }},
		{name: "verification head mismatch", mutate: func(e *PROpenEvidence) { e.VerificationHeadSHA = "old" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := valid
			test.mutate(&evidence)
			if err := AuthorizePROpen(domain.StateFreshReview, evidence); err == nil {
				t.Fatal("invalid PR-open evidence must be rejected")
			}
		})
	}
}
