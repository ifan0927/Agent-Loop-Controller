package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/git"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestCleanupSourceRecoveryIntentReceiptAndSettlementAreAtomicAndReplayable(t *testing.T) {
	store, intent, receipt := cleanupSourceRecoveryStoreFixture(t)
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	var databasePath string
	if err := store.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&databasePath); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, intent.Authority.RunID)
	if err != nil {
		t.Fatal(err)
	}
	profile := repositoryProfileFixture(run.Repository, "b", "c")
	profile.Profile.ProfileID = run.ProfileID
	profile.Profile.ProfileDigest = run.ProfileDigest
	profile.Profile.RepositoryBindingDigest = run.RepositoryBindingDigest
	profile.Authority.ProfileID = run.ProfileID
	profile.Authority.BindingDigest = run.RepositoryBindingDigest
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: intent.CreatedAt.Add(-3 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	repositoryAuthority, err := store.RepositoryOperationAuthority(ctx, run.Repository)
	if err != nil {
		t.Fatal(err)
	}
	disable := repositoryReceiptFixture(application.OperationDisableRepository, repositoryAuthority, profile, intent.CreatedAt.Add(-2*time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, disable); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: disable.OperationID, Expected: repositoryAuthority, Intent: application.RepositoryDisabled, ChangedAt: intent.CreatedAt.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	repositoryAuthority, err = store.RepositoryOperationAuthority(ctx, run.Repository)
	if err != nil {
		t.Fatal(err)
	}
	beforeGuards, err := store.EvaluateRepositoryRemovalGuards(ctx, repositoryAuthority, 1, intent.CreatedAt)
	if err != nil || guardAllowed(beforeGuards, "cleanup_settled") {
		t.Fatalf("before guards=%+v err=%v", beforeGuards, err)
	}
	var generationBefore, generationAfter int64
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}
	persisted, acceptedReceipt, created, err := store.BeginCleanupSourceRecovery(ctx, intent, receipt)
	if err != nil || !created || persisted.Stage != application.CleanupSourceRecoveryAccepted || acceptedReceipt.Phase != application.OperationPhaseAccepted {
		t.Fatalf("intent=%+v receipt=%+v created=%v err=%v", persisted, acceptedReceipt, created, err)
	}
	if err := store.db.QueryRow(`SELECT generation FROM controller_integrity_generation WHERE singleton=1`).Scan(&generationAfter); err != nil || generationAfter <= generationBefore {
		t.Fatalf("integrity generation before=%d after=%d err=%v", generationBefore, generationAfter, err)
	}
	replayed, replayReceipt, created, err := store.BeginCleanupSourceRecovery(ctx, intent, receipt)
	if err != nil || created || replayed.RequestID != intent.RequestID || replayReceipt.OperationID != receipt.OperationID {
		t.Fatalf("replay=%+v receipt=%+v created=%v err=%v", replayed, replayReceipt, created, err)
	}
	stages := []application.CleanupSourceRecoveryStage{application.CleanupSourceRecoveryRepairIntent, application.CleanupSourceRecoveryRepairObserved, application.CleanupSourceRecoveryDetachIntent, application.CleanupSourceRecoveryDetachObserved, application.CleanupSourceRecoveryCleanupIntent, application.CleanupSourceRecoveryCleanupObserved}
	expected := application.CleanupSourceRecoveryAccepted
	for index, next := range stages {
		var changed bool
		persisted, acceptedReceipt, changed, err = store.AdvanceCleanupSourceRecovery(ctx, intent.RequestID, expected, next, intent.CreatedAt.Add(time.Duration(index+1)*time.Second))
		if err != nil || !changed || persisted.Stage != next {
			t.Fatalf("stage=%s persisted=%+v changed=%v err=%v", next, persisted, changed, err)
		}
		expected = next
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err = Open(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		reopened, reopenedReceipt, found, reopenErr := store.GetCleanupSourceRecovery(ctx, intent.RequestID)
		if reopenErr != nil || !found || reopened.Stage != next || reopenedReceipt.OperationID != receipt.OperationID {
			t.Fatalf("restart stage=%s intent=%+v found=%v err=%v", next, reopened, found, reopenErr)
		}
	}
	if acceptedReceipt.Phase != application.OperationPhaseApplied || acceptedReceipt.Outcome != application.OperationOutcomePending {
		t.Fatalf("applied receipt=%+v", acceptedReceipt)
	}
	if _, _, changed, err := store.AdvanceCleanupSourceRecovery(ctx, intent.RequestID, application.CleanupSourceRecoveryCleanupObserved, application.CleanupSourceRecoverySucceeded, intent.CreatedAt.Add(9*time.Second)); err == nil || changed {
		t.Fatalf("generic final advance changed=%v err=%v", changed, err)
	}
	unchangedIntent, unchangedReceipt, found, err := store.GetCleanupSourceRecovery(ctx, intent.RequestID)
	if err != nil || !found || unchangedIntent.Stage != application.CleanupSourceRecoveryCleanupObserved || unchangedReceipt.Phase != application.OperationPhaseApplied || unchangedReceipt.Outcome != application.OperationOutcomePending {
		t.Fatalf("intent=%+v receipt=%+v found=%v err=%v", unchangedIntent, unchangedReceipt, found, err)
	}
	unchangedInspection, err := store.Inspect(ctx, intent.Authority.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range unchangedInspection.Cleanup {
		if item.Kind == "worktree" && item.Status == "deleted" {
			t.Fatal("generic final advance settled worktree cleanup")
		}
	}
	persisted, acceptedReceipt, changed, err := store.SettleCleanupSourceRecovery(ctx, intent.RequestID, intent.CreatedAt.Add(10*time.Second))
	if err != nil || !changed || persisted.Stage != application.CleanupSourceRecoverySucceeded || acceptedReceipt.Phase != application.OperationPhaseObserved || acceptedReceipt.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("settled=%+v receipt=%+v changed=%v err=%v", persisted, acceptedReceipt, changed, err)
	}
	inspection, err := store.Inspect(ctx, intent.Authority.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range inspection.Cleanup {
		if (item.Kind == "worktree" || item.Kind == "branch") && item.Status != "deleted" {
			t.Fatalf("cleanup=%+v", inspection.Cleanup)
		}
	}
	for _, item := range inspection.Resources {
		if (item.Kind == "worktree" || item.Kind == "branch") && item.Status != "deleted" {
			t.Fatalf("resources=%+v", inspection.Resources)
		}
	}
	afterGuards, err := store.EvaluateRepositoryRemovalGuards(ctx, repositoryAuthority, 1, intent.CreatedAt.Add(10*time.Second))
	if err != nil || !allGuardsAllowed(afterGuards) {
		t.Fatalf("after guards=%+v err=%v", afterGuards, err)
	}
	persisted, acceptedReceipt, changed, err = store.SettleCleanupSourceRecovery(ctx, intent.RequestID, intent.CreatedAt.Add(11*time.Second))
	if err != nil || changed || persisted.Stage != application.CleanupSourceRecoverySucceeded || acceptedReceipt.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("settlement replay=%+v changed=%v err=%v", persisted, changed, err)
	}
}

func TestCleanupSourceRecoveryDifferentAndConcurrentRequestIDsConflictTyped(t *testing.T) {
	t.Run("different request", func(t *testing.T) {
		store, intent, receipt := cleanupSourceRecoveryStoreFixture(t)
		defer store.Close()
		if _, _, created, err := store.BeginCleanupSourceRecovery(context.Background(), intent, receipt); err != nil || !created {
			t.Fatalf("first created=%v err=%v", created, err)
		}
		secondIntent, secondReceipt := alternateCleanupSourceRecoveryAcceptance(intent, receipt, "different-request", intent.CreatedAt.Add(time.Second))
		if _, _, created, err := store.BeginCleanupSourceRecovery(context.Background(), secondIntent, secondReceipt); !errors.Is(err, application.ErrOperationReceiptConflict) || created {
			t.Fatalf("second created=%v err=%v", created, err)
		}
		var intents, receipts int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM cleanup_source_recovery_intents WHERE run_id=?`, intent.Authority.RunID).Scan(&intents); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM operation_receipts WHERE operation_type='recover_cleanup_source' AND target_id=?`, intent.Authority.RunID).Scan(&receipts); err != nil {
			t.Fatal(err)
		}
		if intents != 1 || receipts != 1 {
			t.Fatalf("intents=%d receipts=%d", intents, receipts)
		}
	})

	t.Run("concurrent request", func(t *testing.T) {
		store, firstIntent, firstReceipt := cleanupSourceRecoveryStoreFixture(t)
		defer store.Close()
		secondIntent, secondReceipt := alternateCleanupSourceRecoveryAcceptance(firstIntent, firstReceipt, "concurrent-request", firstIntent.CreatedAt.Add(time.Second))
		type result struct {
			created bool
			err     error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		var group sync.WaitGroup
		for _, acceptance := range []struct {
			intent  application.CleanupSourceRecoveryIntent
			receipt application.OperationReceipt
		}{{firstIntent, firstReceipt}, {secondIntent, secondReceipt}} {
			group.Add(1)
			go func(value struct {
				intent  application.CleanupSourceRecoveryIntent
				receipt application.OperationReceipt
			}) {
				defer group.Done()
				<-start
				_, _, created, err := store.BeginCleanupSourceRecovery(context.Background(), value.intent, value.receipt)
				results <- result{created: created, err: err}
			}(acceptance)
		}
		close(start)
		group.Wait()
		close(results)
		created, conflicts := 0, 0
		for value := range results {
			if value.created && value.err == nil {
				created++
			} else if errors.Is(value.err, application.ErrOperationReceiptConflict) {
				conflicts++
			} else {
				t.Fatalf("unexpected concurrent result created=%v err=%v", value.created, value.err)
			}
		}
		if created != 1 || conflicts != 1 {
			t.Fatalf("created=%d conflicts=%d", created, conflicts)
		}
	})

	t.Run("nonconstraint receipt failure", func(t *testing.T) {
		store, intent, receipt := cleanupSourceRecoveryStoreFixture(t)
		defer store.Close()
		if _, err := store.db.Exec(`CREATE TRIGGER cleanup_source_receipt_fault BEFORE INSERT ON operation_receipts WHEN NEW.operation_type='recover_cleanup_source' BEGIN SELECT cleanup_source_missing_function(); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, created, err := store.BeginCleanupSourceRecovery(context.Background(), intent, receipt); err == nil || errors.Is(err, application.ErrOperationReceiptConflict) || created {
			t.Fatalf("created=%v err=%v", created, err)
		}
		var intents int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM cleanup_source_recovery_intents`).Scan(&intents); err != nil || intents != 0 {
			t.Fatalf("intents=%d err=%v", intents, err)
		}
	})
}

func alternateCleanupSourceRecoveryAcceptance(intent application.CleanupSourceRecoveryIntent, original application.OperationReceipt, requestID string, at time.Time) (application.CleanupSourceRecoveryIntent, application.OperationReceipt) {
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRecoverCleanupSource, Scope: application.ScopeRun, TargetID: intent.Authority.RunID, Requester: intent.Requester, RequestDigest: application.SHA256Digest("alternate-cleanup-source-request-v1", requestID), ExpectedAuthorityDigest: application.CleanupSourceRecoveryExpectedAuthorityDigest(intent.Authority), OperationAnchorDigest: original.OperationAnchorDigest, TargetBindingDigest: intent.Authority.RepositoryBindingDigest, AcceptedAt: at})
	intent.RequestID, intent.OperationID, intent.CreatedAt, intent.UpdatedAt = requestID, receipt.OperationID, at, at
	return intent, receipt
}

