package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func removeConfigurationV31(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS configuration_convergence_events`,
		`DROP TABLE IF EXISTS configuration_apply_intents`,
		`DROP TABLE IF EXISTS configuration_authority`,
		`DROP TRIGGER IF EXISTS configuration_generation_identity_immutable`,
		`DROP TABLE IF EXISTS configuration_generations`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigurationV31MigratesV30AndPreservesReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
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
	if version, err := store.SchemaVersion(context.Background()); err != nil || version != 31 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if target, err := store.GetOperationReceiptTarget(context.Background(), receipt.OperationID); err != nil || target.TargetID != receipt.TargetID {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	for _, table := range []string{"configuration_generations", "configuration_authority", "configuration_apply_intents", "configuration_convergence_events"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table=%s count=%d err=%v", table, count, err)
		}
	}
}

func TestConcurrentConfigurationBaselineCreatesExactlyOneGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: filepath.Join(t.TempDir(), "owned.db"), Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}}, CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"), ObservedAt: time.Now().UTC()}
	var wait sync.WaitGroup
	created := make(chan bool, 8)
	errorsSeen := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, wasCreated, err := store.AdoptConfigurationBaseline(context.Background(), input)
			created <- wasCreated
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(created)
	close(errorsSeen)
	wins := 0
	for wasCreated := range created {
		if wasCreated {
			wins++
		}
	}
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("baseline winners=%d", wins)
	}
	if generations, err := store.ListConfigurationGenerations(context.Background()); err != nil || len(generations) != 1 || generations[0].Origin != application.ConfigurationOriginBaseline {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestConfigurationDriftEvidenceRecordsTransitionsNotPollingCadence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	desired := strings.Repeat("a", 64)
	authority, _, err := store.AdoptConfigurationBaseline(context.Background(), application.ConfigurationBaselineInput{
		Candidate:           application.ValidatedConfigurationCandidate{Digest: desired, Size: 42, SchemaVersion: 5, DatabasePath: filepath.Join(t.TempDir(), "owned.db"), Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}},
		CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"),
		ObservedAt:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	observations := []application.ConfigurationDriftObservation{
		{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: desired, ObservedDigest: strings.Repeat("b", 64), Drifted: true, Reason: application.ConfigurationReasonExternalDrift, ObservedAt: now.Add(time.Second)},
		{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: desired, ObservedDigest: strings.Repeat("c", 64), Drifted: true, Reason: application.ConfigurationReasonExternalDrift, ObservedAt: now.Add(2 * time.Second)},
		{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: desired, ObservedDigest: desired, Reason: application.ConfigurationReasonReady, ObservedAt: now.Add(3 * time.Second)},
		{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: desired, ObservedDigest: desired, Reason: application.ConfigurationReasonReady, ObservedAt: now.Add(4 * time.Second)},
		{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: desired, ObservedDigest: strings.Repeat("b", 64), Drifted: true, Reason: application.ConfigurationReasonExternalDrift, ObservedAt: now.Add(5 * time.Second)},
	}
	writes := 0
	for _, observation := range observations {
		changed, err := store.ObserveConfigurationDrift(context.Background(), observation)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			writes++
		}
	}
	if writes != 3 {
		t.Fatalf("meaningful drift writes=%d", writes)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM configuration_convergence_events WHERE event_type IN ('drift_entered','drift_cleared')`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("drift events=%d err=%v", count, err)
	}
}

func TestConfigurationReceiptSettlementFailureRollsBackDesiredAtomically(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	databasePath := filepath.Join(t.TempDir(), "owned.db")
	baseline, _, err := store.AdoptConfigurationBaseline(ctx, application.ConfigurationBaselineInput{
		Candidate:           application.ValidatedConfigurationCandidate{Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator},
		CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"), ObservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{
		OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID,
		Requester: operator, RequestDigest: strings.Repeat("b", 64), ExpectedAuthorityDigest: baseline.Desired.Digest,
		OperationAnchorDigest: strings.Repeat("c", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: now.Add(time.Second),
	})
	generation, _, _, err := store.BeginConfigurationApply(ctx, application.ConfigurationApplyAcceptance{
		ExpectedGenerationID: baseline.Desired.GenerationID, ExpectedDigest: baseline.Desired.Digest,
		Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("b", 64), Size: 43, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator},
		Requester: operator, Receipt: receipt, AcceptedAt: receipt.AcceptedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_configuration_receipt_settlement BEFORE UPDATE OF phase ON operation_receipts WHEN OLD.operation_type='apply_configuration' BEGIN SELECT RAISE(ABORT,'injected receipt settlement failure'); END`); err != nil {
		t.Fatal(err)
	}
	settlement := application.ConfigurationApplySettlement{GenerationID: generation.GenerationID, ParentID: generation.ParentID, OperationID: generation.OperationID, Outcome: application.ConfigurationApplyCommitted, Reason: application.ConfigurationReasonRestartRequired, EvidenceDigest: strings.Repeat("e", 64), SettledAt: now.Add(2 * time.Second)}
	if _, _, _, err := store.SettleConfigurationApply(ctx, settlement); err == nil {
		t.Fatal("receipt settlement failure unexpectedly committed")
	}
	authority, found, err := store.ConfigurationAuthority(ctx)
	if err != nil || !found || authority.Desired.GenerationID != baseline.Desired.GenerationID || authority.Incomplete == nil {
		t.Fatalf("rolled-back authority=%+v found=%t err=%v", authority, found, err)
	}
	var phase string
	if err := store.db.QueryRow(`SELECT phase FROM operation_receipts WHERE operation_id=?`, generation.OperationID).Scan(&phase); err != nil || phase != string(application.OperationPhaseAccepted) {
		t.Fatalf("receipt phase=%q err=%v", phase, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_configuration_receipt_settlement`); err != nil {
		t.Fatal(err)
	}
	settled, settledReceipt, changed, err := store.SettleConfigurationApply(ctx, settlement)
	if err != nil || !changed || settled.Desired.GenerationID != generation.GenerationID || settledReceipt.Phase != application.OperationPhaseObserved {
		t.Fatalf("settled=%+v receipt=%+v changed=%t err=%v", settled, settledReceipt, changed, err)
	}
}
