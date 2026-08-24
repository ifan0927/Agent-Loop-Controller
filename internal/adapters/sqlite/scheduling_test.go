package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	_ "modernc.org/sqlite"
)

func controllerSchedulingRunScopes(t *testing.T, store *admissionTestStore) application.AuthorizedScopeSet {
	t.Helper()
	user := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: user})
	if err != nil {
		t.Fatal(err)
	}
	requester, err := authorizer.ResolveConfiguredRequester(application.Requester{ID: user.Login, Kind: "github_login", DatabaseID: user.DatabaseID, NodeID: user.NodeID, ActorType: user.ActorType})
	if err != nil {
		t.Fatal(err)
	}
	authorities, err := store.ListRunScopeAuthorities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for index := range authorities {
		authorities[index].AllowedLogins = []string{user.Login}
		authorities[index].TrustedOperators = []domain.GitHubUserIdentity{user}
	}
	scopes, err := authorizer.ControllerRunScopes(requester, authorities)
	if err != nil {
		t.Fatal(err)
	}
	return scopes
}

func TestSchedulingReservesDifferentRepositoriesUpToGenericCapacity(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if projection, err := store.ConfigureHeavyCapacity(ctx, 3, "config-three", now); err != nil || projection.EffectiveCapacity != 3 {
		t.Fatalf("projection=%+v err=%v", projection, err)
	}
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	if err != nil || !acquired {
		t.Fatal(err)
	}
	for index, id := range []string{"IFAN-201", "IFAN-202", "IFAN-203"} {
		reservation := automaticAdmissionReservation(fixtureUUID(id), "run-"+id, id, lease)
		reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("binding:" + id))
		reservation.Scheduling = schedulingReservation(id, "config-three", 3, now.Add(time.Duration(index)*time.Second))
		if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
			t.Fatalf("reservation %s reserved=%t err=%v", id, reserved, err)
		}
	}
	fourth := automaticAdmissionReservation(fixtureUUID("IFAN-204"), "run-IFAN-204", "IFAN-204", lease)
	fourth.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("binding:IFAN-204"))
	fourth.Scheduling = schedulingReservation("IFAN-204", "config-three", 3, now)
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, fourth); err != nil || reserved {
		t.Fatalf("fourth reserved=%t err=%v", reserved, err)
	}
	projection, err := store.Capacity(ctx, now)
	if err != nil || projection.InUse != 3 || projection.Available != 0 {
		t.Fatalf("capacity=%+v err=%v", projection, err)
	}
	var sequence, priority int
	var profileID, binding string
	if err := store.db.QueryRowContext(ctx, `SELECT issue_sequence,priority,repository_profile_id,repository_binding_digest FROM scheduling_decisions WHERE issue_uuid=?`, fixtureUUID("IFAN-201")).Scan(&sequence, &priority, &profileID, &binding); err != nil {
		t.Fatal(err)
	}
	if sequence != 201 || priority != 1 || profileID != "profile-fixture" || binding != digestBytes([]byte("binding:IFAN-201")) {
		t.Fatalf("decision sequence=%d priority=%d profile=%q binding=%q", sequence, priority, profileID, binding)
	}
}

