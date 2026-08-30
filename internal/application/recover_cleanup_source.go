package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	CleanupSourceRecoveryConfirmation = "confirm_source_checkout_relocation"
	CleanupSourceRecoveryNextAction   = "apply"
)

type CleanupSourceRecoveryStage string

const (
	CleanupSourceRecoveryAccepted        CleanupSourceRecoveryStage = "accepted"
	CleanupSourceRecoveryRepairIntent    CleanupSourceRecoveryStage = "repair_intent"
	CleanupSourceRecoveryRepairObserved  CleanupSourceRecoveryStage = "repair_observed"
	CleanupSourceRecoveryDetachIntent    CleanupSourceRecoveryStage = "detach_intent"
	CleanupSourceRecoveryDetachObserved  CleanupSourceRecoveryStage = "detach_observed"
	CleanupSourceRecoveryCleanupIntent   CleanupSourceRecoveryStage = "cleanup_intent"
	CleanupSourceRecoveryCleanupObserved CleanupSourceRecoveryStage = "cleanup_observed"
	CleanupSourceRecoverySucceeded       CleanupSourceRecoveryStage = "succeeded"
)

type CleanupSourceRecoveryObservation struct {
	ReplacementSourceDigest   string
	ReplacementIdentityDigest string
	RepositoryOriginDigest    string
	RegistrationDigest        string
	Branch                    string
	CandidateHead             string
	LinkRepaired              bool
	HeadDetached              bool
	WorktreePresent           bool
	BranchPresent             bool
	WorktreeClean             bool
}

type CleanupSourceRecoveryGitRequest struct {
	Repository                 string
	FrozenSourcePath           string
	ReplacementSourcePath      string
	ExpectedOrigin             string
	WorktreePath               string
	Branch                     string
	CandidateHead              string
	ExpectedRegistrationDigest string
}

type CleanupSourceRecoveryGitPort interface {
	ObserveCleanupSourceRecovery(context.Context, CleanupSourceRecoveryGitRequest) (CleanupSourceRecoveryObservation, error)
	RepairCleanupWorktreeLink(context.Context, CleanupSourceRecoveryGitRequest) error
	DetachRecoveredWorktreeHead(context.Context, CleanupSourceRecoveryGitRequest) error
	RemoveRecoveredWorktree(context.Context, CleanupSourceRecoveryGitRequest) error
}

type CleanupSourceRecoveryPreviewRequest struct {
	Requester             Requester
	RunID                 string
	ReplacementSourcePath string
}

type CleanupSourceRecoveryApplyRequest struct {
	CleanupSourceRecoveryPreviewRequest
	RequestID                 string
	PreviewDigest             string
	SourceRelocationConfirmed bool
}

type CleanupSourceRecoveryPreview struct {
	Eligible             bool     `json:"eligible"`
	PreviewDigest        string   `json:"preview_digest"`
	RequiredConfirmation string   `json:"required_confirmation"`
	ResourceClasses      []string `json:"resource_classes"`
	NextAction           string   `json:"next_action"`
}

type CleanupSourceRecoveryResult struct {
	Receipt         OperationReceipt `json:"receipt"`
	RecoveryStage   string           `json:"recovery_stage"`
	ResourceClasses []string         `json:"resource_classes"`
	NextAction      string           `json:"next_action"`
}

type CleanupSourceRecoveryAuthority struct {
	RunID                     string
	Repository                string
	RepositoryBindingDigest   string
	TransitionSequence        int64
	AbandonActionDigest       string
	AttentionEventKey         string
	AttentionEvidenceDigest   string
	OwnershipDigest           string
	CleanupDigest             string
	FrozenSourceDigest        string
	ReplacementSourceDigest   string
	ReplacementIdentityDigest string
	RepositoryOriginDigest    string
	RegistrationDigest        string
	Branch                    string
	CandidateHead             string
	PreviewDigest             string
}

