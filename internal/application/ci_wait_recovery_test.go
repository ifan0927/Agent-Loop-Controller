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

type ciWaitRecoveryStoreFixture struct {
	RunStore
	inspection RunInspection
	events     []OperatorAttentionEvent
	actions    []OperatorActionRecord
	beginErr   error
	applyCalls int
}

func (s *ciWaitRecoveryStoreFixture) Inspect(context.Context, string) (RunInspection, error) {
	value := s.inspection
	value.OperatorAttention = append([]OperatorAttentionEvent(nil), s.events...)
	value.OperatorActions = append([]OperatorActionRecord(nil), s.actions...)
	return value, nil
}

func (s *ciWaitRecoveryStoreFixture) GetRun(context.Context, string) (Run, error) {
	return s.inspection.Run, nil
}

func (s *ciWaitRecoveryStoreFixture) AppendOperatorAttention(_ context.Context, event OperatorAttentionEvent) (bool, error) {
	for _, current := range s.events {
		if current.EventKey == event.EventKey {
			if current.PayloadDigest != event.PayloadDigest {
				return false, errors.New("attention replay changed")
			}
			return false, nil
		}
	}
	s.events = append(s.events, event)
	return true, nil
}

func (s *ciWaitRecoveryStoreFixture) CurrentOperatorAttention(context.Context, string) (OperatorAttentionEvent, bool, error) {
	if len(s.events) == 0 {
		return OperatorAttentionEvent{}, false, nil
	}
	return s.events[len(s.events)-1], true, nil
}

func (s *ciWaitRecoveryStoreFixture) BeginOperatorAction(_ context.Context, action OperatorActionRecord) (OperatorActionRecord, bool, error) {
	if s.beginErr != nil {
		err := s.beginErr
		s.beginErr = nil
		return OperatorActionRecord{}, false, err
	}
	for _, current := range s.actions {
		if current.IdempotencyKey == action.IdempotencyKey {
			return current, false, nil
		}
	}
	s.actions = append(s.actions, action)
	return action, true, nil
}

func (s *ciWaitRecoveryStoreFixture) ApplyOperatorActionResult(context.Context, OperatorActionMutationResult) (OperatorActionRecord, bool, error) {
	return OperatorActionRecord{}, false, errors.New("unexpected generic apply")
}

func (s *ciWaitRecoveryStoreFixture) ObserveOperatorActionResult(_ context.Context, result OperatorActionMutationResult) (OperatorActionRecord, bool, error) {
	for index := range s.actions {
		if s.actions[index].ActionID != result.ActionID {
			continue
		}
		action := &s.actions[index]
		if action.Status == OperatorActionStatusObserved {
			return *action, false, nil
		}
		if action.Status != result.ExpectedStatus {
			return OperatorActionRecord{}, false, errors.New("observe status conflict")
		}
		action.Status = OperatorActionStatusObserved
		action.ResultStatus = result.ResultStatus
		action.OutcomeDigest = result.EvidenceDigest
		action.ObservedAt = result.At
		return *action, true, nil
	}
	return OperatorActionRecord{}, false, errors.New("action not found")
}

func (s *ciWaitRecoveryStoreFixture) ApplyCIWaitRecovery(_ context.Context, input CIWaitRecoveryApply) (OperatorActionRecord, RetrySchedule, bool, error) {
	s.applyCalls++
	for index := range s.actions {
		if s.actions[index].ActionID != input.ActionID {
			continue
		}
		action := &s.actions[index]
		action.Status = OperatorActionStatusApplied
		action.ResultStatus = OperatorActionResultApplied
		action.ResultingState = s.inspection.Run.State
		action.ResultingTransitionSequence = latestTransitionSequence(s.inspection.Timeline)
		action.EvidenceDigest = input.EvidenceDigest
		action.AppliedAt = input.AppliedAt
		for scheduleIndex := range s.inspection.RetrySchedules {
			if s.inspection.RetrySchedules[scheduleIndex].Phase == input.Phase {
				s.inspection.RetrySchedules[scheduleIndex].Status = RetryScheduleSuperseded
				return *action, s.inspection.RetrySchedules[scheduleIndex], true, nil
			}
		}
	}
	return OperatorActionRecord{}, RetrySchedule{}, false, errors.New("recovery action not found")
}

