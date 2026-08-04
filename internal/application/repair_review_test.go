package application

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func repairReviewFixture(t *testing.T, sources ...string) (Run, RunInspection, *repairReviewContext) {
	t.Helper()
	findings := make([]FindingRecord, 0, len(sources))
	for index, source := range sources {
		body := "untrusted finding body " + source
		finding := FindingRecord{
			RunID: "run", Source: source, SourceID: "finding-" + string(rune('a'+index)),
			Body: body, BodyDigest: bytesHash([]byte(body)), HeadSHA: "previous-head",
			File: "internal/example.go", Line: index + 10,
		}
		if source == freshReviewFindingSource {
			finding.SourceID = "fresh-review:finding-" + string(rune('a'+index))
		}
		if source == "github_human_review_comment" {
			finding.ThreadID = "thread-a"
		}
		findings = append(findings, finding)
	}
	evidence := repairEvidenceFor(findings)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	inspection := RunInspection{
		Timeline: []Transition{{From: domain.StateRepairing, To: domain.StateExecuting, BoundHead: "previous-head", EvidenceReference: string(encoded)}},
		Findings: findings,
	}
	for _, finding := range findings {
		if finding.Source != "github_human_review_comment" {
			continue
		}
		inspection.TrustedFeedback = append(inspection.TrustedFeedback, TrustedReviewFeedbackRecord{TrustedReviewFeedback: domain.TrustedReviewFeedback{
			RootCommentNodeID: finding.SourceID, ThreadNodeID: finding.ThreadID, OriginalReviewHeadSHA: finding.HeadSHA,
			Body: finding.Body, BodyDigest: finding.BodyDigest, Lifecycle: domain.TrustedReviewFeedbackSelectedForRepair,
		}})
	}
	run := Run{ID: "run", CandidateHead: "repaired-head"}
	context, err := buildRepairReviewContext(run, inspection)
	if err != nil {
		t.Fatal(err)
	}
	return run, inspection, context
}

func addressedDisposition(finding repairReviewFindingContext) domain.ReviewFindingDisposition {
	return domain.ReviewFindingDisposition{
		Source: finding.Source, SourceID: finding.SourceID, BodyDigest: finding.BodyDigest,
		Status: domain.ReviewFindingAddressed, Summary: "The exact finding is materially addressed.",
	}
}

func TestRepairReviewContextBindsEverySupportedFindingAndBothDeltas(t *testing.T) {
	_, _, context := repairReviewFixture(t, "github_human_review_comment", "github_required_check", freshReviewFindingSource)
	if context == nil || len(context.ExpectedFindings) != 3 || context.PreviousCandidateSHA != "previous-head" || context.RepairedCandidateSHA != "repaired-head" {
		t.Fatalf("context=%+v", context)
	}
	prompt, err := repairReviewPrompt(*context)
	if err != nil || !strings.Contains(prompt, "previous-head") || !strings.Contains(prompt, "repaired-head") || !strings.Contains(prompt, "untrusted_text") || !strings.Contains(prompt, "complete branch") {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
}

func TestRepairReviewPassRequiresExactAddressedDispositionCoverage(t *testing.T) {
	_, _, context := repairReviewFixture(t, "github_required_check", freshReviewFindingSource)
	dispositions := make([]domain.ReviewFindingDisposition, 0, len(context.ExpectedFindings))
	for _, finding := range context.ExpectedFindings {
		dispositions = append(dispositions, addressedDisposition(finding))
	}
	valid := domain.ReviewOutcome{SchemaVersion: domain.ReviewOutcomeSchemaVersion, Verdict: domain.ReviewPass, Summary: "ready", ReviewedHeadSHA: context.RepairedCandidateSHA, Findings: []domain.ReviewFinding{}, ExpectedFindingDispositions: dispositions}
	if err := validateRepairReviewOutcome(valid, context); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.ReviewOutcome)
	}{
		{name: "missing", mutate: func(outcome *domain.ReviewOutcome) {
			outcome.ExpectedFindingDispositions = outcome.ExpectedFindingDispositions[:1]
		}},
		{name: "duplicate", mutate: func(outcome *domain.ReviewOutcome) {
			outcome.ExpectedFindingDispositions[1] = outcome.ExpectedFindingDispositions[0]
		}},
		{name: "digest mismatch", mutate: func(outcome *domain.ReviewOutcome) {
			outcome.ExpectedFindingDispositions[0].BodyDigest = strings.Repeat("a", 64)
		}},
		{name: "not addressed", mutate: func(outcome *domain.ReviewOutcome) {
			outcome.ExpectedFindingDispositions[0].Status = domain.ReviewFindingNotAddressed
		}},
		{name: "stale candidate", mutate: func(outcome *domain.ReviewOutcome) { outcome.ReviewedHeadSHA = "later-head" }},
		{name: "legacy schema", mutate: func(outcome *domain.ReviewOutcome) { outcome.SchemaVersion = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := valid
			outcome.ExpectedFindingDispositions = append([]domain.ReviewFindingDisposition(nil), valid.ExpectedFindingDispositions...)
			test.mutate(&outcome)
			if err := validateRepairReviewOutcome(outcome, context); err == nil {
				t.Fatal("invalid repair review outcome was accepted")
			}
		})
	}
}

func TestRepairReviewReplayRejectsChangedPersistedFindingEvidence(t *testing.T) {
	run, inspection, _ := repairReviewFixture(t, "github_required_check")
	inspection.Findings[0].Body = "tampered"
	if _, err := buildRepairReviewContext(run, inspection); err == nil {
		t.Fatal("tampered persisted finding was reused")
	}
	_, exactInspection, _ := repairReviewFixture(t, "github_required_check")
	exactInspection.Findings[0].Resolved = true
	exactInspection.Findings[0].Outdated = true
	if _, err := buildRepairReviewContext(run, exactInspection); err != nil {
		t.Fatalf("exact immutable repair evidence should survive later remote flags: %v", err)
	}
	run.CandidateHead = "previous-head"
	if _, err := buildRepairReviewContext(run, exactInspection); err == nil {
		t.Fatal("no-op repair candidate was accepted for post-repair review")
	}
}

func TestInitialReviewRequiresNoRepairDispositionsButAcceptsLegacyReplay(t *testing.T) {
	legacy := domain.ReviewOutcome{Verdict: domain.ReviewPass, Summary: "legacy", ReviewedHeadSHA: "head"}
	if err := validateRepairReviewOutcome(legacy, nil); err != nil {
		t.Fatal(err)
	}
	legacy.ExpectedFindingDispositions = []domain.ReviewFindingDisposition{{Source: "x", SourceID: "y", BodyDigest: strings.Repeat("a", 64), Status: domain.ReviewFindingAddressed, Summary: "unexpected"}}
	if err := validateRepairReviewOutcome(legacy, nil); err == nil {
		t.Fatal("initial review disposition was accepted")
	}
}