type CleanupSourceRecoveryIntent struct {
	RequestID   string
	OperationID string
	Authority   CleanupSourceRecoveryAuthority
	Requester   domain.GitHubUserIdentity
	Stage       CleanupSourceRecoveryStage
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type CleanupSourceRecoveryStore interface {
	RunStore
	CurrentOperatorAttentionQuery
	ValidateCleanupSourceRecoveryAuthority(context.Context, CleanupSourceRecoveryAuthority, domain.GitHubUserIdentity) error
	BeginCleanupSourceRecovery(context.Context, CleanupSourceRecoveryIntent, OperationReceipt) (CleanupSourceRecoveryIntent, OperationReceipt, bool, error)
	GetCleanupSourceRecovery(context.Context, string) (CleanupSourceRecoveryIntent, OperationReceipt, bool, error)
	AdvanceCleanupSourceRecovery(context.Context, string, CleanupSourceRecoveryStage, CleanupSourceRecoveryStage, time.Time) (CleanupSourceRecoveryIntent, OperationReceipt, bool, error)
	SettleCleanupSourceRecovery(context.Context, string, time.Time) (CleanupSourceRecoveryIntent, OperationReceipt, bool, error)
}

type CleanupSourceRecoveryService struct {
	store      CleanupSourceRecoveryStore
	git        CleanupSourceRecoveryGitPort
	authorizer *AuthorizationService
	now        func() time.Time
}

func NewCleanupSourceRecoveryService(store CleanupSourceRecoveryStore, git CleanupSourceRecoveryGitPort, authorizer *AuthorizationService) (*CleanupSourceRecoveryService, error) {
	if store == nil || git == nil || authorizer == nil {
		return nil, errors.New("cleanup source recovery dependencies are required")
	}
	return &CleanupSourceRecoveryService{store: store, git: git, authorizer: authorizer, now: func() time.Time { return time.Now().UTC() }}, nil
}

type cleanupSourceRecoveryPrepared struct {
	preview   CleanupSourceRecoveryPreview
	authority CleanupSourceRecoveryAuthority
	request   CleanupSourceRecoveryGitRequest
	requester domain.GitHubUserIdentity
}

func (s *CleanupSourceRecoveryService) Preview(ctx context.Context, input CleanupSourceRecoveryPreviewRequest) (CleanupSourceRecoveryPreview, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(input.Requester)
	if err != nil {
		return CleanupSourceRecoveryPreview{}, err
	}
	prepared, err := s.prepare(ctx, input, configured)
	if err != nil {
		return CleanupSourceRecoveryPreview{}, err
	}
	return prepared.preview, nil
}

func (s *CleanupSourceRecoveryService) Apply(ctx context.Context, input CleanupSourceRecoveryApplyRequest) (CleanupSourceRecoveryResult, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(input.Requester)
	if err != nil {
		return CleanupSourceRecoveryResult{}, err
	}
	if !input.SourceRelocationConfirmed || strings.TrimSpace(input.RequestID) == "" || strings.ContainsRune(input.RequestID, '\x00') || len(input.RequestID) > 128 || !validAuthorityDigest(input.PreviewDigest) {
		return CleanupSourceRecoveryResult{}, serviceError(ErrorInvalidInput, "request ID, exact preview digest, and source-relocation confirmation are required", nil)
	}
	if existing, receipt, found, lookupErr := s.store.GetCleanupSourceRecovery(ctx, input.RequestID); lookupErr != nil {
		return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(lookupErr)
	} else if found {
		return s.resume(ctx, input, configured, existing, receipt)
	}
	prepared, err := s.prepare(ctx, input.CleanupSourceRecoveryPreviewRequest, configured)
	if err != nil {
		return CleanupSourceRecoveryResult{}, err
	}
	if prepared.preview.PreviewDigest != input.PreviewDigest {
		return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup recovery preview authority changed", nil)
	}
	now := s.now().UTC()
	requestDigest := digestText("cleanup-source-recovery-request-v1\x00" + input.RequestID + "\x00" + prepared.authority.PreviewDigest + "\x00" + prepared.authority.ReplacementSourceDigest)
	receipt := NewOperationReceipt(OperationReceiptInput{
		OperationType: OperationRecoverCleanupSource, Scope: ScopeRun, TargetID: prepared.authority.RunID,
		Requester: prepared.requester, RequestDigest: requestDigest,
		ExpectedAuthorityDigest: cleanupSourceRecoveryExpectedAuthorityDigest(prepared.authority),
		OperationAnchorDigest:   digestText("cleanup-source-recovery-anchor-v1\x00" + prepared.authority.RunID + "\x00" + prepared.authority.AttentionEventKey),
		TargetBindingDigest:     prepared.authority.RepositoryBindingDigest, AcceptedAt: now,
	})
	intent := CleanupSourceRecoveryIntent{RequestID: input.RequestID, OperationID: receipt.OperationID, Authority: prepared.authority, Requester: prepared.requester, Stage: CleanupSourceRecoveryAccepted, CreatedAt: now, UpdatedAt: now}
	intent, receipt, _, err = s.store.BeginCleanupSourceRecovery(ctx, intent, receipt)
	if err != nil {
		return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(err)
	}
	return s.drive(ctx, input, prepared.request, intent, receipt)
}

func (s *CleanupSourceRecoveryService) resume(ctx context.Context, input CleanupSourceRecoveryApplyRequest, configured ConfiguredRequester, intent CleanupSourceRecoveryIntent, receipt OperationReceipt) (CleanupSourceRecoveryResult, error) {
	if input.RunID != intent.Authority.RunID || intent.Requester.Login != strings.ToLower(strings.TrimSpace(input.Requester.ID)) || intent.Requester.DatabaseID != input.Requester.DatabaseID || intent.Requester.NodeID != input.Requester.NodeID || intent.Requester.ActorType != input.Requester.ActorType || intent.Authority.PreviewDigest != input.PreviewDigest {
		return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup source recovery replay authority changed", nil)
	}
	run, err := s.store.GetRun(ctx, intent.Authority.RunID)
	if err != nil {
		return CleanupSourceRecoveryResult{}, hiddenTargetError()
	}
	if _, authErr := s.authorizer.ConfiguredFrozenRunScopes(configured, run); authErr != nil {
		return CleanupSourceRecoveryResult{}, hiddenTargetError()
	}
	if configured.Identity() != intent.Requester {
		return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup source recovery replay authority changed", nil)
	}
	var repository LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil {
		return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "frozen repository authority changed", nil)
	}
	request := CleanupSourceRecoveryGitRequest{Repository: run.Repository, FrozenSourcePath: repository.SourcePath, ReplacementSourcePath: input.ReplacementSourcePath, ExpectedOrigin: repository.OriginPath, WorktreePath: run.WorktreePath, Branch: run.WorkingBranch, CandidateHead: run.CandidateHead, ExpectedRegistrationDigest: intent.Authority.RegistrationDigest}
	observation, err := s.git.ObserveCleanupSourceRecovery(ctx, request)
	if err != nil {
		return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup source recovery replay path is not proven", err)
	}
	if err := validateCleanupSourceRecoveryStageObservation(intent, observation); err != nil {
		return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup source recovery replay authority changed", nil)
	}
	if intent.Stage == CleanupSourceRecoverySucceeded {
		return CleanupSourceRecoveryResult{Receipt: receipt, RecoveryStage: string(intent.Stage), ResourceClasses: []string{"worktree"}, NextAction: "validate_repository_removal"}, nil
	}
	return s.drive(ctx, input, request, intent, receipt)
}

