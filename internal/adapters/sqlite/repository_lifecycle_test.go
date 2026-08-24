package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRepositoryBaselineAdoptsExactlyOnceAndStartsUnknown(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	at := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	input := application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: at}
	if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), input); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	authority, err := store.RepositoryOperationAuthority(context.Background(), profile.Authority.Repository)
	if err != nil || authority.Lifecycle.Intent != application.RepositoryEnabled || authority.Lifecycle.Version != 1 || authority.Snapshot.Status != domain.RepositoryUnknown || authority.Snapshot.ReasonCode != "initial_recheck_required" || len(authority.Snapshot.Dimensions) != 8 {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
	drift := repositoryProfileFixture("owner/repo", "c", "d")
	if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{drift}, AdoptedAt: at}); !errors.Is(err, application.ErrRepositoryLifecycleConflict) {
		t.Fatalf("drift error=%v", err)
	}
}

func TestRepositoryLifecyclePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	now := time.Date(2026, 8, 24, 1, 30, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	receipt := repositoryReceiptFixture(application.OperationDisableRepository, authority, profile, now.Add(time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: receipt.OperationID, Expected: authority, Intent: application.RepositoryDisabled, ChangedAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	authority, err = reopened.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil || authority.Lifecycle.Intent != application.RepositoryDisabled || authority.Lifecycle.Version != 2 {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
}

func TestRepositoryRecheckPublishesAtomicallyAndLifecycleFencesAdmission(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	now := time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	receipt := repositoryReceiptFixture(application.OperationRecheckRepository, authority, profile, now)
	if _, _, err := store.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	attempt := application.RepositoryRecheckStart{AttemptID: "repository-recheck-" + receipt.OperationID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(time.Second)}
	if _, created, err := store.BeginRepositoryRecheck(ctx, attempt); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	if decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile); err != nil || decision.Allowed || decision.Reason != "readiness_recheck_in_progress" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	results := repositoryReadyResults(now.Add(2 * time.Second))
	for _, result := range results {
		if err := store.SaveRepositoryRecheckObservation(ctx, attempt.AttemptID, result); err != nil {
			t.Fatal(err)
		}
	}
	projection, settled, err := store.PublishRepositoryRecheck(ctx, application.RepositoryRecheckPublication{AttemptID: attempt.AttemptID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, Results: results, PublishedAt: now.Add(3 * time.Second)})
	if err != nil || projection.Readiness.Status != domain.RepositoryReady || settled.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("projection=%+v receipt=%+v err=%v", projection, settled, err)
	}
	decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile)
	if err != nil || !decision.Allowed || !decision.Token.Valid() {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}

	authority, _ = store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	disable := repositoryReceiptFixture(application.OperationDisableRepository, authority, profile, now.Add(4*time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, disable); err != nil {
		t.Fatal(err)
	}
	projection, settled, err = store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: disable.OperationID, Expected: authority, Intent: application.RepositoryDisabled, ChangedAt: now.Add(5 * time.Second)})
	if err != nil || projection.Lifecycle.Intent != application.RepositoryDisabled || projection.Lifecycle.Version != 2 || settled.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("projection=%+v receipt=%+v err=%v", projection, settled, err)
	}
	if decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile); err != nil || decision.Allowed || decision.Reason != "repository_disabled" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	authority, _ = store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	enable := repositoryReceiptFixture(application.OperationEnableRepository, authority, profile, now.Add(6*time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, enable); err != nil {
		t.Fatal(err)
	}
	projection, settled, err = store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: enable.OperationID, Expected: authority, Intent: application.RepositoryEnabled, ChangedAt: now.Add(7 * time.Second)})
	if err != nil || projection.Lifecycle.Version != 3 || projection.Readiness.Status != domain.RepositoryReady || settled.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("projection=%+v receipt=%+v err=%v", projection, settled, err)
	}
	if decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile); err != nil || !decision.Allowed || decision.Token.LifecycleVersion != 3 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestRepositoryRecheckFailureSettlesReceiptWithoutPublication(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	now := time.Date(2026, 8, 24, 2, 30, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	receipt := repositoryReceiptFixture(application.OperationRecheckRepository, authority, profile, now)
	if _, _, err := store.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	attemptID := "repository-recheck-" + receipt.OperationID
	if _, created, err := store.BeginRepositoryRecheck(ctx, application.RepositoryRecheckStart{AttemptID: attemptID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(time.Second)}); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	if err := store.SettleRepositoryRecheckFailure(ctx, application.RepositoryRecheckFailure{AttemptID: attemptID, OperationID: receipt.OperationID, Outcome: application.OperationOutcomeFailed, ReasonCode: "readiness_observation_failed", SettledAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := store.db.QueryRowContext(ctx, `SELECT outcome FROM operation_receipts WHERE operation_id=?`, receipt.OperationID).Scan(&outcome); err != nil || outcome != string(application.OperationOutcomeFailed) {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	current, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil || current.Recheck != nil || current.Snapshot.SnapshotID != authority.Snapshot.SnapshotID {
		t.Fatalf("authority=%+v err=%v", current, err)
	}
}

func TestRepositoryAdmissionTokenIsRevalidatedWithRunCreation(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	receipt := repositoryReceiptFixture(application.OperationRecheckRepository, authority, profile, now)
	_, _, _ = store.BeginOperationReceipt(ctx, receipt)
	attempt := application.RepositoryRecheckStart{AttemptID: "repository-recheck-" + receipt.OperationID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(time.Second)}
	_, _, _ = store.BeginRepositoryRecheck(ctx, attempt)
	results := repositoryReadyResults(now.Add(2 * time.Second))
	for _, result := range results {
		_ = store.SaveRepositoryRecheckObservation(ctx, attempt.AttemptID, result)
	}
	_, _, err = store.PublishRepositoryRecheck(ctx, application.RepositoryRecheckPublication{AttemptID: attempt.AttemptID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, Results: results, PublishedAt: now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	decision, _ := store.CheckRepositoryAdmission(ctx, profile.Profile)
	run := repositoryRunFixture(profile.Profile, "run-token", "IFAN-121")
	if _, created, err := store.CreateRun(ctx, application.CreateRunInput{Run: run, RepositoryAuthority: decision.Token}); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	stale := decision.Token
	stale.SnapshotDigest = strings.Repeat("f", 64)
	run = repositoryRunFixture(profile.Profile, "run-stale-token", "IFAN-122")
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run, RepositoryAuthority: stale}); !errors.Is(err, application.ErrRepositoryAdmissionConflict) {
		t.Fatalf("stale token error=%v", err)
	}
}

func TestRepositoryAdmissionTokenIsRevalidatedWithAutomaticReservation(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repository", "a", "b")
	now := time.Now().UTC()
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	recheck := repositoryReceiptFixture(application.OperationRecheckRepository, authority, profile, now)
	_, _, _ = store.BeginOperationReceipt(ctx, recheck)
	attemptID := "repository-recheck-" + recheck.OperationID
	_, _, _ = store.BeginRepositoryRecheck(ctx, application.RepositoryRecheckStart{AttemptID: attemptID, OperationID: recheck.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(time.Second)})
	results := repositoryReadyResults(now.Add(2 * time.Second))
	for _, result := range results {
		_ = store.SaveRepositoryRecheckObservation(ctx, attemptID, result)
	}
	_, _, err = store.PublishRepositoryRecheck(ctx, application.RepositoryRecheckPublication{AttemptID: attemptID, OperationID: recheck.OperationID, Expected: authority, Profile: profile.Profile, Results: results, PublishedAt: now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	decision, _ := store.CheckRepositoryAdmission(ctx, profile.Profile)
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "repository-token-test", time.Minute, now.Add(4*time.Second))
	if err != nil || !acquired {
		t.Fatalf("lease acquired=%t err=%v", acquired, err)
	}
	reservation := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174121", "run-repository-token", "IFAN-121", lease)
	reservation.Input.Repository = profile.Profile
	reservation.Input.RepositoryAuthority = decision.Token
	authority, _ = store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	disable := repositoryReceiptFixture(application.OperationDisableRepository, authority, profile, now.Add(5*time.Second))
	_, _, _ = store.BeginOperationReceipt(ctx, disable)
	if _, _, err := store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: disable.OperationID, Expected: authority, Intent: application.RepositoryDisabled, ChangedAt: now.Add(6 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.ReserveLinearTodoAdmission(ctx, reservation); !errors.Is(err, application.ErrRepositoryAdmissionConflict) {
		t.Fatalf("stale automatic reservation error=%v", err)
	}
}

func repositoryProfileFixture(repository, profileByte, bindingByte string) application.RepositoryProfileAuthority {
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	profileDigest, bindingDigest := strings.Repeat(profileByte, 64), strings.Repeat(bindingByte, 64)
	profile := application.LocalRepository{ProfileID: "repository-profile:" + repository, ProfileSnapshotVersion: 1, ProfileDigest: profileDigest, RegistryVersion: 1, RegistryDigest: strings.Repeat("e", 64), RepositoryBindingDigest: bindingDigest, CanonicalRepository: repository, LinearLabel: "repo", BaseBranch: "main", VerifierRegistryRef: "registry", VerifierIDs: []string{"go-test"}, AllowedOperatorLogins: []string{"operator"}, TrustedOperatorActors: []application.TrustedActorIdentity{{Login: identity.Login, DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, Type: identity.ActorType}}}
	authority := application.RepositoryAuthority{Repository: repository, ProfileID: profile.ProfileID, BindingDigest: bindingDigest, AllowedLogins: []string{"operator"}, TrustedOperators: []domain.GitHubUserIdentity{identity}}
	return application.RepositoryProfileAuthority{Authority: authority, Profile: profile}
}

func repositoryReceiptFixture(operation application.OperationType, authority application.RepositoryOperationAuthority, profile application.RepositoryProfileAuthority, at time.Time) application.OperationReceipt {
	requester := profile.Authority.TrustedOperators[0]
	return application.NewOperationReceipt(application.OperationReceiptInput{OperationType: operation, Scope: application.ScopeRepository, TargetID: profile.Authority.Repository, Requester: requester, RequestDigest: strings.Repeat("c", 64), ExpectedAuthorityDigest: strings.Repeat("d", 64), OperationAnchorDigest: application.ConfigurationEvidenceDigest("anchor", string(operation), profile.Authority.Repository, strings.Repeat("x", int(authority.Lifecycle.Version))), TargetBindingDigest: profile.Authority.BindingDigest, AcceptedAt: at})
}

func repositoryReadyResults(at time.Time) []domain.RepositoryDimensionResult {
	results := make([]domain.RepositoryDimensionResult, 0, len(domain.RepositoryReadinessDimensions))
	for _, dimension := range domain.RepositoryReadinessDimensions {
		results = append(results, domain.RepositoryDimensionResult{Dimension: dimension, Status: domain.RepositoryReady, ReasonCode: "ready", EvidenceDigest: application.ConfigurationEvidenceDigest("ready", string(dimension)), ObservedAt: at})
	}
	return results
}

func repositoryRunFixture(profile application.LocalRepository, runID, issueID string) application.Run {
	raw, _ := json.Marshal(profile)
	return application.Run{ID: runID, IssueID: issueID, IdempotencyKey: "key-" + runID, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: profile.CanonicalRepository, RepositoryConfigJSON: string(raw), ProfileID: profile.ProfileID, ProfileSnapshotVersion: profile.ProfileSnapshotVersion, ProfileDigest: profile.ProfileDigest, RegistryVersion: profile.RegistryVersion, RegistryDigest: profile.RegistryDigest, RepositoryBindingDigest: profile.RepositoryBindingDigest, BaseBranch: profile.BaseBranch, WorkingBranch: "ifan/121", ArtifactRoot: "/tmp/" + runID, ImplementationModel: "model", ReviewModel: "review", State: domain.StateReceived}
}
