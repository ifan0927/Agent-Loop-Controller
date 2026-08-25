package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	configurationadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/configuration"
	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/githubapp"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localregistry"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type onboardingRuntime struct {
	loaded        bootstrap.Bootstrap
	store         *sqlitestore.Store
	files         *configurationadapter.Files
	configuration *application.ConfigurationService
	linear        *linearadapter.Client
	repositories  *application.RepositoryService
}

func composeOnboardingService(loaded bootstrap.Bootstrap, store *sqlitestore.Store, withReadiness bool) (*application.OnboardingService, error) {
	files, err := configurationadapter.NewFiles(loaded.Path)
	if err != nil {
		return nil, err
	}
	if err := files.BindDatabaseIdentity(loaded.Controller.DatabasePath, store.DatabaseIdentity()); err != nil {
		return nil, err
	}
	configuration, err := configuredConvergenceService(store, loaded, 0, false)
	if err != nil {
		return nil, err
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return nil, err
	}
	credentials, err := linearCredentialSource(loaded)
	if err != nil {
		return nil, err
	}
	linear, err := linearadapter.New(loaded.Linear, credentials, nil)
	if err != nil {
		return nil, err
	}
	runtime := &onboardingRuntime{loaded: loaded, store: store, files: files, configuration: configuration, linear: linear}
	if withReadiness {
		runtime.repositories, err = composeOnboardingRepositoryService(loaded, store, configuration, linear, authorizer)
		if err != nil {
			return nil, err
		}
	}
	return application.NewOnboardingService(store, files, files, authorizer, configuration, runtime, runtime)
}

func composeOnboardingRepositoryService(loaded bootstrap.Bootstrap, store *sqlitestore.Store, convergence *application.ConfigurationService, linear *linearadapter.Client, authorizer *application.AuthorizationService) (*application.RepositoryService, error) {
	observers := application.RepositoryObservers{
		Configuration: repositoryConfigurationObserver{service: convergence, store: store},
		Git:           repositoryGitObserver{observer: gitadapter.ReadinessObserver{}},
		GitHub:        repositoryGitHubObserver{loaded: loaded},
		Linear:        linear,
		Verifier:      loaded.Registry,
	}
	return application.NewRepositoryService(store, authorizer, loaded.Registry, observers)
}

