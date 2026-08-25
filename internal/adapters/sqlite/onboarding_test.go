package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOnboardingSagaReplaysAndResumesAfterStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC)
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	digest := func(character string) string { return strings.Repeat(character, 64) }
	baseline := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: digest("d"), Size: 1, SchemaVersion: 5, DatabasePath: path, Operator: requester}, CanonicalConfigPath: path + ".json", ObservedAt: now.Add(-time.Second)}
	if err := store.PrepareConfigurationBaseline(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	configuration, _, err := store.AdoptConfigurationBaseline(ctx, baseline)
	if err != nil {
		t.Fatal(err)
	}
	configuration, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: configuration.Desired.GenerationID, ExpectedDigest: configuration.Desired.Digest, WorkerInstanceID: "onboarding-fixture-worker", BuildIdentity: "onboarding-fixture-build", ObservedAt: now, EvidenceDigest: digest("3")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{repositoryProfileFixture("owner/existing", "1", "2")}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	openInput := application.OnboardingOpenInput{OnboardingID: "onboarding-restart-safe", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/repository", Requester: requester, PrivateInputDigest: digest("a"), SourcePathDigest: digest("b"), SourceAncestorDigests: []string{digest("0"), digest("b")}, RequestDigest: digest("c"), ConfigurationBaseGenerationID: configuration.Desired.GenerationID, ConfigurationBaseDigest: configuration.Desired.Digest, ConfigurationAuthorityVersion: configuration.Version, OpenedAt: now}
	opened, created, err := store.OpenOnboarding(ctx, openInput)
	if err != nil || !created || opened.Status != domain.OnboardingOpened {
		t.Fatalf("opened=%+v created=%t err=%v", opened, created, err)
	}
	if replayed, changed, replayErr := store.OpenOnboarding(ctx, openInput); replayErr != nil || changed || replayed.OnboardingID != opened.OnboardingID {
		t.Fatalf("open replay=%+v changed=%t err=%v", replayed, changed, replayErr)
	}
	runtimeVersionReplay := openInput
	runtimeVersionReplay.ConfigurationAuthorityVersion++
	if replayed, changed, replayErr := store.OpenOnboarding(ctx, runtimeVersionReplay); replayErr != nil || changed || replayed.ConfigurationAuthorityVersion != openInput.ConfigurationAuthorityVersion {
		t.Fatalf("runtime-version replay=%+v changed=%t err=%v", replayed, changed, replayErr)
	}
	changedClaims := openInput
	changedClaims.SourceAncestorDigests = []string{digest("b")}
	if _, _, conflictErr := store.OpenOnboarding(ctx, changedClaims); !errors.Is(conflictErr, application.ErrOnboardingConflict) {
		t.Fatalf("changed path claims conflict=%v", conflictErr)
	}
	conflict := openInput
	conflict.OnboardingID = "onboarding-conflicting-source"
	conflict.RequestDigest = digest("e")
	if _, _, conflictErr := store.OpenOnboarding(ctx, conflict); !errors.Is(conflictErr, application.ErrOnboardingConflict) {
		t.Fatalf("duplicate source conflict=%v", conflictErr)
	}
	ancestorConflict := openInput
	ancestorConflict.OnboardingID = "onboarding-ancestor-conflict"
	ancestorConflict.CanonicalRepository = "owner/other"
	ancestorConflict.SourcePathDigest = digest("0")
	ancestorConflict.SourceAncestorDigests = []string{digest("0")}
	ancestorConflict.RequestDigest = digest("7")
	if _, _, conflictErr := store.OpenOnboarding(ctx, ancestorConflict); !errors.Is(conflictErr, application.ErrOnboardingConflict) {
		t.Fatalf("ancestor source conflict=%v", conflictErr)
	}
	preflight := digest("f")
	ready, err := store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: opened.OnboardingID, ExpectedStatus: domain.OnboardingOpened, PreflightDigest: preflight, EvidenceDigest: digest("1"), ObservedAt: now.Add(time.Second)})
	if err != nil || ready.Status != domain.OnboardingPreflightReady {
		t.Fatalf("preflight=%+v err=%v", ready, err)
	}
	profile := application.LocalRepository{CanonicalRepository: opened.CanonicalRepository}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: opened.OnboardingID, Requester: requester, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: opened.ConfigurationBaseDigest, OperationAnchorDigest: digest("2"), TargetBindingDigest: digest("3"), AcceptedAt: now.Add(2 * time.Second)})
	preview := digest("4")
	started, acceptedReceipt, changed, err := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: opened.OnboardingID, Expected: ready, PreflightDigest: preflight, PreviewDigest: preview, Profile: profile, Receipt: receipt, AcceptedAt: receipt.AcceptedAt})
	if err != nil || !changed || started.Status != domain.OnboardingAccepted || acceptedReceipt.OperationID != receipt.OperationID {
		t.Fatalf("started=%+v receipt=%+v changed=%t err=%v", started, acceptedReceipt, changed, err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO configuration_drafts DEFAULT VALUES`); err == nil || !strings.Contains(err.Error(), "configuration mutation is already active") {
		t.Fatalf("parallel configuration draft error=%v", err)
	}
	if replayed, replayReceipt, replayChanged, replayErr := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: opened.OnboardingID, Expected: ready, PreflightDigest: preflight, PreviewDigest: preview, Profile: profile, Receipt: receipt, AcceptedAt: receipt.AcceptedAt}); replayErr != nil || replayChanged || replayed.OperationID != receipt.OperationID || replayReceipt.OperationID != receipt.OperationID {
		t.Fatalf("start replay=%+v receipt=%+v changed=%t err=%v", replayed, replayReceipt, replayChanged, replayErr)
	}
	intent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepRootsCreated, IntentDigest: digest("5"), IntendedAt: now.Add(3 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, intent); err != nil || !changed {
		t.Fatalf("begin changed=%t err=%v", changed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runnable, err := store.ListRunnableOnboardings(ctx, 10)
	if err != nil || len(runnable) != 1 || runnable[0] != opened.OnboardingID {
		t.Fatalf("runnable=%v err=%v", runnable, err)
	}
	settled, err := store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepRootsCreated, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "roots_ready", EvidenceDigest: digest("6")}, ObservedAt: now.Add(4 * time.Second)})
	if err != nil || settled.Status != domain.OnboardingRunning || len(settled.CompletedSteps) != 1 || settled.CompletedSteps[0] != domain.OnboardingStepRootsCreated {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	labelIntent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepLinearLabelObserved, IntentDigest: digest("8"), IntendedAt: now.Add(5 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, labelIntent); err != nil || !changed {
		t.Fatalf("label begin changed=%t err=%v", changed, err)
	}
	settled, err = store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepLinearLabelObserved, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "linear_label_ready", EvidenceDigest: digest("9"), LinearLabelID: "label-immutable-1"}, ObservedAt: now.Add(6 * time.Second)})
	if err != nil || settled.LinearLabelID != "label-immutable-1" || len(settled.CompletedSteps) != 2 {
		t.Fatalf("label settled=%+v err=%v", settled, err)
	}
	profileAuthority := repositoryProfileFixture(opened.CanonicalRepository, "e", "f")
	configurationIntent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepConfigurationApplied, IntentDigest: digest("a"), IntendedAt: now.Add(7 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, configurationIntent); err != nil || !changed {
		t.Fatalf("configuration begin changed=%t err=%v", changed, err)
	}
	candidate := application.ValidatedConfigurationCandidate{Digest: digest("4"), Size: 2, SchemaVersion: 5, DatabasePath: path, Operator: requester}
	configurationReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: requester, RequestDigest: candidate.Digest, ExpectedAuthorityDigest: configuration.Desired.Digest, OperationAnchorDigest: digest("5"), TargetBindingDigest: digest("6"), AcceptedAt: now.Add(8 * time.Second)})
	apply := application.ConfigurationApplyAcceptance{ExpectedGenerationID: configuration.Desired.GenerationID, ExpectedDigest: configuration.Desired.Digest, Candidate: candidate, Requester: requester, Receipt: configurationReceipt, Provenance: application.ConfigurationApplyProvenance{Kind: application.ConfigurationApplyOnboarding, OnboardingSourceID: opened.OnboardingID, OnboardingSourceDigest: opened.RequestDigest}, AcceptedAt: configurationReceipt.AcceptedAt}
	normalReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: requester, RequestDigest: candidate.Digest, ExpectedAuthorityDigest: configuration.Desired.Digest, OperationAnchorDigest: digest("0"), TargetBindingDigest: digest("6"), AcceptedAt: now.Add(8 * time.Second)})
	normalApply := apply
	normalApply.Receipt, normalApply.Provenance = normalReceipt, application.ConfigurationApplyProvenance{Kind: application.ConfigurationApplyNormal}
	if _, _, _, err := store.BeginConfigurationApply(ctx, normalApply); !errors.Is(err, application.ErrConfigurationApplyInProgress) {
		t.Fatalf("parallel normal configuration apply error=%v", err)
	}
	generation, _, changed, err := store.BeginConfigurationApply(ctx, apply)
	if err != nil || !changed {
		t.Fatalf("configuration apply generation=%+v changed=%t err=%v", generation, changed, err)
	}
	if replayed, _, replayChanged, replayErr := store.BeginConfigurationApply(ctx, apply); replayErr != nil || replayChanged || replayed.GenerationID != generation.GenerationID {
		t.Fatalf("configuration replay=%+v changed=%t err=%v", replayed, replayChanged, replayErr)
	}
	configuration, _, _, err = store.SettleConfigurationApply(ctx, application.ConfigurationApplySettlement{GenerationID: generation.GenerationID, ParentID: generation.ParentID, OperationID: generation.OperationID, Outcome: application.ConfigurationApplyCommitted, Reason: application.ConfigurationReasonRestartRequired, EvidenceDigest: digest("7"), SettledAt: now.Add(9 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	configuration, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: generation.GenerationID, ExpectedDigest: generation.Digest, WorkerInstanceID: "replacement-worker", BuildIdentity: "replacement-build", ObservedAt: now.Add(10 * time.Second), EvidenceDigest: digest("8")})
	if err != nil {
		t.Fatal(err)
	}
	settled, err = store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepConfigurationApplied, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "configuration_applied", EvidenceDigest: digest("b"), ProfileID: profileAuthority.Profile.ProfileID, ProfileDigest: profileAuthority.Profile.ProfileDigest, RepositoryBindingDigest: profileAuthority.Profile.RepositoryBindingDigest, ConfigurationGenerationID: generation.GenerationID}, ObservedAt: now.Add(11 * time.Second)})
	if err != nil || settled.ConfigurationGenerationID != generation.GenerationID {
		t.Fatalf("configuration settled=%+v err=%v", settled, err)
	}
	convergenceIntent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepConfigurationConverged, IntentDigest: digest("c"), IntendedAt: now.Add(12 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, convergenceIntent); err != nil || !changed {
		t.Fatalf("convergence begin changed=%t err=%v", changed, err)
	}
	settled, err = store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepConfigurationConverged, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "configuration_converged", EvidenceDigest: digest("d")}, ObservedAt: now.Add(13 * time.Second)})
	if err != nil || len(settled.CompletedSteps) != 4 {
		t.Fatalf("convergence settled=%+v err=%v", settled, err)
	}
	lifecycleIntent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepLifecycleCreated, IntentDigest: digest("e"), IntendedAt: now.Add(14 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, lifecycleIntent); err != nil || !changed {
		t.Fatalf("lifecycle begin changed=%t err=%v", changed, err)
	}
	projection, created, err := store.CreateOnboardingRepositoryLifecycle(ctx, opened.OnboardingID, profileAuthority.Profile, now.Add(14*time.Second))
	if err != nil || !created || projection.Lifecycle.Intent != application.RepositoryDisabled || projection.Readiness.Status != domain.RepositoryUnknown || projection.Readiness.ReasonCode != "initial_recheck_required" {
		t.Fatalf("projection=%+v created=%t err=%v", projection, created, err)
	}
	if replayed, changed, replayErr := store.CreateOnboardingRepositoryLifecycle(ctx, opened.OnboardingID, profileAuthority.Profile, now.Add(14*time.Second)); replayErr != nil || changed || replayed.Lifecycle.IncarnationID != projection.Lifecycle.IncarnationID {
		t.Fatalf("lifecycle replay=%+v changed=%t err=%v", replayed, changed, replayErr)
	}
}

func TestOnboardingV37MigrationPreservesV36AndAddsReceiptScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 36)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.SchemaVersion(context.Background()); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var tables int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('repository_onboardings','repository_onboarding_path_claims','repository_onboarding_steps')`).Scan(&tables); err != nil || tables != 3 {
		t.Fatalf("tables=%d err=%v", tables, err)
	}
	now := time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC)
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: "onboarding-migrated", Requester: requester, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("c", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: now})
	if _, created, err := store.BeginOperationReceipt(context.Background(), receipt); err != nil || !created {
		t.Fatalf("onboarding receipt created=%t err=%v", created, err)
	}
}

