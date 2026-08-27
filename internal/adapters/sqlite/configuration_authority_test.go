package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func removeConfigurationV31(t *testing.T, db *sql.DB) {
	t.Helper()
	removeIntegrityV40(t, db)
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS integrity_guard_activity_link_reject`,
		`DROP TRIGGER IF EXISTS integrity_guard_activity_event_reject`,
		`DROP TRIGGER IF EXISTS integrity_guard_operation_receipt_reject`,
		`DROP TABLE IF EXISTS controller_integrity_finalization_guard`,
		`DROP TABLE IF EXISTS controller_integrity_active_recheck`,
		`DROP TABLE IF EXISTS controller_integrity_rechecks`,
		`DROP TRIGGER IF EXISTS configuration_recovery_excludes_repository_removal`,
		`DROP TRIGGER IF EXISTS configuration_draft_excludes_repository_removal`,
		`DROP TRIGGER IF EXISTS repository_removal_draft_excludes_configuration`,
		`DROP TABLE IF EXISTS repository_removal_intents`,
		`DROP TABLE IF EXISTS repository_removal_drafts`,
		`DROP TRIGGER IF EXISTS configuration_apply_excludes_recovery`,
		`DROP TABLE IF EXISTS configuration_recovery_intents`,
		`DROP TABLE IF EXISTS configuration_drafts`,
		`DROP TABLE IF EXISTS configuration_raw_prune_claims`,
		`DROP TABLE IF EXISTS configuration_convergence_events`,
		`DROP TABLE IF EXISTS configuration_apply_intents`,
		`DROP TABLE IF EXISTS configuration_authority`,
		`DROP TABLE IF EXISTS configuration_baseline_anchor`,
		`DROP TRIGGER IF EXISTS configuration_generation_identity_immutable`,
		`DROP TABLE IF EXISTS configuration_generations`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func removeIntegrityV40(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'integrity_track_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var triggers []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		triggers = append(triggers, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range triggers {
		if _, err := db.Exec(`DROP TRIGGER ` + name); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS controller_integrity_finding_immutable_delete`,
		`DROP TRIGGER IF EXISTS controller_integrity_finding_immutable_update`,
		`DROP TRIGGER IF EXISTS controller_integrity_family_immutable_delete`,
		`DROP TRIGGER IF EXISTS controller_integrity_family_immutable_update`,
		`DROP TRIGGER IF EXISTS controller_integrity_observation_immutable_delete`,
		`DROP TRIGGER IF EXISTS controller_integrity_observation_immutable_update`,
		`DROP TABLE IF EXISTS controller_integrity_current`,
		`DROP TABLE IF EXISTS controller_integrity_observation_findings`,
		`DROP TABLE IF EXISTS controller_integrity_observation_families`,
		`DROP TABLE IF EXISTS controller_integrity_observations`,
		`DROP TABLE IF EXISTS controller_integrity_scan_findings`,
		`DROP TABLE IF EXISTS controller_integrity_checked_families`,
		`DROP TABLE IF EXISTS controller_integrity_scans`,
		`DROP TABLE IF EXISTS controller_integrity_scope_revisions`,
		`DROP TABLE IF EXISTS integrity_registry_sources`,
		`DROP TABLE IF EXISTS integrity_registry_families`,
		`DROP TABLE IF EXISTS controller_integrity_generation`,
		`DELETE FROM schema_migrations WHERE version IN (40,41)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func removeRepositoryLifecycleV35(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS repository_removal_intents`,
		`DROP TABLE IF EXISTS repository_removal_drafts`,
		`DROP TABLE IF EXISTS repository_recheck_observations`,
		`DROP TABLE IF EXISTS repository_recheck_attempts`,
		`DROP TABLE IF EXISTS repository_readiness_dimensions`,
		`DROP TABLE IF EXISTS repository_readiness_snapshots`,
		`DROP TABLE IF EXISTS repository_lifecycles`,
		`DROP TABLE IF EXISTS repository_lifecycle_baseline`,
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
	if version, err := store.SchemaVersion(context.Background()); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if target, err := store.GetOperationReceiptTarget(context.Background(), receipt.OperationID); err != nil || target.TargetID != receipt.TargetID {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	for _, table := range []string{"configuration_baseline_anchor", "configuration_generations", "configuration_authority", "configuration_apply_intents", "configuration_convergence_events"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table=%s count=%d err=%v", table, count, err)
		}
	}
}