func (s *CleanupSourceRecoveryService) drive(ctx context.Context, input CleanupSourceRecoveryApplyRequest, gitRequest CleanupSourceRecoveryGitRequest, intent CleanupSourceRecoveryIntent, receipt OperationReceipt) (CleanupSourceRecoveryResult, error) {
	advance := func(expected, next CleanupSourceRecoveryStage) error {
		var advanceErr error
		intent, receipt, _, advanceErr = s.store.AdvanceCleanupSourceRecovery(ctx, input.RequestID, expected, next, s.now().UTC())
		return advanceErr
	}
	observe := func(stage CleanupSourceRecoveryStage) error {
		observation, observeErr := s.git.ObserveCleanupSourceRecovery(ctx, gitRequest)
		if observeErr != nil {
			return observeErr
		}
		probe := intent
		probe.Stage = stage
		return validateCleanupSourceRecoveryStageObservation(probe, observation)
	}
	for {
		switch intent.Stage {
		case CleanupSourceRecoveryAccepted:
			if err := advance(CleanupSourceRecoveryAccepted, CleanupSourceRecoveryRepairIntent); err != nil {
				return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(err)
			}
		case CleanupSourceRecoveryRepairIntent:
			if err := observe(CleanupSourceRecoveryRepairIntent); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup source recovery repair authority changed", err)
			}
			if err := s.git.RepairCleanupWorktreeLink(ctx, gitRequest); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "exact worktree-link repair could not be proven", err)
			}
			if err := observe(CleanupSourceRecoveryRepairObserved); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "worktree-link repair postcondition changed", err)
			}
			if err := advance(CleanupSourceRecoveryRepairIntent, CleanupSourceRecoveryRepairObserved); err != nil {
				return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(err)
			}
		case CleanupSourceRecoveryRepairObserved:
			if err := advance(CleanupSourceRecoveryRepairObserved, CleanupSourceRecoveryDetachIntent); err != nil {
				return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(err)
			}
		case CleanupSourceRecoveryDetachIntent:
			if err := observe(CleanupSourceRecoveryDetachIntent); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup source recovery detach authority changed", err)
			}
			if err := s.git.DetachRecoveredWorktreeHead(ctx, gitRequest); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "exact recovered worktree HEAD detach could not be proven", err)
			}
			if err := observe(CleanupSourceRecoveryDetachObserved); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "recovered worktree HEAD detach postcondition changed", err)
			}
			if err := advance(CleanupSourceRecoveryDetachIntent, CleanupSourceRecoveryDetachObserved); err != nil {
				return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(err)
			}
		case CleanupSourceRecoveryDetachObserved:
			if err := advance(CleanupSourceRecoveryDetachObserved, CleanupSourceRecoveryCleanupIntent); err != nil {
				return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(err)
			}
		case CleanupSourceRecoveryCleanupIntent:
			if err := observe(CleanupSourceRecoveryCleanupIntent); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup source recovery worktree authority changed", err)
			}
			if err := s.git.RemoveRecoveredWorktree(ctx, gitRequest); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "exact recovered worktree removal could not be proven", err)
			}
			if err := observe(CleanupSourceRecoveryCleanupObserved); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "recovered worktree removal postcondition changed", err)
			}
			if err := advance(CleanupSourceRecoveryCleanupIntent, CleanupSourceRecoveryCleanupObserved); err != nil {
				return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(err)
			}
		case CleanupSourceRecoveryCleanupObserved:
			if err := observe(CleanupSourceRecoveryCleanupObserved); err != nil {
				return CleanupSourceRecoveryResult{}, serviceError(ErrorConflict, "cleanup source recovery settlement authority changed", err)
			}
			var settleErr error
			intent, receipt, _, settleErr = s.store.SettleCleanupSourceRecovery(ctx, input.RequestID, s.now().UTC())
			if settleErr != nil {
				return CleanupSourceRecoveryResult{}, classifyCleanupSourceRecoveryError(settleErr)
			}
		case CleanupSourceRecoverySucceeded:
			return CleanupSourceRecoveryResult{Receipt: receipt, RecoveryStage: string(intent.Stage), ResourceClasses: []string{"worktree"}, NextAction: "validate_repository_removal"}, nil
		default:
			return CleanupSourceRecoveryResult{}, serviceError(ErrorInternal, "cleanup source recovery stage is invalid", nil)
		}
	}
}