func TestEmptyRepositoryOnboardingPersistsKindSpecificOrderAndInitialSHA(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 25, 5, 0, 0, 123456789, time.UTC)
	digest := func(character string) string { return strings.Repeat(character, 64) }
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	input := application.OnboardingOpenInput{OnboardingID: "onboarding-empty-order", Kind: domain.OnboardingEmptyRepository, CanonicalRepository: "owner/empty", Requester: requester, PrivateInputDigest: digest("a"), SourcePathDigest: digest("b"), SourceAncestorDigests: []string{digest("0"), digest("b")}, RequestDigest: digest("c"), ConfigurationBaseGenerationID: 1, ConfigurationBaseDigest: digest("d"), ConfigurationAuthorityVersion: 1, OpenedAt: now}
	opened, _, err := store.OpenOnboarding(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: opened.OnboardingID, ExpectedStatus: domain.OnboardingOpened, PreflightDigest: digest("e"), EvidenceDigest: digest("f"), ObservedAt: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: opened.OnboardingID, Requester: requester, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: opened.ConfigurationBaseDigest, OperationAnchorDigest: digest("1"), TargetBindingDigest: digest("2"), AcceptedAt: now.Add(2 * time.Second)})
	started, _, _, err := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: opened.OnboardingID, Expected: ready, PreflightDigest: ready.PreflightDigest, PreviewDigest: digest("3"), Profile: application.LocalRepository{CanonicalRepository: opened.CanonicalRepository}, Receipt: receipt, AcceptedAt: receipt.AcceptedAt})
	if err != nil || !started.AcceptedAt.Equal(receipt.AcceptedAt.UTC().Truncate(time.Second)) {
		t.Fatalf("started=%+v err=%v", started, err)
	}
	if _, err := store.BeginOnboardingStep(ctx, application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepLinearLabelObserved, IntentDigest: digest("4"), IntendedAt: now.Add(3 * time.Second)}); !errors.Is(err, application.ErrOnboardingConflict) {
		t.Fatalf("out-of-order step error=%v", err)
	}
	sha := strings.Repeat("1", 40)
	for index, step := range domain.EmptyRepositoryOnboardingSteps[:4] {
		if _, err := store.BeginOnboardingStep(ctx, application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: step, IntentDigest: digest(string(rune('5' + index))), IntendedAt: now.Add(time.Duration(3+index*2) * time.Second)}); err != nil {
			t.Fatalf("begin %s: %v", step, err)
		}
		observation := application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "step_ready", EvidenceDigest: digest(string(rune('a' + index)))}
		if step == domain.OnboardingStepInitialRevisionCreated {
			observation.InitialRevisionSHA = sha
		}
		started, err = store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: step, Observation: observation, ObservedAt: now.Add(time.Duration(4+index*2) * time.Second)})
		if err != nil {
			t.Fatalf("settle %s: %v", step, err)
		}
	}
	if started.InitialRevisionSHA != sha || !slices.Equal(started.CompletedSteps, domain.EmptyRepositoryOnboardingSteps[:4]) {
		t.Fatalf("onboarding=%+v", started)
	}
}

