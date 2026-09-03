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
	inspection.OperatorActions = []OperatorActionRecord{{ActionID: "operator-action-safe", ActionType: OperatorActionDecide, Status: OperatorActionStatusObserved, ResultStatus: OperatorActionResultSucceeded, ResultingState: domain.StateExecuting, ReasonCode: "human_decision_required", RequestDigest: "private-request", ExpectedAuthorityDigest: "private-authority", ObservedAt: acceptedAt}}
	inspection.Cleanup = []CleanupRecord{{RunID: run.ID, Kind: "worktree", Name: "/private/worktree", Status: "deleted", LastError: "private-error", UpdatedAt: resumeAt}}
	detail = projectRoutineRunDetail(inspection, nil, resumeAt)
	if detail.DecisionHandoff == nil || detail.DecisionHandoff.Status != RoutineDecisionWorkerResumed || detail.DecisionHandoff.ResumeObservedAt == nil || !detail.DecisionHandoff.ResumeObservedAt.Equal(resumeAt) {
		t.Fatalf("resumed handoff=%+v", detail.DecisionHandoff)
	}
	if len(detail.RecentActions) != 1 || detail.RecentActions[0].ActionID != "operator-action-safe" || len(detail.Cleanup) != 1 || detail.Cleanup[0].ResourceKind != "worktree" || detail.Cleanup[0].Status != "deleted" {
		t.Fatalf("run action/cleanup=%+v %+v", detail.RecentActions, detail.Cleanup)
	}
	handoff, _ := json.Marshal(detail.DecisionHandoff)
	if strings.Contains(string(handoff), "private") {
		t.Fatalf("handoff leaked private evidence: %s", handoff)
	}
	projection, _ := json.Marshal(detail)
	for _, forbidden := range []string{"private-request", "private-authority", "/private/worktree", "private-error", "resource_name"} {
		if strings.Contains(string(projection), forbidden) {
			t.Fatalf("run projection leaked %q: %s", forbidden, projection)
		}
	}
}

func TestRoutineRunRecentActionsUseEffectiveObservationTime(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	actions := []OperatorActionRecord{
		{ActionID: "operator-action-z", TransitionSequence: 7, ReceivedAt: now},
		{ActionID: "operator-action-a", TransitionSequence: 7, ReceivedAt: now.Add(time.Minute)},
		{ActionID: "operator-action-m", TransitionSequence: 7, ReceivedAt: now.Add(30 * time.Second), AppliedAt: now.Add(2 * time.Minute)},
	}
	projected := projectRoutineOperatorActions(actions, 2)
	if len(projected) != 2 || projected[0].ActionID != "operator-action-m" || projected[1].ActionID != "operator-action-a" {
		t.Fatalf("recent actions=%+v", projected)
	}
}
