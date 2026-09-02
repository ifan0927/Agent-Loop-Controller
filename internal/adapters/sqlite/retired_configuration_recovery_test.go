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

func TestConfigurationRecoveryV34MigratesV33AndRetainsHistoricalSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 33)
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
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='configuration_recovery_intents'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery table=%d err=%v", count, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN ('integrity_track_configuration_recovery_intents_insert','integrity_track_configuration_recovery_intents_update','integrity_track_configuration_recovery_intents_delete')`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("retained triggers=%d err=%v", count, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN ('configuration_recovery_identity_immutable','configuration_recovery_settlement_once')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("recovery immutability triggers=%d err=%v", count, err)
	}
	var family string
	if err := store.db.QueryRow(`SELECT family FROM integrity_registry_sources WHERE registry_version='v1' AND table_name='configuration_recovery_intents'`).Scan(&family); err != nil || family != string(application.IntegrityConfiguration) {
		t.Fatalf("integrity family=%s err=%v", family, err)
	}
	var foreignKeyViolations int
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
}

func TestHistoricalConfigurationRecoveryEvidenceRemainsReadableAndImmutable(t *testing.T) {
	for _, state := range []application.ConfigurationRecoveryState{application.ConfigurationRecoveryCommitted, application.ConfigurationRecoveryAccepted, application.ConfigurationRecoveryAmbiguous} {
		t.Run(string(state), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "controller.db")
			store, authority, requester := recoveryTestAuthorityAtPath(t, path)
			acceptedAt := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
			receipt := recoveryTestReceipt(authority, strings.Repeat("b", 64), requester, acceptedAt)
			if state != application.ConfigurationRecoveryAccepted {
				receipt.Phase = application.OperationPhaseObserved
				receipt.Outcome = application.OperationOutcomeAmbiguous
				if state == application.ConfigurationRecoveryCommitted {
					receipt.Outcome = application.OperationOutcomeSucceeded
				}
				receipt.ResultingAuthorityDigest = authority.Desired.Digest
				receipt.ResultingState = string(authority.Desired.State)
				receipt.ResultingVersion = authority.Desired.GenerationID
				receipt.EvidenceDigest = strings.Repeat("e", 64)
				receipt.ResultDigest = strings.Repeat("f", 64)
				receipt.AppliedAt = acceptedAt.Add(time.Minute)
				receipt.SettledAt = acceptedAt.Add(2 * time.Minute)
			}
			if err := application.ValidateOperationReceipt(receipt); err != nil {
				t.Fatal(err)
			}
			insertHistoricalConfigurationRecovery(t, store, authority, receipt, state)
			if _, err := store.db.Exec(`UPDATE configuration_recovery_intents SET observed_digest=? WHERE operation_id=?`, strings.Repeat("c", 64), receipt.OperationID); err == nil {
				t.Fatal("historical recovery evidence was mutable")
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			intent, found, err := configurationRecoveryIntentQuery(context.Background(), reopened.db, `WHERE operation_id=?`, receipt.OperationID)
			if err != nil || !found || intent.State != state || intent.OperationID != receipt.OperationID {
				t.Fatalf("intent=%+v found=%t err=%v", intent, found, err)
			}
			persisted, found, err := getOperationReceiptByIDQuery(context.Background(), reopened.db, receipt.OperationID)
			if err != nil || !found || persisted.OperationType != application.OperationRestoreConfiguration || persisted.Outcome != receipt.Outcome {
				t.Fatalf("receipt=%+v found=%t err=%v", persisted, found, err)
			}
			current, found, err := reopened.ConfigurationAuthority(context.Background())
			if err != nil || !found {
				t.Fatalf("authority=%+v found=%t err=%v", current, found, err)
			}
			if state == application.ConfigurationRecoveryCommitted && current.IncompleteRecovery != nil || state != application.ConfigurationRecoveryCommitted && (current.IncompleteRecovery == nil || current.IncompleteRecovery.State != state) {
				t.Fatalf("incomplete recovery=%+v state=%s", current.IncompleteRecovery, state)
			}
			generations, err := reopened.ListConfigurationGenerations(context.Background())
			if err != nil || len(generations) != 1 || generations[0].GenerationID != authority.Desired.GenerationID {
				t.Fatalf("configuration history=%+v err=%v", generations, err)
			}
			authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: requester})
			if err != nil {
				t.Fatal(err)
			}
			history, err := application.NewOperationReceiptQueryService(reopened, authorizer, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			queryRequester := application.Requester{ID: requester.Login, Kind: "github_login", DatabaseID: requester.DatabaseID, NodeID: requester.NodeID, ActorType: requester.ActorType}
			detail, err := history.Get(context.Background(), queryRequester, receipt.OperationID)
			if err != nil || detail.OperationType != application.OperationRestoreConfiguration || detail.Outcome != receipt.Outcome {
				t.Fatalf("receipt detail=%+v err=%v", detail, err)
			}
			page, err := history.List(context.Background(), application.OperationHistoryQuery{Requester: queryRequester, Filter: application.OperationHistoryFilter{OperationType: application.OperationRestoreConfiguration}, Limit: 10}, acceptedAt.Add(time.Hour))
			if err != nil || len(page.Receipts) != 1 || page.Receipts[0].OperationID != receipt.OperationID {
				t.Fatalf("receipt history=%+v err=%v", page, err)
			}

			completeIntegrityActivityBackfill(t, reopened, acceptedAt.Add(3*time.Hour))
			var activities int
			if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM activity_operation_links WHERE operation_id=?`, receipt.OperationID).Scan(&activities); err != nil {
				t.Fatal(err)
			}
			if state == application.ConfigurationRecoveryAccepted && activities != 0 || state != application.ConfigurationRecoveryAccepted && activities != 1 {
				t.Fatalf("settled activity links=%d state=%s", activities, state)
			}
			publishIntegrityObservation(t, reopened, acceptedAt.Add(4*time.Hour))
			integrity, integrityRequester := integrityQueryService(t, reopened)
			summary, err := integrity.Summary(context.Background(), integrityRequester)
			if err != nil || summary.Readiness != application.IntegrityReady {
				t.Fatalf("integrity=%+v err=%v", summary, err)
			}
		})
	}
}

func insertHistoricalConfigurationRecovery(t *testing.T, store *Store, authority application.ConfigurationAuthority, receipt application.OperationReceipt, state application.ConfigurationRecoveryState) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO operation_receipts(operation_id,authority_key,operation_anchor_digest,operation_type,scope_kind,target_id,requester_login,requester_database_id,requester_node_id,requester_actor_type,request_digest,expected_authority_digest,target_binding_digest,phase,outcome,resulting_authority_digest,resulting_state,resulting_version,evidence_digest,result_digest,accepted_at,applied_at,settled_at,source_action_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.OperationID, receipt.AuthorityKey, receipt.OperationAnchorDigest, string(receipt.OperationType), string(receipt.Scope), receipt.TargetID, receipt.Requester.Login, receipt.Requester.DatabaseID, receipt.Requester.NodeID, receipt.Requester.ActorType, receipt.RequestDigest, receipt.ExpectedAuthorityDigest, receipt.TargetBindingDigest, string(receipt.Phase), string(receipt.Outcome), receipt.ResultingAuthorityDigest, receipt.ResultingState, receipt.ResultingVersion, receipt.EvidenceDigest, receipt.ResultDigest, formatTime(receipt.AcceptedAt), formatTime(receipt.AppliedAt), formatTime(receipt.SettledAt), ""); err != nil {
		t.Fatal(err)
	}
	reason, evidence, settled := "", "", ""
	if state == application.ConfigurationRecoveryCommitted {
		reason, evidence, settled = string(application.ConfigurationReasonReady), strings.Repeat("e", 64), formatTime(receipt.SettledAt)
	}
	if state == application.ConfigurationRecoveryAmbiguous {
		reason, evidence, settled = string(application.ConfigurationReasonRecoveryAmbiguous), strings.Repeat("e", 64), formatTime(receipt.SettledAt)
	}
	if _, err := store.db.Exec(`INSERT INTO configuration_recovery_intents(operation_id,desired_generation_id,desired_digest,authority_version,observed_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,status,accepted_at,settled_at,reason_code,evidence_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, receipt.OperationID, authority.Desired.GenerationID, authority.Desired.Digest, authority.Version, strings.Repeat("b", 64), receipt.Requester.Login, receipt.Requester.DatabaseID, receipt.Requester.NodeID, receipt.Requester.ActorType, string(state), formatTime(receipt.AcceptedAt), settled, reason, evidence); err != nil {
		t.Fatal(err)
	}
}

func recoveryTestAuthorityAtPath(t *testing.T, path string) (*Store, application.ConfigurationAuthority, domain.GitHubUserIdentity) {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
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

func TestCurrentSQLiteBoundariesRejectRetiredConfigurationRestoreReceipts(t *testing.T) {
	store, authority, requester := recoveryTestAuthorityAtPath(t, filepath.Join(t.TempDir(), "controller.db"))
	defer store.Close()
	receipt := recoveryTestReceipt(authority, strings.Repeat("b", 64), requester, time.Now().UTC())
	if _, _, err := store.BeginOperationReceipt(context.Background(), receipt); err == nil {
		t.Fatal("current SQLite store created a retired configuration restore receipt")
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOperationReceiptTx(context.Background(), tx, receipt, ""); err == nil {
		_ = tx.Rollback()
		t.Fatal("internal SQLite insertion created a retired configuration restore receipt")
	}
	_ = tx.Rollback()
	insertHistoricalConfigurationRecovery(t, store, authority, receipt, application.ConfigurationRecoveryAccepted)
	if _, _, err := store.AdvanceOperationReceipt(context.Background(), application.OperationReceiptMutation{OperationID: receipt.OperationID, ExpectedPhase: application.OperationPhaseAccepted, Phase: application.OperationPhaseAccepted, Outcome: application.OperationOutcomeFailed, ResultDigest: strings.Repeat("f", 64), At: receipt.AcceptedAt.Add(time.Minute)}); err == nil {
		t.Fatal("current SQLite store advanced a retired configuration restore receipt")
	}
	var state string
	if err := store.db.QueryRow(`SELECT status FROM configuration_recovery_intents WHERE operation_id=?`, receipt.OperationID).Scan(&state); err != nil || state != string(application.ConfigurationRecoveryAccepted) {
		t.Fatalf("historical intent state=%s err=%v", state, err)
	}
	var phase string
	if err := store.db.QueryRow(`SELECT phase FROM operation_receipts WHERE operation_id=?`, receipt.OperationID).Scan(&phase); err != nil || phase != string(application.OperationPhaseAccepted) {
		t.Fatalf("historical receipt phase=%s err=%v", phase, err)
	}
}
