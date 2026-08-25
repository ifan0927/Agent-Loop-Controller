package application

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

var (
	ErrOnboardingNotFound = errors.New("onboarding not found")
	ErrOnboardingConflict = errors.New("onboarding authority conflicts")
)

type Onboarding struct {
	OnboardingID                  string                        `json:"onboarding_id"`
	Kind                          domain.OnboardingKind         `json:"kind"`
	CanonicalRepository           string                        `json:"canonical_repository"`
	Status                        domain.OnboardingStatus       `json:"status"`
	CompletedSteps                []domain.OnboardingStep       `json:"completed_steps"`
	ReasonCode                    string                        `json:"reason_code,omitempty"`
	LegalNextActions              []domain.OnboardingNextAction `json:"legal_next_actions"`
	OperationID                   string                        `json:"operation_id,omitempty"`
	PrivateInputDigest            string                        `json:"private_input_digest"`
	SourcePathDigest              string                        `json:"source_path_digest"`
	RequestDigest                 string                        `json:"request_digest"`
	ConfigurationBaseGenerationID int64                         `json:"configuration_base_generation_id"`
	ConfigurationBaseDigest       string                        `json:"configuration_base_digest"`
	ConfigurationAuthorityVersion int64                         `json:"configuration_authority_version"`
	PreflightDigest               string                        `json:"preflight_digest,omitempty"`
	PreviewDigest                 string                        `json:"preview_digest,omitempty"`
	ProfileID                     string                        `json:"profile_id,omitempty"`
	ProfileDigest                 string                        `json:"profile_digest,omitempty"`
	RepositoryBindingDigest       string                        `json:"repository_binding_digest,omitempty"`
	ConfigurationGenerationID     int64                         `json:"configuration_generation_id,omitempty"`
	IncarnationID                 string                        `json:"incarnation_id,omitempty"`
	ReadinessSnapshotID           string                        `json:"readiness_snapshot_id,omitempty"`
	LinearLabelID                 string                        `json:"linear_label_id,omitempty"`
	InitialRevisionSHA            string                        `json:"initial_revision_sha,omitempty"`
	CreatedAt                     time.Time                     `json:"created_at"`
	UpdatedAt                     time.Time                     `json:"updated_at"`
	SettledAt                     time.Time                     `json:"settled_at,omitempty"`
	AcceptedAt                    time.Time                     `json:"-"`

	Requester domain.GitHubUserIdentity `json:"-"`
}

type OnboardingPreflight struct {
	OnboardingID              string    `json:"onboarding_id"`
	CanonicalRepository       string    `json:"canonical_repository"`
	Ready                     bool      `json:"ready"`
	ReasonCode                string    `json:"reason_code"`
	PreflightDigest           string    `json:"preflight_digest"`
	ConfigurationGenerationID int64     `json:"configuration_generation_id"`
	ConfigurationDigest       string    `json:"configuration_digest"`
	ObservedAt                time.Time `json:"observed_at"`
}

type OnboardingPreview struct {
	OnboardingID               string                  `json:"onboarding_id"`
	Kind                       domain.OnboardingKind   `json:"kind,omitempty"`
	CanonicalRepository        string                  `json:"canonical_repository"`
	Policy                     OnboardingPolicyPreview `json:"policy"`
	OrderedEffects             []domain.OnboardingStep `json:"ordered_effects"`
	FinalState                 domain.OnboardingStatus `json:"final_state"`
	RetainsPartialProgress     bool                    `json:"retains_partial_progress"`
	RollbackAvailable          bool                    `json:"rollback_available"`
	WorkerRestartMayBeRequired bool                    `json:"worker_restart_may_be_required"`
	PreflightDigest            string                  `json:"preflight_digest"`
	PreviewDigest              string                  `json:"preview_digest"`
}

type OnboardingPolicyPreview struct {
	GitHubAppProfileRef string   `json:"github_app_profile_ref"`
	BaseBranch          string   `json:"base_branch"`
	VerifierIDs         []string `json:"verifier_ids"`
	LinearLabel         string   `json:"linear_label"`
	CISlowThreshold     string   `json:"ci_slow_threshold"`
}

type OnboardingOpenCommand struct {
	Requester Requester
	RequestID string
	Input     domain.ExistingCheckoutOnboardingInput
}

type EmptyRepositoryOnboardingOpenCommand struct {
	Requester Requester
	RequestID string
	Input     domain.EmptyRepositoryOnboardingInput
}