func validateCleanupSourceRecoveryStageObservation(intent CleanupSourceRecoveryIntent, observation CleanupSourceRecoveryObservation) error {
	a := intent.Authority
	if observation.ReplacementSourceDigest != a.ReplacementSourceDigest || observation.ReplacementIdentityDigest != a.ReplacementIdentityDigest || observation.RepositoryOriginDigest != a.RepositoryOriginDigest || observation.RegistrationDigest != a.RegistrationDigest || observation.Branch != a.Branch || observation.CandidateHead != a.CandidateHead || !observation.WorktreeClean {
		return errors.New("replacement checkout identity changed")
	}
	switch intent.Stage {
	case CleanupSourceRecoveryAccepted:
		if !observation.WorktreePresent || observation.BranchPresent || observation.LinkRepaired || observation.HeadDetached {
			return errors.New("accepted recovery authority is ambiguous")
		}
	case CleanupSourceRecoveryRepairIntent:
		if !observation.WorktreePresent || observation.BranchPresent || observation.HeadDetached {
			return errors.New("repair effect state is ambiguous")
		}
	case CleanupSourceRecoveryRepairObserved:
		if !observation.WorktreePresent || observation.BranchPresent || !observation.LinkRepaired || observation.HeadDetached {
			return errors.New("repair postcondition is unavailable")
		}
	case CleanupSourceRecoveryDetachIntent:
		if !observation.WorktreePresent || observation.BranchPresent || !observation.LinkRepaired {
			return errors.New("detach precondition is unavailable")
		}
	case CleanupSourceRecoveryDetachObserved:
		if !observation.WorktreePresent || observation.BranchPresent || !observation.LinkRepaired || !observation.HeadDetached {
			return errors.New("detach postcondition is unavailable")
		}
	case CleanupSourceRecoveryCleanupIntent:
		if observation.WorktreePresent && (!observation.LinkRepaired || !observation.HeadDetached || observation.BranchPresent) {
			return errors.New("worktree cleanup precondition is unavailable")
		}
	case CleanupSourceRecoveryCleanupObserved, CleanupSourceRecoverySucceeded:
		if observation.WorktreePresent || observation.BranchPresent {
			return errors.New("worktree cleanup postcondition is unavailable")
		}
	default:
		return errors.New("cleanup source recovery stage is invalid")
	}
	return nil
}

