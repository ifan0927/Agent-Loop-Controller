package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	"github.com/ifan0927/Agent-Loop-Controller/internal/fixtureevidence"
)

func TestControllerWorkerRejectsRootBeforeOpeningRuntime(t *testing.T) {
	originalUID := workerEffectiveUID
	originalBuild := buildAutomaticWorkerRuntime
	t.Cleanup(func() {
		workerEffectiveUID = originalUID
		buildAutomaticWorkerRuntime = originalBuild
	})
	workerEffectiveUID = func() int { return 0 }
	buildAutomaticWorkerRuntime = func(bootstrap.Bootstrap, string) (automaticWorkerRuntime, error) {
		t.Fatal("root worker reached runtime construction")
		return automaticWorkerRuntime{}, nil
	}
	if err := controllerWorker(nil); err == nil || err.Error() != "automatic admission worker must not run as root" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWorkerDispatchLeavesIntegrityMaintenanceToBoundedBoundary(t *testing.T) {
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fallbackCalled := false
	dispatch := onboardingWorkerDispatch(store, nil, func(context.Context) (application.LinearTodoDispatchResult, error) {
		fallbackCalled = true
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate, ScanDigest: strings.Repeat("a", 64)}, nil
	})
	result, err := dispatch(context.Background())
	if err != nil || !fallbackCalled || result.Outcome != application.LinearTodoDispatchNoCandidate {
		t.Fatalf("result=%+v fallback=%t err=%v", result, fallbackCalled, err)
	}
	next, err := store.RunIntegrityMaintenance(context.Background(), "test-probe", time.Now().UTC())
	if err != nil || next.Family != application.IntegrityStorageSchema {
		t.Fatalf("dispatch unexpectedly advanced integrity maintenance: next=%+v err=%v", next, err)
	}
}

type countingOnboardingContinuer struct {
	calls  int
	id     string
	status domain.OnboardingStatus
}

type workerOnboardingPrivateStore struct {
	input domain.RepositoryOnboardingInput
}

func (s workerOnboardingPrivateStore) Put(string, domain.RepositoryOnboardingInput, string) error {
	return nil
}

func (s workerOnboardingPrivateStore) Get(string, string) (domain.RepositoryOnboardingInput, error) {
	return s.input, nil
}

type workerOnboardingPaths struct{}

func (workerOnboardingPaths) DeriveManagedSource(string) (string, error) { return "/fixture", nil }

type workerOnboardingPreflight struct{}

func (workerOnboardingPreflight) ObserveOnboardingPreflight(context.Context, domain.RepositoryOnboardingInput, application.ConfigurationAdmissionAuthority) (application.OnboardingPreflightEvidence, error) {
	return application.OnboardingPreflightEvidence{}, nil
}

type workerOnboardingExecutor struct {
	outcomes []application.OperationOutcome
	calls    int
}

func (e *workerOnboardingExecutor) ExecuteOnboardingStep(_ context.Context, _ application.Onboarding, _ domain.RepositoryOnboardingInput, _ domain.OnboardingStep) (application.OnboardingStepObservation, error) {
	outcome := e.outcomes[e.calls]
	e.calls++
	return application.OnboardingStepObservation{Outcome: outcome, ReasonCode: "fixture_external_observation", EvidenceDigest: application.ConfigurationEvidenceDigest("worker-onboarding-attempt", strconv.Itoa(e.calls))}, nil
}

func (c *countingOnboardingContinuer) Continue(_ context.Context, onboardingID string) (application.Onboarding, error) {
	c.calls++
	if onboardingID != c.id {
		return application.Onboarding{}, errors.New("unexpected onboarding identity")
	}
	status := c.status
	if status == "" {
		status = domain.OnboardingRunning
	}
	return application.Onboarding{OnboardingID: onboardingID, Status: status}, nil
}