type ciWaitRecoveryRevalidator struct {
	run Run
	err error
}

func (r ciWaitRecoveryRevalidator) RevalidateForCIWaitRecovery(context.Context, LinearRevalidateCommand) (Run, error) {
	return r.run, r.err
}

type ciWaitRecoveryReader struct {
	evidence     domain.GitHubReadEvidence
	authority    GitHubInstallationMetadata
	observations []GitHubRequestObservation
}

func (r *ciWaitRecoveryReader) Authority() GitHubInstallationMetadata { return r.authority }
func (r *ciWaitRecoveryReader) Read(context.Context, int64, string) (domain.GitHubReadEvidence, domain.InlineReviewBodyHandoff, []GitHubRequestObservation, GitHubInstallationMetadata, error) {
	return r.evidence, domain.InlineReviewBodyHandoff{}, r.observations, r.authority, nil
}

type ciWaitRecoveryLocal struct {
	branch      string
	head        string
	validateErr error
}

func (l *ciWaitRecoveryLocal) ValidateOwned(context.Context, WorktreeRecord) error {
	return l.validateErr
}
func (l *ciWaitRecoveryLocal) Head(context.Context, string) (string, error)   { return l.head, nil }
func (l *ciWaitRecoveryLocal) Branch(context.Context, string) (string, error) { return l.branch, nil }

type ciWaitRecoveryFixture struct {
	store     *ciWaitRecoveryStoreFixture
	reader    *ciWaitRecoveryReader
	local     *ciWaitRecoveryLocal
	linearRun Run
	requester Requester
}

func newCIWaitRecoveryFixture(t *testing.T) ciWaitRecoveryFixture {
	t.Helper()
	now := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	repository := domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo", NodeID: "repo-node"}
	requester := Requester{ID: "operator", Kind: "github_login", DatabaseID: 7, NodeID: "user-node", ActorType: "User"}
	config, err := json.Marshal(LocalRepository{ProfileID: "profile", CanonicalRepository: "owner/repo", SourcePath: "/source", OriginPath: "origin", BaseBranch: "main", AllowedOperatorLogins: []string{"operator"}})
	if err != nil {
		t.Fatal(err)
	}
	run := Run{ID: "run", IssueID: "issue", Repository: "owner/repo", RepositoryConfigJSON: string(config), ProfileID: "profile", ProfileSnapshotVersion: 1, ProfileDigest: "profile-digest", ProfileSnapshotJSON: `{}`, IdempotencyKey: "ownership", BaseBranch: "main", WorkingBranch: "ifan/issue", BaseSHA: "base", WorktreePath: "/worktree", CandidateHead: "head", State: domain.StatePROpen}
	pr := domain.PullRequest{Number: 17, DatabaseID: 18, URL: "https://example.invalid/pr/17", NodeID: "pr-node", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "open"}
	ownership, err := json.Marshal(map[string]string{"source_path": "/source", "origin_path": "origin", "path": run.WorktreePath, "branch": run.WorkingBranch, "base_branch": run.BaseBranch, "base_sha": run.BaseSHA, "nonce": "nonce"})
	if err != nil {
		t.Fatal(err)
	}
	resources := []OwnedResource{
		{RunID: run.ID, Kind: "worktree", Name: run.WorktreePath, Status: "owned", CreationEvidence: string(ownership)},
		{RunID: run.ID, Kind: "branch", Name: run.WorkingBranch, Status: "owned", CreationEvidence: string(ownership)},
	}
	schedule := RetrySchedule{RunID: run.ID, Phase: AutomaticRetryPhaseForRun(run), ControllerState: string(run.State), AttemptCount: 1, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: time.Minute, FailureClass: RetryFailureTerminal, ReasonCode: RetryReasonTerminal, Status: RetryScheduleAttention, AttentionAt: now, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	inspection := legacyTopologyInspection()
	inspection.Run = run
	inspection.RepositoryBinding.ProfileID = run.ProfileID
	inspection.RepositoryBinding.ProfileSnapshotVersion = run.ProfileSnapshotVersion
	inspection.RepositoryBinding.ProfileDigest = run.ProfileDigest
	inspection.Resources = resources
	inspection.PullRequest = &pr
	inspection.RetrySchedules = []RetrySchedule{schedule}
	inspection.Timeline = []Transition{{Sequence: 1, From: domain.StateOpeningPR, To: run.State, CreatedAt: now.Add(-time.Minute)}}
	installation := GitHubInstallationMetadata{AppID: 10, InstallationID: 22, Repository: repository, PermissionsDigest: "permissions", ObservedAt: now}
	inspection.GitHubInstallation = &installation
	evidence := domain.GitHubReadEvidence{Repository: repository, PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: run.CandidateHead, State: domain.CheckSuccess}}, ObservedAt: now.Add(time.Minute)}
	observation := GitHubRequestObservation{Operation: "fresh_read", Category: "REST", HTTPStatus: 200, ResponseDigest: digestOperatorRetry("fresh"), InstallationID: installation.InstallationID, Repository: repository, ObservedAt: evidence.ObservedAt}
	return ciWaitRecoveryFixture{
		store:  &ciWaitRecoveryStoreFixture{inspection: inspection},
		reader: &ciWaitRecoveryReader{evidence: evidence, authority: installation, observations: []GitHubRequestObservation{observation}},
		local:  &ciWaitRecoveryLocal{branch: run.WorkingBranch, head: run.CandidateHead}, linearRun: run, requester: requester,
	}
}