func TestOnboardingV38MigrationPreservesVersion37RowsExactly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 37)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	digest := func(character string) string { return strings.Repeat(character, 64) }
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: "onboarding-version-37", Requester: requester, RequestDigest: digest("a"), ExpectedAuthorityDigest: digest("b"), OperationAnchorDigest: digest("c"), TargetBindingDigest: digest("d"), AcceptedAt: now})
	if _, _, err := legacy.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	_, err = legacy.db.ExecContext(ctx, `INSERT INTO repository_onboardings(onboarding_id,onboarding_kind,canonical_repository,private_input_digest,source_path_digest,request_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,base_generation_id,base_digest,configuration_authority_version,status,step_index,preflight_digest,preview_digest,operation_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'running',1,?,?,?,?,?)`, "onboarding-version-37", "existing_checkout", "owner/legacy", digest("e"), digest("f"), receipt.RequestDigest, requester.Login, requester.DatabaseID, requester.NodeID, requester.ActorType, 1, receipt.ExpectedAuthorityDigest, 1, digest("1"), digest("2"), receipt.OperationID, formatTime(now.Add(-time.Hour)), formatTime(now.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.ExecContext(ctx, `INSERT INTO repository_onboarding_path_claims(onboarding_id,path_digest) VALUES(?,?)`, "onboarding-version-37", digest("f")); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.ExecContext(ctx, `INSERT INTO repository_onboarding_steps(onboarding_id,step_name,step_order,intent_digest,status,outcome,evidence_digest,intended_at,observed_at) VALUES(?,'roots_created',1,?,'observed','succeeded',?,?,?)`, "onboarding-version-37", digest("3"), digest("4"), formatTime(now.Add(2*time.Minute)), formatTime(now.Add(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	value, found, err := store.Onboarding(ctx, "onboarding-version-37")
	if err != nil || !found || value.Kind != domain.OnboardingExistingCheckout || value.InitialRevisionSHA != "" || !value.AcceptedAt.Equal(now) || !slices.Equal(value.CompletedSteps, []domain.OnboardingStep{domain.OnboardingStepRootsCreated}) || !value.CreatedAt.Equal(now.Add(-time.Hour)) || !value.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("value=%+v found=%t err=%v", value, found, err)
	}
}
