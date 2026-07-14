package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestAutomaticAdmissionLeaseUsesTTLAndVersionCAS(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	first, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), "owner-one", time.Minute, now)
	if err != nil || !acquired || first.Version != 1 {
		t.Fatalf("first=%+v acquired=%v err=%v", first, acquired, err)
	}
	if held, err := store.LinearTodoAdmissionLeaseHeld(context.Background(), first, now.Add(59*time.Second)); err != nil || !held {
		t.Fatalf("held=%v err=%v", held, err)
	}
	if _, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), "owner-two", time.Minute, now.Add(59*time.Second)); err != nil || acquired {
		t.Fatalf("active lease acquired=%v err=%v", acquired, err)
	}
	leaked, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), "attacker", time.Minute, now.Add(59*time.Second))
	if err != nil || acquired || leaked != (application.LinearTodoAdmissionLease{}) {
		t.Fatalf("failed acquire leaked capability=%+v acquired=%v err=%v", leaked, acquired, err)
	}
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(context.Background(), automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174012", "run-attacker", "IFAN-12", leaked)); err == nil || reserved {
		t.Fatalf("failed-acquire capability reserved=%v err=%v", reserved, err)
	}
	renewed, renewedOK, err := store.RenewLinearTodoAdmissionLease(context.Background(), first, time.Minute, now.Add(30*time.Second))
	if err != nil || !renewedOK || renewed.Version != 2 || !renewed.AcquiredAt.Equal(first.AcquiredAt) {
		t.Fatalf("renewed=%+v ok=%v err=%v", renewed, renewedOK, err)
	}
	if ok, err := store.ReleaseLinearTodoAdmissionLease(context.Background(), first); err != nil || ok {
		t.Fatalf("stale release ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.RenewLinearTodoAdmissionLease(context.Background(), first, time.Minute, now.Add(31*time.Second)); err != nil || ok {
		t.Fatalf("stale renew ok=%v err=%v", ok, err)
	}
	if _, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), "owner-two", time.Minute, renewed.ExpiresAt); err != nil || !acquired {
		t.Fatalf("expiry boundary acquire=%v err=%v", acquired, err)
	}
}