func (r *onboardingRuntime) ObserveOnboardingPreflight(ctx context.Context, input domain.RepositoryOnboardingInput, authority application.ConfigurationAdmissionAuthority) (application.OnboardingPreflightEvidence, error) {
	now := time.Now().UTC()
	fail := func(reason string) (application.OnboardingPreflightEvidence, error) {
		return application.OnboardingPreflightEvidence{Ready: false, ReasonCode: reason, EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-preflight-failed-v1", input.CanonicalRepository, reason, authority.Digest), ObservedAt: now}, nil
	}
	if r.loaded.Registry.HasRepository(input.CanonicalRepository) {
		return fail("repository_already_configured")
	}
	if _, err := r.store.RepositoryOperationAuthority(ctx, input.CanonicalRepository); err == nil {
		return fail("repository_lifecycle_exists")
	} else if !errors.Is(err, application.ErrRepositoryLifecycleMissing) {
		return fail("repository_lifecycle_observation_unavailable")
	}
	profile, ok := r.loaded.GitHubProfiles[input.GitHubAppProfileRef]
	if !ok || !strings.EqualFold(profile.Config.RepositoryOwner+"/"+profile.Config.RepositoryName, input.CanonicalRepository) {
		return fail("github_profile_identity_conflict")
	}
	commands := localregistry.BuiltinVerifierCommands()
	for _, verifierID := range input.VerifierIDs {
		if _, found := commands[verifierID]; !found {
			return fail("verifier_policy_unavailable")
		}
	}
	repositoryRoot, runRoot, worktreeRoot := onboardingRepositoryRoots(r.loaded.Path, input.CanonicalRepository)
	forbidden := []string{r.loaded.Path, r.loaded.Controller.DatabasePath, filepath.Join(filepath.Dir(r.loaded.Path), "authority")}
	for _, binding := range r.loaded.Registry.Bindings() {
		forbidden = append(forbidden, binding.SourcePath, binding.RunRoot, binding.WorktreeRoot)
		if filepath.IsAbs(binding.OriginPath) {
			forbidden = append(forbidden, binding.OriginPath)
		}
	}
	var gitEvidence, origin string
	if input.Kind == domain.OnboardingExistingCheckout {
		existingForbidden := append(append([]string(nil), forbidden...), repositoryRoot, runRoot, worktreeRoot)
		gitObservation, err := (gitadapter.ExistingCheckoutInspector{}).Inspect(ctx, gitadapter.ExistingCheckoutRequest{SourcePath: input.SourcePath, CanonicalRepository: input.CanonicalRepository, BaseBranch: input.BaseBranch, ForbiddenRoots: existingForbidden})
		if err != nil {
			return fail("checkout_preflight_failed")
		}
		gitEvidence, origin = gitObservation.EvidenceDigest, gitObservation.Origin
	} else if input.Kind == domain.OnboardingEmptyRepository {
		derived, deriveErr := r.files.DeriveManagedSource(input.CanonicalRepository)
		if deriveErr != nil || derived != input.SourcePath || configurationadapter.InspectEmptyOnboardingPaths(repositoryRoot, input.SourcePath, runRoot, worktreeRoot, forbidden) != nil {
			return fail("managed_source_preflight_failed")
		}
		emptyGit := gitadapter.EmptyRepositoryGit{}
		if emptyGit.ValidateBaseBranch(ctx, filepath.Dir(r.loaded.Path), input.BaseBranch) != nil {
			return fail("base_branch_invalid")
		}
		remote, remoteErr := emptyGit.ObserveRemoteRefs(ctx, filepath.Dir(r.loaded.Path), input.CanonicalRepository)
		if remoteErr != nil {
			return fail("ssh_remote_observation_unavailable")
		}
		if len(remote.Refs) != 0 {
			return fail("remote_repository_not_empty")
		}
		gitEvidence, origin = remote.EvidenceDigest, canonicalGitHubSSHOrigin(input.CanonicalRepository)
	} else {
		return fail("onboarding_kind_conflict")
	}
	partial := r.partialProfile(input, origin, runRoot, worktreeRoot)
	githubClient, err := githubapp.New(profile.Config, nil, nil)
	if err != nil {
		return fail("github_observation_unavailable")
	}
	githubResults, err := githubClient.ObserveRepositoryGitHub(ctx, partial)
	if err != nil || len(githubResults) != 2 || githubResults[0].Status != domain.RepositoryReady || githubResults[1].Status != domain.RepositoryReady {
		return fail("github_identity_not_ready")
	}
	label, err := r.linear.LookupRepositoryLabel(ctx, partial.LinearLabel)
	if err != nil {
		return fail("linear_label_observation_unavailable")
	}
	evidence := application.ConfigurationEvidenceDigest("onboarding-preflight-v2", string(input.Kind), input.CanonicalRepository, gitEvidence, githubResults[0].EvidenceDigest, githubResults[1].EvidenceDigest, label.EvidenceDigest, authority.Digest, strings.Join(input.VerifierIDs, "\x00"), repositoryRoot, input.SourcePath, runRoot, worktreeRoot)
	if input.Kind == domain.OnboardingExistingCheckout {
		evidence = application.ConfigurationEvidenceDigest("onboarding-preflight-v1", input.CanonicalRepository, gitEvidence, githubResults[0].EvidenceDigest, githubResults[1].EvidenceDigest, label.EvidenceDigest, authority.Digest, strings.Join(input.VerifierIDs, "\x00"), repositoryRoot, runRoot, worktreeRoot)
	}
	return application.OnboardingPreflightEvidence{Ready: true, ReasonCode: "preflight_ready", EvidenceDigest: evidence, Profile: partial, ObservedAt: now}, nil
}

func (r *onboardingRuntime) ExecuteOnboardingStep(ctx context.Context, onboarding application.Onboarding, input domain.RepositoryOnboardingInput, step domain.OnboardingStep) (application.OnboardingStepObservation, error) {
	repositoryRoot, runRoot, worktreeRoot := onboardingRepositoryRoots(r.loaded.Path, input.CanonicalRepository)
	switch step {
	case domain.OnboardingStepRootsCreated:
		var evidence configurationadapter.OnboardingRootEvidence
		var err error
		if input.Kind == domain.OnboardingEmptyRepository {
			evidence, err = configurationadapter.EnsureEmptyOnboardingRoots(repositoryRoot, input.SourcePath, runRoot, worktreeRoot, onboarding.OnboardingID, input.CanonicalRepository)
		} else {
			evidence, err = configurationadapter.EnsureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, onboarding.OnboardingID, input.CanonicalRepository)
		}
		if err != nil {
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "root_ownership_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-root-conflict-v1", onboarding.OnboardingID)}, nil
		}
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "roots_ready", EvidenceDigest: evidence.EvidenceDigest}, nil
	case domain.OnboardingStepManagedSourceCreated:
		if input.Kind != domain.OnboardingEmptyRepository {
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "onboarding_kind_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-kind-conflict-v1", onboarding.OnboardingID, string(step))}, nil
		}
		if _, err := configurationadapter.VerifyEmptyOnboardingRoots(repositoryRoot, input.SourcePath, runRoot, worktreeRoot, onboarding.OnboardingID, input.CanonicalRepository); err != nil {
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "managed_source_ownership_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("managed-source-owner-conflict-v1", onboarding.OnboardingID)}, nil
		}
		evidence, err := (gitadapter.EmptyRepositoryGit{}).EnsureManagedSource(ctx, repositoryRoot, input.SourcePath, input.CanonicalRepository, input.BaseBranch)
		if err != nil {
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "managed_source_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("managed-source-conflict-v1", onboarding.OnboardingID)}, nil
		}
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "managed_source_ready", EvidenceDigest: evidence}, nil
	case domain.OnboardingStepInitialRevisionCreated:
		sha, evidence, err := (gitadapter.EmptyRepositoryGit{}).EnsureInitialRevision(ctx, input.SourcePath, input.CanonicalRepository, input.BaseBranch, onboarding.AcceptedAt)
		if err != nil {
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "initial_revision_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-revision-conflict-v1", onboarding.OnboardingID)}, nil
		}
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "initial_revision_ready", EvidenceDigest: evidence, InitialRevisionSHA: sha}, nil
	case domain.OnboardingStepInitialBasePublished:
		return r.publishInitialBase(ctx, onboarding, input)
	case domain.OnboardingStepLinearLabelObserved:
		return r.ensureLinearLabel(ctx, onboarding, input)
	case domain.OnboardingStepConfigurationApplied:
		return r.applyOnboardingConfiguration(ctx, onboarding, input, runRoot, worktreeRoot)
	case domain.OnboardingStepConfigurationConverged:
		decision, err := r.configuration.CheckNewAdmission(ctx)
		if err != nil || !decision.Allowed || decision.Authority.GenerationID != onboarding.ConfigurationGenerationID {
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeFailed, ReasonCode: "worker_restart_required", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-convergence-wait-v1", onboarding.OnboardingID, fmt.Sprint(onboarding.ConfigurationGenerationID))}, nil
		}
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "configuration_converged", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-convergence-v1", onboarding.OnboardingID, decision.Authority.Digest, fmt.Sprint(decision.Authority.GenerationID))}, nil
	case domain.OnboardingStepLifecycleCreated:
		profile, found, err := r.loaded.Registry.RepositoryProfile(ctx, input.CanonicalRepository)
		if err != nil || !found {
			return application.OnboardingStepObservation{}, errors.New("onboarding repository profile is unavailable")
		}
		projection, _, err := r.store.CreateOnboardingRepositoryLifecycle(ctx, onboarding.OnboardingID, profile.Profile, time.Now().UTC())
		if err != nil {
			return application.OnboardingStepObservation{}, err
		}
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "disabled_lifecycle_created", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-lifecycle-v1", onboarding.OnboardingID, projection.Lifecycle.IncarnationID, projection.Readiness.SnapshotDigest), IncarnationID: projection.Lifecycle.IncarnationID, ReadinessSnapshotID: projection.Readiness.SnapshotID}, nil
	case domain.OnboardingStepReadinessPublished:
		if r.repositories == nil {
			return application.OnboardingStepObservation{}, errors.New("repository readiness service is unavailable")
		}
		label, err := r.linear.LookupRepositoryLabel(ctx, "repo:"+input.LinearLabelSlug)
		if err != nil {
			return application.OnboardingStepObservation{}, err
		}
		if !label.Found || onboarding.LinearLabelID == "" || label.LabelID != onboarding.LinearLabelID {
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "linear_label_identity_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-linear-conflict-v1", onboarding.OnboardingID)}, nil
		}
		requester := application.Requester{ID: onboarding.Requester.Login, Kind: "github_login", DatabaseID: onboarding.Requester.DatabaseID, NodeID: onboarding.Requester.NodeID, ActorType: onboarding.Requester.ActorType}
		result, err := r.repositories.Recheck(ctx, application.RepositoryMutationCommand{Requester: requester, Repository: input.CanonicalRepository, RequestID: "onboarding-readiness-" + onboarding.OnboardingID})
		if err != nil {
			return application.OnboardingStepObservation{}, err
		}
		if result.Repository.Lifecycle.Intent != application.RepositoryDisabled || result.Repository.Readiness.Status != domain.RepositoryReady {
			if result.Repository.Readiness.Status == domain.RepositoryConflict {
				return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "repository_readiness_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-readiness-conflict-v1", onboarding.OnboardingID, result.Repository.Readiness.SnapshotDigest), ReadinessSnapshotID: result.Repository.Readiness.SnapshotID}, nil
			}
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeFailed, ReasonCode: "repository_not_ready", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-readiness-wait-v1", onboarding.OnboardingID, result.Repository.Readiness.SnapshotDigest), ReadinessSnapshotID: result.Repository.Readiness.SnapshotID}, nil
		}
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "repository_ready_disabled", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-readiness-v1", onboarding.OnboardingID, result.Repository.Readiness.SnapshotDigest), ReadinessSnapshotID: result.Repository.Readiness.SnapshotID}, nil
	case domain.OnboardingStepSettled:
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "ready_disabled", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-settled-v1", onboarding.OnboardingID, onboarding.IncarnationID, onboarding.ReadinessSnapshotID)}, nil
	default:
		return application.OnboardingStepObservation{}, errors.New("onboarding step is unsupported")
	}
}