func (s *CleanupSourceRecoveryService) prepare(ctx context.Context, input CleanupSourceRecoveryPreviewRequest, configured ConfiguredRequester) (cleanupSourceRecoveryPrepared, error) {
	if strings.TrimSpace(input.RunID) == "" || !filepath.IsAbs(input.ReplacementSourcePath) {
		return cleanupSourceRecoveryPrepared{}, serviceError(ErrorInvalidInput, "run and an absolute replacement source path are required", nil)
	}
	inspection, err := s.store.Inspect(ctx, input.RunID)
	if err != nil {
		return cleanupSourceRecoveryPrepared{}, classifyServiceError(err)
	}
	if _, err := s.authorizer.ConfiguredFrozenRunScopes(configured, inspection.Run); err != nil {
		return cleanupSourceRecoveryPrepared{}, hiddenTargetError()
	}
	currentAttention, found, err := s.store.CurrentOperatorAttention(ctx, input.RunID)
	if err != nil || !found {
		return cleanupSourceRecoveryPrepared{}, serviceError(ErrorConflict, "current cleanup residue attention is unavailable", err)
	}
	authority, gitRequest, err := cleanupSourceRecoveryAuthorityForInspection(inspection, currentAttention, input.ReplacementSourcePath)
	if err != nil {
		return cleanupSourceRecoveryPrepared{}, serviceError(ErrorConflict, "run is not eligible for cleanup source recovery", err)
	}
	observation, err := s.git.ObserveCleanupSourceRecovery(ctx, gitRequest)
	if err != nil {
		return cleanupSourceRecoveryPrepared{}, serviceError(ErrorConflict, "replacement source authority is not eligible", err)
	}
	if observation.Branch != inspection.Run.WorkingBranch || observation.CandidateHead != inspection.Run.CandidateHead || !observation.WorktreePresent || observation.BranchPresent || observation.LinkRepaired || observation.HeadDetached || !observation.WorktreeClean {
		return cleanupSourceRecoveryPrepared{}, serviceError(ErrorConflict, "replacement source authority does not match exact cleanup evidence", nil)
	}
	for _, value := range []string{observation.ReplacementSourceDigest, observation.ReplacementIdentityDigest, observation.RepositoryOriginDigest, observation.RegistrationDigest} {
		if !validAuthorityDigest(value) {
			return cleanupSourceRecoveryPrepared{}, serviceError(ErrorInternal, "replacement source observation is invalid", nil)
		}
	}
	authority.ReplacementSourceDigest = observation.ReplacementSourceDigest
	authority.ReplacementIdentityDigest = observation.ReplacementIdentityDigest
	authority.RepositoryOriginDigest = observation.RepositoryOriginDigest
	authority.RegistrationDigest = observation.RegistrationDigest
	authority.Branch = observation.Branch
	authority.CandidateHead = observation.CandidateHead
	authority.PreviewDigest = cleanupSourceRecoveryPreviewDigest(authority)
	gitRequest.ExpectedRegistrationDigest = authority.RegistrationDigest
	if err := s.store.ValidateCleanupSourceRecoveryAuthority(ctx, authority, configured.Identity()); err != nil {
		return cleanupSourceRecoveryPrepared{}, serviceError(ErrorConflict, "cleanup source recovery execution authority is unavailable", err)
	}
	preview := CleanupSourceRecoveryPreview{Eligible: true, PreviewDigest: authority.PreviewDigest, RequiredConfirmation: CleanupSourceRecoveryConfirmation, ResourceClasses: []string{"worktree"}, NextAction: CleanupSourceRecoveryNextAction}
	return cleanupSourceRecoveryPrepared{preview: preview, authority: authority, request: gitRequest, requester: configured.Identity()}, nil
}