func guardAllowed(guards []application.RepositoryRemovalGuardResult, name string) bool {
	for _, guard := range guards {
		if guard.Guard == name {
			return guard.Allowed
		}
	}
	return false
}

func TestCleanupSourceRecoveryAcceptanceRejectsAuthorityAndProcessDrift(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*Store, application.CleanupSourceRecoveryIntent)
	}{
		{"active lease", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE runs SET lease_owner='other',lease_expires_unix=? WHERE run_id=?`, time.Now().Add(time.Hour).UnixNano(), intent.Authority.RunID)
		}},
		{"expired owned lease", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE runs SET lease_owner='other',lease_expires_unix=? WHERE run_id=?`, time.Now().Add(-time.Hour).UnixNano(), intent.Authority.RunID)
		}},
		{"started process", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`INSERT INTO attempts(run_id,number,kind,status,started_at,artifact_dir,process_control_key) VALUES(?,1,'implementation','started',?,?,'key')`, intent.Authority.RunID, nowText(), filepath.Join(t.TempDir(), "attempt"))
		}},
		{"attention drift", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE operator_attention_outbox SET evidence_digest=? WHERE event_key=?`, strings.Repeat("f", 64), intent.Authority.AttentionEventKey)
		}},
		{"abandon action drift", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE operator_actions SET outcome_digest=? WHERE run_id=? AND action_type='abandon'`, strings.Repeat("f", 64), intent.Authority.RunID)
		}},
		{"cleanup drift", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE cleanup_results SET status='deleted' WHERE run_id=? AND resource_kind='worktree'`, intent.Authority.RunID)
		}},
		{"cleanup diagnostic drift", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE cleanup_results SET last_error='changed diagnostic' WHERE run_id=? AND resource_kind='worktree'`, intent.Authority.RunID)
		}},
		{"ownership drift", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE owned_resources SET ownership_status='owned' WHERE owning_run=? AND resource_kind='branch'`, intent.Authority.RunID)
		}},
		{"source checkout attention", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE cleanup_results SET status='skipped_attention',error_class='dirty_source',last_error='operator attention required' WHERE run_id=? AND resource_kind='source_checkout'`, intent.Authority.RunID)
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			store, intent, receipt := cleanupSourceRecoveryStoreFixture(t)
			defer store.Close()
			test.mutate(store, intent)
			if _, _, created, err := store.BeginCleanupSourceRecovery(context.Background(), intent, receipt); err == nil || created {
				t.Fatalf("created=%v err=%v", created, err)
			}
		})
	}
}