type OnboardingCommand struct {
	Requester    Requester
	OnboardingID string
}

type OnboardingStartCommand struct {
	Requester       Requester
	OnboardingID    string
	PreflightDigest string
	PreviewDigest   string
}

type OnboardingPreflightEvidence struct {
	Ready          bool
	ReasonCode     string
	EvidenceDigest string
	Profile        LocalRepository
	ObservedAt     time.Time
}

type OnboardingStepObservation struct {
	Outcome                   OperationOutcome
	ReasonCode                string
	EvidenceDigest            string
	ProfileID                 string
	ProfileDigest             string
	RepositoryBindingDigest   string
	ConfigurationGenerationID int64
	IncarnationID             string
	ReadinessSnapshotID       string
	LinearLabelID             string
	InitialRevisionSHA        string
}

type OnboardingOpenInput struct {
	OnboardingID                  string
	Kind                          domain.OnboardingKind
	CanonicalRepository           string
	Requester                     domain.GitHubUserIdentity
	PrivateInputDigest            string
	SourcePathDigest              string
	SourceAncestorDigests         []string
	RequestDigest                 string
	ConfigurationBaseGenerationID int64
	ConfigurationBaseDigest       string
	ConfigurationAuthorityVersion int64
	OpenedAt                      time.Time
}

type OnboardingPreflightInput struct {
	OnboardingID    string
	ExpectedStatus  domain.OnboardingStatus
	PreflightDigest string
	EvidenceDigest  string
	ObservedAt      time.Time
}

type OnboardingStartAcceptance struct {
	OnboardingID    string
	Expected        Onboarding
	PreflightDigest string
	PreviewDigest   string
	Profile         LocalRepository
	Receipt         OperationReceipt
	AcceptedAt      time.Time
}

type OnboardingStepIntent struct {
	OnboardingID string
	Step         domain.OnboardingStep
	IntentDigest string
	IntendedAt   time.Time
}

type OnboardingStepSettlement struct {
	OnboardingID string
	Step         domain.OnboardingStep
	Observation  OnboardingStepObservation
	ObservedAt   time.Time
}

type OnboardingStore interface {
	OperationReceiptStore
	OpenOnboarding(context.Context, OnboardingOpenInput) (Onboarding, bool, error)
	Onboarding(context.Context, string) (Onboarding, bool, error)
	SaveOnboardingPreflight(context.Context, OnboardingPreflightInput) (Onboarding, error)
	StartOnboarding(context.Context, OnboardingStartAcceptance) (Onboarding, OperationReceipt, bool, error)
	CancelOnboarding(context.Context, string, time.Time) (Onboarding, bool, error)
	BeginOnboardingStep(context.Context, OnboardingStepIntent) (bool, error)
	SettleOnboardingStep(context.Context, OnboardingStepSettlement) (Onboarding, error)
	ResumeOnboarding(context.Context, string, time.Time) (Onboarding, bool, error)
	ListRunnableOnboardings(context.Context, int) ([]string, error)
	OnboardingAuthority(context.Context, string) (OnboardingAuthority, bool, error)
}

type OnboardingPrivateInputStore interface {
	Put(string, domain.RepositoryOnboardingInput, string) error
	Get(string, string) (domain.RepositoryOnboardingInput, error)
}

type OnboardingPathAuthority interface {
	DeriveManagedSource(string) (string, error)
}

type OnboardingPreflightPort interface {
	ObserveOnboardingPreflight(context.Context, domain.RepositoryOnboardingInput, ConfigurationAdmissionAuthority) (OnboardingPreflightEvidence, error)
}

type OnboardingStepExecutor interface {
	ExecuteOnboardingStep(context.Context, Onboarding, domain.RepositoryOnboardingInput, domain.OnboardingStep) (OnboardingStepObservation, error)
}

type OnboardingService struct {
	store         OnboardingStore
	private       OnboardingPrivateInputStore
	paths         OnboardingPathAuthority
	authorizer    *AuthorizationService
	configuration *ConfigurationService
	preflight     OnboardingPreflightPort
	executor      OnboardingStepExecutor
	now           func() time.Time
}

