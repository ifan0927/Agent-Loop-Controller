package sqlite

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestConfigurationRecoveryV34MigratesV33AndPreservesReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 33)
	if err != nil {
		t.Fatal(err)
	}
	requester := recoveryTestRequester()
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRetry, Scope: application.ScopeController, TargetID: "local-controller", Requester: requester, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("c", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: time.Now().UTC()})
	if _, _, err := legacy.BeginOperationReceipt(context.Background(), receipt); err != nil {
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
	if target, err := store.GetOperationReceiptTarget(context.Background(), receipt.OperationID); err != nil || target.TargetID != receipt.TargetID {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	var recoveryTable, foreignKeyViolations int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='configuration_recovery_intents'`).Scan(&recoveryTable); err != nil || recoveryTable != 1 {
		t.Fatalf("recovery table=%d err=%v", recoveryTable, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil || foreignKeyViolations != 0 {
		t.Fatalf("foreign key violations=%d err=%v", foreignKeyViolations, err)
	}
}

func TestConcurrentConfigurationRecoveryV34MigrationFromV33(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 33)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			store, openErr := Open(path)
			if openErr == nil {
				openErr = store.Close()
			}
			errorsSeen <- openErr
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.SchemaVersion(context.Background()); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestConfigurationRecoveryV34ReceiptConstraintAcrossOpenPaths(t *testing.T) {
	for _, startingVersion := range []int{0, 31, 32, 33} {
		t.Run(strconv.Itoa(startingVersion), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "controller.db")
			if startingVersion > 0 {
				legacy, err := openWithSupportedSchema(path, startingVersion)
				if err != nil {
					t.Fatal(err)
				}
				if err := legacy.Close(); err != nil {
					t.Fatal(err)
				}
			}
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if version, err := store.SchemaVersion(context.Background()); err != nil || version != schemaVersion {
				t.Fatalf("version=%d err=%v", version, err)
			}
			receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRestoreConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: recoveryTestRequester(), RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("c", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: time.Now().UTC()})
			persisted, created, err := store.BeginOperationReceipt(context.Background(), receipt)
			if err != nil || !created || persisted.OperationType != application.OperationRestoreConfiguration {
				t.Fatalf("receipt=%+v created=%t err=%v", persisted, created, err)
			}
		})
	}
}

func TestConfigurationRecoveryPersistenceIsImmutableAndExcludesApply(t *testing.T) {
	store, authority, requester := recoveryTestAuthority(t)
	ctx := context.Background()
	observed := strings.Repeat("b", 64)
	if _, err := store.ObserveConfigurationDrift(ctx, application.ConfigurationDriftObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, ObservedDigest: observed, Drifted: true, Reason: application.ConfigurationReasonExternalDrift, ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	current, _, err := store.ConfigurationAuthority(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authority = current
	receipt := recoveryTestReceipt(authority, observed, requester, time.Now().UTC())
	input := application.ConfigurationRecoveryAcceptance{DesiredGenerationID: authority.Desired.GenerationID, DesiredDigest: authority.Desired.Digest, AuthorityVersion: authority.Version, ObservedDigest: observed, Requester: requester, Receipt: receipt, AcceptedAt: receipt.AcceptedAt}
	intent, accepted, created, err := store.BeginConfigurationRecovery(ctx, input)
	if err != nil || !created || intent.State != application.ConfigurationRecoveryAccepted || accepted.OperationType != application.OperationRestoreConfiguration {
		t.Fatalf("intent=%+v receipt=%+v created=%t err=%v", intent, accepted, created, err)
	}
	if _, err := store.db.Exec(`UPDATE configuration_recovery_intents SET observed_digest=? WHERE operation_id=?`, strings.Repeat("c", 64), intent.OperationID); err == nil {
		t.Fatal("immutable recovery evidence was updated")
	}
	applyCandidate := application.ValidatedConfigurationCandidate{Digest: strings.Repeat("c", 64), Size: 42, SchemaVersion: 5, DatabasePath: authority.DatabasePath, Operator: requester, Repositories: map[string]application.ConfigurationRepositoryAuthority{}}
	applyReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: requester, RequestDigest: applyCandidate.Digest, ExpectedAuthorityDigest: authority.Desired.Digest, OperationAnchorDigest: strings.Repeat("e", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: time.Now().UTC()})
	if _, _, _, err := store.BeginConfigurationApply(ctx, application.ConfigurationApplyAcceptance{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Candidate: applyCandidate, Requester: requester, Receipt: applyReceipt, Provenance: application.ConfigurationApplyProvenance{Kind: application.ConfigurationApplyNormal}, AcceptedAt: applyReceipt.AcceptedAt}); err == nil {
		t.Fatal("apply acquired authority while recovery was active")
	}
	if _, err := store.db.Exec(`INSERT INTO configuration_apply_intents(generation_id,parent_generation_id,parent_digest,target_digest,operation_id,status,accepted_at) VALUES(?,?,?,?,?,'accepted',?)`, authority.Desired.GenerationID, authority.Desired.GenerationID, authority.Desired.Digest, applyCandidate.Digest, intent.OperationID, formatTime(time.Now().UTC())); err == nil {
		t.Fatal("direct apply intent bypassed recovery exclusion trigger")
	}
	settledAuthority, settledIntent, settledReceipt, changed, err := store.SettleConfigurationRecovery(ctx, application.ConfigurationRecoverySettlement{OperationID: intent.OperationID, Outcome: application.ConfigurationRecoveryCommitted, Reason: application.ConfigurationReasonReady, EvidenceDigest: strings.Repeat("f", 64), SettledAt: time.Now().UTC().Add(time.Second)})
	if err != nil || !changed || settledIntent.State != application.ConfigurationRecoveryCommitted || settledReceipt.Outcome != application.OperationOutcomeSucceeded || settledAuthority.Desired.GenerationID != authority.Desired.GenerationID || settledAuthority.Version != authority.Version+1 {
		t.Fatalf("authority=%+v intent=%+v receipt=%+v changed=%t err=%v", settledAuthority, settledIntent, settledReceipt, changed, err)
	}
	if _, err := store.db.Exec(`UPDATE configuration_recovery_intents SET evidence_digest=? WHERE operation_id=?`, strings.Repeat("e", 64), intent.OperationID); err == nil {
		t.Fatal("settled recovery evidence was updated")
	}
	var generations, cleared int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM configuration_generations`).Scan(&generations); err != nil || generations != 1 {
		t.Fatalf("generations=%d err=%v", generations, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM configuration_convergence_events WHERE event_type='drift_cleared' AND operation_id=?`, intent.OperationID).Scan(&cleared); err != nil || cleared != 1 {
		t.Fatalf("drift cleared=%d err=%v", cleared, err)
	}
}

func TestConfigurationApplyExcludesRecoveryAcceptance(t *testing.T) {
	store, authority, requester := recoveryTestAuthority(t)
	ctx := context.Background()
	candidate := application.ValidatedConfigurationCandidate{Digest: strings.Repeat("c", 64), Size: 42, SchemaVersion: 5, DatabasePath: authority.DatabasePath, Operator: requester, Repositories: map[string]application.ConfigurationRepositoryAuthority{}}
	acceptedAt := time.Now().UTC()
	applyReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: requester, RequestDigest: candidate.Digest, ExpectedAuthorityDigest: authority.Desired.Digest, OperationAnchorDigest: strings.Repeat("e", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: acceptedAt})
	if _, _, created, err := store.BeginConfigurationApply(ctx, application.ConfigurationApplyAcceptance{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Candidate: candidate, Requester: requester, Receipt: applyReceipt, Provenance: application.ConfigurationApplyProvenance{Kind: application.ConfigurationApplyNormal}, AcceptedAt: acceptedAt}); err != nil || !created {
		t.Fatalf("apply created=%t err=%v", created, err)
	}
	observed := strings.Repeat("b", 64)
	recoveryReceipt := recoveryTestReceipt(authority, observed, requester, acceptedAt.Add(time.Second))
	recovery := application.ConfigurationRecoveryAcceptance{DesiredGenerationID: authority.Desired.GenerationID, DesiredDigest: authority.Desired.Digest, AuthorityVersion: authority.Version, ObservedDigest: observed, Requester: requester, Receipt: recoveryReceipt, AcceptedAt: recoveryReceipt.AcceptedAt}
	if _, _, _, err := store.BeginConfigurationRecovery(ctx, recovery); err == nil {
		t.Fatal("recovery acquired authority while apply was active")
	}
	if _, err := store.db.Exec(`INSERT INTO configuration_recovery_intents(operation_id,desired_generation_id,desired_digest,authority_version,observed_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,status,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,'accepted',?)`, applyReceipt.OperationID, authority.Desired.GenerationID, authority.Desired.Digest, authority.Version, observed, requester.Login, requester.DatabaseID, requester.NodeID, requester.ActorType, formatTime(acceptedAt)); err == nil {
		t.Fatal("direct recovery intent bypassed apply exclusion trigger")
	}
}

func recoveryTestAuthority(t *testing.T) (*Store, application.ConfigurationAuthority, domain.GitHubUserIdentity) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	requester := recoveryTestRequester()
	input := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: path, Operator: requester, Repositories: map[string]application.ConfigurationRepositoryAuthority{}}, CanonicalConfigPath: filepath.Join(filepath.Dir(path), "controller.json"), ObservedAt: time.Now().UTC()}
	if err := store.PrepareConfigurationBaseline(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	authority, _, err := store.AdoptConfigurationBaseline(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return store, authority, requester
}

func recoveryTestRequester() domain.GitHubUserIdentity {
	return domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
}

func recoveryTestReceipt(authority application.ConfigurationAuthority, observed string, requester domain.GitHubUserIdentity, at time.Time) application.OperationReceipt {
	authorityDigest := application.ConfigurationEvidenceDigest("recovery-authority-test", authority.Desired.Digest, observed, strconv.FormatInt(authority.Version, 10))
	return application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRestoreConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: requester, RequestDigest: application.ConfigurationEvidenceDigest("recovery-request-test", authorityDigest), ExpectedAuthorityDigest: authorityDigest, OperationAnchorDigest: application.ConfigurationEvidenceDigest("recovery-anchor-test", authorityDigest), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: at})
}