func TestCIWaitRecoveryServiceSuccessAndAuthorityRejections(t *testing.T) {
	command := func(fixture ciWaitRecoveryFixture) CIWaitRecoveryCommand {
		return CIWaitRecoveryCommand{Requester: fixture.requester, RunID: fixture.store.inspection.Run.ID}
	}
	t.Run("success", func(t *testing.T) {
		fixture := newCIWaitRecoveryFixture(t)
		service, err := NewCIWaitRecoveryService(fixture.store, ciWaitRecoveryRevalidator{run: fixture.linearRun})
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.Recover(context.Background(), command(fixture), fixture.reader, fixture.local)
		if err != nil {
			t.Fatal(err)
		}
		if result.Action.Status != OperatorActionStatusObserved || fixture.store.applyCalls != 1 || len(fixture.store.events) != 1 || fixture.store.inspection.RetrySchedules[0].Status != RetryScheduleSuperseded {
			t.Fatalf("result=%+v apply=%d events=%d schedule=%s", result, fixture.store.applyCalls, len(fixture.store.events), fixture.store.inspection.RetrySchedules[0].Status)
		}
	})

	tests := []struct {
		name   string
		mutate func(*ciWaitRecoveryFixture)
	}{
		{"missing worktree", func(f *ciWaitRecoveryFixture) { f.store.inspection.Resources = f.store.inspection.Resources[1:] }},
		{"branch drift", func(f *ciWaitRecoveryFixture) { f.local.branch = "ifan/other" }},
		{"head drift", func(f *ciWaitRecoveryFixture) { f.local.head = "other-head" }},
		{"closed PR", func(f *ciWaitRecoveryFixture) { f.reader.evidence.PullRequest.State = "closed" }},
		{"merged PR", func(f *ciWaitRecoveryFixture) { f.reader.evidence.PullRequest.Merged = true }},
		{"incomplete required checks", func(f *ciWaitRecoveryFixture) {
			f.reader.evidence.Checks = nil
			f.reader.evidence.UnknownEvents = []string{"unknown_check_event"}
		}},
		{"Linear drift", func(f *ciWaitRecoveryFixture) { f.linearRun.SourceRevision = "other-revision" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCIWaitRecoveryFixture(t)
			test.mutate(&fixture)
			service, err := NewCIWaitRecoveryService(fixture.store, ciWaitRecoveryRevalidator{run: fixture.linearRun})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Recover(context.Background(), command(fixture), fixture.reader, fixture.local); err == nil {
				t.Fatal("authority drift was recoverable")
			}
			if fixture.store.applyCalls != 0 || len(fixture.store.actions) != 0 {
				t.Fatalf("authority drift wrote an action: apply=%d actions=%d", fixture.store.applyCalls, len(fixture.store.actions))
			}
		})
	}
}

