package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOperatorAttentionOutboxIsAppendOnlyIdempotentAndBounded(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first := candidateAttention(t, "scan-2", strings.Repeat("b", 64), time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC))
	second := candidateAttention(t, "scan-1", strings.Repeat("a", 64), time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC))
	if created, err := store.AppendOperatorAttention(ctx, first); err != nil || !created {
		t.Fatalf("first created=%v err=%v", created, err)
	}
	if created, err := store.AppendOperatorAttention(ctx, first); err != nil || created {
		t.Fatalf("duplicate created=%v err=%v", created, err)
	}
	conflict := first
	conflict.ObservedAt = conflict.ObservedAt.Add(time.Second)
	conflict.PayloadDigest = application.OperatorAttentionPayloadDigest(conflict)
	if _, err := store.AppendOperatorAttention(ctx, conflict); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict err=%v", err)
	}
	if created, err := store.AppendOperatorAttention(ctx, second); err != nil || !created {
		t.Fatalf("second created=%v err=%v", created, err)
	}
	events, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 1})
	if err != nil || len(events) != 1 || events[0].EventKey != second.EventKey {
		t.Fatalf("bounded projection=%+v err=%v", events, err)
	}
	if _, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{}); err == nil {
		t.Fatal("expected invalid bound")
	}
}