func TestDisabledWorkerSerializesRunnableOnboardingWithoutAdmissionFallback(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.db")
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)
	digest := func(value string) string { return strings.Repeat(value, 64) }
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	baseline := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: digest("a"), Size: 100, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator}, CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"), ObservedAt: now}
	if err := store.PrepareConfigurationBaseline(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	authority, _, err := store.AdoptConfigurationBaseline(ctx, baseline)
	if err != nil {
		t.Fatal(err)
	}
	authority, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, WorkerInstanceID: "fixture-worker", BuildIdentity: "fixture-build", ObservedAt: now.Add(time.Second), EvidenceDigest: digest("b")})
	if err != nil {
		t.Fatal(err)
	}
	onboardingID := "onboarding-disabled-worker"
	opened, _, err := store.OpenOnboarding(ctx, application.OnboardingOpenInput{OnboardingID: onboardingID, Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/new", Requester: operator, PrivateInputDigest: digest("c"), SourcePathDigest: digest("d"), SourceAncestorDigests: []string{digest("d")}, RequestDigest: digest("e"), ConfigurationBaseGenerationID: authority.Desired.GenerationID, ConfigurationBaseDigest: authority.Desired.Digest, ConfigurationAuthorityVersion: authority.Version, OpenedAt: now.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: onboardingID, ExpectedStatus: opened.Status, PreflightDigest: digest("f"), EvidenceDigest: digest("1"), ObservedAt: now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: onboardingID, Requester: operator, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: authority.Desired.Digest, OperationAnchorDigest: digest("2"), TargetBindingDigest: digest("3"), AcceptedAt: now.Add(4 * time.Second)})
	profile := application.LocalRepository{CanonicalRepository: "owner/new", ProfileID: "profile-new", ProfileDigest: digest("4"), RepositoryBindingDigest: digest("5")}
	if _, _, _, err := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: onboardingID, Expected: ready, PreflightDigest: ready.PreflightDigest, PreviewDigest: digest("6"), Profile: profile, Receipt: receipt, AcceptedAt: receipt.AcceptedAt}); err != nil {
		t.Fatal(err)
	}

	configured := bootstrap.LinearTodoAdmission{Enabled: false, HeavyCapacity: application.MaxHeavyCapacity}
	if capacity := automaticWorkerCapacity(configured, false); capacity != 1 {
		t.Fatalf("disabled capacity=%d want=1", capacity)
	}
	continuer := &countingOnboardingContinuer{id: onboardingID}
	fallbackCalls := 0
	dispatch := onboardingWorkerDispatch(store, continuer, func(context.Context) (application.LinearTodoDispatchResult, error) {
		fallbackCalls++
		return application.LinearTodoDispatchResult{}, errors.New("normal admission fallback was called")
	})
	result, err := runBoundedAdmissionWorkerAtObserved(ctx, true, time.Minute, automaticWorkerCapacity(configured, false), dispatch, waitAdmissionWorker, func() time.Time { return now.Add(5 * time.Second) }, nil)
	if err != nil || result.Stopped != "once" || result.LastOutcome != application.RuntimeCycleOnboardingRunning || continuer.calls != 1 || fallbackCalls != 0 {
		t.Fatalf("result=%+v onboarding_calls=%d fallback_calls=%d err=%v", result, continuer.calls, fallbackCalls, err)
	}
	continuer.status = domain.OnboardingOpened
	if _, err := dispatch(ctx); err == nil || err.Error() != "onboarding worker returned an invalid status" || fallbackCalls != 0 {
		t.Fatalf("invalid status err=%v fallback_calls=%d", err, fallbackCalls)
	}
}