func TestAutomaticAdmissionLeaseConcurrentAcquireHasOneOwner(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, owner := range []string{"one", "two"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			<-start
			_, acquired, err := store.AcquireLinearTodoAdmissionLease(context.Background(), owner, time.Minute, now)
			results <- acquired
			errs <- err
		}(owner)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	count := 0
	for result := range results {
		if result {
			count++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("acquire count=%d", count)
	}
}

func TestReserveAutomaticAdmissionIsAtomicAndAdoptable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, ok, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	if err != nil || !ok {
		t.Fatal(err)
	}
	reservation := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174001", "run-auto-one", "IFAN-1", lease)
	run, journal, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation)
	if err != nil || !reserved || run.ID != reservation.Input.Task.RunID || journal.IssueUUID != reservation.IssueUUID || journal.Status != application.LinearTodoAdmissionJournalReserved {
		t.Fatalf("run=%+v journal=%+v reserved=%v err=%v", run, journal, reserved, err)
	}
	loaded, found, err := store.GetLinearTodoAdmissionJournal(ctx, run.ID)
	if err != nil || !found || loaded.RunID != run.ID || loaded.IssueUUID != journal.IssueUUID || loaded.ScanDigest != journal.ScanDigest || loaded.Status != application.LinearTodoAdmissionJournalReserved {
		t.Fatalf("loaded=%+v found=%v err=%v", loaded, found, err)
	}
	if missing, found, err := store.GetLinearTodoAdmissionJournal(ctx, "missing-run"); err != nil || found || missing != (application.LinearTodoAdmissionJournal{}) {
		t.Fatalf("missing=%+v found=%v err=%v", missing, found, err)
	}
	adopted, adoptedJournal, found, err := store.AdoptLinearTodoAdmissionReservation(ctx, reservation)
	if err != nil || !found || adopted.ID != run.ID || adoptedJournal.ScanDigest != journal.ScanDigest {
		t.Fatalf("adopted=%+v journal=%+v found=%v err=%v", adopted, adoptedJournal, found, err)
	}
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174002", "run-auto-two", "IFAN-2", lease)); err != nil || reserved {
		t.Fatalf("second reserve reserved=%v err=%v", reserved, err)
	}
	var journalRows, runRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_todo_admission_journal`).Scan(&journalRows); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runRows); err != nil {
		t.Fatal(err)
	}
	if journalRows != 1 || runRows != 1 {
		t.Fatalf("journalRows=%d runRows=%d", journalRows, runRows)
	}
}

func TestAutomaticAdmissionFailsClosedForActiveOrCorruptEvidence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	lease, ok, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, time.Now().UTC())
	if err != nil || !ok {
		t.Fatal(err)
	}
	manual := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174003", "manual-run", "IFAN-3", lease)
	manualRun, err := application.ReservedRunFromAdmissionSnapshot(manual.Input)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: manualRun}); err != nil {
		t.Fatal(err)
	}
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174004", "run-auto-three", "IFAN-4", lease)); err != nil || reserved {
		t.Fatalf("active manual run reserve=%v err=%v", reserved, err)
	}
	for _, state := range []domain.State{domain.StateAwaitingHumanApproval, domain.StateAwaitingHumanDecision, domain.StateManualIntervention, domain.StateCleaning, domain.StateRepairing} {
		if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, state, manualRun.ID); err != nil {
			t.Fatal(err)
		}
		runs, err := store.ListNonterminalRuns(ctx)
		if err != nil || len(runs) != 1 || runs[0].State != state {
			t.Fatalf("state=%s runs=%+v err=%v", state, runs, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state='completed' WHERE run_id=?`, manualRun.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174005", "run-auto-four", "IFAN-5", lease)); err != nil || !reserved {
		t.Fatalf("terminal manual run reserve=%v err=%v", reserved, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE linear_todo_admission_journal SET task_digest=? WHERE run_id=?`, strings.Repeat("0", 64), "run-auto-four"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReleaseLinearTodoAdmissionLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	lease, ok, err = store.AcquireLinearTodoAdmissionLease(ctx, "new-scheduler", time.Minute, time.Now().UTC())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, _, _, err := store.ReserveLinearTodoAdmission(ctx, automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174006", "run-auto-five", "IFAN-6", lease)); err == nil {
		t.Fatal("corrupt journal was accepted")
	}
}

func TestAutomaticAdmissionJournalAllowsOnlySanitizedStateProgress(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	lease, ok, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, time.Now().UTC())
	if err != nil || !ok {
		t.Fatal(err)
	}
	reservation := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174007", "run-auto-six", "IFAN-7", lease)
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatalf("reserved=%v err=%v", reserved, err)
	}
	intent := digestBytes([]byte("linear intent"))
	if changed, err := store.AdvanceLinearTodoAdmissionJournal(ctx, application.LinearTodoAdmissionJournalTransition{Lease: lease, RunID: "run-auto-six", ExpectedStatus: application.LinearTodoAdmissionJournalReserved, NextStatus: "mutation_intent", MutationIntentRef: intent}); err != nil || !changed {
		t.Fatalf("intent changed=%v err=%v", changed, err)
	}
	if changed, err := store.AdvanceLinearTodoAdmissionJournal(ctx, application.LinearTodoAdmissionJournalTransition{Lease: lease, RunID: "run-auto-six", ExpectedStatus: "mutation_intent", NextStatus: "started", MutationIntentRef: intent}); err != nil || !changed {
		t.Fatalf("started changed=%v err=%v", changed, err)
	}
	if changed, err := store.AdvanceLinearTodoAdmissionJournal(ctx, application.LinearTodoAdmissionJournalTransition{Lease: lease, RunID: "run-auto-six", ExpectedStatus: "started", NextStatus: "manual_intervention", ReasonCode: "raw error with token"}); err == nil || changed {
		t.Fatalf("unsafe reason changed=%v err=%v", changed, err)
	}
	var reference, reason string
	if err := store.db.QueryRowContext(ctx, `SELECT mutation_intent_ref,reason_code FROM linear_todo_admission_journal WHERE run_id=?`, "run-auto-six").Scan(&reference, &reason); err != nil {
		t.Fatal(err)
	}
	if reference != intent || reason != "" || strings.Contains(reference, "token") {
		t.Fatalf("unsafe journal projection reference=%q reason=%q", reference, reason)
	}
	if _, released := store.ReleaseLinearTodoAdmissionLease(ctx, lease); released != nil {
		t.Fatal(released)
	}
	if _, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "replacement", time.Minute, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("replacement acquire=%v err=%v", acquired, err)
	}
	if changed, err := store.AdvanceLinearTodoAdmissionJournal(ctx, application.LinearTodoAdmissionJournalTransition{Lease: lease, RunID: "run-auto-six", ExpectedStatus: "started", NextStatus: "manual_intervention", ReasonCode: "lease_lost"}); err == nil || changed {
		t.Fatalf("lost lease advanced journal changed=%v err=%v", changed, err)
	}
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM linear_todo_admission_journal WHERE run_id=?`, "run-auto-six").Scan(&status); err != nil || status != "started" {
		t.Fatalf("lost lease changed journal status=%q err=%v", status, err)
	}
}