func NewOnboardingService(store OnboardingStore, private OnboardingPrivateInputStore, paths OnboardingPathAuthority, authorizer *AuthorizationService, configuration *ConfigurationService, preflight OnboardingPreflightPort, executor OnboardingStepExecutor) (*OnboardingService, error) {
	if store == nil || private == nil || paths == nil || authorizer == nil || configuration == nil || preflight == nil {
		return nil, errors.New("onboarding dependencies are required")
	}
	return &OnboardingService{store: store, private: private, paths: paths, authorizer: authorizer, configuration: configuration, preflight: preflight, executor: executor, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *OnboardingService) Open(ctx context.Context, command OnboardingOpenCommand) (Onboarding, error) {
	input := command.Input
	input.CanonicalRepository = strings.ToLower(strings.TrimSpace(input.CanonicalRepository))
	input.GitHubAppProfileRef = strings.TrimSpace(input.GitHubAppProfileRef)
	input.BaseBranch = strings.TrimSpace(input.BaseBranch)
	input.LinearLabelSlug = strings.TrimSpace(input.LinearLabelSlug)
	input.VerifierIDs = append([]string(nil), input.VerifierIDs...)
	slices.Sort(input.VerifierIDs)
	if input.Validate() != nil {
		return Onboarding{}, serviceError(ErrorInvalidInput, "existing-checkout onboarding input is invalid", nil)
	}
	return s.open(ctx, command.Requester, command.RequestID, domain.ExistingRepositoryOnboardingInput(input))
}

func (s *OnboardingService) OpenEmpty(ctx context.Context, command EmptyRepositoryOnboardingOpenCommand) (Onboarding, error) {
	input := command.Input
	input.CanonicalRepository = strings.ToLower(strings.TrimSpace(input.CanonicalRepository))
	input.GitHubAppProfileRef = strings.TrimSpace(input.GitHubAppProfileRef)
	input.BaseBranch = strings.TrimSpace(input.BaseBranch)
	input.LinearLabelSlug = strings.TrimSpace(input.LinearLabelSlug)
	input.VerifierIDs = append([]string(nil), input.VerifierIDs...)
	slices.Sort(input.VerifierIDs)
	if input.Validate() != nil {
		return Onboarding{}, serviceError(ErrorInvalidInput, "empty-repository onboarding input is invalid", nil)
	}
	derived, err := s.paths.DeriveManagedSource(input.CanonicalRepository)
	if err != nil {
		return Onboarding{}, serviceError(ErrorConflict, "managed source authority is unavailable", nil)
	}
	return s.open(ctx, command.Requester, command.RequestID, domain.ManagedEmptyRepositoryOnboardingInput(input, derived))
}

func (s *OnboardingService) open(ctx context.Context, requester Requester, requestID string, input domain.RepositoryOnboardingInput) (Onboarding, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return Onboarding{}, hiddenTargetError()
	}
	if strings.TrimSpace(requestID) == "" || len(requestID) > 128 || strings.ContainsRune(requestID, '\x00') || input.Validate() != nil {
		return Onboarding{}, serviceError(ErrorInvalidInput, "repository onboarding input is invalid", nil)
	}
	privateDigest := onboardingInputDigest(input)
	identitySeed := "onboarding-identity-v2\x00" + requestID + "\x00" + string(input.Kind) + "\x00" + privateDigest + "\x00" + configured.Identity().NodeID
	if input.Kind == domain.OnboardingExistingCheckout {
		identitySeed = "onboarding-identity-v1\x00" + requestID + "\x00" + privateDigest + "\x00" + configured.Identity().NodeID
	}
	identitySeedDigest := digestText(identitySeed)
	onboardingID := "onboarding-" + identitySeedDigest[:32]
	if existing, found, lookupErr := s.store.Onboarding(ctx, onboardingID); lookupErr != nil {
		return Onboarding{}, classifyOnboardingError(lookupErr)
	} else if found {
		if existing.Kind != input.Kind || existing.CanonicalRepository != input.CanonicalRepository || existing.PrivateInputDigest != privateDigest || !existing.Requester.Equal(configured.Identity()) {
			return Onboarding{}, serviceError(ErrorConflict, "onboarding open authority changed", nil)
		}
		if err := s.private.Put(onboardingID, input, privateDigest); err != nil {
			return Onboarding{}, serviceError(ErrorConflict, "private onboarding input could not be retained", nil)
		}
		return projectOnboarding(existing), nil
	}
	admission, err := s.configuration.CheckNewAdmissionReadOnly(ctx)
	if err != nil || !admission.Allowed {
		return Onboarding{}, serviceError(ErrorConflict, "configuration authority is unavailable for onboarding", err)
	}
	requestSeed := "onboarding-open-v2\x00" + identitySeedDigest + "\x00" + string(input.Kind) + "\x00" + admission.Authority.Digest + "\x00" + strconv.FormatInt(admission.Authority.GenerationID, 10)
	if input.Kind == domain.OnboardingExistingCheckout {
		requestSeed = "onboarding-open-v1\x00" + identitySeedDigest + "\x00" + admission.Authority.Digest + "\x00" + strconv.FormatInt(admission.Authority.GenerationID, 10)
	}
	requestDigest := digestText(requestSeed)
	if err := s.private.Put(onboardingID, input, privateDigest); err != nil {
		return Onboarding{}, serviceError(ErrorConflict, "private onboarding input could not be retained", nil)
	}
	sourceDigest, ancestorDigests := onboardingSourcePathDigests(input.SourcePath)
	opened, _, err := s.store.OpenOnboarding(ctx, OnboardingOpenInput{OnboardingID: onboardingID, Kind: input.Kind, CanonicalRepository: input.CanonicalRepository, Requester: configured.Identity(), PrivateInputDigest: privateDigest, SourcePathDigest: sourceDigest, SourceAncestorDigests: ancestorDigests, RequestDigest: requestDigest, ConfigurationBaseGenerationID: admission.Authority.GenerationID, ConfigurationBaseDigest: admission.Authority.Digest, ConfigurationAuthorityVersion: admission.Authority.AuthorityVersion, OpenedAt: s.now().UTC()})
	if err != nil {
		return Onboarding{}, classifyOnboardingError(err)
	}
	return projectOnboarding(opened), nil
}