func (r *onboardingRuntime) publishInitialBase(ctx context.Context, onboarding application.Onboarding, input domain.RepositoryOnboardingInput) (application.OnboardingStepObservation, error) {
	sha := onboarding.InitialRevisionSHA
	if input.Kind != domain.OnboardingEmptyRepository || sha == "" {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "initial_revision_authority_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-base-authority-conflict-v1", onboarding.OnboardingID)}, nil
	}
	git := gitadapter.EmptyRepositoryGit{}
	remote, err := git.PublishInitialBase(ctx, input.SourcePath, input.CanonicalRepository, input.BaseBranch, sha)
	if err != nil && remote.EvidenceDigest == "" {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeAmbiguous, ReasonCode: "initial_push_outcome_ambiguous", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-push-ambiguous-v1", onboarding.OnboardingID, sha)}, nil
	}
	ref := "refs/heads/" + input.BaseBranch
	if len(remote.Refs) == 0 {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeFailed, ReasonCode: "initial_push_not_observed", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-push-empty-v1", onboarding.OnboardingID, sha, remote.EvidenceDigest)}, nil
	}
	if len(remote.Refs) != 1 || remote.Refs[ref] != sha {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "initial_remote_ref_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-push-conflict-v1", onboarding.OnboardingID, sha, remote.EvidenceDigest)}, nil
	}
	profile, ok := r.loaded.GitHubProfiles[input.GitHubAppProfileRef]
	if !ok {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "github_profile_identity_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-push-profile-conflict-v1", onboarding.OnboardingID)}, nil
	}
	client, clientErr := githubapp.New(profile.Config, nil, nil)
	if clientErr != nil {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeFailed, ReasonCode: "github_base_observation_unavailable", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-push-github-wait-v1", onboarding.OnboardingID, sha)}, nil
	}
	github := client.ObserveInitialBase(ctx, input.CanonicalRepository, input.BaseBranch, sha)
	switch github.Status {
	case githubapp.BaseRefUnavailable:
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeFailed, ReasonCode: github.ReasonCode, EvidenceDigest: github.EvidenceDigest}, nil
	case githubapp.BaseRefConflict:
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: github.ReasonCode, EvidenceDigest: github.EvidenceDigest}, nil
	case githubapp.BaseRefReady:
		localEvidence, settleErr := git.SettlePublishedSource(ctx, input.SourcePath, input.CanonicalRepository, input.BaseBranch, sha)
		if settleErr != nil {
			return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "published_source_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("published-source-conflict-v1", onboarding.OnboardingID, sha)}, nil
		}
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "initial_base_published", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-base-published-v1", onboarding.OnboardingID, sha, remote.EvidenceDigest, github.EvidenceDigest, localEvidence)}, nil
	default:
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeAmbiguous, ReasonCode: "github_base_observation_ambiguous", EvidenceDigest: application.ConfigurationEvidenceDigest("initial-push-github-ambiguous-v1", onboarding.OnboardingID, sha)}, nil
	}
}

