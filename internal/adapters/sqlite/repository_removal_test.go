package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRepositoryRemovalPersistsIntentWaitsForObservationAndNeverResurrects(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	baseDigest := strings.Repeat("a", 64)
	baseline := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: baseDigest, Size: 100, SchemaVersion: 5, DatabasePath: path, Operator: operator}, CanonicalConfigPath: path + ".json", ObservedAt: now}
	if err := store.PrepareConfigurationBaseline(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	configuration, _, err := store.AdoptConfigurationBaseline(ctx, baseline)
	if err != nil {
		t.Fatal(err)
	}
	configuration, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: configuration.Desired.GenerationID, ExpectedDigest: configuration.Desired.Digest, WorkerInstanceID: "worker-1", BuildIdentity: "build-1", ObservedAt: now.Add(time.Second), EvidenceDigest: strings.Repeat("e", 64)})
	if err != nil {
		t.Fatal(err)
	}

	profile := repositoryProfileFixture("owner/repo", "b", "c")
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	authority, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil {
		t.Fatal(err)
	}
	disable := repositoryReceiptFixture(application.OperationDisableRepository, authority, profile, now.Add(3*time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, disable); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: disable.OperationID, Expected: authority, Intent: application.RepositoryDisabled, ChangedAt: now.Add(4 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	authority, err = store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil {
		t.Fatal(err)
	}
	incarnationID := authority.Lifecycle.IncarnationID
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	configured, err := authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		t.Fatal(err)
	}
	draftID := "repository-removal-draft-0123456789abcdef0123456789abcdef"
	draft, created, err := store.OpenRepositoryRemovalDraft(ctx, application.RepositoryRemovalOpenInput{DraftID: draftID, Authority: authority, Profile: profile.Profile, RepositoryCount: 1, Requester: configured, OpenedAt: now.Add(5 * time.Second)})
	if err != nil || !created {
		t.Fatalf("draft=%+v created=%t err=%v", draft, created, err)
	}
	guards, err := store.EvaluateRepositoryRemovalGuards(ctx, authority, 1, now.Add(6*time.Second))
	expectedGuards := []string{"lifecycle_disabled", "no_active_onboarding", "no_nonterminal_run", "no_recheck", "no_repository_mutation", "no_repository_slot", "no_execution_lease", "no_heavy_permit", "no_scheduling_conflict", "cleanup_settled", "configuration_mutation_idle", "configuration_converged"}
	if err != nil || !allGuardsAllowed(guards) {
		t.Fatalf("guards=%+v err=%v", guards, err)
	}
	for _, name := range expectedGuards {
		if !hasRemovalGuard(guards, name) {
			t.Fatalf("missing allowed guard %s in %+v", name, guards)
		}
	}
	candidateDigest := strings.Repeat("d", 64)
	previewDigest := strings.Repeat("f", 64)
	validation := application.RepositoryRemovalValidation{DraftID: draftID, Revision: 1, CandidateDigest: candidateDigest, ValidationDigest: strings.Repeat("1", 64), Valid: true, Guards: guards, ValidatedAt: now.Add(7 * time.Second)}
	preview := application.RepositoryRemovalPreview{DraftID: draftID, Revision: 1, Repository: profile.Authority.Repository, IncarnationID: incarnationID, ProfileID: profile.Profile.ProfileID, RepositoryCountBefore: 1, RepositoryCountAfter: 0, BaseGenerationID: configuration.Desired.GenerationID, BaseDigest: baseDigest, ProposedConfigurationDigest: candidateDigest, LifecycleVersion: authority.Lifecycle.Version, ConfigurationAuthorityVersion: configuration.Version, WorkerRestartRequired: true, ExpectedState: application.RepositoryRemovalPending, PreservedResources: []string{"history"}, PreviewDigest: previewDigest, PreviewedAt: now.Add(8 * time.Second)}
	if _, err := store.RecordRepositoryRemovalMetadata(ctx, application.RepositoryRemovalMetadataInput{DraftID: draftID, ExpectedRevision: 1, Validation: validation, Preview: &preview, UpdatedAt: now.Add(8 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	removalReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRemoveRepository, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: operator, RequestDigest: strings.Repeat("2", 64), ExpectedAuthorityDigest: strings.Repeat("3", 64), OperationAnchorDigest: strings.Repeat("4", 64), TargetBindingDigest: strings.Repeat("5", 64), AcceptedAt: now.Add(9 * time.Second)})
	draft, receipt, accepted, err := store.AcceptRepositoryRemoval(ctx, application.RepositoryRemovalAcceptance{DraftID: draftID, ExpectedRevision: 1, Expected: authority, CandidateDigest: candidateDigest, PreviewDigest: previewDigest, Receipt: removalReceipt, AcceptedAt: removalReceipt.AcceptedAt})
	if err != nil || !accepted || draft.State != application.RepositoryRemovalDraftApplying || receipt.Phase != application.OperationPhaseAccepted {
		t.Fatalf("draft=%+v receipt=%+v accepted=%t err=%v", draft, receipt, accepted, err)
	}
	locked, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil || locked.Removal == nil || locked.Removal.State != "accepted" {
		t.Fatalf("locked=%+v err=%v", locked, err)
	}
	if decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile); err != nil || decision.Allowed {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}

	generation, configurationReceipt := beginRemovalConfigurationApply(t, ctx, store, configuration, operator, path, candidateDigest, now.Add(10*time.Second), "6")
	configuration, _, _, err = store.SettleConfigurationApply(ctx, application.ConfigurationApplySettlement{GenerationID: generation.GenerationID, ParentID: generation.ParentID, OperationID: generation.OperationID, Outcome: application.ConfigurationApplyCommitted, Reason: application.ConfigurationReasonRestartRequired, EvidenceDigest: strings.Repeat("7", 64), SettledAt: now.Add(11 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	draft, receipt, changed, err := store.RecordRepositoryRemovalApplied(ctx, application.RepositoryRemovalApplied{DraftID: draftID, RemovalOperationID: removalReceipt.OperationID, ConfigurationOperationID: configurationReceipt.OperationID, GenerationID: generation.GenerationID, Digest: candidateDigest, AppliedAt: now.Add(12 * time.Second)})
	if err != nil || !changed || draft.State != application.RepositoryRemovalDraftApplied || receipt.Phase != application.OperationPhaseApplied || receipt.Outcome != application.OperationOutcomePending {
		t.Fatalf("draft=%+v receipt=%+v changed=%t err=%v", draft, receipt, changed, err)
	}
	replayedDraft, replayedReceipt, replayed, err := store.RecordRepositoryRemovalApplied(ctx, application.RepositoryRemovalApplied{DraftID: draftID, RemovalOperationID: removalReceipt.OperationID, ConfigurationOperationID: configurationReceipt.OperationID, GenerationID: generation.GenerationID, Digest: candidateDigest, AppliedAt: now.Add(12 * time.Second)})
	if err != nil || replayed || replayedDraft.State != application.RepositoryRemovalDraftApplied || replayedReceipt.OperationID != receipt.OperationID {
		t.Fatalf("replayed draft=%+v receipt=%+v changed=%t err=%v", replayedDraft, replayedReceipt, replayed, err)
	}
	if _, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository); err != nil {
		t.Fatalf("pending removal disappeared before observation: %v", err)
	}

	configuration, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: generation.GenerationID, ExpectedDigest: candidateDigest, WorkerInstanceID: "worker-2", BuildIdentity: "build-2", ObservedAt: now.Add(13 * time.Second), EvidenceDigest: strings.Repeat("8", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository); !errors.Is(err, application.ErrRepositoryLifecycleMissing) {
		t.Fatalf("retired repository remained current: %v", err)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{AdoptedAt: now.Add(13 * time.Second)}); err != nil {
		t.Fatalf("return-to-zero baseline replay failed: %v", err)
	}
	draft, found, err := store.RepositoryRemovalDraft(ctx, draftID)
	if err != nil || !found || draft.ReasonCode != "retired" || draft.Receipt == nil || draft.Receipt.Phase != application.OperationPhaseObserved || draft.Receipt.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("draft=%+v found=%t err=%v", draft, found, err)
	}

	// A later configuration rollback can restore bytes, but cannot resurrect the
	// retired lifecycle incarnation.
	rollback, _ := beginRemovalConfigurationApply(t, ctx, store, configuration, operator, path, baseDigest, now.Add(14*time.Second), "9")
	if _, _, _, err := store.SettleConfigurationApply(ctx, application.ConfigurationApplySettlement{GenerationID: rollback.GenerationID, ParentID: rollback.ParentID, OperationID: rollback.OperationID, Outcome: application.ConfigurationApplyCommitted, Reason: application.ConfigurationReasonRestartRequired, EvidenceDigest: strings.Repeat("a", 64), SettledAt: now.Add(15 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: rollback.GenerationID, ExpectedDigest: baseDigest, WorkerInstanceID: "worker-3", BuildIdentity: "build-3", ObservedAt: now.Add(16 * time.Second), EvidenceDigest: strings.Repeat("b", 64)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository); !errors.Is(err, application.ErrRepositoryLifecycleMissing) {
		t.Fatalf("rollback resurrected retired incarnation: %v", err)
	}

	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now.Add(17 * time.Second)}); !errors.Is(err, application.ErrRepositoryLifecycleConflict) {
		t.Fatalf("repository was implicitly re-onboarded without accepted saga: %v", err)
	}
	var history int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_lifecycles WHERE repository=?`, profile.Authority.Repository).Scan(&history); err != nil || history != 1 {
		t.Fatalf("history=%d err=%v", history, err)
	}
}

func TestRepositoryRemovalV36MigrationBackfillsIncarnationAndPreservesReadiness(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 35)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	digest := strings.Repeat("a", 64)
	baseline := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: digest, Size: 100, SchemaVersion: 5, DatabasePath: path, Operator: operator}, CanonicalConfigPath: path + ".json", ObservedAt: now}
	if err := legacy.PrepareConfigurationBaseline(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	configuration, _, err := legacy.AdoptConfigurationBaseline(ctx, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`UPDATE configuration_generations SET lifecycle='effective',effective_at=?,reason_code='' WHERE generation_id=?`, formatTime(now.Add(time.Second)), configuration.Desired.GenerationID); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`UPDATE configuration_authority SET effective_generation_id=?,authority_version=2,updated_at=? WHERE authority_id=1`, configuration.Desired.GenerationID, formatTime(now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	configuration.Version = 2
	profile := repositoryProfileFixture("owner/repo", "b", "c")
	snapshotID := "repository-snapshot-v35"
	if _, err := legacy.db.Exec(`INSERT INTO repository_lifecycles(repository,profile_id,profile_digest,repository_binding_digest,intent,lifecycle_version,current_snapshot_id,updated_at) VALUES(?,?,?,?, 'disabled',2,?,?)`, profile.Authority.Repository, profile.Profile.ProfileID, profile.Profile.ProfileDigest, profile.Profile.RepositoryBindingDigest, snapshotID, formatTime(now.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`INSERT INTO repository_readiness_snapshots(snapshot_id,repository,profile_id,profile_digest,repository_binding_digest,lifecycle_version,configuration_generation_id,configuration_digest,configuration_authority_version,overall_status,reason_code,snapshot_digest,observed_at,published_at) VALUES(?,?,?,?,?,?,?, ?,?,'unknown','initial_recheck_required',?,?,?)`, snapshotID, profile.Authority.Repository, profile.Profile.ProfileID, profile.Profile.ProfileDigest, profile.Profile.RepositoryBindingDigest, 2, configuration.Desired.GenerationID, configuration.Desired.Digest, configuration.Version, strings.Repeat("f", 64), formatTime(now.Add(2*time.Second)), formatTime(now.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	for _, dimension := range domain.RepositoryReadinessDimensions {
		if _, err := legacy.db.Exec(`INSERT INTO repository_readiness_dimensions(snapshot_id,dimension,status,reason_code,evidence_digest,observed_at) VALUES(?,?,'unknown','initial_recheck_required',?,?)`, snapshotID, string(dimension), application.ConfigurationEvidenceDigest("v35", string(dimension)), formatTime(now.Add(2*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	wantIncarnation := "repository-incarnation-" + profile.Profile.RepositoryBindingDigest[:32]
	if err != nil || authority.Lifecycle.IncarnationID != wantIncarnation || authority.Snapshot.IncarnationID != wantIncarnation || authority.Lifecycle.Intent != application.RepositoryDisabled || authority.Lifecycle.Version != 2 || authority.Snapshot.Status != domain.RepositoryUnknown {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
	for _, table := range []string{"repository_removal_drafts", "repository_removal_intents"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table=%s count=%d err=%v", table, count, err)
		}
	}
}

func beginRemovalConfigurationApply(t *testing.T, ctx context.Context, store *Store, authority application.ConfigurationAuthority, operator domain.GitHubUserIdentity, databasePath, digest string, at time.Time, anchorByte string) (application.ConfigurationGeneration, application.OperationReceipt) {
	t.Helper()
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: operator, RequestDigest: digest, ExpectedAuthorityDigest: authority.Desired.Digest, OperationAnchorDigest: strings.Repeat(anchorByte, 64), TargetBindingDigest: strings.Repeat("c", 64), AcceptedAt: at})
	generation, persisted, _, err := store.BeginConfigurationApply(ctx, application.ConfigurationApplyAcceptance{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Candidate: application.ValidatedConfigurationCandidate{Digest: digest, Size: 80, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator}, Requester: operator, Receipt: receipt, AcceptedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return generation, persisted
}

func hasRemovalGuard(guards []application.RepositoryRemovalGuardResult, name string) bool {
	for _, guard := range guards {
		if guard.Guard == name && guard.Allowed {
			return true
		}
	}
	return false
}