func (s *OnboardingService) Show(ctx context.Context, command OnboardingCommand) (Onboarding, error) {
	onboarding, err := s.authorized(ctx, command)
	if err != nil {
		return Onboarding{}, err
	}
	return projectOnboarding(onboarding), nil
}

func (s *OnboardingService) Preflight(ctx context.Context, command OnboardingCommand) (OnboardingPreflight, error) {
	onboarding, err := s.authorized(ctx, command)
	if err != nil {
		return OnboardingPreflight{}, err
	}
	if onboarding.Status != domain.OnboardingOpened && onboarding.Status != domain.OnboardingPreflightReady {
		return OnboardingPreflight{}, serviceError(ErrorConflict, "onboarding cannot be preflighted in its current state", nil)
	}
	input, err := s.private.Get(onboarding.OnboardingID, onboarding.PrivateInputDigest)
	if err != nil {
		return OnboardingPreflight{}, serviceError(ErrorConflict, "private onboarding input is unavailable", nil)
	}
	admission, err := s.configuration.CheckNewAdmissionReadOnly(ctx)
	if err != nil || !admission.Allowed || admission.Authority.GenerationID != onboarding.ConfigurationBaseGenerationID || admission.Authority.Digest != onboarding.ConfigurationBaseDigest {
		return OnboardingPreflight{}, serviceError(ErrorConflict, "configuration is not exactly converged", err)
	}
	evidence, err := s.preflight.ObserveOnboardingPreflight(ctx, input, admission.Authority)
	if err != nil {
		return OnboardingPreflight{}, classifyServiceError(err)
	}
	preflightDigest := onboardingPreflightDigest(onboarding, evidence, admission.Authority)
	if !evidence.Ready {
		return OnboardingPreflight{OnboardingID: onboarding.OnboardingID, CanonicalRepository: onboarding.CanonicalRepository, Ready: false, ReasonCode: evidence.ReasonCode, PreflightDigest: preflightDigest, ConfigurationGenerationID: admission.Authority.GenerationID, ConfigurationDigest: admission.Authority.Digest, ObservedAt: evidence.ObservedAt}, nil
	}
	if !validOnboardingProfile(input, evidence.Profile) || !validAuthorityDigest(evidence.EvidenceDigest) {
		return OnboardingPreflight{}, serviceError(ErrorConflict, "preflight evidence is invalid", nil)
	}
	if _, err := s.store.SaveOnboardingPreflight(ctx, OnboardingPreflightInput{OnboardingID: onboarding.OnboardingID, ExpectedStatus: onboarding.Status, PreflightDigest: preflightDigest, EvidenceDigest: evidence.EvidenceDigest, ObservedAt: evidence.ObservedAt}); err != nil {
		return OnboardingPreflight{}, classifyOnboardingError(err)
	}
	return OnboardingPreflight{OnboardingID: onboarding.OnboardingID, CanonicalRepository: onboarding.CanonicalRepository, Ready: true, ReasonCode: evidence.ReasonCode, PreflightDigest: preflightDigest, ConfigurationGenerationID: admission.Authority.GenerationID, ConfigurationDigest: admission.Authority.Digest, ObservedAt: evidence.ObservedAt}, nil
}

