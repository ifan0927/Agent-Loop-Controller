package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOperationReceiptPersistsScopeNeutralLifecycleReplayAndConflict(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	base := application.OperationReceiptInput{OperationType: application.OperationRetry, Scope: application.ScopeRepository, TargetID: "owner/repo", Requester: requester, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("d", 64), TargetBindingDigest: strings.Repeat("c", 64), AcceptedAt: time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)}
	receipt := application.NewOperationReceipt(base)
	persisted, created, err := store.BeginOperationReceipt(ctx, receipt)
	if err != nil || !created || persisted.OperationID != receipt.OperationID || persisted.Phase != application.OperationPhaseAccepted {
		t.Fatalf("persisted=%+v created=%t err=%v", persisted, created, err)
	}
	replay := receipt
	replay.AcceptedAt = replay.AcceptedAt.Add(time.Hour)
	persisted, created, err = store.BeginOperationReceipt(ctx, replay)
	if err != nil || created || !persisted.AcceptedAt.Equal(receipt.AcceptedAt) {
		t.Fatalf("replay=%+v created=%t err=%v", persisted, created, err)
	}
	drift := base
	drift.RequestDigest = strings.Repeat("d", 64)
	if _, _, err := store.BeginOperationReceipt(ctx, application.NewOperationReceipt(drift)); !errors.Is(err, application.ErrOperationReceiptConflict) {
		t.Fatalf("payload drift error=%v", err)
	}
	authorityDrift := base
	authorityDrift.ExpectedAuthorityDigest = strings.Repeat("e", 64)
	if _, _, err := store.BeginOperationReceipt(ctx, application.NewOperationReceipt(authorityDrift)); !errors.Is(err, application.ErrOperationReceiptConflict) {
		t.Fatalf("authority drift error=%v", err)
	}
	appliedAt := receipt.AcceptedAt.Add(time.Second)
	applied, changed, err := store.AdvanceOperationReceipt(ctx, application.OperationReceiptMutation{OperationID: receipt.OperationID, ExpectedPhase: application.OperationPhaseAccepted, Phase: application.OperationPhaseApplied, Outcome: application.OperationOutcomePending, ResultingState: "ready", ResultingVersion: 2, EvidenceDigest: strings.Repeat("e", 64), At: appliedAt})
	if err != nil || !changed || applied.Phase != application.OperationPhaseApplied || applied.Outcome != application.OperationOutcomePending {
		t.Fatalf("applied=%+v changed=%t err=%v", applied, changed, err)
	}
	settledMutation := application.OperationReceiptMutation{OperationID: receipt.OperationID, ExpectedPhase: application.OperationPhaseApplied, Phase: application.OperationPhaseObserved, Outcome: application.OperationOutcomeSucceeded, ResultingState: "ready", ResultingVersion: 2, EvidenceDigest: strings.Repeat("e", 64), ResultDigest: strings.Repeat("f", 64), At: appliedAt.Add(time.Second)}
	settled, changed, err := store.AdvanceOperationReceipt(ctx, settledMutation)
	if err != nil || !changed || settled.Phase != application.OperationPhaseObserved || settled.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("settled=%+v changed=%t err=%v", settled, changed, err)
	}
	if replayed, changed, err := store.AdvanceOperationReceipt(ctx, settledMutation); err != nil || changed || replayed.OperationID != settled.OperationID {
		t.Fatalf("settled replay=%+v changed=%t err=%v", replayed, changed, err)
	}
}

func TestFindRepositoryOperationReceiptNormalizesRequesterLogin(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	requester := domain.GitHubUserIdentity{Login: "Operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	requestDigest := strings.Repeat("a", 64)
	bindingDigest := strings.Repeat("b", 64)
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{
		OperationType:           application.OperationEnableRepository,
		Scope:                   application.ScopeRepository,
		TargetID:                "owner/repo",
		Requester:               requester,
		RequestDigest:           requestDigest,
		ExpectedAuthorityDigest: strings.Repeat("c", 64),
		OperationAnchorDigest:   strings.Repeat("d", 64),
		TargetBindingDigest:     bindingDigest,
		AcceptedAt:              time.Date(2026, 9, 3, 1, 0, 0, 0, time.UTC),
	})
	if _, created, err := store.BeginOperationReceipt(context.Background(), receipt); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	found, ok, err := store.FindRepositoryOperationReceipt(context.Background(), application.OperationEnableRepository, "owner/repo", requester, requestDigest, bindingDigest)
	if err != nil || !ok || found.OperationID != receipt.OperationID {
		t.Fatalf("found=%+v ok=%t err=%v", found, ok, err)
	}
}