func TestCleanupSourceRecoveryPreviewRejectsConfiguredOperatorAndActiveExecution(t *testing.T) {
	ctx := context.Background()
	store, intent, _ := cleanupSourceRecoveryStoreFixture(t)
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: intent.Requester})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewCleanupSourceRecoveryService(store, cleanupSourceRecoveryGitObservation{}, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	request := application.CleanupSourceRecoveryPreviewRequest{Requester: requesterFromIdentity(intent.Requester), RunID: intent.Authority.RunID, ReplacementSourcePath: filepath.Join(t.TempDir(), "replacement")}
	if preview, err := service.Preview(ctx, request); err != nil || !preview.Eligible {
		store.Close()
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	service, err = application.NewCleanupSourceRecoveryService(store, cleanupSourceRecoveryGitObservation{repaired: true}, authorizer)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := service.Preview(ctx, request); err == nil {
		store.Close()
		t.Fatal("already repaired worktree received a preview")
	}
	store.Close()

	wrong := domain.GitHubUserIdentity{Login: "other", DatabaseID: 2, NodeID: "U_2", ActorType: "User"}
	store, intent, _ = cleanupSourceRecoveryStoreFixture(t)
	authorizer, _ = application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: wrong})
	service, _ = application.NewCleanupSourceRecoveryService(store, cleanupSourceRecoveryGitObservation{}, authorizer)
	request = application.CleanupSourceRecoveryPreviewRequest{Requester: requesterFromIdentity(intent.Requester), RunID: intent.Authority.RunID, ReplacementSourcePath: filepath.Join(t.TempDir(), "replacement")}
	if _, err := service.Preview(ctx, request); err == nil {
		store.Close()
		t.Fatal("non-configured requester received an eligible preview")
	}
	store.Close()

	mutations := []struct {
		name   string
		mutate func(*Store, application.CleanupSourceRecoveryIntent)
	}{
		{"run lease", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE runs SET lease_owner='worker',lease_expires_unix=? WHERE run_id=?`, time.Now().Add(time.Hour).UnixNano(), intent.Authority.RunID)
		}},
		{"expired owned lease", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE runs SET lease_owner='worker',lease_expires_unix=? WHERE run_id=?`, time.Now().Add(-time.Hour).UnixNano(), intent.Authority.RunID)
		}},
		{"started process", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`INSERT INTO attempts(run_id,number,kind,status,started_at,artifact_dir,process_control_key) VALUES(?,1,'implementation','started',?,?,'process-key')`, intent.Authority.RunID, nowText(), filepath.Join(t.TempDir(), "attempt"))
		}},
		{"repository slot", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`INSERT INTO repository_slots(repository_binding_digest,run_id,version,acquired_at) VALUES(?,?,1,?)`, intent.Authority.RepositoryBindingDigest, intent.Authority.RunID, nowText())
		}},
		{"heavy permit", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`INSERT INTO heavy_permits(run_id,owner_nonce,version,acquired_at,updated_at) VALUES(?,'worker',1,?,?)`, intent.Authority.RunID, nowText(), nowText())
		}},
		{"nonterminal scheduling", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE run_scheduling SET supervisor_state='running',updated_at=? WHERE run_id=?`, nowText(), intent.Authority.RunID)
		}},
		{"source checkout attention", func(store *Store, intent application.CleanupSourceRecoveryIntent) {
			_, _ = store.db.Exec(`UPDATE cleanup_results SET status='skipped_attention',error_class='dirty_source',last_error='operator attention required' WHERE run_id=? AND resource_kind='source_checkout'`, intent.Authority.RunID)
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			store, intent, _ := cleanupSourceRecoveryStoreFixture(t)
			defer store.Close()
			test.mutate(store, intent)
			authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: intent.Requester})
			service, _ := application.NewCleanupSourceRecoveryService(store, cleanupSourceRecoveryGitObservation{}, authorizer)
			request := application.CleanupSourceRecoveryPreviewRequest{Requester: requesterFromIdentity(intent.Requester), RunID: intent.Authority.RunID, ReplacementSourcePath: filepath.Join(t.TempDir(), "replacement")}
			if _, err := service.Preview(ctx, request); err == nil {
				t.Fatal("active execution authority received an eligible preview")
			}
		})
	}
}

func TestCleanupSourceRecoveryRealGitSQLiteApplyReplayAndRemovalGuard(t *testing.T) {
	ctx := context.Background()
	store, fixtureIntent, _ := cleanupSourceRecoveryStoreFixture(t)
	defer store.Close()
	replacement := installCleanupSourceRecoveryGitTopology(t, store, fixtureIntent.Authority.RunID)
	run, err := store.GetRun(ctx, fixtureIntent.Authority.RunID)
	if err != nil {
		t.Fatal(err)
	}
	profile := repositoryProfileFixture(run.Repository, "b", "c")
	profile.Profile.ProfileID, profile.Authority.ProfileID = run.ProfileID, run.ProfileID
	profile.Profile.ProfileDigest = run.ProfileDigest
	profile.Profile.RepositoryBindingDigest, profile.Authority.BindingDigest = run.RepositoryBindingDigest, run.RepositoryBindingDigest
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: time.Now().Add(-3 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	repositoryAuthority, err := store.RepositoryOperationAuthority(ctx, run.Repository)
	if err != nil {
		t.Fatal(err)
	}
	disable := repositoryReceiptFixture(application.OperationDisableRepository, repositoryAuthority, profile, time.Now().Add(-2*time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, disable); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: disable.OperationID, Expected: repositoryAuthority, Intent: application.RepositoryDisabled, ChangedAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	repositoryAuthority, _ = store.RepositoryOperationAuthority(ctx, run.Repository)
	before, err := store.EvaluateRepositoryRemovalGuards(ctx, repositoryAuthority, 1, time.Now())
	if err != nil || guardAllowed(before, "cleanup_settled") {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: fixtureIntent.Requester})
	service, err := application.NewCleanupSourceRecoveryService(store, cleanupSourceRecoveryRealGit{adapter: gitadapter.CleanupSourceRecovery{}}, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	previewRequest := application.CleanupSourceRecoveryPreviewRequest{Requester: requesterFromIdentity(fixtureIntent.Requester), RunID: run.ID, ReplacementSourcePath: replacement}
	preview, err := service.Preview(ctx, previewRequest)
	if err != nil || !preview.Eligible {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	apply := application.CleanupSourceRecoveryApplyRequest{CleanupSourceRecoveryPreviewRequest: previewRequest, RequestID: "real-git-sqlite-recovery", PreviewDigest: preview.PreviewDigest, SourceRelocationConfirmed: true}
	result, err := service.Apply(ctx, apply)
	if err != nil || result.Receipt.Outcome != application.OperationOutcomeSucceeded || result.RecoveryStage != string(application.CleanupSourceRecoverySucceeded) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	replayed, err := service.Apply(ctx, apply)
	if err != nil || replayed.Receipt.OperationID != result.Receipt.OperationID || replayed.Receipt.ResultDigest != result.Receipt.ResultDigest {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	persisted, _, found, err := store.GetCleanupSourceRecovery(ctx, apply.RequestID)
	if err != nil || !found {
		t.Fatalf("persisted=%+v found=%v err=%v", persisted, found, err)
	}
	after, err := store.EvaluateRepositoryRemovalGuards(ctx, repositoryAuthority, 1, time.Now().Add(time.Second))
	if err != nil || !allGuardsAllowed(after) {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	serialized, _ := json.Marshal(struct {
		Preview application.CleanupSourceRecoveryPreview
		Result  application.CleanupSourceRecoveryResult
		Intent  application.CleanupSourceRecoveryIntent
	}{preview, result, persisted})
	if strings.Contains(string(serialized), replacement) {
		t.Fatal("private replacement path escaped the sanitized projection")
	}
}

func TestCleanupSourceRecoveryDetachAndRemovalResponseLossFailClosed(t *testing.T) {
	t.Run("repair before durable intent is not adopted", func(t *testing.T) {
		store, fixture, receipt := cleanupSourceRecoveryStoreFixture(t)
		defer store.Close()
		if _, _, created, err := store.BeginCleanupSourceRecovery(context.Background(), fixture, receipt); err != nil || !created {
			t.Fatalf("created=%v err=%v", created, err)
		}
		git := &cleanupSourceRecoveryFaultGit{present: true, repaired: true, candidatePresent: true}
		service := cleanupSourceRecoveryServiceForTest(t, store, fixture, git)
		request := application.CleanupSourceRecoveryApplyRequest{CleanupSourceRecoveryPreviewRequest: application.CleanupSourceRecoveryPreviewRequest{Requester: requesterFromIdentity(fixture.Requester), RunID: fixture.Authority.RunID, ReplacementSourcePath: filepath.Join(t.TempDir(), "replacement")}, RequestID: fixture.RequestID, PreviewDigest: fixture.Authority.PreviewDigest, SourceRelocationConfirmed: true}
		if _, err := service.Apply(context.Background(), request); err == nil {
			t.Fatal("repair before durable intent was adopted")
		}
		intent, _, found, err := store.GetCleanupSourceRecovery(context.Background(), fixture.RequestID)
		if err != nil || !found || intent.Stage != application.CleanupSourceRecoveryAccepted {
			t.Fatalf("intent=%+v found=%v err=%v", intent, found, err)
		}
	})

	t.Run("detach response loss is adopted", func(t *testing.T) {
		store, fixture, _ := cleanupSourceRecoveryStoreFixture(t)
		defer store.Close()
		git := &cleanupSourceRecoveryFaultGit{present: true, candidatePresent: true, loseDetachResponse: true}
		service := cleanupSourceRecoveryServiceForTest(t, store, fixture, git)
		request := application.CleanupSourceRecoveryPreviewRequest{Requester: requesterFromIdentity(fixture.Requester), RunID: fixture.Authority.RunID, ReplacementSourcePath: filepath.Join(t.TempDir(), "replacement")}
		preview, err := service.Preview(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		apply := application.CleanupSourceRecoveryApplyRequest{CleanupSourceRecoveryPreviewRequest: request, RequestID: "detach-response-loss", PreviewDigest: preview.PreviewDigest, SourceRelocationConfirmed: true}
		if _, err := service.Apply(context.Background(), apply); err == nil {
			t.Fatal("lost detach response unexpectedly settled")
		}
		intent, _, found, err := store.GetCleanupSourceRecovery(context.Background(), apply.RequestID)
		if err != nil || !found || intent.Stage != application.CleanupSourceRecoveryDetachIntent || !git.detached {
			t.Fatalf("intent=%+v found=%v detached=%v err=%v", intent, found, git.detached, err)
		}
		git.loseDetachResponse = false
		result, err := service.Apply(context.Background(), apply)
		if err != nil || result.Receipt.Outcome != application.OperationOutcomeSucceeded {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("missing candidate after removal prevents settlement", func(t *testing.T) {
		store, fixture, _ := cleanupSourceRecoveryStoreFixture(t)
		defer store.Close()
		git := &cleanupSourceRecoveryFaultGit{present: true, candidatePresent: true, loseRemoveResponseAndObject: true}
		service := cleanupSourceRecoveryServiceForTest(t, store, fixture, git)
		request := application.CleanupSourceRecoveryPreviewRequest{Requester: requesterFromIdentity(fixture.Requester), RunID: fixture.Authority.RunID, ReplacementSourcePath: filepath.Join(t.TempDir(), "replacement")}
		preview, err := service.Preview(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		apply := application.CleanupSourceRecoveryApplyRequest{CleanupSourceRecoveryPreviewRequest: request, RequestID: "missing-object-response-loss", PreviewDigest: preview.PreviewDigest, SourceRelocationConfirmed: true}
		if _, err := service.Apply(context.Background(), apply); err == nil {
			t.Fatal("missing candidate after removal unexpectedly settled")
		}
		if _, err := service.Apply(context.Background(), apply); err == nil {
			t.Fatal("missing candidate replay unexpectedly settled")
		}
		intent, receipt, found, err := store.GetCleanupSourceRecovery(context.Background(), apply.RequestID)
		if err != nil || !found || intent.Stage != application.CleanupSourceRecoveryCleanupIntent || receipt.Outcome != application.OperationOutcomePending {
			t.Fatalf("intent=%+v receipt=%+v found=%v err=%v", intent, receipt, found, err)
		}
		inspection, err := store.Inspect(context.Background(), fixture.Authority.RunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range inspection.Cleanup {
			if item.Kind == "worktree" && item.Status == "deleted" {
				t.Fatal("unproven worktree removal was settled")
			}
		}
	})

	t.Run("recreated branch during detach prevents settlement", func(t *testing.T) {
		store, fixture, _ := cleanupSourceRecoveryStoreFixture(t)
		defer store.Close()
		git := &cleanupSourceRecoveryFaultGit{present: true, candidatePresent: true, recreateBranchOnDetach: true}
		service := cleanupSourceRecoveryServiceForTest(t, store, fixture, git)
		request := application.CleanupSourceRecoveryPreviewRequest{Requester: requesterFromIdentity(fixture.Requester), RunID: fixture.Authority.RunID, ReplacementSourcePath: filepath.Join(t.TempDir(), "replacement")}
		preview, err := service.Preview(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		apply := application.CleanupSourceRecoveryApplyRequest{CleanupSourceRecoveryPreviewRequest: request, RequestID: "recreated-branch-detach", PreviewDigest: preview.PreviewDigest, SourceRelocationConfirmed: true}
		if _, err := service.Apply(context.Background(), apply); err == nil {
			t.Fatal("recreated branch unexpectedly settled")
		}
		intent, receipt, found, err := store.GetCleanupSourceRecovery(context.Background(), apply.RequestID)
		if err != nil || !found || intent.Stage != application.CleanupSourceRecoveryDetachIntent || receipt.Outcome != application.OperationOutcomePending || !git.branchPresent {
			t.Fatalf("intent=%+v receipt=%+v branch=%v found=%v err=%v", intent, receipt, git.branchPresent, found, err)
		}
	})
}

type cleanupSourceRecoveryFaultGit struct {
	present, repaired, detached, branchPresent, candidatePresent bool
	loseDetachResponse, loseRemoveResponseAndObject              bool
	recreateBranchOnDetach                                       bool
}

func (g *cleanupSourceRecoveryFaultGit) ObserveCleanupSourceRecovery(_ context.Context, request application.CleanupSourceRecoveryGitRequest) (application.CleanupSourceRecoveryObservation, error) {
	if !g.candidatePresent {
		return application.CleanupSourceRecoveryObservation{}, errors.New("candidate object missing")
	}
	return application.CleanupSourceRecoveryObservation{ReplacementSourceDigest: strings.Repeat("3", 64), ReplacementIdentityDigest: strings.Repeat("4", 64), RepositoryOriginDigest: strings.Repeat("5", 64), RegistrationDigest: strings.Repeat("6", 64), Branch: request.Branch, CandidateHead: request.CandidateHead, WorktreePresent: g.present, BranchPresent: g.branchPresent, LinkRepaired: g.repaired, HeadDetached: g.detached, WorktreeClean: true}, nil
}

func (g *cleanupSourceRecoveryFaultGit) RepairCleanupWorktreeLink(context.Context, application.CleanupSourceRecoveryGitRequest) error {
	g.repaired = true
	return nil
}

func (g *cleanupSourceRecoveryFaultGit) DetachRecoveredWorktreeHead(context.Context, application.CleanupSourceRecoveryGitRequest) error {
	if g.detached {
		return nil
	}
	g.detached = true
	if g.recreateBranchOnDetach {
		g.branchPresent = true
		return errors.New("candidate branch recreated during detach")
	}
	if g.loseDetachResponse {
		return errors.New("detach response lost")
	}
	return nil
}

func (g *cleanupSourceRecoveryFaultGit) RemoveRecoveredWorktree(context.Context, application.CleanupSourceRecoveryGitRequest) error {
	if !g.present {
		return nil
	}
	g.present = false
	if g.loseRemoveResponseAndObject {
		g.candidatePresent = false
		return errors.New("remove response and candidate object lost")
	}
	return nil
}

func cleanupSourceRecoveryServiceForTest(t *testing.T, store *Store, fixture application.CleanupSourceRecoveryIntent, git application.CleanupSourceRecoveryGitPort) *application.CleanupSourceRecoveryService {
	t.Helper()
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: fixture.Requester})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewCleanupSourceRecoveryService(store, git, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type cleanupSourceRecoveryGitObservation struct{ repaired bool }

type cleanupSourceRecoveryRealGit struct {
	adapter gitadapter.CleanupSourceRecovery
}

func cleanupSourceRecoveryGitAdapterRequest(value application.CleanupSourceRecoveryGitRequest) gitadapter.CleanupSourceRecoveryRequest {
	return gitadapter.CleanupSourceRecoveryRequest{Repository: value.Repository, FrozenSourcePath: value.FrozenSourcePath, ReplacementSourcePath: value.ReplacementSourcePath, ExpectedOrigin: value.ExpectedOrigin, WorktreePath: value.WorktreePath, Branch: value.Branch, CandidateHead: value.CandidateHead, ExpectedRegistrationDigest: value.ExpectedRegistrationDigest}
}

func (a cleanupSourceRecoveryRealGit) ObserveCleanupSourceRecovery(ctx context.Context, request application.CleanupSourceRecoveryGitRequest) (application.CleanupSourceRecoveryObservation, error) {
	value, err := a.adapter.ObserveCleanupSourceRecovery(ctx, cleanupSourceRecoveryGitAdapterRequest(request))
	return application.CleanupSourceRecoveryObservation{ReplacementSourceDigest: value.ReplacementSourceDigest, ReplacementIdentityDigest: value.ReplacementIdentityDigest, RepositoryOriginDigest: value.RepositoryOriginDigest, RegistrationDigest: value.RegistrationDigest, Branch: value.Branch, CandidateHead: value.CandidateHead, LinkRepaired: value.LinkRepaired, HeadDetached: value.HeadDetached, WorktreePresent: value.WorktreePresent, BranchPresent: value.BranchPresent, WorktreeClean: value.WorktreeClean}, err
}

func (a cleanupSourceRecoveryRealGit) RepairCleanupWorktreeLink(ctx context.Context, request application.CleanupSourceRecoveryGitRequest) error {
	return a.adapter.RepairCleanupWorktreeLink(ctx, cleanupSourceRecoveryGitAdapterRequest(request))
}

func (a cleanupSourceRecoveryRealGit) DetachRecoveredWorktreeHead(ctx context.Context, request application.CleanupSourceRecoveryGitRequest) error {
	return a.adapter.DetachRecoveredWorktreeHead(ctx, cleanupSourceRecoveryGitAdapterRequest(request))
}

func (a cleanupSourceRecoveryRealGit) RemoveRecoveredWorktree(ctx context.Context, request application.CleanupSourceRecoveryGitRequest) error {
	return a.adapter.RemoveRecoveredWorktree(ctx, cleanupSourceRecoveryGitAdapterRequest(request))
}

func (g cleanupSourceRecoveryGitObservation) ObserveCleanupSourceRecovery(_ context.Context, request application.CleanupSourceRecoveryGitRequest) (application.CleanupSourceRecoveryObservation, error) {
	return application.CleanupSourceRecoveryObservation{ReplacementSourceDigest: strings.Repeat("3", 64), ReplacementIdentityDigest: strings.Repeat("4", 64), RepositoryOriginDigest: strings.Repeat("5", 64), RegistrationDigest: strings.Repeat("6", 64), Branch: request.Branch, CandidateHead: request.CandidateHead, WorktreePresent: true, BranchPresent: false, LinkRepaired: g.repaired, WorktreeClean: true}, nil
}

func (cleanupSourceRecoveryGitObservation) RepairCleanupWorktreeLink(context.Context, application.CleanupSourceRecoveryGitRequest) error {
	return nil
}

func (cleanupSourceRecoveryGitObservation) DetachRecoveredWorktreeHead(context.Context, application.CleanupSourceRecoveryGitRequest) error {
	return nil
}

func (cleanupSourceRecoveryGitObservation) RemoveRecoveredWorktree(context.Context, application.CleanupSourceRecoveryGitRequest) error {
	return nil
}

func requesterFromIdentity(identity domain.GitHubUserIdentity) application.Requester {
	return application.Requester{ID: identity.Login, Kind: "github_login", DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, ActorType: identity.ActorType}
}

func installCleanupSourceRecoveryGitTopology(t *testing.T, store *Store, runID string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldSource := filepath.Join(root, "old-source")
	replacement := filepath.Join(root, "replacement-source")
	worktree := filepath.Join(root, "owned-worktree")
	runCleanupRecoveryGit(t, "init", "-b", "main", oldSource)
	runCleanupRecoveryGit(t, "-C", oldSource, "config", "user.name", "Fixture")
	runCleanupRecoveryGit(t, "-C", oldSource, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(oldSource, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCleanupRecoveryGit(t, "-C", oldSource, "add", "README.md")
	runCleanupRecoveryGit(t, "-C", oldSource, "commit", "-m", "fixture")
	base := strings.TrimSpace(runCleanupRecoveryGit(t, "-C", oldSource, "rev-parse", "HEAD"))
	origin := "https://github.com/owner/repository.git"
	runCleanupRecoveryGit(t, "-C", oldSource, "remote", "add", "origin", origin)
	branch := "codex/recovery"
	runCleanupRecoveryGit(t, "-C", oldSource, "worktree", "add", "-b", branch, worktree)
	if err := os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCleanupRecoveryGit(t, "-C", worktree, "add", "candidate.txt")
	runCleanupRecoveryGit(t, "-C", worktree, "commit", "-m", "candidate")
	candidate := strings.TrimSpace(runCleanupRecoveryGit(t, "-C", worktree, "rev-parse", "HEAD"))
	runCleanupRecoveryGit(t, "-C", oldSource, "update-ref", "-d", "refs/heads/"+branch, candidate)
	if err := os.Rename(oldSource, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("frozen source remains available: %v", err)
	}
	run, err := store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var repository application.LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil {
		t.Fatal("repository config")
	}
	repository.SourcePath, repository.OriginPath = oldSource, origin
	raw, _ := json.Marshal(repository)
	evidence, _ := json.Marshal(map[string]string{"source_path": oldSource, "origin_path": origin, "path": worktree, "branch": branch, "base_branch": run.BaseBranch, "base_sha": base, "nonce": "recovery-nonce"})
	if _, err := store.db.Exec(`UPDATE runs SET repository_config_json=?,working_branch=?,base_sha=?,candidate_head=?,worktree_path=? WHERE run_id=?`, string(raw), branch, base, candidate, worktree, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE owned_resources SET resource_name=CASE resource_kind WHEN 'worktree' THEN ? ELSE ? END,creation_evidence=? WHERE owning_run=? AND resource_kind IN ('worktree','branch','local_branch')`, worktree, branch, string(evidence), runID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE cleanup_results SET resource_name=CASE resource_kind WHEN 'worktree' THEN ? ELSE ? END WHERE run_id=? AND resource_kind IN ('worktree','branch','local_branch')`, worktree, branch, runID); err != nil {
		t.Fatal(err)
	}
	return replacement
}

func runCleanupRecoveryGit(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git fixture failed: %v: %s", err, output)
	}
	return string(output)
}

func TestMigratesSchemaV44ToV45PreservingOperationReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 44)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 1, NodeID: "U_1", ActorType: "User"}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRecheckRepository, Scope: application.ScopeRepository, TargetID: "owner/repository", Requester: requester, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("c", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: now})
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
	var operationType string
	if err := store.db.QueryRow(`SELECT operation_type FROM operation_receipts WHERE operation_id=?`, receipt.OperationID).Scan(&operationType); err != nil || operationType != string(receipt.OperationType) {
		t.Fatalf("type=%s err=%v", operationType, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cleanup_source_recovery_intents'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("table count=%d err=%v", count, err)
	}
	var family string
	if err := store.db.QueryRow(`SELECT family FROM integrity_registry_sources WHERE registry_version='v1' AND table_name='cleanup_source_recovery_intents'`).Scan(&family); err != nil || family != string(application.IntegrityOwnedResourceCleanup) {
		t.Fatalf("integrity family=%s err=%v", family, err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN ('integrity_track_cleanup_source_recovery_intents_insert','integrity_track_cleanup_source_recovery_intents_update','integrity_track_cleanup_source_recovery_intents_delete')`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("integrity triggers=%d err=%v", count, err)
	}
}