func TestOperatorAttentionOutboxConcurrentInsertAndSanitizedProjectionParity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := outboxRun(t, "run-outbox")
	if _, _, err := store.CreateRun(context.Background(), application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	event, err := application.SourceCheckoutSkippedAttentionEvent(run, 0, string(application.SourceSyncReasonDirtySource), strings.Repeat("c", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	created := make(chan bool, 16)
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, appendErr := store.AppendOperatorAttention(context.Background(), event)
			created <- ok
			errs <- appendErr
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	count := 0
	for ok := range created {
		if ok {
			count++
		}
	}
	for appendErr := range errs {
		if appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	if count != 1 {
		t.Fatalf("created=%d", count)
	}
	status, err := application.NewQueryService(store).Inspect(context.Background(), application.QueryInput{Requester: application.Requester{ID: "operator", Kind: "github_login"}, RunID: run.ID, Repository: run.Repository})
	if err != nil || len(status.OperatorAttentionEvents) != 1 {
		t.Fatalf("status=%+v err=%v", status.OperatorAttentionEvents, err)
	}
	projected, _ := json.Marshal(status.OperatorAttentionEvents[0])
	wantProjection := application.OperatorAttentionEventResult{SchemaVersion: event.SchemaVersion, EventKey: event.EventKey, EventType: event.EventType, RunID: event.RunID, LinearIdentifier: event.LinearIdentifier, RepositoryProfileID: event.RepositoryProfileID, RepositoryProfileName: event.RepositoryProfileName, ControllerState: event.ControllerState, Severity: event.Severity, ReasonCode: event.ReasonCode, AllowedActions: event.AllowedActions, PayloadDigest: event.PayloadDigest, EvidenceDigest: event.EvidenceDigest, OccurredAt: event.OccurredAt, ObservedAt: event.ObservedAt}
	if !reflect.DeepEqual(status.OperatorAttentionEvents[0], wantProjection) || !bytes.Contains(projected, []byte("payload_digest")) || bytes.Contains(projected, []byte("delivery_status")) {
		t.Fatalf("projection parity failed: status=%+v event=%+v", status.OperatorAttentionEvents[0], event)
	}
	secret := "Authorization: Bearer not-for-output"
	unknown, err := application.CandidateScanIncompleteAttentionEvent("scan-secret", application.OperatorAttentionProfile{ID: "repository-profile:owner/repo", Name: "owner/repo"}, secret, strings.Repeat("d", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOperatorAttention(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("database leak=%v", err)
	}
}

func TestControllerAttentionCandidatesUseStableReaderWithoutVisibilityPredicates(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	other := domain.GitHubUserIdentity{Login: "other", DatabaseID: 8, NodeID: "USER_8", ActorType: "User"}
	makeRun := func(id, repository, profile string, user domain.GitHubUserIdentity) application.Run {
		config, _ := json.Marshal(application.LocalRepository{ProfileID: profile, CanonicalRepository: repository, AllowedOperatorLogins: []string{user.Login}, TrustedOperatorActors: []application.TrustedActorIdentity{{Login: user.Login, DatabaseID: user.DatabaseID, NodeID: user.NodeID, Type: user.ActorType}}})
		issue := "IFAN-177"
		if strings.Contains(id, "hidden") {
			issue = "IFAN-178"
		}
		return application.Run{ID: id, IssueID: issue, IdempotencyKey: "key-" + id, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: repository, RepositoryConfigJSON: string(config), ProfileID: profile, BaseBranch: "main", WorkingBranch: "ifan/177", ArtifactRoot: "/tmp/" + id, ImplementationModel: "model", ReviewModel: "review", State: domain.StateReceived}
	}
	visibleRun := makeRun("run-visible-attention", "owner/visible", "profile-visible", operator)
	hiddenRun := makeRun("run-hidden-attention", "owner/hidden", "profile-hidden", other)
	for _, run := range []application.Run{visibleRun, hiddenRun} {
		if _, created, createErr := store.CreateRun(ctx, application.CreateRunInput{Run: run}); createErr != nil || !created {
			t.Fatalf("create %s created=%t err=%v", run.ID, created, createErr)
		}
	}
	visibleAuthority, err := store.GetRunScopeAuthority(ctx, visibleRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	hiddenAuthority, err := store.GetRunScopeAuthority(ctx, hiddenRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	visibleRun.RepositoryBindingDigest = visibleAuthority.PersistenceBindingValue
	hiddenRun.RepositoryBindingDigest = hiddenAuthority.PersistenceBindingValue
	attentionForRun := func(run application.Run, marker string) application.OperatorAttentionEvent {
		schedule := application.RetrySchedule{RunID: run.ID, Phase: application.AutomaticRetryPhaseForRun(run), ControllerState: string(run.State), AttemptCount: 4, MaxAttempts: 3, InitialDelay: time.Second, MaximumDelay: 30 * time.Second, FailureClass: application.RetryFailureProcessStart, FailureEvidenceRef: marker, ReasonCode: application.RetryReasonBudgetExhausted, Status: application.RetryScheduleAttention, AttentionAt: now, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
		event, eventErr := application.AutomaticRetryAttentionEvent(run, schedule)
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		return event
	}
	visibleEvent := attentionForRun(visibleRun, "attempt:1")
	hiddenEvent := attentionForRun(hiddenRun, "attempt:2")
	visibleRepositoryEvent := candidateAttention(t, "visible-repository", strings.Repeat("e", 64), now.Add(time.Second))
	visibleRepositoryEvent.RepositoryProfileID, visibleRepositoryEvent.RepositoryProfileName = "profile-visible", "owner/visible"
	visibleRepositoryEvent.PayloadDigest = application.OperatorAttentionPayloadDigest(visibleRepositoryEvent)
	hiddenRepositoryEvent := candidateAttention(t, "hidden-repository", strings.Repeat("f", 64), now.Add(time.Second))
	hiddenRepositoryEvent.RepositoryProfileID, hiddenRepositoryEvent.RepositoryProfileName = "profile-hidden", "owner/hidden"
	hiddenRepositoryEvent.PayloadDigest = application.OperatorAttentionPayloadDigest(hiddenRepositoryEvent)
	controllerEvent, err := application.CandidateScanIncompleteAttentionEvent("controller-scan", application.OperatorAttentionProfile{ID: "automation", Name: "linear-todo-admission"}, "truncated", strings.Repeat("a", 64), now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []application.OperatorAttentionEvent{visibleEvent, hiddenEvent, visibleRepositoryEvent, hiddenRepositoryEvent, controllerEvent} {
		if _, appendErr := store.AppendOperatorAttention(ctx, event); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType})
	reader, err := authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET repository_config_json='not-json'`); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListControllerAttentionCandidates(ctx, application.ControllerAttentionCandidateQuery{Authority: reader, Limit: 1001})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, event := range events {
		ids[event.EventKey] = true
	}
	if len(events) != 5 || !ids[visibleEvent.EventKey] || !ids[hiddenEvent.EventKey] || !ids[visibleRepositoryEvent.EventKey] || !ids[hiddenRepositoryEvent.EventKey] || !ids[controllerEvent.EventKey] {
		t.Fatalf("controller candidates=%+v", events)
	}
}

func TestOperatorAttentionCurrentFamilyReaderPreservesWinnersFiltersAndExactCurrentAuthority(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	configured, err := authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		t.Fatal(err)
	}
	controllerReader, err := authorizer.ControllerReadCollectionAuthority(configured)
	if err != nil {
		t.Fatal(err)
	}

	profileID := "repository-profile:owner/one"
	repository := application.LocalRepository{ProfileID: profileID, CanonicalRepository: "owner/one", AllowedOperatorLogins: []string{operator.Login}, TrustedOperatorActors: []application.TrustedActorIdentity{{Login: operator.Login, DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, Type: operator.ActorType}}}
	repositoryJSON, _ := json.Marshal(repository)
	run := application.Run{ID: "run-family-filter", IssueID: "IFAN-190", IdempotencyKey: "family-filter-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(repositoryJSON), ProfileID: profileID, BaseBranch: "main", WorkingBranch: "ifan/190", ArtifactRoot: "/tmp/run-family-filter", ImplementationModel: "model", ReviewModel: "review", State: domain.StateReceived}
	if _, created, createErr := store.CreateRun(ctx, application.CreateRunInput{Run: run}); createErr != nil || !created {
		t.Fatalf("create run created=%t err=%v", created, createErr)
	}
	runAuthority, err := store.GetRunScopeAuthority(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	runScopes, err := authorizer.RunScopes(configured, runAuthority)
	if err != nil {
		t.Fatal(err)
	}
	repositoryScopes, err := authorizer.RepositoryScopes(configured, application.RepositoryAuthority{Repository: run.Repository, ProfileID: profileID, BindingDigest: runAuthority.BindingDigest, AllowedLogins: []string{operator.Login}, TrustedOperators: []domain.GitHubUserIdentity{operator}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO repository_lifecycles(incarnation_id,repository,profile_id,profile_digest,repository_binding_digest,intent,lifecycle_version,current_snapshot_id,updated_at) VALUES(?,?,?,?,?,'enabled',1,'',?),(?,?,?,?,?,'enabled',1,'',?)`, "incarnation-one", run.Repository, profileID, strings.Repeat("7", 64), runAuthority.BindingDigest, formatTime(now), "incarnation-two", "owner/two", "repository-profile:owner/two", strings.Repeat("8", 64), strings.Repeat("9", 64), formatTime(now)); err != nil {
		t.Fatal(err)
	}

	newerRun, err := application.SourceCheckoutSkippedAttentionEvent(run, 0, string(application.SourceSyncReasonDirtySource), strings.Repeat("a", 64), now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	olderButInsertedLast, err := application.SourceCheckoutSkippedAttentionEvent(run, 0, string(application.SourceSyncReasonWrongBranch), strings.Repeat("b", 64), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	olderRepository := candidateAttention(t, "repository-old", strings.Repeat("c", 64), now.Add(3*time.Minute))
	olderRepository.RepositoryProfileID, olderRepository.RepositoryProfileName = profileID, run.Repository
	olderRepository.PayloadDigest = application.OperatorAttentionPayloadDigest(olderRepository)
	newerRepository := candidateAttention(t, "repository-new", strings.Repeat("d", 64), now.Add(4*time.Minute))
	newerRepository.RepositoryProfileID, newerRepository.RepositoryProfileName = profileID, run.Repository
	newerRepository.PayloadDigest = application.OperatorAttentionPayloadDigest(newerRepository)
	hiddenRepository := candidateAttention(t, "repository-hidden", strings.Repeat("e", 64), now.Add(5*time.Minute))
	hiddenRepository.RepositoryProfileID, hiddenRepository.RepositoryProfileName = "repository-profile:owner/two", "owner/two"
	hiddenRepository.PayloadDigest = application.OperatorAttentionPayloadDigest(hiddenRepository)
	distinctFamily, err := application.SchedulerLeaseAttentionEvent("scheduler-one", application.OperatorAttentionProfile{ID: profileID, Name: run.Repository}, "lease_conflict", strings.Repeat("f", 64), now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []application.OperatorAttentionEvent{newerRun, olderButInsertedLast, olderRepository, newerRepository, hiddenRepository, distinctFamily} {
		if _, appendErr := store.AppendOperatorAttention(ctx, event); appendErr != nil {
			t.Fatal(appendErr)
		}
	}

	controller, err := store.ListControllerAttentionCandidates(ctx, application.ControllerAttentionCandidateQuery{Authority: controllerReader, Limit: 1001})
	if err != nil {
		t.Fatal(err)
	}
	wantController := map[string]bool{newerRun.EventKey: true, newerRepository.EventKey: true, hiddenRepository.EventKey: true, distinctFamily.EventKey: true}
	if len(controller) != len(wantController) {
		t.Fatalf("controller candidates=%+v", controller)
	}
	for _, event := range controller {
		if !wantController[event.EventKey] {
			t.Fatalf("unexpected controller winner=%s", event.EventKey)
		}
	}

	repositoryCandidates, err := store.ListRoutineAttentionCandidates(ctx, application.RoutineAttentionCandidateQuery{Scopes: repositoryScopes, Scope: application.ScopeRepository, TargetID: run.Repository, RepositoryProfileID: profileID, Limit: 1001})
	if err != nil {
		t.Fatal(err)
	}
	wantRepository := map[string]bool{newerRun.EventKey: true, newerRepository.EventKey: true, distinctFamily.EventKey: true}
	if len(repositoryCandidates) != len(wantRepository) {
		t.Fatalf("repository candidates=%+v", repositoryCandidates)
	}
	for _, event := range repositoryCandidates {
		if !wantRepository[event.EventKey] {
			t.Fatalf("repository filter leaked=%s", event.EventKey)
		}
	}

	runCandidates, err := store.ListRoutineAttentionCandidates(ctx, application.RoutineAttentionCandidateQuery{Scopes: runScopes, Scope: application.ScopeRun, TargetID: run.ID, Limit: 1001})
	if err != nil || len(runCandidates) != 1 || runCandidates[0].EventKey != newerRun.EventKey {
		t.Fatalf("run candidates=%+v err=%v", runCandidates, err)
	}
	if _, err := store.ListRoutineAttentionCandidates(ctx, application.RoutineAttentionCandidateQuery{Scopes: repositoryScopes, Scope: application.ScopeController, Limit: 1001}); err == nil {
		t.Fatal("target-specific candidate path accepted Controller scope")
	}

	exact, found, err := store.CurrentOperatorAttention(ctx, run.ID)
	if err != nil || !found || exact.EventKey != olderButInsertedLast.EventKey {
		t.Fatalf("exact current=%+v found=%t err=%v", exact, found, err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var overview application.RoutinePersistedOverviewSnapshot
	if err := readRoutineOverviewAttention(ctx, tx, 100, &overview); err != nil {
		t.Fatal(err)
	}
	if overview.AttentionTotal != len(wantController) || len(overview.Attention) != len(wantController) {
		t.Fatalf("overview attention=%+v total=%d", overview.Attention, overview.AttentionTotal)
	}
	overviewWinners := make(map[string]bool, len(overview.Attention))
	for _, item := range overview.Attention {
		overviewWinners[item.EventID] = true
	}
	for winner := range wantController {
		if !overviewWinners[winner] {
			t.Fatalf("overview and complete Attention disagree on winner %s", winner)
		}
	}
	wantTargets := map[string]struct {
		scope  application.AuthorityScopeKind
		target string
	}{
		newerRun.EventKey:         {scope: application.ScopeRun, target: run.ID},
		newerRepository.EventKey:  {scope: application.ScopeRepository, target: run.Repository},
		hiddenRepository.EventKey: {scope: application.ScopeRepository, target: "owner/two"},
		distinctFamily.EventKey:   {scope: application.ScopeRepository, target: run.Repository},
	}
	for index, item := range overview.Attention {
		want := wantTargets[item.EventID]
		if item.Scope != want.scope || item.TargetID != want.target || overview.Actionable[index].ItemID != item.EventID || overview.Actionable[index].Scope != item.Scope || overview.Actionable[index].TargetID != item.TargetID {
			t.Fatalf("overview target/actionable mismatch attention=%+v actionable=%+v", item, overview.Actionable[index])
		}
	}
	var bounded application.RoutinePersistedOverviewSnapshot
	if err := readRoutineOverviewAttention(ctx, tx, 2, &bounded); err != nil {
		t.Fatal(err)
	}
	if bounded.AttentionTotal != len(wantController) || !bounded.AttentionTruncated || bounded.ActionableTotal != len(wantController) || !bounded.ActionableTruncated || len(bounded.Attention) != 2 || len(bounded.Actionable) != 2 || bounded.Attention[0].EventID != newerRun.EventKey || bounded.Attention[1].EventID != newerRepository.EventKey {
		t.Fatalf("bounded overview=%+v", bounded)
	}
	if bounded.QueueAttention == nil || bounded.QueueAttention.Degraded || bounded.QueueAttention.ReasonCode != "candidate_scan_attention" || bounded.QueueAttention.OccurredAt != newerRepository.OccurredAt {
		t.Fatalf("bounded queue attention=%+v", bounded.QueueAttention)
	}
}

func TestOperatorAttentionCurrentFamilyReaderValidatesHistoricalSchemasAndCorruption(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	legacy := candidateAttention(t, "legacy-family", strings.Repeat("a", 64), now)
	legacy.SchemaVersion = application.OperatorAttentionLegacySchemaVersion
	legacy.AllowedActions = []application.OperatorAttentionActionID{}
	legacy.PayloadDigest = legacyOperatorAttentionPayloadDigest(legacy, "pending_local")
	insertOperatorAttentionFixture(t, store, legacy, legacy.PayloadDigest, "pending_local")

	previous, err := application.SchedulerLeaseAttentionEvent("previous-family", application.OperatorAttentionProfile{ID: "repository-profile:owner/repo", Name: "owner/repo"}, "lease_conflict", strings.Repeat("b", 64), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	previous.SchemaVersion = application.OperatorAttentionPreviousSchemaVersion
	previous.PayloadDigest = previousOperatorAttentionPayloadDigestFixture(previous)
	insertOperatorAttentionFixture(t, store, previous, "", "")

	current := candidateAttention(t, "current-family", strings.Repeat("c", 64), now.Add(2*time.Minute))
	current.RepositoryProfileID, current.RepositoryProfileName = "repository-profile:owner/other", "owner/other"
	current.PayloadDigest = application.OperatorAttentionPayloadDigest(current)
	if _, err := store.AppendOperatorAttention(ctx, current); err != nil {
		t.Fatal(err)
	}
	read, err := readCurrentOperatorAttentionFamilies(ctx, store.db, currentOperatorAttentionFamilyRead{Filter: currentOperatorAttentionFamilyAll, Limit: 10, Count: true})
	if err != nil || read.Total != 3 || len(read.Events) != 3 {
		t.Fatalf("historical read=%+v err=%v", read, err)
	}
	schemas := map[int]bool{}
	for _, event := range read.Events {
		schemas[event.SchemaVersion] = true
	}
	if !schemas[0] || !schemas[1] || !schemas[2] {
		t.Fatalf("schemas=%v", schemas)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE operator_attention_outbox SET payload_digest=? WHERE event_key=?`, strings.Repeat("0", 64), previous.EventKey); err != nil {
		t.Fatal(err)
	}
	if _, err := readCurrentOperatorAttentionFamilies(ctx, store.db, currentOperatorAttentionFamilyRead{Filter: currentOperatorAttentionFamilyAll, Limit: 10}); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt historical row error=%v", err)
	}
}

func TestRoutineOverviewAttentionUsesCallerReadTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := second.db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	older, err := application.CandidateScanIncompleteAttentionEvent("overview-old", application.OperatorAttentionProfile{ID: "automation", Name: "linear-todo-admission"}, "truncated", strings.Repeat("a", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := application.CandidateScanIncompleteAttentionEvent("overview-new", application.OperatorAttentionProfile{ID: "automation", Name: "linear-todo-admission"}, "truncated", strings.Repeat("b", 64), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendOperatorAttention(ctx, older); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var snapshotCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operator_attention_outbox`).Scan(&snapshotCount); err != nil || snapshotCount != 1 {
		t.Fatalf("snapshot count=%d err=%v", snapshotCount, err)
	}
	if _, err := second.AppendOperatorAttention(ctx, newer); err != nil {
		t.Fatal(err)
	}
	var overview application.RoutinePersistedOverviewSnapshot
	if err := readRoutineOverviewAttention(ctx, tx, 10, &overview); err != nil {
		t.Fatal(err)
	}
	if overview.AttentionTotal != 1 || len(overview.Attention) != 1 || overview.Attention[0].EventID != older.EventKey {
		t.Fatalf("transaction snapshot=%+v", overview)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	configured, _ := authorizer.ResolveConfiguredRequester(application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType})
	reader, _ := authorizer.ControllerReadCollectionAuthority(configured)
	candidates, err := store.ListControllerAttentionCandidates(ctx, application.ControllerAttentionCandidateQuery{Authority: reader, Limit: 10})
	if err != nil || len(candidates) != 1 || candidates[0].EventKey != newer.EventKey {
		t.Fatalf("post-transaction candidates=%+v err=%v", candidates, err)
	}
}

func TestOperatorAttentionMigrationPreservesLegacyEvidenceAndNormalizesEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	removeOnboardingV37(t, store.db)
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_attention_outbox`); err != nil {
		t.Fatal(err)
	}
	removeConfigurationV31(t, store.db)
	removeRepositoryLifecycleV35(t, store.db)
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_actions`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operation_receipts`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE automatic_retry_schedules DROP COLUMN failure_evidence_ref`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE attempts DROP COLUMN process_control_key`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range migrationV17 {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC)
	evidence := strings.Repeat("e", 64)
	key := "automation:run-legacy:automatic_retry_attention:" + evidence
	legacyEvent := application.OperatorAttentionEvent{SchemaVersion: application.OperatorAttentionLegacySchemaVersion, EventKey: key, EventType: application.OperatorAttentionRetry, RunID: "run-legacy", RepositoryProfileID: "repository-profile:owner/repo", RepositoryProfileName: "owner/repo", ControllerState: string(domain.StateExecuting), Severity: "error", ReasonCode: application.RetryReasonBudgetExhausted, AllowedActions: []application.OperatorAttentionActionID{}, EvidenceDigest: evidence, OccurredAt: now, ObservedAt: now}
	legacyDigest := legacyOperatorAttentionPayloadDigest(legacyEvent, "pending_local")
	_, err = store.db.ExecContext(ctx, `INSERT INTO operator_attention_outbox(event_key,payload_digest,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,evidence_digest,occurred_at,observed_at,delivery_status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, key, legacyDigest, application.OperatorAttentionRetry, "run-legacy", "", "repository-profile:owner/repo", "owner/repo", string(domain.StateExecuting), "error", application.RetryReasonBudgetExhausted, evidence, formatTime(now), formatTime(now), "pending_local", formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	event := events[0]
	if event.SchemaVersion != application.OperatorAttentionLegacySchemaVersion || event.PayloadDigest != legacyDigest || len(event.AllowedActions) != 0 {
		t.Fatalf("normalized event=%+v", event)
	}
	var retainedDigest, retainedStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT legacy_payload_digest,legacy_delivery_status FROM operator_attention_outbox WHERE event_key=?`, key).Scan(&retainedDigest, &retainedStatus); err != nil {
		t.Fatal(err)
	}
	if retainedDigest != legacyDigest || retainedStatus != "pending_local" {
		t.Fatalf("legacy digest=%q status=%q", retainedDigest, retainedStatus)
	}
	replay := event
	replay.SchemaVersion = application.OperatorAttentionSchemaVersion
	// Legacy rows did not persist a failure class. A current replay must stay
	// conservative and cannot infer retry authority from the old presentation.
	replay.AllowedActions = []application.OperatorAttentionActionID{application.OperatorAttentionActionAbandon}
	replay.PayloadDigest = application.OperatorAttentionPayloadDigest(replay)
	if created, err := store.AppendOperatorAttention(ctx, replay); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	conflict := replay
	conflict.ObservedAt = conflict.ObservedAt.Add(time.Second)
	conflict.PayloadDigest = application.OperatorAttentionPayloadDigest(conflict)
	if _, err := store.AppendOperatorAttention(ctx, conflict); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("legacy conflict err=%v", err)
	}
}

func TestOperatorAttentionMigrationAcceptsFrozenLegacyProfileContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	removeOnboardingV37(t, store.db)
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_attention_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operator_actions`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE operation_receipts`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE automatic_retry_schedules DROP COLUMN failure_evidence_ref`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE attempts DROP COLUMN process_control_key`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range migrationV17 {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	removeConfigurationV31(t, store.db)
	removeRepositoryLifecycleV35(t, store.db)
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 5, 0, 0, 0, time.UTC)
	evidence := strings.Repeat("a", 64)
	event := application.OperatorAttentionEvent{SchemaVersion: application.OperatorAttentionLegacySchemaVersion, EventKey: "automation:scan-legacy:candidate_scan_incomplete:" + evidence, EventType: application.OperatorAttentionCandidateScan, RepositoryProfileID: "https://host/profile", RepositoryProfileName: "file:/private/repo", ControllerState: "scan", Severity: "warning", ReasonCode: "truncated", AllowedActions: []application.OperatorAttentionActionID{}, EvidenceDigest: evidence, OccurredAt: now, ObservedAt: now}
	event.PayloadDigest = legacyOperatorAttentionPayloadDigest(event, "pending_local")
	_, err = store.db.ExecContext(ctx, `INSERT INTO operator_attention_outbox(event_key,payload_digest,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,evidence_digest,occurred_at,observed_at,delivery_status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.EventKey, event.PayloadDigest, event.EventType, "", "", event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, event.EvidenceDigest, formatTime(now), formatTime(now), "pending_local", formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{Limit: 10})
	if err != nil || len(events) != 1 || events[0].PayloadDigest != event.PayloadDigest || events[0].RepositoryProfileID != event.RepositoryProfileID || events[0].RepositoryProfileName != event.RepositoryProfileName {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestManualInterventionReplayBindsTransitionInsteadOfMutableRunTimestamp(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run := outboxRun(t, "run-manual-transition")
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]domain.State{{domain.StateReceived, domain.StateAdmitting}, {domain.StateAdmitting, domain.StateProvisioning}, {domain.StateProvisioning, domain.StateExecuting}, {domain.StateExecuting, domain.StateManualIntervention}} {
		if err := store.Transition(ctx, run.ID, edge[0], edge[1], "fixture transition", "fixture_evidence", ""); err != nil {
			t.Fatal(err)
		}
	}
	run, err = store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, run.ID)
	if err != nil || len(inspection.Timeline) == 0 {
		t.Fatalf("inspection=%+v err=%v", inspection, err)
	}
	transition := inspection.Timeline[len(inspection.Timeline)-1]
	first, err := application.ManualInterventionAttentionEvent(run, transition)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.AppendOperatorAttention(ctx, first); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if acquired, err := store.AcquireLease(ctx, run.ID, "fixture-owner", time.Now().UTC().Add(time.Minute)); err != nil || !acquired {
		t.Fatalf("acquired=%v err=%v", acquired, err)
	}
	if err := store.ReleaseLease(ctx, run.ID, "fixture-owner"); err != nil {
		t.Fatal(err)
	}
	run, err = store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := application.ManualInterventionAttentionEvent(run, transition)
	if err != nil || second.EventKey != first.EventKey || second.PayloadDigest != first.PayloadDigest {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
	if created, err := store.AppendOperatorAttention(ctx, second); err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
}

func candidateAttention(t *testing.T, scanID, digest string, now time.Time) application.OperatorAttentionEvent {
	t.Helper()
	event, err := application.CandidateScanIncompleteAttentionEvent(scanID, application.OperatorAttentionProfile{ID: "repository-profile:owner/repo", Name: "owner/repo"}, "truncated", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func insertOperatorAttentionFixture(t *testing.T, store *admissionTestStore, event application.OperatorAttentionEvent, legacyPayload, legacyDelivery string) {
	t.Helper()
	actions, err := json.Marshal(event.AllowedActions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(context.Background(), `INSERT INTO operator_attention_outbox(event_key,payload_digest,schema_version,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,allowed_actions_json,evidence_digest,occurred_at,observed_at,legacy_payload_digest,legacy_delivery_status,created_at,retry_failure_class) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.EventKey, event.PayloadDigest, event.SchemaVersion, event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, string(actions), event.EvidenceDigest, formatTime(event.OccurredAt), formatTime(event.ObservedAt), legacyPayload, legacyDelivery, nowText(), event.RetryFailureClass)
	if err != nil {
		t.Fatal(err)
	}
}

func previousOperatorAttentionPayloadDigestFixture(event application.OperatorAttentionEvent) string {
	payload := struct {
		SchemaVersion                                                                                                                         int
		EventType, RunID, LinearIdentifier, RepositoryProfileID, RepositoryProfileName, ControllerState, Severity, ReasonCode, EvidenceDigest string
		AllowedActions                                                                                                                        []application.OperatorAttentionActionID
		OccurredAt, ObservedAt                                                                                                                string
	}{event.SchemaVersion, event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, event.EvidenceDigest, event.AllowedActions, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.ObservedAt.UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(payload)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func outboxRun(t *testing.T, id string) application.Run {
	t.Helper()
	config, err := json.Marshal(application.LocalRepository{ProfileID: "repository-profile:owner/repo", CanonicalRepository: "owner/repo", AllowedOperatorLogins: []string{"operator"}})
	if err != nil {
		t.Fatal(err)
	}
	return application.Run{ID: id, IssueID: "IFAN-34", IdempotencyKey: "key-" + id, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "owner/repo", RepositoryConfigJSON: string(config), ProfileID: "repository-profile:owner/repo", BaseBranch: "main", WorkingBranch: "ifan/34", ArtifactRoot: "/tmp/" + id, ImplementationModel: "model", ReviewModel: "review", State: domain.StateReceived}
}