func (r *onboardingRuntime) ensureLinearLabel(ctx context.Context, onboarding application.Onboarding, input domain.RepositoryOnboardingInput) (application.OnboardingStepObservation, error) {
	name := "repo:" + input.LinearLabelSlug
	observed, err := r.linear.LookupRepositoryLabel(ctx, name)
	if err != nil {
		return application.OnboardingStepObservation{}, err
	}
	if !observed.Found {
		if _, err := r.linear.CreateRepositoryLabel(ctx, observed.TeamID, name); err != nil {
			// A lost response is never retried before an exact reread.
			observed, err = r.linear.LookupRepositoryLabel(ctx, name)
			if err != nil || !observed.Found {
				return application.OnboardingStepObservation{Outcome: application.OperationOutcomeFailed, ReasonCode: "linear_label_outcome_unknown", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-linear-unknown-v1", onboarding.OnboardingID)}, nil
			}
		}
		observed, err = r.linear.LookupRepositoryLabel(ctx, name)
		if err != nil {
			return application.OnboardingStepObservation{}, err
		}
	}
	if !observed.Found || observed.Name != name || observed.TeamKey != r.loaded.Linear.TeamKey {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "linear_label_identity_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-linear-conflict-v1", onboarding.OnboardingID)}, nil
	}
	return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "linear_label_ready", EvidenceDigest: observed.EvidenceDigest, LinearLabelID: observed.LabelID}, nil
}