func TestDisabledZeroRepositoryWorkerAdoptsBaselineAndPublishesHeartbeatWithoutAdmissionEffects(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, databasePath := writeCurrentManagedDraftConfig(t, root)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configuredLoaded, err := bootstrap.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configuredProfiles, err := configuredLoaded.Registry.ListRepositoryProfiles(context.Background())
	if err != nil || len(configuredProfiles) != 1 {
		t.Fatalf("configured profiles=%d err=%v", len(configuredProfiles), err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config["repositories"] = []any{}
	rewritten, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IFAN_LOOP_LINEAR_TOKEN", "")
	loaded, err := loadManagedConfiguration(configPath)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := loaded.Registry.ListRepositoryProfiles(context.Background())
	if err != nil || len(profiles) != 0 || loaded.Automation.LinearTodoAdmission.Enabled {
		t.Fatalf("loaded repositories=%d enabled=%t err=%v", len(profiles), loaded.Automation.LinearTodoAdmission.Enabled, err)
	}
	runtime, err := newAutomaticWorkerRuntime(loaded, "zero-repository-worker")
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = runtime.store.Close()
		}
	}()
	for cycle := 0; cycle < 2; cycle++ {
		result, err := runtime.dispatch(context.Background())
		if err != nil || result.Outcome != application.LinearTodoDispatchNoCandidate {
			t.Fatalf("cycle=%d result=%+v err=%v", cycle, result, err)
		}
	}
	reporter, err := newWorkerStatusReporter(configPath, "zero-repository-worker", version, loaded.Digest)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runBoundedAdmissionWorkerWithHeartbeatAndMaintenance(context.Background(), true, time.Minute, automaticWorkerCapacity(loaded.Automation.LinearTodoAdmission, false), runtime.dispatch, runtime.maintenance, reporter, nil)
	if err != nil || result.Stopped != "once" || result.LastOutcome != application.LinearTodoDispatchNoCandidate {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	heartbeat, state := readWorkerStatusEvidence(configPath, os.Getuid())
	if state != application.RuntimeHeartbeatCurrent || heartbeat.SchemaVersion != workerStatusSchemaVersion || heartbeat.ConfigurationDigest != loaded.Digest || heartbeat.LastCycleOutcome != application.LinearTodoDispatchNoCandidate {
		t.Fatalf("heartbeat=%+v state=%s", heartbeat, state)
	}
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled, err := runBoundedAdmissionWorkerWithHeartbeatAndMaintenance(cancelledContext, false, time.Minute, automaticWorkerCapacity(loaded.Automation.LinearTodoAdmission, false), runtime.dispatch, runtime.maintenance, reporter, nil)
	if err != nil || cancelled.Stopped != "canceled" {
		t.Fatalf("cancelled=%+v err=%v", cancelled, err)
	}
	convergence, err := configuredConvergenceService(runtime.store, loaded, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convergence.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	decision, err := convergence.CheckNewAdmissionReadOnly(context.Background())
	if err != nil || !decision.Allowed || decision.Authority.Digest != loaded.Digest {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	operator := loaded.Controller.Operator
	onboardingID := "onboarding-zero-repository-bridge"
	digest := func(value string) string {
		return application.ConfigurationEvidenceDigest("zero-repository-bridge", value)
	}
	opened, created, err := runtime.store.OpenOnboarding(ctx, application.OnboardingOpenInput{OnboardingID: onboardingID, Kind: domain.OnboardingExistingCheckout, CanonicalRepository: configuredProfiles[0].Authority.Repository, Requester: operator, PrivateInputDigest: digest("private"), SourcePathDigest: digest("source"), SourceAncestorDigests: []string{digest("source")}, RequestDigest: digest("request"), ConfigurationBaseGenerationID: decision.Authority.GenerationID, ConfigurationBaseDigest: decision.Authority.Digest, ConfigurationAuthorityVersion: decision.Authority.AuthorityVersion, OpenedAt: now})
	if err != nil || !created {
		t.Fatalf("opened=%+v created=%t err=%v", opened, created, err)
	}
	ready, err := runtime.store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: onboardingID, ExpectedStatus: domain.OnboardingOpened, PreflightDigest: digest("preflight"), EvidenceDigest: digest("preflight-evidence"), ObservedAt: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	startReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: onboardingID, Requester: operator, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: opened.ConfigurationBaseDigest, OperationAnchorDigest: digest("start-anchor"), TargetBindingDigest: digest("start-binding"), AcceptedAt: now.Add(2 * time.Second)})
	if _, _, _, err := runtime.store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: onboardingID, Expected: ready, PreflightDigest: ready.PreflightDigest, PreviewDigest: digest("preview"), Profile: application.LocalRepository{CanonicalRepository: opened.CanonicalRepository}, Receipt: startReceipt, AcceptedAt: startReceipt.AcceptedAt}); err != nil {
		t.Fatal(err)
	}
	for index, step := range []domain.OnboardingStep{domain.OnboardingStepRootsCreated, domain.OnboardingStepLinearLabelObserved} {
		at := now.Add(time.Duration(3+index*2) * time.Second)
		if _, err := runtime.store.BeginOnboardingStep(ctx, application.OnboardingStepIntent{OnboardingID: onboardingID, Step: step, IntentDigest: digest("intent-" + string(step)), IntendedAt: at}); err != nil {
			t.Fatal(err)
		}
		observation := application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "fixture_ready", EvidenceDigest: digest("observed-" + string(step))}
		if step == domain.OnboardingStepLinearLabelObserved {
			observation.LinearLabelID = "label-zero-repository"
		}
		if _, err := runtime.store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: onboardingID, Step: step, Observation: observation, ObservedAt: at.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.store.BeginOnboardingStep(ctx, application.OnboardingStepIntent{OnboardingID: onboardingID, Step: domain.OnboardingStepConfigurationApplied, IntentDigest: digest("intent-configuration"), IntendedAt: now.Add(7 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	apply, err := convergence.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: decision.Authority.GenerationID, ExpectedDigest: decision.Authority.Digest, ExpectedAuthorityVersion: decision.Authority.AuthorityVersion, Payload: raw, Provenance: application.ConfigurationApplyProvenance{Kind: application.ConfigurationApplyOnboarding, OnboardingSourceID: onboardingID, OnboardingSourceDigest: opened.RequestDigest}})
	if err != nil {
		t.Fatal(err)
	}
	profile := configuredProfiles[0]
	configurationEvidence := application.ConfigurationEvidenceDigest("onboarding-configuration-v1", onboardingID, apply.Generation.Digest, strconv.FormatInt(apply.Generation.GenerationID, 10), apply.Receipt.EvidenceDigest)
	if _, err := runtime.store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: onboardingID, Step: domain.OnboardingStepConfigurationApplied, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "configuration_applied", EvidenceDigest: configurationEvidence, ProfileID: profile.Authority.ProfileID, ProfileDigest: profile.Profile.ProfileDigest, RepositoryBindingDigest: profile.Authority.BindingDigest, ConfigurationGenerationID: apply.Generation.GenerationID}, ObservedAt: now.Add(8 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.store.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	updated, err := loadManagedConfiguration(configPath)
	if err != nil || updated.Digest != apply.Generation.Digest {
		t.Fatalf("updated digest=%s generation=%s err=%v", updated.Digest, apply.Generation.Digest, err)
	}
	bridgedStore, err := openManagedConfigurationStore(updated)
	if err != nil {
		t.Fatalf("managed bridge reopen failed: %v", err)
	}
	if err := bridgedStore.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var baselineCount, repositoryCount, snapshotCount, reservationCount, runCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM repository_lifecycle_baseline WHERE authority_id=1 AND repository_count=0`).Scan(&baselineCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM repository_lifecycles`).Scan(&repositoryCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM repository_readiness_snapshots`).Scan(&snapshotCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM linear_todo_admission_journal`).Scan(&reservationCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runCount); err != nil || baselineCount != 1 || repositoryCount != 0 || snapshotCount != 0 || reservationCount != 0 || runCount != 0 {
		t.Fatalf("baseline=%d repositories=%d snapshots=%d reservations=%d runs=%d err=%v", baselineCount, repositoryCount, snapshotCount, reservationCount, runCount, err)
	}
}

