package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type routineOverviewStoreFixture struct {
	snapshot RoutinePersistedOverviewSnapshot
	calls    int
}

type routineRunAttentionStore struct {
	serviceStore
	attention []OperatorAttentionEvent
}

type changingRoutineRunStore struct {
	routineRunAttentionStore
	inspectCalls int
}

func (s *changingRoutineRunStore) Inspect(ctx context.Context, runID string) (RunInspection, error) {
	s.inspectCalls++
	inspection, err := s.routineRunAttentionStore.serviceStore.Inspect(ctx, runID)
	if err == nil && s.inspectCalls >= 3 {
		inspection.Run.UpdatedAt = inspection.Run.UpdatedAt.Add(time.Second)
	}
	return inspection, err
}

func (s routineRunAttentionStore) ListOperatorAttention(_ context.Context, input OperatorAttentionQueryInput) ([]OperatorAttentionEvent, error) {
	if input.Limit < 1 || input.Limit > maxOperatorAttentionProjection {
		return nil, errors.New("operator attention projection limit is out of bounds")
	}
	return append([]OperatorAttentionEvent(nil), s.attention...), nil
}

func (s routineRunAttentionStore) CurrentOperatorAttention(_ context.Context, runID string) (OperatorAttentionEvent, bool, error) {
	for index := len(s.attention) - 1; index >= 0; index-- {
		if s.attention[index].RunID == runID {
			return s.attention[index], true, nil
		}
	}
	return OperatorAttentionEvent{}, false, nil
}

func (f *routineOverviewStoreFixture) ReadRoutineOverviewSnapshot(context.Context, AuthorizedScopeSet, domain.GitHubUserIdentity, int) (RoutinePersistedOverviewSnapshot, error) {
	f.calls++
	return f.snapshot, nil
}

func TestRoutineReadinessPrecedence(t *testing.T) {
	tests := []struct {
		states []AggregateReadiness
		want   AggregateReadiness
	}{
		{[]AggregateReadiness{AggregateReady}, AggregateReady},
		{[]AggregateReadiness{AggregateReady, AggregateDegraded}, AggregateDegraded},
		{[]AggregateReadiness{AggregateOffline, AggregateRestartRequired}, AggregateRestartRequired},
		{[]AggregateReadiness{AggregateAttentionRequired, AggregateUnknown}, AggregateUnknown},
		{[]AggregateReadiness{AggregateConflict, AggregateUnknown, AggregateReady}, AggregateConflict},
		{nil, AggregateUnknown},
	}
	for _, test := range tests {
		if got := ClassifyAggregateReadiness(test.states...); got != test.want {
			t.Fatalf("states=%v got=%s want=%s", test.states, got, test.want)
		}
	}
}

func TestRoutineOverviewUsesOnePersistedSnapshotAndDeterministicActionOrder(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	runtime, reader, _, requester := runtimeObservationFixture(t, currentRuntimeEvidence(now), RuntimeHeartbeatCurrent, RuntimeProcessObservation{State: RuntimeProcessPresent, StartIdentity: "darwin:100:2"})
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	store := &routineOverviewStoreFixture{snapshot: RoutinePersistedOverviewSnapshot{
		ObservedAt:     now.Add(-time.Second),
		Settings:       RoutineSettingsProjection{Convergence: ConfigurationConvergenceProjection{State: ConfigurationReady}},
		Attention:      []RoutineAttentionSummary{{EventID: "attention-1", State: RoutineAttentionActive}},
		AttentionTotal: 1,
		Actionable: []RoutineActionableItem{
			{ItemID: "later", Scope: ScopeRun, TargetID: "run-2", Severity: "warning", ObservedAt: now},
			{ItemID: "first", Scope: ScopeRun, TargetID: "run-1", Severity: "critical", ObservedAt: now},
		},
	}}
	service, err := NewRoutineOverviewService(store, authorizer, runtime)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := service.Get(context.Background(), requester, now)
	if err != nil {
		t.Fatal(err)
	}
	if store.calls != 1 || reader.reads != 1 || projection.Readiness != AggregateAttentionRequired || projection.ActionableTotal != 2 || len(projection.Actionable) != 2 || projection.Actionable[0].ItemID != "first" || projection.Actionable[1].ItemID != "later" || projection.Metadata.Digest == "" {
		t.Fatalf("projection=%+v persisted_reads=%d heartbeat_reads=%d", projection, store.calls, reader.reads)
	}
}