func TestConcurrentConfigurationV31MigrationFromV30(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			store, openErr := openPinnedStore(context.Background(), path, schemaVersion, true, application.DatabaseFileIdentity{}, nil, openPinnedStoreHooks{beforeEffects: func() {
				ready <- struct{}{}
				<-release
			}})
			if openErr == nil {
				openErr = store.Close()
			}
			results <- openErr
		}()
	}
	<-ready
	<-ready
	close(release)
	for range 2 {
		if err := <-results; err != nil {
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

func TestConcurrentConfigurationBaselineCreatesExactlyOneGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: filepath.Join(t.TempDir(), "owned.db"), Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}}, CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"), ObservedAt: time.Now().UTC()}
	if err := store.PrepareConfigurationBaseline(context.Background(), input); err != nil {
		t.Fatal(err)
	}
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

func TestConfigurationBindingProofAllowsTrustedForwardMigrationOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	configPath := filepath.Join(t.TempDir(), "controller.json")
	input := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: path, Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}}, CanonicalConfigPath: configPath, ObservedAt: now}
	if err := store.PrepareConfigurationBaseline(context.Background(), input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, _, err := store.AdoptConfigurationBaseline(context.Background(), input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	boundConfig, boundDatabase, bound, err := inspectConfigurationBindingReadOnly(context.Background(), path, schemaVersion+1)
	if err != nil || !bound || boundConfig != configPath || boundDatabase != path {
		t.Fatalf("config=%q database=%q bound=%t err=%v", boundConfig, boundDatabase, bound, err)
	}
	if _, _, _, err := inspectConfigurationBindingReadOnly(context.Background(), path, 30); err == nil {
		t.Fatal("pre-authority schema accepted trusted binding proof")
	}
}

func TestConfigurationAuthorityOpenDoesNotMigrateReplacementAfterProof(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "controller.json")
	input := application.ConfigurationBaselineInput{
		Candidate: application.ValidatedConfigurationCandidate{
			Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: path,
			Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"},
		},
		CanonicalConfigPath: configPath,
		ObservedAt:          time.Now().UTC(),
	}
	if err := store.PrepareConfigurationBaseline(context.Background(), input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, _, err := store.AdoptConfigurationBaseline(context.Background(), input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	expected := store.DatabaseIdentity()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	replacementPath := filepath.Join(root, "replacement.db")
	replacement, err := openWithSupportedSchema(replacementPath, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	displacedPath := filepath.Join(root, "proved.db")
	opened, err := openConfigurationAuthority(context.Background(), path, configPath, expected, false, openPinnedStoreHooks{beforeEffects: func() {
		if renameErr := os.Rename(path, displacedPath); renameErr != nil {
			t.Fatal(renameErr)
		}
		if renameErr := os.Rename(replacementPath, path); renameErr != nil {
			t.Fatal(renameErr)
		}
	}})
	if opened != nil {
		opened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replacement open err=%v", err)
	}

	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil || version != 30 {
		t.Fatalf("replacement schema version=%d err=%v", version, err)
	}
}

func TestConfigurationAuthorityOpenRejectsConnectionInodeABASwap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "controller.json")
	input := application.ConfigurationBaselineInput{
		Candidate: application.ValidatedConfigurationCandidate{
			Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: path,
			Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"},
		},
		CanonicalConfigPath: configPath,
		ObservedAt:          time.Now().UTC(),
	}
	if err := store.PrepareConfigurationBaseline(context.Background(), input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, _, err := store.AdoptConfigurationBaseline(context.Background(), input); err != nil {
		store.Close()
		t.Fatal(err)
	}
	expected := store.DatabaseIdentity()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	replacementPath := filepath.Join(root, "replacement.db")
	replacement, err := openWithSupportedSchema(replacementPath, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	displacedPath := filepath.Join(root, "proved.db")
	opened, err := openConfigurationAuthority(context.Background(), path, configPath, expected, false, openPinnedStoreHooks{
		beforeConnectionOpen: func() {
			if renameErr := os.Rename(path, displacedPath); renameErr != nil {
				t.Fatal(renameErr)
			}
			if renameErr := os.Rename(replacementPath, path); renameErr != nil {
				t.Fatal(renameErr)
			}
		},
		afterConnectionOpen: func() {
			if renameErr := os.Rename(path, replacementPath); renameErr != nil {
				t.Fatal(renameErr)
			}
			if renameErr := os.Rename(displacedPath, path); renameErr != nil {
				t.Fatal(renameErr)
			}
		},
	})
	if opened != nil {
		opened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("ABA connection open err=%v", err)
	}

	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: replacementPath}).String()+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil || version != 30 {
		t.Fatalf("ABA replacement schema version=%d err=%v", version, err)
	}
}

func TestPinnedStoreRejectsReplacementOnIdleConnectionReuse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Rename(path, filepath.Join(root, "original.db")); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SchemaVersion(context.Background()); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("idle connection reuse err=%v", err)
	}
}