func (s *OnboardingService) Preview(ctx context.Context, command OnboardingCommand) (OnboardingPreview, error) {
	onboarding, err := s.authorized(ctx, command)
	if err != nil {
		return OnboardingPreview{}, err
	}
	if onboarding.Status != domain.OnboardingPreflightReady || !validAuthorityDigest(onboarding.PreflightDigest) {
		return OnboardingPreview{}, serviceError(ErrorConflict, "onboarding preflight is required", nil)
	}
	input, err := s.private.Get(onboarding.OnboardingID, onboarding.PrivateInputDigest)
	if err != nil {
		return OnboardingPreview{}, serviceError(ErrorConflict, "private onboarding input is unavailable", nil)
	}
	threshold := input.CISlowThreshold
	if threshold == 0 {
		threshold = 20 * time.Minute
	}
	plan, ok := domain.OnboardingStepPlan(onboarding.Kind)
	if !ok {
		return OnboardingPreview{}, serviceError(ErrorConflict, "onboarding kind is invalid", nil)
	}
	preview := OnboardingPreview{OnboardingID: onboarding.OnboardingID, CanonicalRepository: onboarding.CanonicalRepository, Policy: OnboardingPolicyPreview{GitHubAppProfileRef: input.GitHubAppProfileRef, BaseBranch: input.BaseBranch, VerifierIDs: append([]string(nil), input.VerifierIDs...), LinearLabel: "repo:" + input.LinearLabelSlug, CISlowThreshold: threshold.String()}, OrderedEffects: plan, FinalState: domain.OnboardingReadyDisabled, RetainsPartialProgress: true, RollbackAvailable: false, WorkerRestartMayBeRequired: true, PreflightDigest: onboarding.PreflightDigest}
	if onboarding.Kind == domain.OnboardingEmptyRepository {
		preview.Kind = onboarding.Kind
	}
	raw, _ := json.Marshal(preview)
	preview.PreviewDigest = digestText("onboarding-preview-v1\x00" + string(raw))
	return preview, nil
}