func TestAutomaticAdmissionReservationRejectsLostLeaseAndMismatchedRestartEvidence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, ok, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	if err != nil || !ok {
		t.Fatal(err)
	}
	renewed, ok, err := store.RenewLinearTodoAdmissionLease(ctx, lease, time.Minute, now.Add(time.Second))
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, _, _, err := store.ReserveLinearTodoAdmission(ctx, automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174008", "run-auto-seven", "IFAN-8", lease)); err == nil {
		t.Fatal("lost lease reserved a run")
	}
	var runs, journals int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_todo_admission_journal`).Scan(&journals); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || journals != 0 {
		t.Fatalf("lost lease left partial state runs=%d journals=%d", runs, journals)
	}
	reservation := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174009", "run-auto-eight", "IFAN-9", renewed)
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatalf("reserved=%v err=%v", reserved, err)
	}
	mismatched := reservation
	mismatched.ScanDigest = digestBytes([]byte("different scan"))
	if _, _, found, err := store.AdoptLinearTodoAdmissionReservation(ctx, mismatched); err == nil || found {
		t.Fatalf("mismatched restart evidence found=%v err=%v", found, err)
	}
}

func TestAutomaticAdmissionConcurrentReserveCreatesAtMostOneRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	lease, ok, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, time.Now().UTC())
	if err != nil || !ok {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for _, reservation := range []application.LinearTodoAdmissionReservation{
		automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174010", "run-auto-nine", "IFAN-10", lease),
		automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174011", "run-auto-ten", "IFAN-11", lease),
	} {
		wg.Add(1)
		go func(reservation application.LinearTodoAdmissionReservation) {
			defer wg.Done()
			<-start
			_, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation)
			if err != nil {
				results <- false
				return
			}
			results <- reserved
		}(reservation)
	}
	close(start)
	wg.Wait()
	close(results)
	count := 0
	for result := range results {
		if result {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reserved count=%d", count)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("runs=%d err=%v", count, err)
	}
}

func TestAutomaticAdmissionRejectsEveryCorruptUnrelatedJournalRow(t *testing.T) {
	cases := []struct {
		name  string
		query string
		value string
	}{
		{name: "raw prose reason", query: `UPDATE linear_todo_admission_journal SET reason_code=? WHERE run_id=?`, value: "untrusted issue body with a token"},
		{name: "invalid digest", query: `UPDATE linear_todo_admission_journal SET scan_digest=? WHERE run_id=?`, value: "not-a-digest"},
		{name: "raw prose reference", query: `UPDATE linear_todo_admission_journal SET mutation_intent_ref=?,status='mutation_intent' WHERE run_id=?`, value: "mutation payload"},
		{name: "invalid timestamp", query: `UPDATE linear_todo_admission_journal SET updated_at=? WHERE run_id=?`, value: "not-a-time"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, time.Now().UTC())
			if err != nil || !acquired {
				t.Fatal(err)
			}
			old := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174013", "run-corrupt-old", "IFAN-13", lease)
			if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, old); err != nil || !reserved {
				t.Fatalf("reserved=%v err=%v", reserved, err)
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state='completed' WHERE run_id=?`, old.Input.Task.RunID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, test.query, test.value, old.Input.Task.RunID); err != nil {
				t.Fatal(err)
			}
			if changed, err := store.AdvanceLinearTodoAdmissionJournal(ctx, application.LinearTodoAdmissionJournalTransition{Lease: lease, RunID: old.Input.Task.RunID, ExpectedStatus: application.LinearTodoAdmissionJournalReserved, NextStatus: "mutation_intent", MutationIntentRef: digestBytes([]byte("intent"))}); err == nil || changed {
				t.Fatalf("corrupt journal advanced changed=%v err=%v", changed, err)
			}
			if _, _, _, err := store.ReserveLinearTodoAdmission(ctx, automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174014", "run-corrupt-new", "IFAN-14", lease)); err == nil {
				t.Fatal("corrupt unrelated journal was accepted")
			}
		})
	}
}