func cleanupSourceRecoveryStoreFixture(t *testing.T) (*Store, application.CleanupSourceRecoveryIntent, application.OperationReceipt) {
	t.Helper()
	store, run, _ := prepareAutomaticAbandonmentRun(t, domain.StateReceived)
	ctx := context.Background()
	request := automaticAbandonmentRequest(run, run.State, run.LeaseOwner)
	if abandoned, _, err := store.AbandonAutomaticAdmission(ctx, request); err != nil {
		store.Close()
		t.Fatal(err)
	} else {
		run = abandoned
	}
	if err := store.ReleaseLease(ctx, run.ID, run.LeaseOwner); err != nil {
		store.Close()
		t.Fatal(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "old-source")
	worktree := filepath.Join(root, "owned-worktree")
	branch := "codex/recovery"
	candidate := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	origin := "https://github.com/owner/repository.git"
	nonce := "recovery-nonce"
	var repository application.LocalRepository
	if json.Unmarshal([]byte(run.RepositoryConfigJSON), &repository) != nil {
		store.Close()
		t.Fatal("repository config")
	}
	repository.SourcePath = source
	repository.OriginPath = origin
	repository.CanonicalRepository = run.Repository
	repository.BaseBranch = run.BaseBranch
	repository.AllowedOperatorLogins = append(repository.AllowedOperatorLogins, "fixture-operator")
	repository.TrustedOperatorActors = []application.TrustedActorIdentity{{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", Type: "User"}}
	raw, _ := json.Marshal(repository)
	if _, err := store.db.Exec(`UPDATE runs SET repository_config_json=?,working_branch=?,base_sha=?,candidate_head=?,worktree_path=?,lease_owner='',lease_expires_unix=0 WHERE run_id=?`, string(raw), branch, base, candidate, worktree, run.ID); err != nil {
		store.Close()
		t.Fatal(err)
	}
	run, _ = store.GetRun(ctx, run.ID)
	evidence, _ := json.Marshal(map[string]string{"source_path": source, "origin_path": origin, "path": worktree, "branch": branch, "base_branch": run.BaseBranch, "base_sha": run.BaseSHA, "nonce": nonce})
	for _, resource := range []application.OwnedResource{{RunID: run.ID, Kind: "worktree", Name: worktree, CreationEvidence: string(evidence), Status: "owned"}, {RunID: run.ID, Kind: "branch", Name: branch, CreationEvidence: string(evidence), Status: "owned"}} {
		if err := store.AddOwnedResource(ctx, resource); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE owned_resources SET ownership_status='deleted' WHERE owning_run=? AND resource_kind='branch' AND resource_name=?`, run.ID, branch); err != nil {
		store.Close()
		t.Fatal(err)
	}
	for _, record := range []application.CleanupRecord{{RunID: run.ID, Kind: "worktree", Name: worktree, Status: "failed", ErrorClass: "operation_failed"}, {RunID: run.ID, Kind: "branch", Name: branch, Status: "deleted"}, {RunID: run.ID, Kind: "source_checkout", Name: "configured_source_checkout", Status: "synced"}} {
		if err := store.UpsertCleanup(ctx, record); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	inspection, _ := store.Inspect(ctx, run.ID)
	seq := inspection.Timeline[len(inspection.Timeline)-1].Sequence
	now := time.Now().UTC()
	attention, _ := application.CleanupResidueAttentionEvent(run, seq, strings.Repeat("e", 64), now)
	if _, err := store.AppendOperatorAttention(ctx, attention); err != nil {
		store.Close()
		t.Fatal(err)
	}
	legacyPayload := struct {
		RunID, Repository, ExpectedState, RunKey, ActionType, RequesterLogin, RequesterNode, RequesterType, Reason, EventKey string
		TransitionSequence, RequesterDatabaseID                                                                              int64
	}{run.ID, run.Repository, string(domain.StateReceived), run.IdempotencyKey, string(application.OperatorActionAbandon), "operator", "U_1", "User", "abandon_requested", "pre-abandon-attention", seq - 1, 1}
	legacyRaw, _ := json.Marshal(legacyPayload)
	payloadSum := sha256.Sum256(legacyRaw)
	payloadDigest := hex.EncodeToString(payloadSum[:])
	keySum := sha256.Sum256([]byte("operator-action-idempotency:" + payloadDigest))
	actionKey := hex.EncodeToString(keySum[:])
	actionID := "operator-action-" + actionKey[:24]
	d := strings.Repeat("1", 64)
	_, err := store.db.Exec(`INSERT INTO operator_actions(action_id,idempotency_key,payload_digest,request_digest,expected_authority_digest,run_id,repository,expected_state,run_idempotency_key,transition_sequence,action_type,requester_login,requester_database_id,requester_node_id,requester_actor_type,reason_code,attention_event_key,status,result_status,resulting_state,resulting_transition_sequence,evidence_digest,outcome_digest,received_at,validated_at,applied_at,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,'abandon','operator',1,'U_1','User','abandon_requested',?,'observed','succeeded','failed',?,?,?, ?,?,?,?)`, actionID, actionKey, payloadDigest, payloadDigest, d, run.ID, run.Repository, string(domain.StateReceived), run.IdempotencyKey, seq-1, legacyPayload.EventKey, seq, d, d, formatTime(now), formatTime(now), formatTime(now), formatTime(now))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	ownedDigest, cleanupDigest, err := application.ValidateCleanupSourceRecoveryResidue(run, inspection.Resources, inspection.Cleanup)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	requester := domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"}
	action := application.OperatorActionRecord{ActionID: actionID, PayloadDigest: payloadDigest, RequestDigest: payloadDigest, ExpectedAuthorityDigest: d, EvidenceDigest: d, OutcomeDigest: d, TransitionSequence: seq - 1, ResultingTransitionSequence: seq}
	authority := application.CleanupSourceRecoveryAuthority{RunID: run.ID, Repository: run.Repository, RepositoryBindingDigest: run.RepositoryBindingDigest, TransitionSequence: seq, AbandonActionDigest: application.CleanupSourceRecoveryAbandonActionDigest(action), AttentionEventKey: attention.EventKey, AttentionEvidenceDigest: attention.EvidenceDigest, OwnershipDigest: ownedDigest, CleanupDigest: cleanupDigest, FrozenSourceDigest: application.DigestCleanupFrozenSource(source), ReplacementSourceDigest: strings.Repeat("3", 64), ReplacementIdentityDigest: strings.Repeat("4", 64), RepositoryOriginDigest: strings.Repeat("5", 64), RegistrationDigest: strings.Repeat("6", 64), Branch: branch, CandidateHead: candidate, PreviewDigest: strings.Repeat("7", 64)}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRecoverCleanupSource, Scope: application.ScopeRun, TargetID: run.ID, Requester: requester, RequestDigest: strings.Repeat("8", 64), ExpectedAuthorityDigest: application.CleanupSourceRecoveryExpectedAuthorityDigest(authority), OperationAnchorDigest: strings.Repeat("9", 64), TargetBindingDigest: run.RepositoryBindingDigest, AcceptedAt: now})
	intent := application.CleanupSourceRecoveryIntent{RequestID: "recovery-request", OperationID: receipt.OperationID, Authority: authority, Requester: requester, Stage: application.CleanupSourceRecoveryAccepted, CreatedAt: now, UpdatedAt: now}
	return store.Store, intent, receipt
}