func TestSchedulingProjectionQueriesAreReadOnlyBoundedAndOrdered(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.ConfigureHeavyCapacity(ctx, 3, "projection-three", now); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	if err != nil || !acquired {
		t.Fatalf("lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	for index, id := range []string{"IFAN-207", "IFAN-208", "IFAN-209"} {
		reservation := automaticAdmissionReservation(fixtureUUID(id), "projection-"+id, id, lease)
		reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("projection-binding:" + id))
		reservation.Scheduling = schedulingReservation(id, "projection-three", 3, now.Add(time.Duration(index)*time.Second))
		reservation.Scheduling.IssueSequence = 207 + index
		if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
			t.Fatalf("reservation %s reserved=%t err=%v", id, reserved, err)
		}
	}
	scopes := controllerSchedulingRunScopes(t, store)
	runs, err := store.ListSchedulingRuns(ctx, scopes, 2)
	if err != nil || len(runs) != 2 || runs[0].RunID != "projection-IFAN-207" || runs[1].RunID != "projection-IFAN-208" || !runs[0].HasHeavyPermit || runs[0].WaitingForCapacity {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	decisions, err := store.ListSchedulingDecisions(ctx, scopes, 2)
	if err != nil || len(decisions) != 2 || decisions[0].IssueSequence != 209 || decisions[1].IssueSequence != 208 {
		t.Fatalf("decisions=%+v err=%v", decisions, err)
	}
	for _, invalid := range []int{0, application.MaxSchedulingQueryItems + 1} {
		if _, err := store.ListSchedulingRuns(ctx, scopes, invalid); err == nil {
			t.Fatalf("scheduling run query accepted limit %d", invalid)
		}
		if _, err := store.ListSchedulingDecisions(ctx, scopes, invalid); err == nil {
			t.Fatalf("scheduling decision query accepted limit %d", invalid)
		}
	}
	var schedulingRowsBefore int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_scheduling`).Scan(&schedulingRowsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListSchedulingRuns(ctx, scopes, 1); err != nil {
		t.Fatal(err)
	}
	var schedulingRowsAfter int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_scheduling`).Scan(&schedulingRowsAfter); err != nil || schedulingRowsAfter != schedulingRowsBefore {
		t.Fatalf("read-only query changed scheduling rows: before=%d after=%d err=%v", schedulingRowsBefore, schedulingRowsAfter, err)
	}
}