func TestAutomaticAdmissionLeaseExpiryUsesNumericTimeComparison(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "first", 30*time.Second, base.Add(-30*time.Second))
	if err != nil || !acquired || !lease.ExpiresAt.Equal(base) {
		t.Fatalf("lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	// RFC3339Nano formats the first expiry without a fraction while this time
	// includes .5. Lexical TEXT comparison would incorrectly keep first alive.
	whenExpired := base.Add(500 * time.Millisecond)
	if _, renewed, err := store.RenewLinearTodoAdmissionLease(ctx, lease, time.Minute, whenExpired); err != nil || renewed {
		t.Fatalf("expired renewal renewed=%t err=%v", renewed, err)
	}
	replacement, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "second", time.Minute, whenExpired)
	if err != nil || !acquired || replacement.OwnerNonce != "second" {
		t.Fatalf("replacement=%+v acquired=%t err=%v", replacement, acquired, err)
	}
	var numericExpiry int64
	if err := store.db.QueryRowContext(ctx, `SELECT expires_at_unix_ns FROM linear_todo_admission_lease WHERE namespace=?`, application.LinearTodoAdmissionLeaseNamespace).Scan(&numericExpiry); err != nil {
		t.Fatal(err)
	}
	if numericExpiry != replacement.ExpiresAt.UnixNano() {
		t.Fatalf("numeric expiry=%d want=%d", numericExpiry, replacement.ExpiresAt.UnixNano())
	}
}

func TestMigrationV18BackfillsLegacyAutomaticAdmissionLeaseExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	migrations := [][]string{migrationV1, migrationV2, migrationV3, migrationV4, migrationV5, migrationV6, migrationV7, migrationV8, migrationV9, migrationV10, migrationV11, migrationV12, migrationV13, migrationV14, migrationV15, migrationV16, migrationV17}
	for index, migration := range migrations {
		for _, statement := range migration {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("migration=%d err=%v", index+1, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, index+1, "legacy"); err != nil {
			t.Fatal(err)
		}
	}
	expires := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO linear_todo_admission_lease(namespace,owner_nonce,version,acquired_at,renewed_at,expires_at) VALUES(?,?,?,?,?,?)`, application.LinearTodoAdmissionLeaseNamespace, "legacy-owner", 7, formatTime(expires.Add(-time.Minute)), formatTime(expires.Add(-30*time.Second)), formatTime(expires)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var got int64
	if err := store.db.QueryRowContext(context.Background(), `SELECT expires_at_unix_ns FROM linear_todo_admission_lease WHERE namespace=?`, application.LinearTodoAdmissionLeaseNamespace).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != expires.UnixNano() {
		t.Fatalf("backfilled expiry=%d want=%d", got, expires.UnixNano())
	}
}

func TestAutomaticAdmissionJournalRejectsStatusAndRunStateContradictions(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
	}{
		{name: "reserved executing", status: application.LinearTodoAdmissionJournalReserved},
		{name: "mutation intent executing", status: "mutation_intent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, time.Now().UTC())
			if err != nil || !acquired {
				t.Fatal(err)
			}
			reservation := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174020", "run-state-mismatch", "IFAN-20", lease)
			if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
				t.Fatalf("reserved=%t err=%v", reserved, err)
			}
			if test.status == "mutation_intent" {
				if _, err := store.db.ExecContext(ctx, `UPDATE linear_todo_admission_journal SET status=?,mutation_intent_ref=? WHERE run_id=?`, test.status, digestBytes([]byte("intent")), reservation.Input.Task.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state='executing' WHERE run_id=?`, reservation.Input.Task.RunID); err != nil {
				t.Fatal(err)
			}
			if _, found, err := store.GetLinearTodoAdmissionJournal(ctx, reservation.Input.Task.RunID); err == nil || found {
				t.Fatalf("contradictory journal found=%t err=%v", found, err)
			}
			if _, _, _, err := store.ReserveLinearTodoAdmission(ctx, automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174021", "run-state-mismatch-next", "IFAN-21", lease)); err == nil {
				t.Fatal("global journal corruption check accepted contradictory state")
			}
		})
	}
}