func TestRoutineDeliveryGatesUseFixedOrderAndTypedEvidence(t *testing.T) {
	now := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	merge := strings.Repeat("c", 40)
	inspection := RunInspection{
		Run:              Run{ID: "run-1", IssueID: "IFAN-133", Repository: "owner/repo", State: domain.StateCompleted, CandidateHead: head},
		Verifications:    []VerificationRecord{{VerifiedHead: head, ProcessOutcome: VerificationOutcomeExited, ExitCode: 0, CreatedAt: now}},
		Reviews:          []ReviewRecord{{ReviewedHead: head, Verdict: string(domain.ReviewPass), CreatedAt: now}},
		Resources:        []OwnedResource{{Kind: "remote_branch", Status: "owned", CreatedAt: now}},
		SideEffects:      []SideEffectRecord{{Kind: "push", Status: "observed", IdempotencyKey: head, ResultJSON: `{"pushed_sha":"` + head + `","exit_code":0}`, ObservedAt: now}},
		PullRequest:      &domain.PullRequest{Number: 133, HeadSHA: head, State: "MERGED", Merged: true},
		GitHubEvidence:   &domain.GitHubReadEvidence{PullRequest: domain.PullRequest{Number: 133, HeadSHA: head, State: "OPEN"}, Checks: []domain.GitHubCheck{{Required: true, State: domain.CheckSuccess, ObservedSHA: head}}, ReviewThreads: []domain.GitHubReviewThread{{Resolved: true}}, ObservedAt: now},
		Approval:         &domain.HumanApproval{PRNumber: 133, ApprovedSHA: head, ReviewSHA: head, ObservedAt: now},
		Merge:            &MergeRecord{RunID: "run-1", PRNumber: 133, PreMergeSHA: head, BaseSHA: base, Method: "squash", MergeSHA: merge, MergedAt: now},
		LinearCompletion: []LinearCompletionObservation{{RunID: "run-1", MergeSHA: merge, Status: LinearCompletionCompleted, ObservedAt: now}},
		Cleanup:          []CleanupRecord{{Kind: "source_checkout", Status: "synced", UpdatedAt: now}, {Kind: "worktree", Status: "deleted", UpdatedAt: now}},
	}
	gates := ClassifyRoutineDeliveryGates(inspection)
	if len(gates) != len(routineDeliveryGateOrder) {
		t.Fatalf("gate count=%d", len(gates))
	}
	for index, gate := range gates {
		if gate.Name != routineDeliveryGateOrder[index] || gate.Status != GatePassed {
			t.Fatalf("gate[%d]=%+v", index, gate)
		}
	}
}

func TestRoutineDeliveryGateNeverPassesStaleHeadEvidence(t *testing.T) {
	now := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	inspection := RunInspection{Run: Run{ID: "run-1", State: domain.StateVerifying, CandidateHead: strings.Repeat("a", 40)}, Verifications: []VerificationRecord{{VerifiedHead: strings.Repeat("b", 40), ProcessOutcome: VerificationOutcomeExited, ExitCode: 0, CreatedAt: now}}}
	gate := ClassifyRoutineDeliveryGates(inspection)[0]
	if gate.Status != GateConflict || gate.ReasonCode != "stale_head_evidence" {
		t.Fatalf("gate=%+v", gate)
	}
}

func TestRoutineRunPhaseAndWaitAssessmentRemainApplicationOwned(t *testing.T) {
	tests := []struct {
		name       string
		state      domain.State
		attention  []RoutineAttentionSummary
		gates      []RoutineDeliveryGate
		wantPhase  RoutineRunPhase
		wantWait   RoutineWaitClassification
		assessment RoutineWaitAssessment
	}{
		{name: "working", state: domain.StateExecuting, wantPhase: RoutinePhaseImplementation, wantWait: RoutineWaitNone, assessment: RoutineAssessmentProgressing},
		{name: "normal wait", state: domain.StateAwaitingHumanApproval, wantPhase: RoutinePhaseApproval, wantWait: RoutineWaitHumanApproval, assessment: RoutineAssessmentNormalWait},
		{name: "abnormal attention", state: domain.StatePROpen, attention: []RoutineAttentionSummary{{State: RoutineAttentionActive}}, wantPhase: RoutinePhasePullRequest, wantWait: RoutineWaitExternalChecks, assessment: RoutineAssessmentAbnormalWait},
		{name: "manual", state: domain.StateManualIntervention, wantPhase: RoutinePhaseManual, wantWait: RoutineWaitManual, assessment: RoutineAssessmentAbnormalWait},
		{name: "unknown", state: domain.State("future_state"), wantPhase: RoutinePhaseUnknown, wantWait: RoutineWaitUnknown, assessment: RoutineAssessmentUnknown},
		{name: "conflict", state: domain.StateVerifying, gates: []RoutineDeliveryGate{{Status: GateConflict}}, wantPhase: RoutinePhaseVerification, wantWait: RoutineWaitNone, assessment: RoutineAssessmentConflict},
		{name: "ended", state: domain.StateCompleted, gates: []RoutineDeliveryGate{{Status: GateConflict}}, wantPhase: RoutinePhaseEnded, wantWait: RoutineWaitTerminal, assessment: RoutineAssessmentEnded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wait := ClassifyRoutineWait(test.state)
			if phase := ClassifyRoutineRunPhase(test.state); phase != test.wantPhase || wait != test.wantWait {
				t.Fatalf("phase=%q wait=%q", phase, wait)
			}
			if got := classifyRoutineWaitAssessment(wait, test.attention, test.gates); got != test.assessment {
				t.Fatalf("assessment=%q want=%q", got, test.assessment)
			}
		})
	}
}