func TestWorkerDispatchParksUnavailableOnboardingRetryAndContinuesAfterResume(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "controller.db")
	store, err := sqlitestore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 31, 3, 0, 0, 0, time.UTC)
	digest := func(value string) string { return strings.Repeat(value, 64) }
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	baseline := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: digest("d"), Size: 100, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator}, CanonicalConfigPath: filepath.Join(root, "controller.json"), ObservedAt: now}
	if err := store.PrepareConfigurationBaseline(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	authority, _, err := store.AdoptConfigurationBaseline(ctx, baseline)
	if err != nil {
		t.Fatal(err)
	}
	onboardingID := "onboarding-worker-retry"
	opened, _, err := store.OpenOnboarding(ctx, application.OnboardingOpenInput{OnboardingID: onboardingID, Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/retry", Requester: operator, PrivateInputDigest: digest("a"), SourcePathDigest: digest("b"), SourceAncestorDigests: []string{digest("b")}, RequestDigest: digest("c"), ConfigurationBaseGenerationID: authority.Desired.GenerationID, ConfigurationBaseDigest: authority.Desired.Digest, ConfigurationAuthorityVersion: authority.Version, OpenedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: onboardingID, ExpectedStatus: opened.Status, PreflightDigest: digest("e"), EvidenceDigest: digest("f"), ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: onboardingID, Requester: operator, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: opened.ConfigurationBaseDigest, OperationAnchorDigest: digest("1"), TargetBindingDigest: digest("2"), AcceptedAt: now})
	if _, _, _, err := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: onboardingID, Expected: ready, PreflightDigest: ready.PreflightDigest, PreviewDigest: digest("3"), Profile: application.LocalRepository{CanonicalRepository: opened.CanonicalRepository}, Receipt: receipt, AcceptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	executor := &workerOnboardingExecutor{outcomes: []application.OperationOutcome{application.OperationOutcomePending, application.OperationOutcomeSucceeded, application.OperationOutcomePending}}
	input := domain.ExistingRepositoryOnboardingInput(domain.ExistingCheckoutOnboardingInput{SourcePath: "/fixture", CanonicalRepository: opened.CanonicalRepository, GitHubAppProfileRef: "fixture", BaseBranch: "main", VerifierIDs: []string{"fixture"}, LinearLabelSlug: "retry"})
	service, err := application.NewOnboardingService(store, workerOnboardingPrivateStore{input: input}, workerOnboardingPaths{}, authorizer, new(application.ConfigurationService), workerOnboardingPreflight{}, executor)
	if err != nil {
		t.Fatal(err)
	}
	fallbackCalls := 0
	dispatch := onboardingWorkerDispatch(store, service, func(context.Context) (application.LinearTodoDispatchResult, error) {
		fallbackCalls++
		return application.LinearTodoDispatchResult{}, errors.New("normal admission fallback was called")
	})
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	reporter, err := newWorkerStatusReporter(filepath.Join(root, "controller.json"), "worker-onboarding-retry", version, digest("7"))
	if err != nil {
		t.Fatal(err)
	}
	clock := now.Add(20 * time.Minute)
	reporter.now = func() time.Time { return clock }
	runtimeService, err := application.NewRuntimeObservationService(workerHeartbeatReader{configPath: filepath.Join(root, "controller.json"), expectedUID: os.Getuid()}, workerProcessIdentityObserver{}, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	assertHeartbeat := func(cycles int) {
		t.Helper()
		snapshot, state := readWorkerStatusEvidence(filepath.Join(root, "controller.json"), os.Getuid())
		if state != application.RuntimeHeartbeatCurrent || snapshot.Cycles != cycles || snapshot.Status != workerStatusParked || snapshot.LastCycleOutcome != application.RuntimeCycleOnboardingWaitingForOperator || snapshot.LastCycleCompletedAt.IsZero() {
			t.Fatalf("snapshot=%+v state=%s", snapshot, state)
		}
		observation, observeErr := runtimeService.Observe(context.Background(), requester, clock)
		if observeErr != nil || observation.Liveness != application.RuntimeLivenessFresh || observation.Activity != application.RuntimeActivityParked || observation.AdmissionCadence.State != application.RuntimeAdmissionCadenceKnown || observation.AdmissionCadence.LastCycleOutcome != application.RuntimeCycleOnboardingWaitingForOperator {
			t.Fatalf("observation=%+v err=%v", observation, observeErr)
		}
	}
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	waits := 0
	result, err := runBoundedAdmissionWorkerAtObserved(workerCtx, false, time.Minute, 1, dispatch, func(context.Context, time.Duration) error {
		waits++
		assertHeartbeat(waits)
		if waits == 1 {
			resumed, resumeErr := service.Resume(ctx, application.OnboardingCommand{Requester: requester, OnboardingID: onboardingID})
			if resumeErr != nil || resumed.Status != domain.OnboardingRunning {
				t.Fatalf("resumed=%+v err=%v", resumed, resumeErr)
			}
			clock = clock.Add(time.Second)
			return nil
		}
		cancelWorker()
		return context.Canceled
	}, func() time.Time { return clock }, reporter.Observe)
	if err != nil || result.Stopped != "canceled" || result.Cycles != 2 || result.LastOutcome != application.RuntimeCycleOnboardingWaitingForOperator || result.PreviousStatus != workerStatusParked || executor.calls != 3 || fallbackCalls != 0 || waits != 2 {
		t.Fatalf("result=%+v waits=%d executor_calls=%d fallback_calls=%d err=%v", result, waits, executor.calls, fallbackCalls, err)
	}
	final, state := readWorkerStatusEvidence(filepath.Join(root, "controller.json"), os.Getuid())
	if state != application.RuntimeHeartbeatCurrent || final.Status != workerStatusStopping || final.PreviousStatus != workerStatusParked || final.LastCycleOutcome != application.RuntimeCycleOnboardingWaitingForOperator {
		t.Fatalf("final=%+v state=%s", final, state)
	}
	configured, err := authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := authorizer.ControllerScopes(configured)
	if err != nil {
		t.Fatal(err)
	}
	var events int
	if err := queryWorkerOnboardingRetryEvidence(store, onboardingID, scopes, &events); err != nil || events != 3 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	workerActivity, err := store.ListActivity(ctx, application.ActivityStoreQuery{Scopes: scopes, Filter: application.ActivityFilter{Category: application.ActivityWorker, Scope: application.ScopeController}, Limit: 10})
	expectedWorkerEvidence := application.ConfigurationEvidenceDigest("worker-readiness-v2", "automatic-worker", string(application.RuntimeActivityParked), authority.Desired.Digest, onboardingID, application.RuntimeCycleOnboardingWaitingForOperator)
	conflictWorkerEvidence := application.ConfigurationEvidenceDigest("worker-readiness-v2", "automatic-worker", string(application.RuntimeActivityParked), authority.Desired.Digest, onboardingID, application.RuntimeCycleOnboardingConflict)
	if expectedWorkerEvidence == conflictWorkerEvidence {
		t.Fatal("distinct parked onboarding outcomes share runtime evidence")
	}
	if err != nil || len(workerActivity.Events) != 1 {
		t.Fatalf("worker activity=%+v err=%v", workerActivity, err)
	}
	workerEvent := workerActivity.Events[0]
	if workerEvent.ResultingState != string(application.RuntimeActivityParked) || workerEvent.SourceEvidenceDigest != expectedWorkerEvidence || workerEvent.TargetBindingDigest != authority.Desired.Digest || len(workerEvent.EvidenceDigests) != 1 || workerEvent.EvidenceDigests[0] != expectedWorkerEvidence {
		t.Fatalf("worker event=%+v expected_evidence=%s", workerEvent, expectedWorkerEvidence)
	}
}

func queryWorkerOnboardingRetryEvidence(store *sqlitestore.Store, onboardingID string, scopes application.AuthorizedScopeSet, events *int) error {
	// Public worker tests intentionally avoid SQLite internals. The persisted
	// projection and activity query independently prove the retry stayed normal.
	value, found, err := store.Onboarding(context.Background(), onboardingID)
	if err != nil || !found || len(value.CompletedSteps) != 1 {
		return errors.New("onboarding retry projection is unavailable")
	}
	page, err := store.ListActivity(context.Background(), application.ActivityStoreQuery{Scopes: scopes, Filter: application.ActivityFilter{Scope: application.ScopeOnboarding, TargetID: onboardingID}, Limit: 10})
	if err != nil {
		return err
	}
	*events = len(page.Events)
	return nil
}

func TestWorkerProcessLockRejectsConcurrentWorkerAndRecoversAfterClose(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := acquireWorkerProcessLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireWorkerProcessLock(directory); err == nil || err.Error() != "automatic admission worker is already running" {
		t.Fatalf("unexpected concurrent lock error: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireWorkerProcessLock(directory)
	if err != nil {
		t.Fatalf("lock did not recover after close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundWorkerLogStreamTruncatesOnlyPrivateRegularFileAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := boundWorkerLogStream(file, 64); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("size=%d err=%v", info.Size(), err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := boundWorkerLogStream(file, 64); err == nil {
		t.Fatal("public worker log was accepted")
	}
}

func TestAutomaticWorkerKeepsAdmissionAndDeliveryCadencesIndependent(t *testing.T) {
	configured := bootstrap.LinearTodoAdmission{PollInterval: 5 * time.Minute, DeliveryPollInterval: 30 * time.Second}
	policy := automaticWorkerDriverPolicy(configured, "fixture-owner")
	if configured.PollInterval != 5*time.Minute || policy.PollInterval != 30*time.Second || policy.MaxImmediateAction != 32 || policy.HeavyPermitOwner != "fixture-owner" {
		t.Fatalf("configured=%+v policy=%+v", configured, policy)
	}
}

func TestControllerWorkerSubprocessSIGTERMClosesCompleteRuntime(t *testing.T) {
	root := resolvedTempDir(t)
	configPath, dbPath := writeControllerStatusConfig(t, root)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	registryPath, _ := config["repository_registry_file"].(string)
	registryRaw, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry map[string]any
	if err := json.Unmarshal(registryRaw, &registry); err != nil {
		t.Fatal(err)
	}
	config["repositories"] = registry["repositories"]
	delete(config, "repository_registry_file")
	config["version"] = 3
	config["automation"] = map[string]any{"linear_todo_admission": map[string]any{
		"enabled": true, "team_id": "123e4567-e89b-42d3-a456-426614174100", "team_key": "IFAN",
		"todo_state":        map[string]any{"id": offlineAdmissionTodoState.ID, "name": offlineAdmissionTodoState.Name, "type": offlineAdmissionTodoState.Type},
		"in_progress_state": map[string]any{"id": offlineAdmissionInProgressState.ID, "name": offlineAdmissionInProgressState.Name, "type": offlineAdmissionInProgressState.Type},
		"poll_interval":     "1m", "delivery_poll_interval": "30s", "scheduler_lease_ttl": "1m", "scheduler_lease_renewal_interval": "20s",
		"max_candidates": 10, "max_pages": 1, "max_active_runs": 1,
		"requester":         map[string]any{"database_id": 33, "node_id": "MDQ6VXNlcjMz", "login": "ifan0927", "type": "User"},
		"notification_mode": "local_outbox", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN",
	}}
	rewritten, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "managed-child.pid")
	closeMarker := filepath.Join(root, "worker-store.closed")
	stdoutPath, stderrPath := filepath.Join(root, "worker.stdout.log"), filepath.Join(root, "worker.stderr.log")
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		stdout.Close()
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestControllerWorkerSubprocessHelper$")
	command.Env = append(os.Environ(), "IFAN_WORKER_SUBPROCESS=1", "IFAN_WORKER_CONFIG="+configPath, "IFAN_WORKER_CHILD_MARKER="+marker, "IFAN_WORKER_CLOSE_MARKER="+closeMarker, "IFAN_WORKER_ROOT="+root)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		stdout.Close()
		stderr.Close()
		t.Fatal(err)
	}
	// Race-instrumented subprocess startup can exceed five seconds on hosted
	// runners while the full package suite is contending for CPU.
	deadline := time.Now().Add(15 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(marker)
		if readErr == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID < 1 {
		_ = command.Process.Kill()
		_ = command.Wait()
		stdout.Close()
		stderr.Close()
		workerOutput, _ := os.ReadFile(stdoutPath)
		workerError, _ := os.ReadFile(stderrPath)
		t.Fatalf("managed child did not start stdout=%s stderr=%s", workerOutput, workerError)
	}
	liveStatus, err := readWorkerStatusSnapshot(configPath)
	// The bounded supervisor may complete admission-coordination cycles while a
	// sibling continues driving the managed process. This fixture only requires
	// evidence that work started before SIGTERM; the exact cycle count is not a
	// shutdown invariant.
	if err != nil || liveStatus.Cycles < 1 || liveStatus.Status != workerStatusDriving && liveStatus.Status != workerStatusParked {
		t.Fatalf("live worker status=%+v err=%v", liveStatus, err)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("worker subprocess exit=%v", err)
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("worker subprocess did not stop within bound")
	}
	stdout.Close()
	stderr.Close()
	output, err := os.ReadFile(stdoutPath)
	if err != nil || !strings.Contains(string(output), `"stopped": "canceled"`) || strings.Contains(string(output), "failed") || strings.Contains(string(output), "abandoned") {
		t.Fatalf("terminal output=%s err=%v", output, err)
	}
	if closed, err := os.ReadFile(closeMarker); err != nil || string(closed) != "closed" {
		t.Fatalf("explicit SQLite close marker=%q err=%v", closed, err)
	}
	stoppedStatus, err := readWorkerStatusSnapshot(configPath)
	if err != nil || stoppedStatus.Status != workerStatusStopping || stoppedStatus.PreviousStatus != workerStatusDriving && stoppedStatus.PreviousStatus != workerStatusParked || stoppedStatus.WorkerInstanceID != liveStatus.WorkerInstanceID {
		t.Fatalf("stopped worker status=%+v live=%+v err=%v", stoppedStatus, liveStatus, err)
	}
	status, statusErr := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(childPID)).Output()
	trimmedStatus := strings.TrimSpace(string(status))
	if statusErr == nil && trimmedStatus != "" && !strings.HasPrefix(trimmedStatus, "Z") {
		t.Fatalf("managed child pid=%d remains runnable with status=%q", childPID, trimmedStatus)
	}
	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runs, err := store.ListNonterminalRuns(context.Background())
	if err != nil || len(runs) != 1 || runs[0].State != domain.StateAwaitingHumanDecision {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), "restart-owner", time.Minute, time.Now().UTC())
	if err != nil || !acquired {
		t.Fatalf("replacement lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	_, _ = store.ReleaseLinearTodoAdmissionLease(context.Background(), lease)
	fixtureevidence.Emit(t, fixtureevidence.Evidence{
		Scenario:               "indefinite_restart",
		RunIDs:                 []string{runs[0].ID},
		IssueIdentifiers:       []string{runs[0].IssueID},
		StateSequence:          []string{"driving", "sigterm", "awaiting_human_decision", "restart_ready"},
		LeaseEvidence:          []string{"released_on_shutdown", "replacement_acquired"},
		ExactCandidateBindings: []string{"same_persisted_run"},
		FinalWorkerState:       "stopped",
	})
}

func TestControllerWorkerSubprocessHelper(t *testing.T) {
	if os.Getenv("IFAN_WORKER_SUBPROCESS") != "1" {
		return
	}
	closeMarker := os.Getenv("IFAN_WORKER_CLOSE_MARKER")
	observeAutomaticWorkerStoreClosed = func() {
		if err := os.WriteFile(closeMarker, []byte("closed"), 0o600); err != nil {
			panic(err)
		}
	}
	emitAutomaticWorkerOutput = func(output workerOutput) error {
		if closed, err := os.ReadFile(closeMarker); err != nil || string(closed) != "closed" {
			return errors.New("worker terminal output preceded SQLite close")
		}
		return printJSON(output)
	}
	buildAutomaticWorkerRuntime = func(loaded bootstrap.Bootstrap, instanceID string) (automaticWorkerRuntime, error) {
		store, err := sqlitestore.Open(loaded.Controller.DatabasePath)
		if err != nil {
			return automaticWorkerRuntime{}, err
		}
		repository := offlineAdmissionRepository(t)
		candidate := offlineAdmissionCandidate()
		reader := newOfflineAdmissionReader(offlineAdmissionSource(candidate))
		scanner := &offlineAdmissionScanner{scan: application.LinearTodoCandidateScan{Candidates: []application.LinearTodoCandidate{candidate}, Digest: offlineAdmissionDigest("subprocess-scan"), ObservedAt: candidate.UpdatedAt}}
		starter := &offlineAdmissionStarter{reader: reader}
		controller := application.NewLocalController(store, &offlineAdmissionWorktrees{}, &offlineAdmissionCodex{}, offlineAdmissionVerifier{}, offlineAdmissionGit{}, "fixture-codex", repository.WorktreeRoot)
		driver := workerManagedProcessDriver{root: os.Getenv("IFAN_WORKER_ROOT"), marker: os.Getenv("IFAN_WORKER_CHILD_MARKER")}
		dispatcher, err := newOfflineAdmissionDispatcher(scanner, reader, starter, store, controller, driver, repository, instanceID)
		if err != nil {
			store.Close()
			return automaticWorkerRuntime{}, err
		}
		return automaticWorkerRuntime{store: store, dispatch: dispatcher.Dispatch, maintenance: integrityWorkerMaintenance(store)}, nil
	}
	if err := controllerWorker([]string{"--config", os.Getenv("IFAN_WORKER_CONFIG")}); err != nil {
		t.Fatal(err)
	}
}

type workerManagedProcessDriver struct{ root, marker string }

func (d workerManagedProcessDriver) Drive(ctx context.Context, command application.ProductionDriveCommand) (application.ProductionDriveResult, error) {
	result, err := (processadapter.OSRunner{InterruptGrace: 100 * time.Millisecond}).Run(ctx, processadapter.Spec{
		Program: os.Args[0], Args: []string{"-test.run=^TestControllerWorkerManagedChild$"}, WorkingDir: d.root,
		StdoutPath: filepath.Join(d.root, "managed-child.stdout"), StderrPath: filepath.Join(d.root, "managed-child.stderr"),
		Environment: []string{"IFAN_WORKER_MANAGED_CHILD=1", "IFAN_WORKER_CHILD_MARKER=" + d.marker},
	})
	_ = result
	return application.ProductionDriveResult{Run: application.RunResult{RunID: command.RunID}}, err
}

func TestControllerWorkerManagedChild(t *testing.T) {
	if os.Getenv("IFAN_WORKER_MANAGED_CHILD") != "1" {
		return
	}
	signal.Ignore(syscall.SIGINT)
	if err := os.WriteFile(os.Getenv("IFAN_WORKER_CHILD_MARKER"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestCloseWorkerStateStoreClosesSQLiteBeforeTerminalOutput(t *testing.T) {
	store, err := sqlitestore.Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closeWorkerStateStore(store); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRun(context.Background(), "missing"); err == nil {
		t.Fatal("SQLite remained usable after worker close")
	}
}

func TestWorkerSIGTERMStopsActiveDispatchWithSanitizedTerminalStatus(t *testing.T) {
	ctx, stop := workerSignalContext()
	defer stop()
	done := make(chan admissionWorkerResult, 1)
	started := make(chan struct{})
	go func() {
		result, err := runAdmissionWorker(ctx, false, time.Minute, func(dispatchCtx context.Context) (application.LinearTodoDispatchResult, error) {
			close(started)
			<-dispatchCtx.Done()
			return application.LinearTodoDispatchResult{}, dispatchCtx.Err()
		}, waitAdmissionWorker)
		if err != nil {
			done <- admissionWorkerResult{Stopped: "unexpected_error"}
			return
		}
		done <- result
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter active dispatch")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Cycles != 1 || result.Stopped != "canceled" {
			t.Fatalf("result=%+v", result)
		}
		raw, err := json.Marshal(workerOutput{Cycles: result.Cycles, Stopped: result.Stopped})
		if err != nil || !strings.Contains(string(raw), `"stopped":"canceled"`) || strings.Contains(string(raw), "failed") || strings.Contains(string(raw), "abandoned") {
			t.Fatalf("terminal output=%s err=%v", raw, err)
		}
	case <-time.After(time.Second):
		t.Fatal("SIGTERM did not stop active worker dispatch")
	}
}