func cleanupSourceRecoveryAuthorityForInspection(inspection RunInspection, attention OperatorAttentionEvent, replacement string) (CleanupSourceRecoveryAuthority, CleanupSourceRecoveryGitRequest, error) {
	run := inspection.Run
	if run.State != domain.StateFailed || run.LastError != AutomaticAdmissionAbandonTransition || strings.TrimSpace(run.CandidateHead) == "" || strings.TrimSpace(run.WorkingBranch) == "" {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("run is not an abandoned terminal failure")
	}
	if run.LeaseOwner != "" || !run.LeaseExpiresAt.IsZero() {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("run remains leased")
	}
	if len(inspection.Timeline) == 0 {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("abandon transition is unavailable")
	}
	transition := inspection.Timeline[len(inspection.Timeline)-1]
	if transition.To != domain.StateFailed || transition.Reason != AutomaticAdmissionAbandonTransition {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("exact abandon transition is unavailable")
	}
	var abandonAction *OperatorActionRecord
	for _, action := range inspection.OperatorActions {
		if action.RunID == run.ID && action.ActionType == OperatorActionAbandon && action.Status == OperatorActionStatusObserved && action.ResultStatus == OperatorActionResultSucceeded && action.ResultingTransitionSequence == transition.Sequence {
			if abandonAction != nil {
				return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("exact observed abandon action is ambiguous")
			}
			value := action
			abandonAction = &value
		}
	}
	if abandonAction == nil {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("exact observed abandon action is unavailable")
	}
	if attention.RunID != run.ID || attention.EventType != OperatorAttentionCleanupResidue || attention.ControllerState != string(domain.StateFailed) || attention.EventKey != CleanupSourceRecoveryAttentionEventKey(run.ID, transition.Sequence) {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("current cleanup residue attention is unavailable")
	}
	var repository LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil || repository.CanonicalRepository != run.Repository || repository.SourcePath == "" || repository.OriginPath == "" {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("frozen repository authority is invalid")
	}
	ownedDigest, cleanupDigest, err := validateCleanupSourceRecoveryResidue(run, inspection.Resources, inspection.Cleanup)
	if err != nil {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, err
	}
	var worktreeNonce, branchNonce string
	for _, resource := range inspection.Resources {
		if resource.Kind != "worktree" && resource.Kind != "branch" && resource.Kind != "local_branch" {
			continue
		}
		if _, err := validateAbandonLocalResource(run, repository, resource); err != nil {
			return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, err
		}
		if resource.Kind == "worktree" {
			worktreeNonce = worktreeEvidenceNonce(resource)
		} else {
			branchNonce = worktreeEvidenceNonce(resource)
		}
	}
	if worktreeNonce == "" || branchNonce == "" || worktreeNonce != branchNonce {
		return CleanupSourceRecoveryAuthority{}, CleanupSourceRecoveryGitRequest{}, errors.New("local ownership nonce conflicts")
	}
	frozenDigest := DigestCleanupFrozenSource(repository.SourcePath)
	authority := CleanupSourceRecoveryAuthority{RunID: run.ID, Repository: run.Repository, RepositoryBindingDigest: run.RepositoryBindingDigest, TransitionSequence: transition.Sequence, AbandonActionDigest: CleanupSourceRecoveryAbandonActionDigest(*abandonAction), AttentionEventKey: attention.EventKey, AttentionEvidenceDigest: attention.EvidenceDigest, OwnershipDigest: ownedDigest, CleanupDigest: cleanupDigest, FrozenSourceDigest: frozenDigest}
	request := CleanupSourceRecoveryGitRequest{Repository: run.Repository, FrozenSourcePath: repository.SourcePath, ReplacementSourcePath: replacement, ExpectedOrigin: repository.OriginPath, WorktreePath: run.WorktreePath, Branch: run.WorkingBranch, CandidateHead: run.CandidateHead}
	return authority, request, nil
}