func TestRoutineRunDetailLoadsCurrentAttentionEvidenceFromStore(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	binding := strings.Repeat("a", 64)
	repository := LocalRepository{
		CanonicalRepository: "owner/repo", RepositoryBindingDigest: binding,
		AllowedOperatorLogins: []string{operator.Login},
		TrustedOperatorActors: []TrustedActorIdentity{{Login: operator.Login, DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, Type: operator.ActorType}},
	}
	raw, err := json.Marshal(repository)
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "run-cleanup", IssueID: "IFAN-206", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), RepositoryBindingDigest: binding, State: domain.StateFailed, UpdatedAt: now}
	event, err := CleanupResidueAttentionEvent(run, 4, strings.Repeat("b", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	store := routineRunAttentionStore{
		serviceStore: serviceStore{run: run, inspection: RunInspection{Run: run, Cleanup: []CleanupRecord{{RunID: run.ID, Kind: "pull_request", Status: "failed", UpdatedAt: now}}}},
		attention:    []OperatorAttentionEvent{event},
	}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewRoutineRunQueryService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := service.Detail(context.Background(), RunDetailQuery{Requester: requesterForUser(operator), RunID: run.ID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Attention) != 1 || detail.Attention[0].EventID != event.EventKey || detail.Attention[0].State != RoutineAttentionActive || !detail.Run.Attention {
		t.Fatalf("run detail attention=%+v run=%+v", detail.Attention, detail.Run)
	}
}

func TestRoutineRunDetailRejectsAuthorityMixedAcrossReads(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 30, 0, 0, time.UTC)
	legal, authorizer, requester := legalActionDecisionFixture(t)
	legal.run.State = domain.StatePROpen
	legal.run.UpdatedAt = now
	legal.inspection = RunInspection{Run: legal.run}
	store := &changingRoutineRunStore{routineRunAttentionStore: routineRunAttentionStore{serviceStore: serviceStore{run: legal.run, inspection: legal.inspection}}}
	service, err := NewRoutineRunQueryService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Detail(context.Background(), RunDetailQuery{Requester: requester, RunID: legal.run.ID}, now)
	var safe *ServiceError
	if !errors.As(err, &safe) || safe.Category != ErrorConflict || store.inspectCalls < 3 {
		t.Fatalf("detail drift err=%v calls=%d", err, store.inspectCalls)
	}
}

type routineQueueStoreFixture struct{ snapshot QueueSnapshot }

func (f routineQueueStoreFixture) LatestQueueSnapshot(context.Context) (QueueSnapshot, bool, error) {
	return f.snapshot, true, nil
}

type routineProfileSourceFixture struct{ profile RepositoryProfileAuthority }

func (f routineProfileSourceFixture) RepositoryProfile(context.Context, string) (RepositoryProfileAuthority, bool, error) {
	return f.profile, true, nil
}
func (f routineProfileSourceFixture) ListRepositoryProfiles(context.Context) ([]RepositoryProfileAuthority, error) {
	return []RepositoryProfileAuthority{f.profile}, nil
}

func TestRoutineQueueSanitizesCandidatesAndRanksSnapshotOrder(t *testing.T) {
	now := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	binding := strings.Repeat("a", 64)
	profile := RepositoryProfileAuthority{Authority: RepositoryAuthority{Repository: "owner/repo", ProfileID: "repo-profile", BindingDigest: binding, AllowedLogins: []string{"operator"}, TrustedOperators: []domain.GitHubUserIdentity{identity}}, Profile: LocalRepository{CanonicalRepository: "owner/repo", ProfileID: "repo-profile", RepositoryBindingDigest: binding}}
	snapshot := QueueSnapshot{Digest: strings.Repeat("b", 64), ObservedAt: now, EffectiveCapacityIdentity: "capacity-v1", Candidates: []QueueCandidateProjection{{IssueUUID: "123e4567-e89b-12d3-a456-426614174000", TeamKey: "IFAN", IssueSequence: 133, Priority: 2, RepositoryProfileID: "repo-profile", RepositoryBindingDigest: binding, Classification: QueueCandidateSelected, ReasonCode: "selected"}}}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: identity})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewRoutineQueueService(routineQueueStoreFixture{snapshot}, authorizer, routineProfileSourceFixture{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Get(context.Background(), Requester{ID: identity.Login, Kind: "github_login", DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, ActorType: identity.ActorType}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].Rank != 1 || result.Candidates[0].LinearIdentifier != "IFAN-133" || result.Candidates[0].Repository != "owner/repo" || result.Metadata.Digest == "" {
		t.Fatalf("result=%+v", result)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "123e4567") || strings.Contains(string(raw), "repository_binding_digest") {
		t.Fatalf("private queue identity leaked: %s", raw)
	}
}