func TestCIWaitRecoveryAttentionReplayIsStableAfterAppendBeforePrepareFailure(t *testing.T) {
	fixture := newCIWaitRecoveryFixture(t)
	fixture.store.beginErr = errors.New("injected begin failure")
	service, err := NewCIWaitRecoveryService(fixture.store, ciWaitRecoveryRevalidator{run: fixture.linearRun})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC) }
	command := CIWaitRecoveryCommand{Requester: fixture.requester, RunID: fixture.store.inspection.Run.ID}
	if _, err := service.Recover(context.Background(), command, fixture.reader, fixture.local); err == nil {
		t.Fatal("injected prepare failure was ignored")
	}
	if len(fixture.store.events) != 1 {
		t.Fatalf("events=%d", len(fixture.store.events))
	}
	first := fixture.store.events[0]
	service.now = func() time.Time { return time.Date(2026, 7, 17, 5, 0, 0, 0, time.UTC) }
	result, err := service.Recover(context.Background(), command, fixture.reader, fixture.local)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action.Status != OperatorActionStatusObserved || len(fixture.store.events) != 1 || fixture.store.events[0].EventKey != first.EventKey || fixture.store.events[0].PayloadDigest != first.PayloadDigest || !fixture.store.events[0].OccurredAt.Equal(first.OccurredAt) {
		t.Fatalf("attention replay changed: before=%+v after=%+v", first, fixture.store.events)
	}
}

func TestCIWaitRecoveryAppliedReplayBecomesObserved(t *testing.T) {
	fixture := newCIWaitRecoveryFixture(t)
	service, err := NewCIWaitRecoveryService(fixture.store, ciWaitRecoveryRevalidator{run: fixture.linearRun})
	if err != nil {
		t.Fatal(err)
	}
	command := CIWaitRecoveryCommand{Requester: fixture.requester, RunID: fixture.store.inspection.Run.ID}
	if _, err := service.Recover(context.Background(), command, fixture.reader, fixture.local); err != nil {
		t.Fatal(err)
	}
	action := &fixture.store.actions[0]
	action.Status = OperatorActionStatusApplied
	action.ResultStatus = OperatorActionResultApplied
	action.ObservedAt = time.Time{}
	action.OutcomeDigest = ""
	result, err := service.Recover(context.Background(), command, fixture.reader, fixture.local)
	if err != nil {
		t.Fatal(err)
	}
	if result.Action.Status != OperatorActionStatusObserved || fixture.store.actions[0].Status != OperatorActionStatusObserved || fixture.store.applyCalls != 1 {
		t.Fatalf("applied replay was not observed: %+v", result)
	}
}

func TestCIWaitRecoveryEvidenceDigestIsStableAndAuthoritySensitive(t *testing.T) {
	fixture := newCIWaitRecoveryFixture(t)
	run := fixture.store.inspection.Run
	schedule := fixture.store.inspection.RetrySchedules[0]
	metadata := fixture.reader.authority
	evidence := fixture.reader.evidence
	observations := append([]GitHubRequestObservation(nil), fixture.reader.observations...)
	observations = append(observations, GitHubRequestObservation{Operation: "second", Category: "REST", HTTPStatus: 200, ResponseDigest: strings.Repeat("a", 64), InstallationID: metadata.InstallationID, Repository: metadata.Repository, ObservedAt: evidence.ObservedAt.Add(time.Second)})
	action := OperatorActionRecord{ActionID: "operator-action", ActionType: OperatorActionRecoverCIWait, ExpectedState: run.State, TransitionSequence: 1}
	want := ciWaitRecoveryEvidenceDigest(run, action, schedule, metadata, evidence, observations)
	if replay := ciWaitRecoveryEvidenceDigest(run, action, schedule, metadata, evidence, append([]GitHubRequestObservation(nil), observations...)); replay != want {
		t.Fatalf("same read replay digest=%s want=%s", replay, want)
	}

	var changed []string
	action2 := action
	action2.TransitionSequence++
	changed = append(changed, ciWaitRecoveryEvidenceDigest(run, action2, schedule, metadata, evidence, observations))
	schedule2 := schedule
	schedule2.AttemptCount++
	changed = append(changed, ciWaitRecoveryEvidenceDigest(run, action, schedule2, metadata, evidence, observations))
	metadata2 := metadata
	metadata2.PermissionsDigest = "different-permissions"
	changed = append(changed, ciWaitRecoveryEvidenceDigest(run, action, schedule, metadata2, evidence, observations))
	evidence2 := evidence
	evidence2.Checks = append([]domain.GitHubCheck(nil), evidence.Checks...)
	evidence2.Checks[0].State = domain.CheckFailure
	changed = append(changed, ciWaitRecoveryEvidenceDigest(run, action, schedule, metadata, evidence2, observations))
	observations2 := append([]GitHubRequestObservation(nil), observations...)
	observations2[0].ResponseDigest = strings.Repeat("b", 64)
	changed = append(changed, ciWaitRecoveryEvidenceDigest(run, action, schedule, metadata, evidence, observations2))
	observations3 := []GitHubRequestObservation{observations[1], observations[0]}
	changed = append(changed, ciWaitRecoveryEvidenceDigest(run, action, schedule, metadata, evidence, observations3))
	run2 := run
	run2.ProfileDigest = "different-profile"
	changed = append(changed, ciWaitRecoveryEvidenceDigest(run2, action, schedule, metadata, evidence, observations))
	for index, digest := range changed {
		if digest == want {
			t.Fatalf("authority mutation %d did not change digest", index)
		}
	}
}