func TestRepositorySchedulingScopeDoesNotDiscloseSiblingRunsOrDecisions(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.ConfigureHeavyCapacity(ctx, 2, "projection-two", now); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	if err != nil || !acquired {
		t.Fatalf("lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	bindings := []string{digestBytes([]byte("visible-binding")), digestBytes([]byte("sibling-binding"))}
	for index, id := range []string{"IFAN-301", "IFAN-302"} {
		reservation := automaticAdmissionReservation(fixtureUUID(id), "scoped-"+id, id, lease)
		reservation.Input.Repository.RepositoryBindingDigest = bindings[index]
		reservation.Scheduling = schedulingReservation(id, "projection-two", 2, now.Add(time.Duration(index)*time.Second))
		reservation.Scheduling.IssueSequence = 301 + index
		if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
			t.Fatalf("reservation %s reserved=%t err=%v", id, reserved, err)
		}
	}
	scopes := repositoryRunScopes(t, bindings[0])
	runs, err := store.ListSchedulingRuns(ctx, scopes, 10)
	if err != nil || len(runs) != 1 || runs[0].RunID != "scoped-IFAN-301" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	visible, err := store.GetSchedulingRun(ctx, scopes, "scoped-IFAN-301")
	if err != nil || visible.RunID != "scoped-IFAN-301" {
		t.Fatalf("visible=%+v err=%v", visible, err)
	}
	if _, err := store.GetSchedulingRun(ctx, scopes, "scoped-IFAN-302"); !errors.Is(err, application.ErrRunNotFound) {
		t.Fatalf("sibling lookup error=%v", err)
	}
	decisions, err := store.ListSchedulingDecisions(ctx, scopes, 10)
	if err != nil || len(decisions) != 1 || decisions[0].RunID != "scoped-IFAN-301" {
		t.Fatalf("decisions=%+v err=%v", decisions, err)
	}
}

func TestSchedulingProjectionQueriesFailClosedForMissingOrCorruptEvidence(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	reservation := automaticAdmissionReservation(fixtureUUID("projection-corrupt"), "projection-corrupt", "IFAN-210", lease)
	reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("projection-corrupt-binding"))
	reservation.Scheduling = schedulingReservation("IFAN-210", "schema-v29-default", 2, now)
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatalf("reserved=%t err=%v", reserved, err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM run_scheduling WHERE run_id=?`, reservation.Input.Task.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListSchedulingRuns(ctx, controllerSchedulingRunScopes(t, store), 10); err == nil {
		t.Fatal("missing scheduling row was projected as healthy")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE scheduling_decisions SET reason_code='raw /private/path' WHERE run_id=?`, reservation.Input.Task.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListSchedulingDecisions(ctx, controllerSchedulingRunScopes(t, store), 10); err == nil {
		t.Fatal("corrupt scheduling decision was projected")
	}
}

func TestSchedulingConcurrentSameRepositoryReservationHasOneWinner(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	if err != nil || !acquired {
		t.Fatal(err)
	}
	reservations := []application.LinearTodoAdmissionReservation{
		automaticAdmissionReservation(fixtureUUID("same-a"), "same-a", "IFAN-211", lease),
		automaticAdmissionReservation(fixtureUUID("same-b"), "same-b", "IFAN-212", lease),
	}
	binding := digestBytes([]byte("same-repository"))
	for index := range reservations {
		reservations[index].Input.Repository.RepositoryBindingDigest = binding
	}
	start := make(chan struct{})
	results := make(chan bool, 2)
	var wait sync.WaitGroup
	for _, reservation := range reservations {
		wait.Add(1)
		go func(reservation application.LinearTodoAdmissionReservation) {
			defer wait.Done()
			<-start
			_, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation)
			results <- err == nil && reserved
		}(reservation)
	}
	close(start)
	wait.Wait()
	close(results)
	winners := 0
	for won := range results {
		if won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
}

func TestManualCreateRunCannotBypassRepositoryOrCapacityAuthority(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.ConfigureHeavyCapacity(ctx, 1, "manual-capacity-one", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	manual := func(id, binding string) application.CreateRunInput {
		return application.CreateRunInput{Run: application.Run{ID: id, IssueID: "IFAN-301", IdempotencyKey: "key-" + id, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task-" + id, Repository: "owner/" + id, RepositoryConfigJSON: "{}", RepositoryBindingDigest: binding, BaseBranch: "main", WorkingBranch: "ifan/" + id, ArtifactRoot: "/tmp/" + id, WorktreePath: "/tmp/worktree-" + id, ImplementationModel: "implementation", ReviewModel: "review"}}
	}
	sharedBinding := digestBytes([]byte("manual-shared-binding"))
	if _, created, err := store.CreateRun(ctx, manual("manual-first", sharedBinding)); err != nil || !created {
		t.Fatalf("first created=%t err=%v", created, err)
	}
	if _, created, err := store.CreateRun(ctx, manual("manual-same-repository", sharedBinding)); err == nil || created {
		t.Fatalf("same repository created=%t err=%v", created, err)
	}
	if _, created, err := store.CreateRun(ctx, manual("manual-other-repository", digestBytes([]byte("manual-other-binding")))); err == nil || created {
		t.Fatalf("over capacity created=%t err=%v", created, err)
	}
	var runs int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
}

func TestSchedulingCapacityReductionDrainsWithoutCancellation(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.ConfigureHeavyCapacity(ctx, 2, "capacity-two", now); err != nil {
		t.Fatal(err)
	}
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	for index, id := range []string{"IFAN-221", "IFAN-222"} {
		reservation := automaticAdmissionReservation(fixtureUUID(id), id, id, lease)
		reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte(id))
		reservation.Scheduling = schedulingReservation(id, "capacity-two", 2, now.Add(time.Duration(index)*time.Second))
		if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
			t.Fatal(err)
		}
	}
	projection, err := store.ConfigureHeavyCapacity(ctx, 1, "capacity-one", now.Add(time.Minute))
	if err != nil || !projection.Draining || projection.InUse != 2 || projection.Available != 0 {
		t.Fatalf("projection=%+v err=%v", projection, err)
	}
	var permits int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM heavy_permits`).Scan(&permits); err != nil || permits != 2 {
		t.Fatalf("permits=%d err=%v", permits, err)
	}
}

func TestSchedulingFutureRetryDoesNotBlockAvailableCapacity(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	if err != nil || !acquired {
		t.Fatalf("lease=%+v acquired=%t err=%v", lease, acquired, err)
	}
	first := automaticAdmissionReservation(fixtureUUID("future-retry"), "future-retry", "IFAN-225", lease)
	first.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("future-retry-binding"))
	first.Scheduling = schedulingReservation("IFAN-225", "schema-v29-default", 2, now)
	_, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, first)
	if err != nil || !reserved {
		t.Fatalf("first reserved=%t err=%v", reserved, err)
	}
	permit, acquired, err := store.AcquireHeavyPermit(ctx, first.Input.Task.RunID, "worker", now)
	if err != nil || !acquired {
		t.Fatalf("permit=%+v acquired=%t err=%v", permit, acquired, err)
	}
	if released, err := store.ReleaseHeavyPermit(ctx, permit, "retry_delay", now.Add(time.Second)); err != nil || !released {
		t.Fatalf("released=%t err=%v", released, err)
	}
	eligibleAt := now.Add(time.Hour)
	if deferred, err := store.DeferSchedulingRun(ctx, first.Input.Task.RunID, eligibleAt, now.Add(2*time.Second)); err != nil || !deferred {
		t.Fatalf("deferred=%t err=%v", deferred, err)
	}
	reconciled, err := store.ReconcileSchedulingAuthorities(ctx, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0].SupervisorState != "external_wait" || reconciled[0].WaitingForCapacity {
		t.Fatalf("reconciled=%+v", reconciled)
	}
	second := automaticAdmissionReservation(fixtureUUID("available-repository"), "available-repository", "IFAN-226", lease)
	second.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("available-binding"))
	second.Scheduling = schedulingReservation("IFAN-226", "schema-v29-default", 2, now.Add(3*time.Second))
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, second); err != nil || !reserved {
		t.Fatalf("available capacity reservation=%t err=%v", reserved, err)
	}
}

func TestSchedulingHumanWaitRequiresLatestTransitionAttention(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	repositoryJSON := `{"profile_id":"repository-profile:owner/repo","canonical_repository":"owner/repo"}`
	input := application.CreateRunInput{Run: application.Run{ID: "repeated-human", IssueID: "IFAN-227", IdempotencyKey: "repeated-human-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: repositoryJSON, ProfileID: "repository-profile:owner/repo", RepositoryBindingDigest: digestBytes([]byte("repeated-human-binding")), BaseBranch: "main", WorkingBranch: "ifan/repeated-human", ArtifactRoot: "/tmp/repeated-human", WorktreePath: "/tmp/repeated-human-worktree", ImplementationModel: "implementation", ReviewModel: "review"}}
	run, created, err := store.CreateRun(ctx, input)
	if err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	first := application.Transition{Sequence: 2, From: domain.StateExecuting, To: domain.StateAwaitingHumanDecision, Reason: "first decision", EvidenceReference: "first", CreatedAt: now}
	second := application.Transition{Sequence: 3, From: domain.StateExecuting, To: domain.StateAwaitingHumanDecision, Reason: "second decision", EvidenceReference: "second", CreatedAt: now.Add(time.Minute)}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateAwaitingHumanDecision, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []application.Transition{first, second} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO transitions(run_id,sequence,from_state,to_state,reason,evidence_reference,bound_head,created_at) VALUES(?,?,?,?,?,?,?,?)`, run.ID, transition.Sequence, transition.From, transition.To, transition.Reason, transition.EvidenceReference, transition.BoundHead, formatTime(transition.CreatedAt)); err != nil {
			t.Fatal(err)
		}
	}
	run.State = domain.StateAwaitingHumanDecision
	firstEvent, err := application.HumanDecisionAttentionEvent(run, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOperatorAttention(ctx, firstEvent); err != nil {
		t.Fatal(err)
	}
	reconciled, err := store.ReconcileSchedulingAuthorities(ctx, now.Add(2*time.Minute))
	if err != nil || len(reconciled) != 1 || reconciled[0].SupervisorState != "external_wait" {
		t.Fatalf("before latest attention runs=%+v err=%v", reconciled, err)
	}
	secondEvent, err := application.HumanDecisionAttentionEvent(run, second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOperatorAttention(ctx, secondEvent); err != nil {
		t.Fatal(err)
	}
	reconciled, err = store.ReconcileSchedulingAuthorities(ctx, now.Add(3*time.Minute))
	if err != nil || len(reconciled) != 1 || reconciled[0].SupervisorState != "human_wait" {
		t.Fatalf("after latest attention runs=%+v err=%v", reconciled, err)
	}
}

func TestSchedulingRestartReconcilesSlotsAndReleasesSafeExternalWaitPermit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	reservation := automaticAdmissionReservation(fixtureUUID("restart"), "restart-run", "IFAN-231", lease)
	reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("restart-binding"))
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StatePROpen, reservation.Input.Task.RunID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	runs, err := reopened.ReconcileSchedulingAuthorities(ctx, now.Add(time.Minute))
	if err != nil || len(runs) != 1 || runs[0].SupervisorState != "external_wait" || runs[0].HasHeavyPermit {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	projection, err := reopened.Capacity(ctx, now.Add(time.Minute))
	if err != nil || projection.InUse != 0 || projection.Available != 2 {
		t.Fatalf("projection=%+v err=%v", projection, err)
	}
}

func TestSchedulingKeepsGitHubApprovalAsRunnableExternalWait(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	reservation := automaticAdmissionReservation(fixtureUUID("approval-wait"), "approval-wait", "IFAN-232", lease)
	reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("approval-binding"))
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateAwaitingHumanApproval, reservation.Input.Task.RunID); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ReconcileSchedulingAuthorities(ctx, now.Add(time.Second))
	if err != nil || len(runs) != 1 || runs[0].SupervisorState != "external_wait" || runs[0].HasHeavyPermit {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}

