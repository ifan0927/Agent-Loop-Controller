package application

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRoutineRunDecisionProjectionSanitizesContentAndObservesWorkerHandoff(t *testing.T) {
	now := time.Date(2026, 9, 3, 4, 0, 0, 0, time.UTC)
	outcome := domain.AgentOutcome{Status: domain.AgentNeedsHumanDecision, Summary: "decision required", DecisionRequest: &domain.DecisionRequest{
		Question: "Choose an option?", Context: "token=private-value /private/controller/path", BlockingReason: "A persisted choice is required.", Recommendation: "safe", Options: []domain.DecisionOption{{ID: "safe", Description: "Use /private/controller/path."}, {ID: "other", Description: "Use the other contract."}},
	}}
	raw, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "outcome.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "run", Repository: "owner/repo", State: domain.StateAwaitingHumanDecision, UpdatedAt: now}
	inspection := RunInspection{Run: run, Attempts: []Attempt{{Kind: "implementation", Status: "succeeded", StartedAt: now.Add(-time.Minute), OutcomePath: path, OutcomeHash: bytesHash(raw)}}}
	offers := []LegalActionOffer{{Action: OperationDecide, Scope: ScopeRun, TargetID: run.ID}}
	detail := projectRoutineRunDetail(inspection, offers, now)
	if detail.Decision == nil || detail.Decision.ContentTrust != "untrusted" || len(detail.Decision.Options) != 2 || detail.Decision.Options[0].ID != "safe" {
		t.Fatalf("decision projection=%+v", detail.Decision)
	}
	projected, _ := json.Marshal(detail.Decision)
	if strings.Contains(string(projected), "private-value") || strings.Contains(string(projected), "/private/controller/path") || !strings.Contains(string(projected), "[redacted]") || !strings.Contains(string(projected), "[redacted path]") {
		t.Fatalf("decision projection leaked private content: %s", projected)
	}

	acceptedAt := now.Add(time.Second)
	inspection.Run.State = domain.StateExecuting
	inspection.Timeline = append(inspection.Timeline, Transition{Sequence: 4, From: domain.StateAwaitingHumanDecision, To: domain.StateExecuting, CreatedAt: acceptedAt})
	detail = projectRoutineRunDetail(inspection, nil, acceptedAt)
	if detail.Decision != nil || detail.DecisionHandoff == nil || detail.DecisionHandoff.Status != RoutineDecisionAwaitingWorker || detail.DecisionHandoff.ResumeObservedAt != nil {
		t.Fatalf("accepted handoff=%+v decision=%+v", detail.DecisionHandoff, detail.Decision)
	}
	resumeAt := acceptedAt.Add(time.Second)
	inspection.Attempts = append(inspection.Attempts, Attempt{Kind: "resume", Status: "prepared", StartedAt: resumeAt, ArtifactDir: "/private/prepared"})
	detail = projectRoutineRunDetail(inspection, nil, resumeAt)
	if detail.DecisionHandoff == nil || detail.DecisionHandoff.Status != RoutineDecisionAwaitingWorker || detail.DecisionHandoff.ResumeObservedAt != nil {
		t.Fatalf("prepared attempt claimed a worker resume=%+v", detail.DecisionHandoff)
	}
	inspection.Attempts = append(inspection.Attempts, Attempt{Kind: "resume", Status: "started", StartedAt: resumeAt, ArtifactDir: "/private/resume"})
	detail = projectRoutineRunDetail(inspection, nil, resumeAt)
	if detail.DecisionHandoff == nil || detail.DecisionHandoff.Status != RoutineDecisionWorkerResumed || detail.DecisionHandoff.ResumeObservedAt == nil || !detail.DecisionHandoff.ResumeObservedAt.Equal(resumeAt) {
		t.Fatalf("resumed handoff=%+v", detail.DecisionHandoff)
	}
	handoff, _ := json.Marshal(detail.DecisionHandoff)
	if strings.Contains(string(handoff), "private") {
		t.Fatalf("handoff leaked private evidence: %s", handoff)
	}
}