func TestMigratesVersionFifteenDatabaseToAutomaticAdmissionSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	migrations := [][]string{migrationV1, migrationV2, migrationV3, migrationV4, migrationV5, migrationV6, migrationV7, migrationV8, migrationV9, migrationV10, migrationV11, migrationV12, migrationV13, migrationV14, migrationV15}
	for index, migration := range migrations {
		for _, statement := range migration {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("migration=%d err=%v", index+1, err)
			}
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, index+1, "legacy"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	for _, table := range []string{"linear_todo_admission_lease", "linear_todo_admission_journal"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table=%s count=%d err=%v", table, count, err)
		}
	}
}

func automaticAdmissionReservation(issueUUID, runID, identifier string, lease application.LinearTodoAdmissionLease) application.LinearTodoAdmissionReservation {
	task := domain.CodingTask{RunID: runID, IssueID: identifier, IssueURL: "https://linear.invalid/" + identifier, Title: "sanitized fixture", Description: "## Goal\nFixture\n\n## Acceptance Criteria\n- persists\n", Repository: "owner/repository", BaseBranch: "main", WorkingBranch: "ifan/" + strings.ToLower(identifier), Goal: "Fixture", AcceptanceCriteria: []string{"persists"}, VerifierIDs: []string{"go-test"}, Policy: domain.TaskPolicy{HumanApprovalRequired: true, MergeMethod: "squash", MaxRepairAttempts: domain.DefaultMaxRepairAttempts}, SourceRevision: "2026-07-15T02:00:00Z", CreatedAt: time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)}
	source := application.LinearTaskSource{Provider: "linear", IssueID: issueUUID, Identifier: identifier, SourceRevision: task.SourceRevision}
	raw, _ := json.Marshal(source)
	normalized, _ := json.Marshal(task)
	digest := func(value []byte) string { return digestBytes(value) }
	profileDigest := digest([]byte("profile:" + identifier))
	return application.LinearTodoAdmissionReservation{Lease: lease, IssueUUID: issueUUID, ScanDigest: digest([]byte("scan:" + identifier)), Input: application.LocalStartInput{Task: task, RawIssueJSON: raw, RawIssueHash: digest(raw), NormalizedJSON: normalized, TaskHash: digest(normalized), IdempotencyKey: digest([]byte("key:" + identifier)), Repository: application.LocalRepository{ProfileID: "profile-" + identifier, ProfileSnapshotVersion: 1, ProfileDigest: profileDigest, ProfileSnapshotJSON: `{"profile":"sanitized"}`, RegistryVersion: 1, RegistryDigest: digest([]byte("registry:" + identifier)), RepositoryBindingDigest: digest([]byte("binding:" + identifier)), CanonicalRepository: task.Repository, BaseBranch: task.BaseBranch, VerifierIDs: task.VerifierIDs}, RunRoot: "/tmp/automatic-admission-runs", WorktreeRoot: "/tmp/automatic-admission-worktrees"}}
}