func TestPinnedStoreRejectsModeChangeOnIdleConnectionReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SchemaVersion(context.Background()); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("mode-change reuse err=%v", err)
	}
}

func TestPinnedRowsRejectReplacementBeforeConsumption(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.db.QueryContext(context.Background(), `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if err := os.Rename(path, filepath.Join(root, "original.db")); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("row consumption crossed database pathname replacement")
	}
	if err := rows.Err(); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("row consumption err=%v", err)
	}
}

func TestPinnedTransactionRejectsReplacementBeforeCommit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(context.Background(), `CREATE TABLE replacement_guard_probe (value TEXT)`); err != nil {
		t.Fatal(err)
	}
	displaced := filepath.Join(root, "original.db")
	if err := os.Rename(path, displaced); err != nil {
		t.Fatal(err)
	}
	replacement, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("transaction commit err=%v", err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: displaced}).String()+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='replacement_guard_probe'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back table count=%d err=%v", count, err)
	}
}

func TestPinnedTransactionRejectsReplacementBeforeCommitReturns(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "controller.db")
	displaced := filepath.Join(root, "original.db")
	replacementPath := filepath.Join(root, "replacement.db")
	armed := false
	store, err := openPinnedStore(context.Background(), path, schemaVersion, true, application.DatabaseFileIdentity{}, nil, openPinnedStoreHooks{afterTransactionCommit: func() {
		if !armed {
			return
		}
		armed = false
		if err := os.Rename(path, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacementPath, path); err != nil {
			t.Fatal(err)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	replacement, err := Open(replacementPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(context.Background(), `CREATE TABLE post_commit_guard_probe (value TEXT)`); err != nil {
		t.Fatal(err)
	}
	armed = true
	if err := tx.Commit(); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("post-commit identity err=%v", err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='post_commit_guard_probe'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("replacement table count=%d err=%v", count, err)
	}
}

func TestSchemaV31WithoutConfigurationAuthorityRejectsDirectAdmission(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "unfenced-run", IssueID: "IFAN-1", IdempotencyKey: "unfenced-run", SourceRevision: "source", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{}`, BaseBranch: "main", WorkingBranch: "ifan/unfenced"}}
	if _, created, err := store.CreateRun(context.Background(), input); !errors.Is(err, application.ErrConfigurationAuthorityConflict) || created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), "missing-authority-fixture", time.Minute, time.Now().UTC())
	if err != nil || !acquired {
		t.Fatalf("lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	reservation := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174031", "unfenced-reservation", "IFAN-31", lease)
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(context.Background(), reservation); !errors.Is(err, application.ErrConfigurationAuthorityConflict) || reserved {
		t.Fatalf("reserved=%t err=%v", reserved, err)
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
	input := application.ConfigurationBaselineInput{
		Candidate:           application.ValidatedConfigurationCandidate{Digest: desired, Size: 42, SchemaVersion: 5, DatabasePath: filepath.Join(t.TempDir(), "owned.db"), Operator: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}},
		CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"),
		ObservedAt:          now,
	}
	if err := store.PrepareConfigurationBaseline(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	authority, _, err := store.AdoptConfigurationBaseline(context.Background(), input)
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
	input := application.ConfigurationBaselineInput{
		Candidate:           application.ValidatedConfigurationCandidate{Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator},
		CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"), ObservedAt: now,
	}
	if err := store.PrepareConfigurationBaseline(ctx, input); err != nil {
		t.Fatal(err)
	}
	baseline, _, err := store.AdoptConfigurationBaseline(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	identitySum := sha256.Sum256([]byte(strings.ToLower(operator.Login) + "\x00" + hex.EncodeToString([]byte(operator.NodeID)) + "\x00" + operator.ActorType + "\x00" + strconv.FormatInt(operator.DatabaseID, 10)))
	targetBinding := hex.EncodeToString(identitySum[:])
	candidateDigest := strings.Repeat("b", 64)
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{
		OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID,
		Requester: operator, RequestDigest: candidateDigest, ExpectedAuthorityDigest: baseline.Desired.Digest,
		OperationAnchorDigest: application.ConfigurationEvidenceDigest("configuration-apply", strconv.FormatInt(baseline.Desired.GenerationID, 10), baseline.Desired.Digest, candidateDigest), TargetBindingDigest: targetBinding, AcceptedAt: now.Add(time.Second),
	})
	generation, _, _, err := store.BeginConfigurationApply(ctx, application.ConfigurationApplyAcceptance{
		ExpectedGenerationID: baseline.Desired.GenerationID, ExpectedDigest: baseline.Desired.Digest,
		Candidate: application.ValidatedConfigurationCandidate{Digest: candidateDigest, Size: 43, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator},
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
	nextReceipt := application.NewOperationReceipt(application.OperationReceiptInput{
		OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID,
		Requester: operator, RequestDigest: strings.Repeat("f", 64), ExpectedAuthorityDigest: settled.Desired.Digest,
		OperationAnchorDigest: strings.Repeat("1", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: now.Add(3 * time.Second),
	})
	next, _, _, err := store.BeginConfigurationApply(ctx, application.ConfigurationApplyAcceptance{
		ExpectedGenerationID: settled.Desired.GenerationID, ExpectedDigest: settled.Desired.Digest,
		Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("f", 64), Size: 44, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator},
		Requester: operator, Receipt: nextReceipt, AcceptedAt: nextReceipt.AcceptedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.SettleConfigurationApply(ctx, application.ConfigurationApplySettlement{GenerationID: next.GenerationID, ParentID: next.ParentID, OperationID: next.OperationID, Outcome: application.ConfigurationApplyCommitted, Reason: application.ConfigurationReasonRestartRequired, EvidenceDigest: strings.Repeat("2", 64), SettledAt: now.Add(4 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	findGeneration := func() application.ConfigurationGeneration {
		generations, listErr := store.ListConfigurationGenerations(ctx)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, item := range generations {
			if item.GenerationID == generation.GenerationID {
				return item
			}
		}
		t.Fatal("superseded generation is missing")
		return application.ConfigurationGeneration{}
	}
	if source := findGeneration(); !source.SettlementEvidenceValid || !source.EffectiveAt.IsZero() {
		t.Fatalf("source settlement evidence=%+v", source)
	}
	if _, err := store.db.Exec(`UPDATE operation_receipts SET requester_login='contradictory-operator' WHERE operation_id=?`, generation.OperationID); err != nil {
		t.Fatal(err)
	}
	if source := findGeneration(); source.SettlementEvidenceValid {
		t.Fatalf("contradictory receipt requester remained valid: %+v", source)
	}
	currentAuthority, found, authorityErr := store.ConfigurationAuthority(ctx)
	if authorityErr != nil || !found {
		t.Fatalf("current authority found=%t err=%v", found, authorityErr)
	}
	settings := configurationDraftSettings()
	if _, _, openErr := store.OpenConfigurationDraft(ctx, application.ConfigurationDraftOpenInput{DraftID: "configuration-draft-00000000000000000000000000000031", BaseGenerationID: currentAuthority.Desired.GenerationID, BaseDigest: currentAuthority.Desired.Digest, Settings: settings, SettingsDigest: application.ConfigurationSettingsDigest(settings), DraftOrigin: application.ConfigurationDraftOriginRollback, RollbackSourceGenerationID: generation.GenerationID, RollbackSourceDigest: generation.Digest, OpenedAt: now.Add(5 * time.Second)}); !errors.Is(openErr, application.ErrConfigurationAuthorityConflict) {
		t.Fatalf("rollback draft accepted contradictory receipt requester: %v", openErr)
	}
	if _, err := store.db.Exec(`UPDATE operation_receipts SET requester_login=? WHERE operation_id=?`, operator.Login, generation.OperationID); err != nil {
		t.Fatal(err)
	}
	if source := findGeneration(); !source.SettlementEvidenceValid {
		t.Fatalf("restored receipt evidence remained invalid: %+v", source)
	}
	if _, err := store.db.Exec(`UPDATE operation_receipts SET operation_anchor_digest=? WHERE operation_id=?`, strings.Repeat("0", 64), generation.OperationID); err != nil {
		t.Fatal(err)
	}
	if source := findGeneration(); source.SettlementEvidenceValid {
		t.Fatalf("contradictory receipt identity remained valid: %+v", source)
	}
	if _, err := store.db.Exec(`UPDATE operation_receipts SET operation_anchor_digest=? WHERE operation_id=?`, receipt.OperationAnchorDigest, generation.OperationID); err != nil {
		t.Fatal(err)
	}
	if source := findGeneration(); !source.SettlementEvidenceValid {
		t.Fatalf("restored receipt identity remained invalid: %+v", source)
	}
	if _, err := store.db.Exec(`UPDATE configuration_apply_intents SET target_digest=? WHERE generation_id=?`, strings.Repeat("0", 64), generation.GenerationID); err != nil {
		t.Fatal(err)
	}
	if source := findGeneration(); source.SettlementEvidenceValid {
		t.Fatalf("contradictory intent remained valid: %+v", source)
	}
}

func TestConfigurationRawPruneClaimAndApplyAcceptanceAreMutuallyExclusive(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	databasePath := filepath.Join(t.TempDir(), "owned.db")
	input := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator}, CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"), ObservedAt: now}
	if err := store.PrepareConfigurationBaseline(ctx, input); err != nil {
		t.Fatal(err)
	}
	authority, _, err := store.AdoptConfigurationBaseline(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	anchorIndex := 0
	begin := func(digest string, at time.Time) (application.ConfigurationGeneration, error) {
		anchorIndex++
		anchor := strings.Repeat(string(rune('0'+anchorIndex)), 64)
		receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: operator, RequestDigest: digest, ExpectedAuthorityDigest: authority.Desired.Digest, OperationAnchorDigest: anchor, TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: at})
		generation, _, _, beginErr := store.BeginConfigurationApply(ctx, application.ConfigurationApplyAcceptance{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Candidate: application.ValidatedConfigurationCandidate{Digest: digest, Size: 43, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator}, Requester: operator, Receipt: receipt, AcceptedAt: at})
		return generation, beginErr
	}
	commit := func(generation application.ConfigurationGeneration, at time.Time) {
		settled, _, _, settleErr := store.SettleConfigurationApply(ctx, application.ConfigurationApplySettlement{GenerationID: generation.GenerationID, ParentID: generation.ParentID, OperationID: generation.OperationID, Outcome: application.ConfigurationApplyCommitted, Reason: application.ConfigurationReasonRestartRequired, EvidenceDigest: strings.Repeat("9", 64), SettledAt: at})
		if settleErr != nil {
			t.Fatal(settleErr)
		}
		authority = settled
	}
	oldDigest := strings.Repeat("b", 64)
	old, err := begin(oldDigest, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	commit(old, now.Add(2*time.Second))
	current, err := begin(strings.Repeat("c", 64), now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	commit(current, now.Add(4*time.Second))
	claimed, err := store.ClaimConfigurationRawPrune(ctx, oldDigest)
	if err != nil || !claimed {
		t.Fatalf("claimed=%t err=%v", claimed, err)
	}
	if _, err := begin(oldDigest, now.Add(5*time.Second)); !errors.Is(err, application.ErrConfigurationAuthorityConflict) {
		t.Fatalf("apply during prune claim error=%v", err)
	}
	if err := store.CompleteConfigurationRawPrune(ctx, oldDigest, true); err != nil {
		t.Fatal(err)
	}
	accepted, err := begin(strings.Repeat("e", 64), now.Add(6*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimConfigurationRawPrune(ctx, accepted.Digest); err != nil || claimed {
		t.Fatalf("accepted digest prune claimed=%t err=%v", claimed, err)
	}
}

func TestConfigurationApplyAndAdmissionShareSQLiteCASAuthority(t *testing.T) {
	newReady := func(t *testing.T) (*Store, application.ConfigurationAuthority, domain.GitHubUserIdentity, string) {
		t.Helper()
		store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		now := time.Now().UTC()
		operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
		databasePath := filepath.Join(t.TempDir(), "owned.db")
		input := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("a", 64), Size: 42, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator}, CanonicalConfigPath: filepath.Join(t.TempDir(), "controller.json"), ObservedAt: now}
		if err := store.PrepareConfigurationBaseline(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		authority, _, err := store.AdoptConfigurationBaseline(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		authority, _, err = store.ObserveConfigurationEffective(context.Background(), application.ConfigurationEffectiveObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, WorkerInstanceID: "worker", BuildIdentity: "build", ObservedAt: now.Add(time.Second), EvidenceDigest: strings.Repeat("e", 64)})
		if err != nil {
			t.Fatal(err)
		}
		return store, authority, operator, databasePath
	}
	beginApply := func(t *testing.T, store *Store, authority application.ConfigurationAuthority, operator domain.GitHubUserIdentity, databasePath string) error {
		t.Helper()
		now := time.Now().UTC()
		receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: operator, RequestDigest: strings.Repeat("b", 64), ExpectedAuthorityDigest: authority.Desired.Digest, OperationAnchorDigest: strings.Repeat("c", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: now})
		_, _, _, err := store.BeginConfigurationApply(context.Background(), application.ConfigurationApplyAcceptance{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Candidate: application.ValidatedConfigurationCandidate{Digest: strings.Repeat("b", 64), Size: 43, SchemaVersion: 5, DatabasePath: databasePath, Operator: operator, Repositories: map[string]application.ConfigurationRepositoryAuthority{}}, Requester: operator, Receipt: receipt, AcceptedAt: now})
		return err
	}

	t.Run("accepted apply invalidates admission token", func(t *testing.T) {
		store, authority, operator, databasePath := newReady(t)
		token := application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Now().UTC().Add(time.Minute)}
		if err := beginApply(t, store, authority, operator, databasePath); err != nil {
			t.Fatal(err)
		}
		_, created, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: "run-after-apply", IssueID: "IFAN-1", IdempotencyKey: "run-after-apply", SourceRevision: "source", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{}`, BaseBranch: "main", WorkingBranch: "ifan/run"}, ConfigurationAuthority: token})
		if err == nil || created {
			t.Fatalf("created=%t err=%v", created, err)
		}
	})

	t.Run("expired runtime evidence invalidates admission token", func(t *testing.T) {
		store, authority, _, _ := newReady(t)
		token := application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Now().UTC().Add(-time.Second)}
		_, created, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: "run-after-expiry", IssueID: "IFAN-3", IdempotencyKey: "run-after-expiry", SourceRevision: "source", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{}`, BaseBranch: "main", WorkingBranch: "ifan/run"}, ConfigurationAuthority: token})
		if err == nil || created {
			t.Fatalf("created=%t err=%v", created, err)
		}
	})

	t.Run("durable drift invalidates prior admission token", func(t *testing.T) {
		store, authority, _, _ := newReady(t)
		token := application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Now().UTC().Add(time.Minute)}
		if _, err := store.ObserveConfigurationDrift(context.Background(), application.ConfigurationDriftObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, ObservedDigest: strings.Repeat("f", 64), Drifted: true, Reason: application.ConfigurationReasonExternalDrift, ObservedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		_, created, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: "run-after-drift", IssueID: "IFAN-4", IdempotencyKey: "run-after-drift", SourceRevision: "source", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{}`, BaseBranch: "main", WorkingBranch: "ifan/run"}, ConfigurationAuthority: token})
		if err == nil || created {
			t.Fatalf("created=%t err=%v", created, err)
		}
	})

	t.Run("cleared drift does not revive prior admission token", func(t *testing.T) {
		store, authority, _, _ := newReady(t)
		token := application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Now().UTC().Add(time.Minute)}
		entered := application.ConfigurationDriftObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, ObservedDigest: strings.Repeat("f", 64), Drifted: true, Reason: application.ConfigurationReasonExternalDrift, ObservedAt: time.Now().UTC()}
		if _, err := store.ObserveConfigurationDrift(context.Background(), entered); err != nil {
			t.Fatal(err)
		}
		cleared := entered
		cleared.Drifted, cleared.Reason, cleared.ObservedDigest, cleared.ObservedAt = false, application.ConfigurationReasonReady, authority.Desired.Digest, entered.ObservedAt.Add(time.Second)
		if _, err := store.ObserveConfigurationDrift(context.Background(), cleared); err != nil {
			t.Fatal(err)
		}
		_, created, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: "run-after-drift-clear", IssueID: "IFAN-5", IdempotencyKey: "run-after-drift-clear", SourceRevision: "source", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{}`, BaseBranch: "main", WorkingBranch: "ifan/run"}, ConfigurationAuthority: token})
		if err == nil || created {
			t.Fatalf("created=%t err=%v", created, err)
		}
	})

	t.Run("accepted admission is visible to apply transaction", func(t *testing.T) {
		store, authority, operator, databasePath := newReady(t)
		token := application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Now().UTC().Add(time.Minute)}
		_, created, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: application.Run{ID: "run-before-apply", IssueID: "IFAN-2", IdempotencyKey: "run-before-apply", SourceRevision: "source", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: `{}`, BaseBranch: "main", WorkingBranch: "ifan/run"}, ConfigurationAuthority: token})
		if err != nil || !created {
			t.Fatalf("created=%t err=%v", created, err)
		}
		if err := beginApply(t, store, authority, operator, databasePath); err == nil {
			t.Fatal("apply ignored concurrently admitted run")
		}
	})
}