func TestSchedulingCancellationIsolationReleasesOnlyTerminalSibling(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	ids := []string{"isolation-a", "isolation-b"}
	for _, id := range ids {
		reservation := automaticAdmissionReservation(fixtureUUID(id), id, "IFAN-24"+id[len(id)-1:], lease)
		reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("binding:" + id))
		if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET current_state=? WHERE run_id=?`, domain.StateFailed, ids[0]); err != nil {
		t.Fatal(err)
	}
	var firstSlots, secondSlots, firstPermits, secondPermits int
	queries := []struct {
		destination *int
		query       string
		id          string
	}{
		{&firstSlots, `SELECT COUNT(*) FROM repository_slots WHERE run_id=?`, ids[0]},
		{&secondSlots, `SELECT COUNT(*) FROM repository_slots WHERE run_id=?`, ids[1]},
		{&firstPermits, `SELECT COUNT(*) FROM heavy_permits WHERE run_id=?`, ids[0]},
		{&secondPermits, `SELECT COUNT(*) FROM heavy_permits WHERE run_id=?`, ids[1]},
	}
	for _, query := range queries {
		if err := store.db.QueryRowContext(ctx, query.query, query.id).Scan(query.destination); err != nil {
			t.Fatal(err)
		}
	}
	if firstSlots != 0 || firstPermits != 0 || secondSlots != 1 || secondPermits != 1 {
		t.Fatalf("slots=%d/%d permits=%d/%d", firstSlots, secondSlots, firstPermits, secondPermits)
	}
}

func TestHeavyPermitOwnerMismatchRequiresExclusiveSupervisorFencing(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	reservation := automaticAdmissionReservation(fixtureUUID("permit-fence"), "permit-fence", "IFAN-251", lease)
	reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("permit-fence-binding"))
	reservation.Scheduling = schedulingReservation("IFAN-251", "schema-v29-default", 2, now)
	reservation.Scheduling.OwnerNonce = "old-supervisor"
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatalf("reserved=%t err=%v", reserved, err)
	}
	if _, held, err := store.AcquireHeavyPermit(ctx, reservation.Input.Task.RunID, "new-supervisor", now.Add(time.Second)); err == nil || held {
		t.Fatalf("unfenced takeover held=%t err=%v", held, err)
	}
	if err := store.AuthorizeHeavyPermitAdoption("new-supervisor"); err != nil {
		t.Fatal(err)
	}
	permit, held, err := store.AcquireHeavyPermit(ctx, reservation.Input.Task.RunID, "new-supervisor", now.Add(2*time.Second))
	if err != nil || !held || permit.OwnerNonce != "new-supervisor" || permit.Version < 2 {
		t.Fatalf("permit=%+v held=%t err=%v", permit, held, err)
	}
}

func TestHeavyPermitAdoptionWaitsForLiveManualRunLease(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	reservation := automaticAdmissionReservation(fixtureUUID("manual-live"), "manual-live", "IFAN-252", lease)
	reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("manual-live-binding"))
	reservation.Scheduling = schedulingReservation("IFAN-252", "schema-v29-default", 2, now)
	reservation.Scheduling.OwnerNonce = "manual:manual-live"
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatalf("reserved=%t err=%v", reserved, err)
	}
	if acquired, err := store.AcquireLease(ctx, reservation.Input.Task.RunID, "manual-controller", now.Add(time.Minute)); err != nil || !acquired {
		t.Fatalf("lease acquired=%t err=%v", acquired, err)
	}
	if err := store.AuthorizeHeavyPermitAdoption("worker"); err != nil {
		t.Fatal(err)
	}
	if _, held, err := store.AcquireHeavyPermit(ctx, reservation.Input.Task.RunID, "worker", now.Add(time.Second)); err != nil || held {
		t.Fatalf("held=%t err=%v", held, err)
	}
}

func TestHeavyPermitFirstCreationRequiresStartedAttemptReconciliation(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	reservation := automaticAdmissionReservation(fixtureUUID("permit-first"), "permit-first", "IFAN-253", lease)
	reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("permit-first-binding"))
	reservation.Scheduling = schedulingReservation("IFAN-253", "schema-v29-default", 2, now)
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatalf("reserved=%t err=%v", reserved, err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM heavy_permits WHERE run_id=?`, reservation.Input.Task.RunID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.BeginAttempt(ctx, reservation.Input.Task.RunID, "implementation", "gpt-5.6-sol", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if launched, err := store.CommitAttemptProcessLaunch(ctx, attempt.ID); err != nil || !launched {
		t.Fatalf("launched=%t err=%v", launched, err)
	}
	if err := store.AuthorizeHeavyPermitAdoption("new-supervisor"); err != nil {
		t.Fatal(err)
	}
	if _, held, err := store.AcquireHeavyPermit(ctx, reservation.Input.Task.RunID, "new-supervisor", now.Add(time.Second)); !errors.Is(err, application.ErrHeavyPermitProcessReconciliationRequired) || held {
		t.Fatalf("held=%t err=%v", held, err)
	}
}

func TestSchedulingProjectsLiveRunLeaseExpiryAsRestartWake(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	reservation := automaticAdmissionReservation(fixtureUUID("lease-wake"), "lease-wake", "IFAN-254", lease)
	reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("lease-wake-binding"))
	reservation.Scheduling = schedulingReservation("IFAN-254", "schema-v29-default", 2, now)
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatalf("reserved=%t err=%v", reserved, err)
	}
	expires := now.Add(45 * time.Second)
	if acquired, err := store.AcquireLease(ctx, reservation.Input.Task.RunID, "old-process", expires); err != nil || !acquired {
		t.Fatalf("acquired=%t err=%v", acquired, err)
	}
	runs, err := store.ReconcileSchedulingAuthorities(ctx, now.Add(time.Second))
	if err != nil || len(runs) != 1 || !runs[0].RunnableSince.Equal(expires) {
		t.Fatalf("runs=%+v expires=%s err=%v", runs, expires, err)
	}
}