func TestLegacyCheckTopologyRecoveryRequiresExactSanitizedTrace(t *testing.T) {
	inspection := legacyTopologyInspection()
	if err := validateLegacyCheckTopologyTrace(inspection); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*RunInspection)
	}{
		{"review topology terminal", func(value *RunInspection) {
			value.GitHubRequests = append(value.GitHubRequests, value.GitHubRequests[6])
		}},
		{"protection read terminal", func(value *RunInspection) { value.GitHubRequests = value.GitHubRequests[:4] }},
		{"identity terminal", func(value *RunInspection) { value.GitHubRequests[2].Repository.ID++ }},
		{"wrong category", func(value *RunInspection) { value.GitHubRequests[6].Category = "REST" }},
		{"different response digest", func(value *RunInspection) { value.GitHubRequests[8].ResponseDigest = strings.Repeat("0", 64) }},
		{"reordered", func(value *RunInspection) {
			value.GitHubRequests[3], value.GitHubRequests[4] = value.GitHubRequests[4], value.GitHubRequests[3]
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := inspection
			copy.GitHubRequests = append([]GitHubRequestObservation(nil), inspection.GitHubRequests...)
			test.mutate(&copy)
			if err := validateLegacyCheckTopologyTrace(copy); err == nil {
				t.Fatal("unrelated terminal trace was recoverable")
			}
		})
	}
}

func legacyTopologyInspection() RunInspection {
	repository := domain.RepositoryIdentity{ID: 99, Owner: "owner", Name: "repo"}
	operations := []struct{ operation, category string }{
		{"mint_installation_token", "REST"}, {"repository", "REST"}, {"pull_request", "REST"}, {"required_checks", "REST"}, {"check_runs", "REST"}, {"commit_statuses", "REST"},
		{"ReadPullRequestReviews", "GraphQL"}, {"ReadPullRequestReviewThreads", "GraphQL"}, {"ReadPullRequestReviews", "GraphQL"}, {"ReadPullRequestReviewThreads", "GraphQL"},
		{"required_checks", "REST"}, {"check_runs", "REST"}, {"commit_statuses", "REST"},
	}
	digests := strings.Split(legacyCIWaitIncidentResponseDigestAggregate, ":")
	requests := make([]GitHubRequestObservation, len(operations))
	for index, operation := range operations {
		requests[index] = GitHubRequestObservation{Operation: operation.operation, Category: operation.category, HTTPStatus: 200, ResponseDigest: digests[index], InstallationID: 22, Repository: repository, ObservedAt: time.Date(2026, 7, 17, 0, 0, index, 0, time.UTC)}
	}
	return RunInspection{RepositoryBinding: &SanitizedRepositoryBinding{CanonicalRepository: "owner/repo", GitHubInstallationID: 22, ExpectedRepositoryID: 99}, GitHubRequests: requests}
}