func (s *OnboardingService) Start(ctx context.Context, command OnboardingStartCommand) (Onboarding, OperationReceipt, error) {
	onboarding, err := s.authorized(ctx, OnboardingCommand{Requester: command.Requester, OnboardingID: command.OnboardingID})
	if err != nil {
		return Onboarding{}, OperationReceipt{}, err
	}
	if onboarding.OperationID != "" {
		if command.PreflightDigest != onboarding.PreflightDigest || command.PreviewDigest != onboarding.PreviewDigest {
			return Onboarding{}, OperationReceipt{}, serviceError(ErrorConflict, "onboarding start authority changed", nil)
		}
		configured, resolveErr := s.authorizer.ResolveConfiguredRequester(command.Requester)
		authority, found, authorityErr := s.store.OnboardingAuthority(ctx, onboarding.OnboardingID)
		if resolveErr != nil || authorityErr != nil || !found {
			return Onboarding{}, OperationReceipt{}, hiddenTargetError()
		}
		scopes, scopeErr := s.authorizer.OnboardingScopes(configured, authority)
		if scopeErr != nil {
			return Onboarding{}, OperationReceipt{}, hiddenTargetError()
		}
		receipt, receiptErr := s.store.GetAuthorizedOperationReceipt(ctx, onboarding.OperationID, scopes)
		if receiptErr != nil || receipt.OperationType != OperationOnboardRepository {
			return Onboarding{}, OperationReceipt{}, hiddenTargetError()
		}
		return projectOnboarding(onboarding), receipt, nil
	}
	if onboarding.Status != domain.OnboardingPreflightReady || command.PreflightDigest != onboarding.PreflightDigest {
		return Onboarding{}, OperationReceipt{}, serviceError(ErrorConflict, "onboarding preflight authority changed", nil)
	}
	preview, err := s.Preview(ctx, OnboardingCommand{Requester: command.Requester, OnboardingID: command.OnboardingID})
	if err != nil || command.PreviewDigest != preview.PreviewDigest {
		return Onboarding{}, OperationReceipt{}, serviceError(ErrorConflict, "onboarding preview authority changed", err)
	}
	input, err := s.private.Get(onboarding.OnboardingID, onboarding.PrivateInputDigest)
	if err != nil {
		return Onboarding{}, OperationReceipt{}, serviceError(ErrorConflict, "private onboarding input is unavailable", nil)
	}
	admission, err := s.configuration.CheckNewAdmissionReadOnly(ctx)
	if err != nil || !admission.Allowed || admission.Authority.GenerationID != onboarding.ConfigurationBaseGenerationID || admission.Authority.Digest != onboarding.ConfigurationBaseDigest {
		return Onboarding{}, OperationReceipt{}, serviceError(ErrorConflict, "configuration is not exactly converged", err)
	}
	evidence, err := s.preflight.ObserveOnboardingPreflight(ctx, input, admission.Authority)
	if err != nil || !evidence.Ready || onboardingPreflightDigest(onboarding, evidence, admission.Authority) != command.PreflightDigest || !validOnboardingProfile(input, evidence.Profile) {
		return Onboarding{}, OperationReceipt{}, serviceError(ErrorConflict, "onboarding preflight became stale", err)
	}
	receipt := NewOperationReceipt(OperationReceiptInput{OperationType: OperationOnboardRepository, Scope: ScopeOnboarding, TargetID: onboarding.OnboardingID, Requester: onboarding.Requester, RequestDigest: onboarding.RequestDigest, ExpectedAuthorityDigest: onboarding.ConfigurationBaseDigest, OperationAnchorDigest: digestText("onboarding-start-v1\x00" + onboarding.OnboardingID + "\x00" + onboarding.PreflightDigest + "\x00" + preview.PreviewDigest), TargetBindingDigest: identityDigest(onboarding.Requester), AcceptedAt: s.now().UTC()})
	started, receipt, _, err := s.store.StartOnboarding(ctx, OnboardingStartAcceptance{OnboardingID: onboarding.OnboardingID, Expected: onboarding, PreflightDigest: command.PreflightDigest, PreviewDigest: command.PreviewDigest, Profile: evidence.Profile, Receipt: receipt, AcceptedAt: receipt.AcceptedAt})
	if err != nil {
		return Onboarding{}, OperationReceipt{}, classifyOnboardingError(err)
	}
	return projectOnboarding(started), receipt, nil
}

func (s *OnboardingService) Cancel(ctx context.Context, command OnboardingCommand) (Onboarding, error) {
	onboarding, err := s.authorized(ctx, command)
	if err != nil {
		return Onboarding{}, err
	}
	if onboarding.Status == domain.OnboardingCancelled {
		return projectOnboarding(onboarding), nil
	}
	if !domain.OnboardingCanCancel(onboarding.Status) {
		return Onboarding{}, serviceError(ErrorConflict, "onboarding cannot be cancelled after start", nil)
	}
	cancelled, _, err := s.store.CancelOnboarding(ctx, onboarding.OnboardingID, s.now().UTC())
	if err != nil {
		return Onboarding{}, classifyOnboardingError(err)
	}
	return projectOnboarding(cancelled), nil
}

func (s *OnboardingService) Resume(ctx context.Context, command OnboardingCommand) (Onboarding, error) {
	onboarding, err := s.authorized(ctx, command)
	if err != nil {
		return Onboarding{}, err
	}
	if onboarding.Status == domain.OnboardingRunning {
		return projectOnboarding(onboarding), nil
	}
	if onboarding.Status != domain.OnboardingWaitingForOperator {
		return Onboarding{}, serviceError(ErrorConflict, "onboarding is not waiting for operator correction", nil)
	}
	if onboarding.ReasonCode == "worker_restart_required" {
		return Onboarding{}, serviceError(ErrorConflict, "onboarding requires worker restart", nil)
	}
	resumed, _, err := s.store.ResumeOnboarding(ctx, onboarding.OnboardingID, s.now().UTC())
	if err != nil {
		return Onboarding{}, classifyOnboardingError(err)
	}
	return projectOnboarding(resumed), nil
}