func TestOperationReceiptServiceClassifiesAuthorityAndLifecycleDriftAsConflict(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := application.NewOperationReceiptService(store)
	if err != nil {
		t.Fatal(err)
	}
	input := application.OperationReceiptInput{OperationType: application.OperationRetry, Scope: application.ScopeController, TargetID: "local-controller", Requester: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("d", 64), TargetBindingDigest: strings.Repeat("c", 64)}
	receipt, _, err := service.Accept(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	drift := input
	drift.RequestDigest = strings.Repeat("d", 64)
	_, _, driftErr := service.Accept(context.Background(), drift)
	_, _, lifecycleErr := service.RecordSettled(context.Background(), application.OperationReceiptMutation{OperationID: receipt.OperationID, ExpectedPhase: application.OperationPhaseApplied, Phase: application.OperationPhaseObserved, Outcome: application.OperationOutcomeConflict, At: receipt.AcceptedAt.Add(time.Second)})
	for _, err := range []error{driftErr, lifecycleErr} {
		var serviceErr *application.ServiceError
		if !errors.As(err, &serviceErr) || serviceErr.Category != application.ErrorConflict {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestOperationReceiptPersistsPreApplyTerminalOutcomes(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	for index, outcome := range []application.OperationOutcome{application.OperationOutcomeFailed, application.OperationOutcomeConflict, application.OperationOutcomeAmbiguous} {
		anchor := strings.Repeat(string(rune('d'+index)), 64)
		receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationAbandon, Scope: application.ScopeController, TargetID: "local-controller", Requester: requester, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: anchor, TargetBindingDigest: strings.Repeat("c", 64), AcceptedAt: time.Date(2026, 8, 13, 1, index, 0, 0, time.UTC)})
		if _, _, err := store.BeginOperationReceipt(context.Background(), receipt); err != nil {
			t.Fatal(err)
		}
		settled, changed, err := store.AdvanceOperationReceipt(context.Background(), application.OperationReceiptMutation{OperationID: receipt.OperationID, ExpectedPhase: application.OperationPhaseAccepted, Phase: application.OperationPhaseAccepted, Outcome: outcome, ResultDigest: strings.Repeat("f", 64), At: receipt.AcceptedAt.Add(time.Second)})
		if err != nil || !changed || settled.Phase != application.OperationPhaseAccepted || settled.Outcome != outcome || settled.SettledAt.IsZero() || !settled.AppliedAt.IsZero() {
			t.Fatalf("outcome=%s receipt=%+v changed=%t err=%v", outcome, settled, changed, err)
		}
	}
}

func TestConcurrentOperationReceiptAcceptanceConverges(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationAbandon, Scope: application.ScopeController, TargetID: "local-controller", Requester: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("d", 64), TargetBindingDigest: strings.Repeat("c", 64), AcceptedAt: time.Now().UTC()})
	var wg sync.WaitGroup
	created := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, wasCreated, beginErr := store.BeginOperationReceipt(context.Background(), receipt)
			created <- wasCreated
			errs <- beginErr
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	createdCount := 0
	for value := range created {
		if value {
			createdCount++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created=%d", createdCount)
	}
}

func TestAuthorizedOperationReceiptQueryIsNonDisclosing(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	repository := application.LocalRepository{ProfileID: "repository-profile:owner/repo", CanonicalRepository: "owner/repo", AllowedOperatorLogins: []string{"operator"}, TrustedOperatorActors: []application.TrustedActorIdentity{{Login: identity.Login, DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, Type: identity.ActorType}}}
	raw, _ := json.Marshal(repository)
	run := application.Run{ID: "run-receipt", IssueID: "IFAN-108", IdempotencyKey: "run-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), ProfileID: repository.ProfileID, RepositoryBindingDigest: strings.Repeat("c", 64), BaseBranch: "main", WorkingBranch: "ifan/108", ArtifactRoot: "artifact-root", ImplementationModel: "model", ReviewModel: "review", State: domain.StateReceived}
	if _, created, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil || !created {
		t.Fatalf("create=%t err=%v", created, err)
	}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationRetry, Scope: application.ScopeRun, TargetID: run.ID, Requester: identity, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("d", 64), TargetBindingDigest: run.RepositoryBindingDigest, AcceptedAt: time.Now().UTC()})
	if _, _, err := store.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	authorizer, _ := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: identity})
	query, _ := application.NewOperationReceiptQueryService(store, authorizer, nil, nil)
	requester := application.Requester{ID: identity.Login, Kind: "github_login", DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, ActorType: identity.ActorType}
	got, err := query.Get(ctx, requester, receipt.OperationID)
	if err != nil || got.OperationID != receipt.OperationID {
		t.Fatalf("receipt=%+v err=%v", got, err)
	}
	_, unknownErr := query.Get(ctx, requester, "operation-unknown")
	denied := requester
	denied.DatabaseID++
	_, deniedErr := query.Get(ctx, denied, receipt.OperationID)
	var unknownService, deniedService *application.ServiceError
	if !errors.As(unknownErr, &unknownService) || !errors.As(deniedErr, &deniedService) || unknownService.Category != application.ErrorNotFound || deniedService.Category != application.ErrorNotFound || unknownErr.Error() != deniedErr.Error() {
		t.Fatalf("unknown=%v denied=%v", unknownErr, deniedErr)
	}
}