func TestQuarantinedRunRetainsFailClosedSchedulingAuthority(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	binding := digestBytes([]byte("quarantine-binding"))
	manual := func(id string) application.CreateRunInput {
		return application.CreateRunInput{Run: application.Run{ID: id, IssueID: "IFAN-253", IdempotencyKey: "key-" + id, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task-" + id, Repository: "owner/repo", RepositoryConfigJSON: "{}", RepositoryBindingDigest: binding, BaseBranch: "main", WorkingBranch: "ifan/" + id, ArtifactRoot: "/tmp/" + id, WorktreePath: "/tmp/worktree-" + id, ImplementationModel: "implementation", ReviewModel: "review"}}
	}
	first, created, err := store.CreateRun(ctx, manual("quarantine-a"))
	if err != nil || !created {
		t.Fatal(err)
	}
	// Simulate a pre-v29 duplicate that restart reconciliation must quarantine.
	second := manual("quarantine-b").Run
	second.IssueID = "IFAN-254"
	second.RepositoryBindingDigest = first.RepositoryBindingDigest
	if _, err := store.db.ExecContext(ctx, `INSERT INTO runs(run_id,issue_id,idempotency_key,source_revision,raw_issue_json,raw_issue_hash,normalized_task_json,task_hash,repository,repository_config_json,repository_binding_digest,base_branch,working_branch,worktree_path,artifact_root,current_state,implementation_model,review_model,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, second.ID, second.IssueID, second.IdempotencyKey, second.SourceRevision, second.RawIssueJSON, second.RawIssueHash, second.NormalizedTaskJSON, second.TaskHash, second.Repository, second.RepositoryConfigJSON, second.RepositoryBindingDigest, second.BaseBranch, second.WorkingBranch, second.WorktreePath, second.ArtifactRoot, domain.StateExecuting, second.ImplementationModel, second.ReviewModel, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileSchedulingAuthorities(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if enabled, err := store.HasSchedulingAuthority(ctx, first.ID); err == nil || enabled {
		t.Fatalf("enabled=%t err=%v", enabled, err)
	}
	var slots, permits int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repository_slots WHERE repository_binding_digest=?`, binding).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM heavy_permits WHERE run_id=?`, first.ID).Scan(&permits); err != nil {
		t.Fatal(err)
	}
	if slots != 1 || permits != 1 {
		t.Fatalf("quarantine released authority: slots=%d permits=%d", slots, permits)
	}
	// Even if a stale/terminal cleanup removes the physical slot, the persisted
	// quarantine remains a fail-closed repository-binding authority.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM repository_slots WHERE repository_binding_digest=?`, binding); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.CreateRun(ctx, manual("quarantine-third")); err == nil || created {
		t.Fatalf("third quarantined-binding run created=%t err=%v", created, err)
	}
}

func TestAutomaticRunMissingSchedulingRowFailsClosed(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	lease, _, _ := store.AcquireLinearTodoAdmissionLease(ctx, "scheduler", time.Minute, now)
	reservation := automaticAdmissionReservation(fixtureUUID("missing-scheduling"), "missing-scheduling", "IFAN-255", lease)
	reservation.Input.Repository.RepositoryBindingDigest = digestBytes([]byte("missing-scheduling-binding"))
	reservation.Scheduling = schedulingReservation("IFAN-255", "schema-v29-default", 2, now)
	if _, _, reserved, err := store.ReserveLinearTodoAdmission(ctx, reservation); err != nil || !reserved {
		t.Fatalf("reserved=%t err=%v", reserved, err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM run_scheduling WHERE run_id=?`, reservation.Input.Task.RunID); err != nil {
		t.Fatal(err)
	}
	if enabled, err := store.HasSchedulingAuthority(ctx, reservation.Input.Task.RunID); err == nil || enabled {
		t.Fatalf("enabled=%t err=%v", enabled, err)
	}
}

func TestPreConcurrencySchemaRefusesConcurrencyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.ConfigureHeavyCapacity(ctx, 2, "compatibility-two", now); err != nil {
		t.Fatal(err)
	}
	for index, id := range []string{"compatibility-a", "compatibility-b"} {
		issueID := []string{"IFAN-260", "IFAN-261"}[index]
		input := application.CreateRunInput{Run: application.Run{ID: id, IssueID: issueID, IdempotencyKey: "key-" + id, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw-" + id, NormalizedTaskJSON: "{}", TaskHash: "task-" + id, Repository: "owner/" + id, RepositoryConfigJSON: "{}", RepositoryBindingDigest: digestBytes([]byte("binding:" + id)), BaseBranch: "main", WorkingBranch: "ifan/" + id, ArtifactRoot: "/tmp/" + id, WorktreePath: "/tmp/worktree-" + id, ImplementationModel: "implementation", ReviewModel: "review"}}
		if _, created, err := store.CreateRun(ctx, input); err != nil || !created {
			t.Fatalf("create %s: created=%t err=%v", id, created, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if legacy, err := openWithSupportedSchema(path, 28); err == nil {
		legacy.Close()
		t.Fatal("pre-concurrency schema reader accepted concurrency database")
	} else if !strings.Contains(err.Error(), "database schema version 33 is newer than supported 28") {
		t.Fatalf("compatibility error=%v", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if version <= 28 {
		t.Fatal("pre-concurrency binary could misclassify the database")
	}
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE current_state NOT IN ('rejected','failed','completed')`).Scan(&runs); err != nil || runs != 2 {
		t.Fatalf("compatibility refusal mutated runs=%d err=%v", runs, err)
	}
}

func fixtureUUID(seed string) string {
	return "123e4567-e89b-42d3-a456-" + digestBytes([]byte(seed))[:12]
}

func schedulingReservation(issueID, identity string, capacity int, runnableSince time.Time) application.SchedulingReservation {
	return application.SchedulingReservation{
		OwnerNonce:          "worker",
		CapacityIdentity:    identity,
		Capacity:            capacity,
		RunnableSince:       runnableSince,
		DecisionID:          digestBytes([]byte("decision:" + issueID)),
		IssueSequence:       201,
		Priority:            1,
		RepositoryProfileID: "profile-fixture",
	}
}