func (s *OnboardingService) Continue(ctx context.Context, onboardingID string) (Onboarding, error) {
	if s.executor == nil {
		return Onboarding{}, serviceError(ErrorInternal, "onboarding worker is unavailable", nil)
	}
	onboarding, found, err := s.store.Onboarding(ctx, onboardingID)
	if err != nil || !found {
		return Onboarding{}, classifyOnboardingError(ErrOnboardingNotFound)
	}
	if onboarding.Status == domain.OnboardingWaitingForOperator && onboarding.ReasonCode == "worker_restart_required" {
		onboarding, _, err = s.store.ResumeOnboarding(ctx, onboarding.OnboardingID, s.now().UTC())
		if err != nil {
			return Onboarding{}, classifyOnboardingError(err)
		}
	}
	if onboarding.Status != domain.OnboardingAccepted && onboarding.Status != domain.OnboardingRunning {
		return projectOnboarding(onboarding), nil
	}
	input, err := s.private.Get(onboarding.OnboardingID, onboarding.PrivateInputDigest)
	if err != nil {
		return Onboarding{}, serviceError(ErrorConflict, "private onboarding input is unavailable", nil)
	}
	plan, ok := domain.OnboardingStepPlan(onboarding.Kind)
	if !ok {
		return Onboarding{}, serviceError(ErrorConflict, "onboarding kind is invalid", nil)
	}
	for len(onboarding.CompletedSteps) < len(plan) {
		step := plan[len(onboarding.CompletedSteps)]
		intentParts := "onboarding-step-intent-v2\x00" + onboarding.OnboardingID + "\x00" + string(onboarding.Kind) + "\x00" + string(step) + "\x00" + onboarding.RequestDigest
		if onboarding.Kind == domain.OnboardingExistingCheckout {
			intentParts = "onboarding-step-intent-v1\x00" + onboarding.OnboardingID + "\x00" + string(step) + "\x00" + onboarding.RequestDigest
		}
		if step == domain.OnboardingStepInitialRevisionCreated {
			intentParts += "\x00" + onboarding.AcceptedAt.UTC().Truncate(time.Second).Format(time.RFC3339)
		}
		if step == domain.OnboardingStepInitialBasePublished {
			intentParts += "\x00refs/heads/" + input.BaseBranch + "\x00" + onboarding.InitialRevisionSHA + "\x00empty-remote-v1"
		}
		intentDigest := digestText(intentParts)
		if _, err := s.store.BeginOnboardingStep(ctx, OnboardingStepIntent{OnboardingID: onboarding.OnboardingID, Step: step, IntentDigest: intentDigest, IntendedAt: s.now().UTC()}); err != nil {
			return Onboarding{}, classifyOnboardingError(err)
		}
		observation, err := s.executor.ExecuteOnboardingStep(ctx, onboarding, input, step)
		if err != nil {
			if ctx.Err() != nil {
				return Onboarding{}, classifyServiceError(ctx.Err())
			}
			observation = OnboardingStepObservation{Outcome: OperationOutcomeFailed, ReasonCode: onboardingStepFailureReason(step), EvidenceDigest: digestText("onboarding-step-failure-v1\x00" + onboarding.OnboardingID + "\x00" + string(step))}
		}
		if observation.Outcome == "" {
			observation.Outcome = OperationOutcomeSucceeded
		}
		if !validAuthorityDigest(observation.EvidenceDigest) {
			return Onboarding{}, serviceError(ErrorConflict, "onboarding step evidence is invalid", nil)
		}
		onboarding, err = s.store.SettleOnboardingStep(ctx, OnboardingStepSettlement{OnboardingID: onboarding.OnboardingID, Step: step, Observation: observation, ObservedAt: s.now().UTC()})
		if err != nil {
			return Onboarding{}, classifyOnboardingError(err)
		}
		if onboarding.Status == domain.OnboardingWaitingForOperator || onboarding.Status == domain.OnboardingConflict || onboarding.Status == domain.OnboardingReadyDisabled {
			break
		}
	}
	return projectOnboarding(onboarding), nil
}

func (s *OnboardingService) authorized(ctx context.Context, command OnboardingCommand) (Onboarding, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(command.Requester)
	if err != nil || strings.TrimSpace(command.OnboardingID) == "" {
		return Onboarding{}, hiddenTargetError()
	}
	authority, found, err := s.store.OnboardingAuthority(ctx, command.OnboardingID)
	if err != nil || !found {
		return Onboarding{}, hiddenTargetError()
	}
	if _, err := s.authorizer.OnboardingScopes(configured, authority); err != nil {
		return Onboarding{}, hiddenTargetError()
	}
	onboarding, found, err := s.store.Onboarding(ctx, command.OnboardingID)
	if err != nil || !found {
		return Onboarding{}, hiddenTargetError()
	}
	return onboarding, nil
}