func TestMigrationV30BackfillsLegacyOperatorActionAsReadableReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openWithSupportedSchema(path, 29)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 33, NodeID: "USER_33", ActorType: "User"}
	repository := application.LocalRepository{ProfileID: "repository-profile:owner/repo", CanonicalRepository: "owner/repo", AllowedOperatorLogins: []string{"operator"}, TrustedOperatorActors: []application.TrustedActorIdentity{{Login: identity.Login, DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, Type: identity.ActorType}}}
	raw, _ := json.Marshal(repository)
	run := application.Run{ID: "run-legacy-action", IssueID: "IFAN-108", IdempotencyKey: "legacy-run-key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: repository.CanonicalRepository, RepositoryConfigJSON: string(raw), ProfileID: repository.ProfileID, RepositoryBindingDigest: strings.Repeat("c", 64), BaseBranch: "main", WorkingBranch: "ifan/108", ArtifactRoot: "artifact-root", ImplementationModel: "model", ReviewModel: "review", State: domain.StateExecuting}
	if _, created, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil || !created {
		t.Fatalf("create=%t err=%v", created, err)
	}
	var sequence int64
	if err := store.db.QueryRowContext(ctx, `SELECT MAX(sequence) FROM transitions WHERE run_id=?`, run.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	event := application.OperatorAttentionEvent{SchemaVersion: application.OperatorAttentionSchemaVersion, EventKey: "automation:" + run.ID + ":automatic_retry_attention:" + strings.Repeat("e", 64), EventType: application.OperatorAttentionRetry, RunID: run.ID, RepositoryProfileID: run.ProfileID, RepositoryProfileName: run.Repository, ControllerState: string(run.State), Severity: "error", ReasonCode: application.RetryReasonBudgetExhausted, RetryFailureClass: application.RetryFailureProcessStart, AllowedActions: []application.OperatorAttentionActionID{application.OperatorAttentionActionRetry, application.OperatorAttentionActionAbandon}, EvidenceDigest: strings.Repeat("e", 64), OccurredAt: now, ObservedAt: now}
	event.PayloadDigest = application.OperatorAttentionPayloadDigest(event)
	if _, err := store.AppendOperatorAttention(ctx, event); err != nil {
		t.Fatal(err)
	}
	payload := struct {
		RunID, Repository, ExpectedState, RunKey, ActionType, RequesterLogin, RequesterNode, RequesterType, Reason, EventKey string
		TransitionSequence, RequesterDatabaseID                                                                              int64
	}{run.ID, run.Repository, string(run.State), run.IdempotencyKey, string(application.OperatorActionRetry), identity.Login, identity.NodeID, identity.ActorType, event.ReasonCode, event.EventKey, sequence, identity.DatabaseID}
	encoded, _ := json.Marshal(payload)
	payloadSum := sha256.Sum256(encoded)
	payloadDigest := hex.EncodeToString(payloadSum[:])
	idempotencySum := sha256.Sum256([]byte("operator-action-idempotency:" + payloadDigest))
	idempotency := hex.EncodeToString(idempotencySum[:])
	actionID := "operator-action-" + idempotency[:24]
	if _, err := store.db.ExecContext(ctx, `INSERT INTO operator_actions(action_id,idempotency_key,payload_digest,run_id,repository,expected_state,run_idempotency_key,transition_sequence,action_type,requester_login,requester_database_id,requester_node_id,requester_actor_type,reason_code,attention_event_key,status,result_status,received_at,validated_at,next_eligible_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, actionID, idempotency, payloadDigest, run.ID, run.Repository, string(run.State), run.IdempotencyKey, sequence, string(application.OperatorActionRetry), identity.Login, identity.DatabaseID, identity.NodeID, identity.ActorType, event.ReasonCode, event.EventKey, application.OperatorActionStatusValidated, application.OperatorActionResultPending, formatTime(now), formatTime(now), ""); err != nil {
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
	inspection, err := reopened.Inspect(ctx, run.ID)
	if err != nil || len(inspection.OperatorActions) != 1 || inspection.OperatorActions[0].ActionID != actionID {
		t.Fatalf("inspection=%+v err=%v", inspection.OperatorActions, err)
	}
	service, err := application.NewOperatorActionService(reopened)
	if err != nil {
		t.Fatal(err)
	}
	replayInput := operatorActionInput(run, event, sequence, application.OperatorActionRetry)
	replayInput.Requester = application.Requester{ID: identity.Login, Kind: "github_login", DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, ActorType: identity.ActorType}
	replayed, created, err := service.Prepare(ctx, replayInput)
	if err != nil || created || replayed.ActionID != actionID {
		t.Fatalf("legacy replay=%+v created=%t err=%v", replayed, created, err)
	}
	var operationID string
	if err := reopened.db.QueryRowContext(ctx, `SELECT operation_id FROM operation_receipts WHERE source_action_id=?`, actionID).Scan(&operationID); err != nil || !strings.HasPrefix(operationID, "operation-") {
		t.Fatalf("operation=%q err=%v", operationID, err)
	}
}
