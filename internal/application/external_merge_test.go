package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type externalMergeTestStore struct {
	RunStore
	DeliveryStore
	run        Run
	inspection RunInspection
	saved      []MergeRecord
}

func (s *externalMergeTestStore) GetRun(context.Context, string) (Run, error) { return s.run, nil }
func (s *externalMergeTestStore) Inspect(context.Context, string) (RunInspection, error) {
	s.inspection.Run = s.run
	return s.inspection, nil
}
func (s *externalMergeTestStore) SaveMerge(_ context.Context, merge MergeRecord) error {
	s.saved = append(s.saved, merge)
	s.inspection.Merge = &merge
	return nil
}
func (s *externalMergeTestStore) Transition(_ context.Context, _ string, expected, next domain.State, _, _, _ string) error {
	if s.run.State != expected {
		return errors.New("state compare failed")
	}
	s.run.State = next
	return nil
}

type externalMergeTestValidator struct{ err error }

func (v externalMergeTestValidator) ValidateExternalMergeCandidate(context.Context, string) error {
	return v.err
}

type externalMergeTestVerifier struct{ result ExternalMergeVerification }

func (v externalMergeTestVerifier) Verify(context.Context, ExternalMergeVerificationRequest) (ExternalMergeVerification, error) {
	return v.result, nil
}

func TestValidateExternalMergeEvidenceRequiresExactMergedHeadChecksAndApproval(t *testing.T) {
	head := "1111111111111111111111111111111111111111"
	merge := "2222222222222222222222222222222222222222"
	now := time.Unix(10, 0).UTC()
	run := Run{ID: "run", IdempotencyKey: "owner", WorkingBranch: "ifan/task", BaseBranch: "main", BaseSHA: "base", CandidateHead: head}
	pr := domain.PullRequest{Number: 7, NodeID: "PR_node", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: head, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "closed", Merged: true, MergeSHA: merge, MergedAt: now}
	actor := domain.ActorIdentity{DatabaseID: 1, NodeID: "U_node", Login: "ifan0927", Type: "User"}
	approval := domain.HumanApproval{PRNumber: pr.Number, Approver: actor.Login, Actor: actor, ReviewDatabaseID: 2, ReviewNodeID: "R_node", Source: "github_pull_request_review", ApprovedSHA: head, CIStatus: "pass", ReviewSHA: head, ApprovedAt: now, ObservedAt: now}
	evidence := domain.GitHubReadEvidence{PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "CI", Required: true, State: domain.CheckSuccess, ObservedSHA: head}}}
	inspection := RunInspection{Timeline: []Transition{{From: domain.StateMerging, To: domain.StateManualIntervention, Reason: "GitHub pull request closed or merged outside the controller gate", BoundHead: head}}, PullRequest: &pr, Approval: &approval, GitHubEvidence: &evidence}

	if _, err := validateExternalMergeEvidence(run, inspection); err != nil {
		t.Fatal(err)
	}
	evidence.Checks[0].ObservedSHA = merge
	if _, err := validateExternalMergeEvidence(run, inspection); err == nil {
		t.Fatal("drifted required check must not authorize external merge")
	}
}

func TestAcceptedMergeMethodsRemainNarrow(t *testing.T) {
	if !acceptedMergeMethod("squash") || !acceptedMergeMethod("external") || acceptedMergeMethod("merge") {
		t.Fatal("accepted merge methods changed")
	}
}

func TestAcceptExternalMergePersistsEvidenceThenResumesLinearCompletion(t *testing.T) {
	head := "1111111111111111111111111111111111111111"
	mergeSHA := "2222222222222222222222222222222222222222"
	tree := "3333333333333333333333333333333333333333"
	now := time.Unix(10, 0).UTC()
	repositoryJSON, _ := json.Marshal(LocalRepository{CanonicalRepository: "owner/repo", BaseBranch: "main", SourcePath: "/source", OriginPath: "/origin", AllowedOperatorLogins: []string{"ifan0927"}})
	run := Run{ID: "run", Repository: "owner/repo", RepositoryConfigJSON: string(repositoryJSON), IdempotencyKey: "owner", WorkingBranch: "ifan/task", BaseBranch: "main", BaseSHA: "base", CandidateHead: head, State: domain.StateManualIntervention}
	pr := domain.PullRequest{Number: 7, NodeID: "PR_node", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: head, BaseSHA: run.BaseSHA, BodyDigest: "body", OwnershipKey: run.IdempotencyKey, State: "closed", Merged: true, MergeSHA: mergeSHA, MergedAt: now}
	actor := domain.ActorIdentity{DatabaseID: 1, NodeID: "U_node", Login: "ifan0927", Type: "User"}
	approval := domain.HumanApproval{PRNumber: pr.Number, Approver: actor.Login, Actor: actor, ReviewDatabaseID: 2, ReviewNodeID: "R_node", Source: "github_pull_request_review", ApprovedSHA: head, CIStatus: "pass", ReviewSHA: head, ApprovedAt: now, ObservedAt: now}
	evidence := domain.GitHubReadEvidence{PullRequest: pr, Checks: []domain.GitHubCheck{{Name: "CI", Required: true, State: domain.CheckSuccess, ObservedSHA: head}}}
	store := &externalMergeTestStore{run: run, inspection: RunInspection{Timeline: []Transition{{From: domain.StateMerging, To: domain.StateManualIntervention, Reason: "GitHub pull request closed or merged outside the controller gate", BoundHead: head}}, PullRequest: &pr, Approval: &approval, GitHubEvidence: &evidence}}
	coordinator := &ProductionCoordinator{store: store}
	verified := ExternalMergeVerification{CandidateSHA: head, MergeSHA: mergeSHA, BaseSHA: mergeSHA, TreeSHA: tree}
	result, err := coordinator.AcceptExternalMerge(context.Background(), ProductionAcceptExternalMergeCommand{Requester: Requester{ID: "ifan0927", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, IdempotencyKey: run.IdempotencyKey}, externalMergeTestValidator{}, externalMergeTestVerifier{result: verified})
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != ProductionReconcileLinear || result.Run.State != domain.StateAwaitingLinearCompletion || len(store.saved) != 1 || store.saved[0].Method != "external" {
		t.Fatalf("result=%+v saved=%+v", result, store.saved)
	}
}