func onboardingInputDigest(input domain.RepositoryOnboardingInput) string {
	if input.Kind == domain.OnboardingExistingCheckout {
		raw, _ := json.Marshal(struct {
			SourcePath, Repository, Profile, Branch, Label, Threshold string
			Verifiers                                                 []string
		}{input.SourcePath, input.CanonicalRepository, input.GitHubAppProfileRef, input.BaseBranch, input.LinearLabelSlug, input.CISlowThreshold.String(), input.VerifierIDs})
		return digestText("onboarding-private-input-v1\x00" + string(raw))
	}
	raw, _ := json.Marshal(input)
	return digestText("onboarding-private-input-v2\x00" + string(raw))
}

func OnboardingPrivateInputDigest(input domain.RepositoryOnboardingInput) string {
	return onboardingInputDigest(input)
}

func onboardingSourcePathDigests(path string) (string, []string) {
	digest := func(value string) string { return digestText("onboarding-source-path-v1\x00" + value) }
	result := make([]string, 0, 16)
	for current := path; ; current = filepath.Dir(current) {
		result = append(result, digest(current))
		parent := filepath.Dir(current)
		if parent == current || len(result) == 256 {
			break
		}
	}
	slices.Sort(result)
	return digest(path), result
}

func onboardingPreflightDigest(onboarding Onboarding, evidence OnboardingPreflightEvidence, authority ConfigurationAdmissionAuthority) string {
	return digestText("onboarding-preflight-v1\x00" + onboarding.OnboardingID + "\x00" + onboarding.PrivateInputDigest + "\x00" + evidence.EvidenceDigest + "\x00" + strconv.FormatInt(authority.GenerationID, 10) + "\x00" + authority.Digest + "\x00" + strconv.FormatInt(authority.AuthorityVersion, 10) + "\x00" + evidence.Profile.ProfileDigest + "\x00" + evidence.Profile.RepositoryBindingDigest)
}

func validOnboardingProfile(input domain.RepositoryOnboardingInput, profile LocalRepository) bool {
	identitiesValid := (profile.ProfileDigest == "" && profile.RepositoryBindingDigest == "") || validAuthorityDigest(profile.ProfileDigest) && validAuthorityDigest(profile.RepositoryBindingDigest)
	return profile.CanonicalRepository == input.CanonicalRepository && profile.SourcePath == input.SourcePath && profile.BaseBranch == input.BaseBranch && profile.GitHubAppProfileRef == input.GitHubAppProfileRef && profile.LinearLabel == "repo:"+input.LinearLabelSlug && slices.Equal(profile.VerifierIDs, input.VerifierIDs) && identitiesValid
}

func projectOnboarding(value Onboarding) Onboarding {
	value.Requester = domain.GitHubUserIdentity{}
	value.CompletedSteps = append([]domain.OnboardingStep(nil), value.CompletedSteps...)
	value.LegalNextActions = domain.OnboardingLegalActions(value.Status, validAuthorityDigest(value.PreflightDigest), value.ReasonCode)
	return value
}

func classifyOnboardingError(err error) error {
	switch {
	case errors.Is(err, ErrOnboardingNotFound):
		return hiddenTargetError()
	case errors.Is(err, ErrOnboardingConflict), errors.Is(err, ErrOperationReceiptConflict):
		return serviceError(ErrorConflict, "onboarding authority changed", err)
	default:
		return classifyServiceError(err)
	}
}

func onboardingStepFailureReason(step domain.OnboardingStep) string {
	switch step {
	case domain.OnboardingStepRootsCreated:
		return "controller_roots_unavailable"
	case domain.OnboardingStepManagedSourceCreated:
		return "managed_source_unavailable"
	case domain.OnboardingStepInitialRevisionCreated:
		return "initial_revision_unavailable"
	case domain.OnboardingStepInitialBasePublished:
		return "initial_base_publication_unavailable"
	case domain.OnboardingStepLinearLabelObserved:
		return "linear_label_observation_failed"
	case domain.OnboardingStepConfigurationApplied:
		return "configuration_apply_failed"
	case domain.OnboardingStepConfigurationConverged:
		return "configuration_convergence_failed"
	case domain.OnboardingStepLifecycleCreated:
		return "lifecycle_creation_failed"
	case domain.OnboardingStepReadinessPublished:
		return "readiness_observation_failed"
	default:
		return "onboarding_settlement_failed"
	}
}