func (r *onboardingRuntime) applyOnboardingConfiguration(ctx context.Context, onboarding application.Onboarding, input domain.RepositoryOnboardingInput, runRoot, worktreeRoot string) (application.OnboardingStepObservation, error) {
	label, err := r.linear.LookupRepositoryLabel(ctx, "repo:"+input.LinearLabelSlug)
	if err != nil {
		return application.OnboardingStepObservation{}, err
	}
	if !label.Found || onboarding.LinearLabelID == "" || label.LabelID != onboarding.LinearLabelID {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "linear_label_identity_conflict", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-linear-conflict-v1", onboarding.OnboardingID)}, nil
	}
	authority, err := r.configuration.Reconcile(ctx)
	if err != nil {
		return application.OnboardingStepObservation{}, err
	}
	if authority.Desired.GenerationID != onboarding.ConfigurationBaseGenerationID || authority.Desired.Digest != onboarding.ConfigurationBaseDigest {
		return application.OnboardingStepObservation{Outcome: application.OperationOutcomeConflict, ReasonCode: "configuration_base_changed", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-config-conflict-v1", onboarding.OnboardingID)}, nil
	}
	base, err := r.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if err != nil {
		return application.OnboardingStepObservation{}, err
	}
	githubProfile := r.loaded.GitHubProfiles[input.GitHubAppProfileRef]
	repository := r.repositoryDocument(input, runRoot, worktreeRoot, githubProfile)
	candidate, profile, err := r.files.MaterializeRepositoryAddition(base, repository)
	if err != nil {
		return application.OnboardingStepObservation{}, err
	}
	if _, err := r.files.ValidateRepositoryAdditionCandidate(base, candidate, profile); err != nil {
		return application.OnboardingStepObservation{}, err
	}
	result, err := r.configuration.Apply(ctx, application.ConfigurationApplyCommand{Requester: application.Requester{ID: onboarding.Requester.Login, Kind: "github_login", DatabaseID: onboarding.Requester.DatabaseID, NodeID: onboarding.Requester.NodeID, ActorType: onboarding.Requester.ActorType}, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: candidate, Provenance: application.ConfigurationApplyProvenance{Kind: application.ConfigurationApplyOnboarding, OnboardingSourceID: onboarding.OnboardingID, OnboardingSourceDigest: onboarding.RequestDigest}})
	if err != nil {
		return application.OnboardingStepObservation{}, err
	}
	return application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "configuration_applied", EvidenceDigest: application.ConfigurationEvidenceDigest("onboarding-configuration-v1", onboarding.OnboardingID, result.Generation.Digest, fmt.Sprint(result.Generation.GenerationID), result.Receipt.EvidenceDigest), ProfileID: profile.ProfileID, ProfileDigest: profile.ProfileDigest, RepositoryBindingDigest: profile.RepositoryBindingDigest, ConfigurationGenerationID: result.Generation.GenerationID}, nil
}