func validateCleanupSourceRecoveryResidue(run Run, resources []OwnedResource, cleanup []CleanupRecord) (string, string, error) {
	resourceRows := make([]string, 0, len(resources))
	cleanupRows := make([]string, 0, len(cleanup))
	w, b := 0, 0
	for _, r := range resources {
		if r.RunID != run.ID {
			return "", "", errors.New("foreign ownership evidence")
		}
		resourceRows = append(resourceRows, fmt.Sprintf("%s\x00%s\x00%s\x00%s", r.Kind, r.Name, r.Status, digestText(r.CreationEvidence)))
		switch r.Kind {
		case "worktree":
			if r.Name != run.WorktreePath || (r.Status != "owned" && r.Status != "reserved") {
				return "", "", errors.New("worktree ownership changed")
			}
			w++
		case "branch", "local_branch":
			if r.Name != run.WorkingBranch || r.Status != "deleted" {
				return "", "", errors.New("local branch ownership changed")
			}
			b++
		case "artifact_root":
			if r.Status != "owned" && r.Status != "reserved" {
				return "", "", errors.New("artifact ownership changed")
			}
		case "remote_branch", "pull_request":
			if r.Status != "owned" && r.Status != "reserved" && r.Status != "deleted" {
				return "", "", errors.New("retained ownership changed")
			}
		default:
			return "", "", errors.New("unrelated owned residue remains")
		}
	}
	if w != 1 || b != 1 {
		return "", "", errors.New("exact local ownership evidence is unavailable")
	}
	cw, cb := 0, 0
	for _, c := range cleanup {
		if c.RunID != run.ID {
			return "", "", errors.New("foreign cleanup evidence")
		}
		cleanupRows = append(cleanupRows, fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", c.Kind, c.Name, c.Status, c.ErrorClass, digestText(c.LastError)))
		switch c.Kind {
		case "worktree":
			if c.Name != run.WorktreePath || (c.Status != "failed" && c.Status != "intent") {
				return "", "", errors.New("worktree cleanup is not recoverable")
			}
			cw++
		case "branch", "local_branch":
			if c.Name != run.WorkingBranch || c.Status != "deleted" {
				return "", "", errors.New("local branch cleanup is not durably deleted")
			}
			cb++
		case "artifact_root", "remote_branch", "pull_request":
			if c.Status != "retained" && c.Status != "deleted" {
				return "", "", errors.New("nonlocal cleanup residue remains")
			}
		case "source_checkout":
			if c.Status != "synced" {
				return "", "", errors.New("source checkout residue remains")
			}
		default:
			return "", "", errors.New("unrelated cleanup residue remains")
		}
	}
	if cw != 1 || cb != 1 {
		return "", "", errors.New("exact local cleanup residue is unavailable")
	}
	sort.Strings(resourceRows)
	sort.Strings(cleanupRows)
	return digestText("cleanup-source-ownership-v1\x00" + strings.Join(resourceRows, "\x01")), digestText("cleanup-source-progress-v1\x00" + strings.Join(cleanupRows, "\x01")), nil
}

// ValidateCleanupSourceRecoveryResidue is shared with the SQLite acceptance
// transaction so the read-only preview and durable CAS use one closed residue
// classifier.
func ValidateCleanupSourceRecoveryResidue(run Run, resources []OwnedResource, cleanup []CleanupRecord) (string, string, error) {
	return validateCleanupSourceRecoveryResidue(run, resources, cleanup)
}

func CleanupSourceRecoveryExpectedAuthorityDigest(a CleanupSourceRecoveryAuthority) string {
	return cleanupSourceRecoveryExpectedAuthorityDigest(a)
}

func CleanupSourceRecoveryAttentionEventKey(runID string, transitionSequence int64) string {
	return fmt.Sprintf("automation:%s:%s:%d", runID, OperatorAttentionCleanupResidue, transitionSequence)
}

func cleanupSourceRecoveryPreviewDigest(a CleanupSourceRecoveryAuthority) string {
	raw, _ := json.Marshal(a)
	return digestText("cleanup-source-preview-v1\x00" + string(raw))
}

func cleanupSourceRecoveryExpectedAuthorityDigest(a CleanupSourceRecoveryAuthority) string {
	raw, _ := json.Marshal(a)
	return digestText("cleanup-source-authority-v1\x00" + string(raw))
}

func CleanupSourceRecoveryAbandonActionDigest(action OperatorActionRecord) string {
	return SHA256Digest("cleanup-source-abandon-action-v1", action.ActionID, action.PayloadDigest, action.RequestDigest, action.ExpectedAuthorityDigest, action.EvidenceDigest, action.OutcomeDigest, fmt.Sprint(action.TransitionSequence), fmt.Sprint(action.ResultingTransitionSequence))
}

func DigestCleanupSourcePath(path string) string {
	return digestText("cleanup-source-path-v1\x00" + path)
}

func DigestCleanupFrozenSource(path string) string {
	return digestText("cleanup-source-frozen-v1\x00" + path)
}

func SHA256Digest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func classifyCleanupSourceRecoveryError(err error) error {
	if errors.Is(err, ErrOperationReceiptConflict) {
		return serviceError(ErrorConflict, "cleanup source recovery authority changed", err)
	}
	return classifyServiceError(err)
}