func (r *onboardingRuntime) partialProfile(input domain.RepositoryOnboardingInput, origin, runRoot, worktreeRoot string) application.LocalRepository {
	profile := r.loaded.GitHubProfiles[input.GitHubAppProfileRef]
	threshold := input.CISlowThreshold
	if threshold == 0 {
		threshold = localregistry.DefaultCISlowThreshold
	}
	trusted := onboardingTrustedActors(r.loaded)
	allowed := make([]string, 0, len(trusted))
	actors := make([]application.TrustedActorIdentity, 0, len(trusted))
	for _, actor := range trusted {
		allowed = append(allowed, strings.ToLower(actor.Login))
		actors = append(actors, application.TrustedActorIdentity{DatabaseID: actor.DatabaseID, NodeID: actor.NodeID, Login: strings.ToLower(actor.Login), Type: actor.Type})
	}
	slices.Sort(allowed)
	return application.LocalRepository{CanonicalRepository: input.CanonicalRepository, LinearLabel: "repo:" + input.LinearLabelSlug, OriginPath: origin, SourcePath: input.SourcePath, RunRoot: runRoot, WorktreeRoot: worktreeRoot, BaseBranch: input.BaseBranch, VerifierRegistryRef: "builtin:v1", VerifierIDs: append([]string(nil), input.VerifierIDs...), GitHubAppProfileRef: input.GitHubAppProfileRef, GitHubAppID: profile.Config.AppID, GitHubInstallationID: profile.Config.InstallationID, ExpectedRepositoryID: profile.Config.RepositoryID, CISlowThreshold: threshold, AllowedOperatorLogins: allowed, TrustedOperatorActors: actors}
}

func (r *onboardingRuntime) repositoryDocument(input domain.RepositoryOnboardingInput, runRoot, worktreeRoot string, profile bootstrap.GitHubProfile) localregistry.Repository {
	parts := strings.Split(input.CanonicalRepository, "/")
	trusted := onboardingTrustedActors(r.loaded)
	allowed := make([]string, 0, len(trusted))
	for _, actor := range trusted {
		allowed = append(allowed, strings.ToLower(actor.Login))
	}
	threshold := ""
	if input.CISlowThreshold != 0 {
		threshold = input.CISlowThreshold.String()
	}
	return localregistry.Repository{Owner: parts[0], Name: parts[1], LinearLabel: "repo:" + input.LinearLabelSlug, OriginURL: canonicalGitHubOrigin(profile.Config.RepositoryOwner, profile.Config.RepositoryName), SourcePath: input.SourcePath, RunRoot: runRoot, WorktreeRoot: worktreeRoot, BaseBranch: input.BaseBranch, VerifierRegistryRef: "builtin:v1", VerifierIDs: append([]string(nil), input.VerifierIDs...), GitHubAppProfileRef: input.GitHubAppProfileRef, GitHubAppID: profile.Config.AppID, GitHubInstallationID: profile.Config.InstallationID, ExpectedRepositoryID: profile.Config.RepositoryID, CISlowThreshold: threshold, OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: allowed, TrustedActors: trusted}}
}

func onboardingTrustedActors(loaded bootstrap.Bootstrap) []localregistry.TrustedActorIdentity {
	result := []localregistry.TrustedActorIdentity{{DatabaseID: loaded.Controller.Operator.DatabaseID, NodeID: loaded.Controller.Operator.NodeID, Login: strings.ToLower(loaded.Controller.Operator.Login), Type: loaded.Controller.Operator.ActorType}}
	if configured := loaded.Automation.LinearTodoAdmission; configured.Enabled {
		candidate := configured.Requester
		if !slices.ContainsFunc(result, func(existing localregistry.TrustedActorIdentity) bool {
			return existing.DatabaseID == candidate.DatabaseID && existing.NodeID == candidate.NodeID
		}) {
			result = append(result, localregistry.TrustedActorIdentity{DatabaseID: candidate.DatabaseID, NodeID: candidate.NodeID, Login: strings.ToLower(candidate.Login), Type: candidate.Type})
		}
	}
	slices.SortFunc(result, func(left, right localregistry.TrustedActorIdentity) int {
		return strings.Compare(left.NodeID, right.NodeID)
	})
	return result
}

func onboardingRepositoryRoots(configPath, repository string) (string, string, string) {
	name := strings.ReplaceAll(repository, "/", "--")
	root := filepath.Join(filepath.Dir(configPath), "repositories", name)
	return root, filepath.Join(root, "runs"), filepath.Join(root, "worktrees")
}

func canonicalGitHubOrigin(owner, repository string) string {
	return "https://github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(repository) + ".git"
}

func canonicalGitHubSSHOrigin(repository string) string {
	remote, _ := gitadapter.CanonicalGitHubSSHRemote(repository)
	return remote
}
