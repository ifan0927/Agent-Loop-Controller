package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestHistoricalCIWaitRecoveryEvidenceReopensReadOnly(t *testing.T) {
	for _, state := range []string{application.OperatorActionStatusValidated, application.OperatorActionStatusApplied, application.OperatorActionStatusObserved} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "controller.db")
			store, err := openAdmissionTestStore(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			now := time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC)
			run := outboxRun(t, "run-retired-ci-"+state)
			var repositoryConfig application.LocalRepository
			if err := json.Unmarshal([]byte(run.RepositoryConfigJSON), &repositoryConfig); err != nil {
				store.Close()
				t.Fatal(err)
			}
			repositoryConfig.TrustedOperatorActors = []application.TrustedActorIdentity{{Login: "operator", DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", Type: "User"}}
			configRaw, _ := json.Marshal(repositoryConfig)
			run.RepositoryConfigJSON = string(configRaw)
			run.State = domain.StatePROpen
			run.RepositoryBindingDigest = strings.Repeat("c", 64)
			run.CandidateHead = strings.Repeat("a", 40)
			run.BaseSHA = strings.Repeat("b", 40)
			if _, created, err := store.CreateRun(ctx, application.CreateRunInput{Run: run}); err != nil || !created {
				store.Close()
				t.Fatalf("created=%t err=%v", created, err)
			}
			if err := store.SetWorkspace(ctx, run.ID, strings.Repeat("b", 40), filepath.Join(t.TempDir(), "historical-worktree")); err != nil {
				store.Close()
				t.Fatal(err)
			}
			if err := store.SetCandidateHead(ctx, run.ID, strings.Repeat("a", 40)); err != nil {
				store.Close()
				t.Fatal(err)
			}
			states := []domain.State{domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning, domain.StateExecuting, domain.StateVerifying, domain.StateFreshReview, domain.StateApprovalReady, domain.StatePushingBranch, domain.StateBranchPushed, domain.StateOpeningPR, domain.StatePROpen}
			for index := 0; index < len(states)-1; index++ {
				if err := store.Transition(ctx, run.ID, states[index], states[index+1], "historical lifecycle", "historical-evidence", strings.Repeat("a", 40)); err != nil {
					store.Close()
					t.Fatal(err)
				}
			}
			inspection, err := store.Inspect(ctx, run.ID)
			if err != nil || len(inspection.Timeline) == 0 {
				store.Close()
				t.Fatalf("inspection=%+v err=%v", inspection, err)
			}
			run = inspection.Run
			sequence := inspection.Timeline[len(inspection.Timeline)-1].Sequence
			schedule, changed, err := store.ApplyRetryFailure(ctx, application.RetryFailureRequest{RunID: run.ID, Phase: application.AutomaticRetryPhaseForRun(run), ControllerState: run.State, ExpectedAttempt: 0, FailureClass: application.RetryFailureTerminal, ReasonCode: application.RetryReasonTerminal, Now: now, Policy: application.DefaultAutomaticRetryPolicy()})
			if err != nil || !changed || schedule.Status != application.RetryScheduleAttention {
				store.Close()
				t.Fatalf("schedule=%+v changed=%t err=%v", schedule, changed, err)
			}
			event := historicalCIWaitAttention(run, schedule, now)
			insertHistoricalCIWaitAttention(t, store, event)

			repository := domain.RepositoryIdentity{ID: 99, NodeID: "REPO_99", Owner: "owner", Name: "repo"}
			pullRequest := domain.PullRequest{Number: 7, DatabaseID: 70, URL: "https://example.invalid/pull/7", NodeID: "PR_7", HeadBranch: run.WorkingBranch, BaseBranch: run.BaseBranch, HeadSHA: run.CandidateHead, BaseSHA: run.BaseSHA, BodyDigest: strings.Repeat("d", 64), OwnershipKey: run.IdempotencyKey, State: "open"}
			if err := store.SavePullRequest(ctx, run.ID, pullRequest); err != nil {
				store.Close()
				t.Fatal(err)
			}
			metadata := application.GitHubInstallationMetadata{AppID: 11, InstallationID: 22, Repository: repository, TokenExpiresAt: now.Add(time.Hour), PermissionsDigest: strings.Repeat("e", 64), ObservedAt: now}
			if err := store.SaveGitHubInstallation(ctx, run.ID, metadata); err != nil {
				store.Close()
				t.Fatal(err)
			}
			request := application.GitHubRequestObservation{RunID: run.ID, Operation: "historical_ci_wait_recovery", Category: "REST", HTTPStatus: 200, ResponseDigest: strings.Repeat("f", 64), InstallationID: metadata.InstallationID, Repository: repository, ObservedAt: now}
			if err := store.SaveGitHubRequest(ctx, request); err != nil {
				store.Close()
				t.Fatal(err)
			}
			evidence := domain.GitHubReadEvidence{Repository: repository, PullRequest: pullRequest, Checks: []domain.GitHubCheck{{Name: "test", Required: true, ObservedSHA: run.CandidateHead, State: domain.CheckSuccess}}, ObservedAt: now}
			if err := store.SaveGitHubEvidence(ctx, run.ID, evidence); err != nil {
				store.Close()
				t.Fatal(err)
			}

			action := historicalCIWaitAction(run, event, sequence, state, now)
			if err := application.ValidateOperatorActionRecord(action); err != nil {
				store.Close()
				t.Fatal(err)
			}
			receipt, err := application.OperationReceiptForOperatorAction(action, run.RepositoryBindingDigest)
			if err != nil {
				store.Close()
				t.Fatal(err)
			}
			insertHistoricalCIWaitActionAndReceipt(t, store, action, receipt)
			// Historical recovery rows predate the Activity projection. Remove the
			// fixture's eagerly written transition projections so the retained
			// backfill path reconstructs the same old-store shape on reopen.
			if _, err := store.db.ExecContext(ctx, `DELETE FROM activity_events WHERE source_kind='run_transition' AND target_id=?`, run.ID); err != nil {
				store.Close()
				t.Fatal(err)
			}
			if state != application.OperatorActionStatusValidated {
				if _, err := store.db.ExecContext(ctx, `UPDATE automatic_retry_schedules SET status='superseded',next_eligible_at='',next_eligible_unix_ns=0,updated_at=? WHERE run_id=? AND phase=?`, formatTime(action.AppliedAt), run.ID, schedule.Phase); err != nil {
					store.Close()
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			inspection, err = reopened.Inspect(ctx, run.ID)
			attention, attentionErr := reopened.ListOperatorAttention(ctx, application.OperatorAttentionQueryInput{RunID: run.ID, Limit: 10})
			if err != nil || attentionErr != nil || len(inspection.OperatorActions) != 1 || inspection.OperatorActions[0].ActionID != action.ActionID || len(attention) != 1 || attention[0].EventType != application.OperatorAttentionCIWaitRecovery || len(inspection.RetrySchedules) != 1 || len(inspection.GitHubRequests) != 1 || inspection.GitHubEvidence == nil || inspection.GitHubInstallation == nil {
				t.Fatalf("inspection action=%+v attention=%+v retry=%+v requests=%+v evidence=%+v installation=%+v err=%v attentionErr=%v", inspection.OperatorActions, attention, inspection.RetrySchedules, inspection.GitHubRequests, inspection.GitHubEvidence, inspection.GitHubInstallation, err, attentionErr)
			}
			persisted, found, err := getOperationReceiptByIDQuery(ctx, reopened.db, receipt.OperationID)
			if err != nil || !found || persisted.OperationType != application.OperationRecoverCIWait || persisted.Phase != receipt.Phase || persisted.Outcome != receipt.Outcome {
				t.Fatalf("receipt=%+v found=%t err=%v", persisted, found, err)
			}
			offers := historicalCIWaitLegalOffers(t, reopened, run, action.Requester)
			if len(offers) != 0 {
				t.Fatalf("historical attention advertised live offers: %+v", offers)
			}
			admission, admissionErr := application.EvaluateRunProgressAdmission(ctx, reopened, run)
			wantAllowed := state == application.OperatorActionStatusObserved
			if admissionErr != nil || admission.Allowed != wantAllowed {
				t.Fatalf("state=%s admission=%+v err=%v", state, admission, admissionErr)
			}
			masked, err := application.AdmissionAuthorityConflictAttentionEvent(run, "admission_authority_conflict", strings.Repeat("9", 64), now.Add(4*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if created, err := reopened.AppendOperatorAttention(ctx, masked); err != nil || !created {
				t.Fatalf("created=%t err=%v", created, err)
			}
			admission, admissionErr = application.EvaluateRunProgressAdmission(ctx, reopened, run)
			if admissionErr != nil || admission.Allowed != wantAllowed {
				t.Fatalf("masked state=%s admission=%+v err=%v", state, admission, admissionErr)
			}
			if state == application.OperatorActionStatusObserved {
				assertHistoricalCIWaitAdmissionFailures(t, reopened, run, action, receipt)
			}

			assertHistoricalCIWaitWriteFences(t, reopened, run, event, action, receipt)
			var actionStatus, receiptPhase, scheduleStatus string
			if err := reopened.db.QueryRow(`SELECT status FROM operator_actions WHERE action_id=?`, action.ActionID).Scan(&actionStatus); err != nil {
				t.Fatal(err)
			}
			if err := reopened.db.QueryRow(`SELECT phase FROM operation_receipts WHERE operation_id=?`, receipt.OperationID).Scan(&receiptPhase); err != nil {
				t.Fatal(err)
			}
			if err := reopened.db.QueryRow(`SELECT status FROM automatic_retry_schedules WHERE run_id=? AND phase=?`, run.ID, schedule.Phase).Scan(&scheduleStatus); err != nil {
				t.Fatal(err)
			}
			if actionStatus != action.Status || receiptPhase != string(receipt.Phase) || scheduleStatus != string(inspection.RetrySchedules[0].Status) {
				t.Fatalf("historical evidence mutated action=%s receipt=%s schedule=%s", actionStatus, receiptPhase, scheduleStatus)
			}

			completeIntegrityActivityBackfill(t, reopened, now.Add(2*time.Hour))
			var operationActivities, attentionActivities int
			if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM activity_operation_links WHERE operation_id=?`, receipt.OperationID).Scan(&operationActivities); err != nil {
				t.Fatal(err)
			}
			if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE source_kind IN ('operator_attention','operator_attention_resolution') AND target_id=?`, run.ID).Scan(&attentionActivities); err != nil {
				t.Fatal(err)
			}
			if state == application.OperatorActionStatusObserved && operationActivities != 1 || state != application.OperatorActionStatusObserved && operationActivities != 0 || attentionActivities < 1 {
				t.Fatalf("state=%s operation activities=%d attention activities=%d", state, operationActivities, attentionActivities)
			}
			publishIntegrityObservation(t, reopened, now.Add(3*time.Hour))
			integrity, requester := integrityQueryService(t, reopened)
			summary, err := integrity.Summary(ctx, requester)
			if err != nil || summary.Readiness != application.IntegrityReady {
				t.Fatalf("integrity=%+v err=%v", summary, err)
			}
		})
	}
}

func historicalCIWaitAttention(run application.Run, schedule application.RetrySchedule, now time.Time) application.OperatorAttentionEvent {
	raw := schedule.RunID + "\x00" + schedule.Phase + "\x00" + "1" + "\x00" + string(schedule.FailureClass) + "\x00" + schedule.ReasonCode
	sum := sha256.Sum256([]byte(raw))
	evidence := hex.EncodeToString(sum[:])
	event := application.OperatorAttentionEvent{SchemaVersion: application.OperatorAttentionPreviousSchemaVersion, EventKey: "automation:" + run.ID + ":ci_wait_recovery:" + evidence, EventType: application.OperatorAttentionCIWaitRecovery, RunID: run.ID, RepositoryProfileID: run.ProfileID, RepositoryProfileName: run.Repository, ControllerState: string(run.State), Severity: "warning", ReasonCode: "legacy_ci_topology_drift", AllowedActions: []application.OperatorAttentionActionID{application.OperatorAttentionActionRecoverCIWait}, EvidenceDigest: evidence, OccurredAt: now, ObservedAt: now}
	event.PayloadDigest = previousOperatorAttentionPayloadDigestFixture(event)
	return event
}

func insertHistoricalCIWaitAttention(t *testing.T, store *admissionTestStore, event application.OperatorAttentionEvent) {
	t.Helper()
	if err := application.ValidatePreviousOperatorAttentionEvent(event); err != nil {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	actions, _ := json.Marshal(event.AllowedActions)
	if _, err := store.db.Exec(`INSERT INTO operator_attention_outbox(event_key,payload_digest,schema_version,event_type,run_id,linear_identifier,repository_profile_id,repository_profile_name,controller_state,severity,reason_code,allowed_actions_json,evidence_digest,occurred_at,observed_at,legacy_payload_digest,legacy_delivery_status,created_at,retry_failure_class) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.EventKey, event.PayloadDigest, event.SchemaVersion, event.EventType, event.RunID, event.LinearIdentifier, event.RepositoryProfileID, event.RepositoryProfileName, event.ControllerState, event.Severity, event.ReasonCode, string(actions), event.EvidenceDigest, formatTime(event.OccurredAt), formatTime(event.ObservedAt), "", "", formatTime(event.ObservedAt), ""); err != nil {
		t.Fatal(err)
	}
}

func historicalCIWaitAction(run application.Run, event application.OperatorAttentionEvent, sequence int64, status string, now time.Time) application.OperatorActionRecord {
	requester := application.Requester{ID: "operator", Kind: "github_login", DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", ActorType: "User"}
	payload := struct {
		RunID, Repository, ExpectedState, RunKey, ActionType, RequesterLogin, RequesterNode, RequesterType, Reason, EventKey string
		TransitionSequence, RequesterDatabaseID                                                                              int64
	}{run.ID, run.Repository, string(run.State), run.IdempotencyKey, string(application.OperatorActionRecoverCIWait), requester.ID, requester.NodeID, requester.ActorType, event.ReasonCode, event.EventKey, sequence, requester.DatabaseID}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	idempotencySum := sha256.Sum256([]byte("operator-action-idempotency:" + digest))
	idempotency := hex.EncodeToString(idempotencySum[:])
	record := application.OperatorActionRecord{ActionID: "operator-action-" + idempotency[:24], IdempotencyKey: idempotency, PayloadDigest: digest, RequestDigest: digest, RunID: run.ID, Repository: run.Repository, ExpectedState: run.State, RunIdempotencyKey: run.IdempotencyKey, TransitionSequence: sequence, ActionType: application.OperatorActionRecoverCIWait, Requester: requester, ReasonCode: event.ReasonCode, AttentionEventKey: event.EventKey, Status: status, ResultStatus: application.OperatorActionResultPending, ReceivedAt: now.Add(time.Minute), ValidatedAt: now.Add(time.Minute)}
	if status != application.OperatorActionStatusValidated {
		record.ResultStatus = application.OperatorActionResultApplied
		record.ResultingState = run.State
		record.ResultingTransitionSequence = sequence
		record.EvidenceDigest = strings.Repeat("2", 64)
		record.AppliedAt = now.Add(2 * time.Minute)
	}
	if status == application.OperatorActionStatusObserved {
		record.ResultStatus = application.OperatorActionResultSucceeded
		record.OutcomeDigest = strings.Repeat("3", 64)
		record.ObservedAt = now.Add(3 * time.Minute)
	}
	return record
}

func insertHistoricalCIWaitActionAndReceipt(t *testing.T, store *admissionTestStore, action application.OperatorActionRecord, receipt application.OperationReceipt) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO operator_actions(action_id,idempotency_key,payload_digest,request_digest,expected_authority_digest,run_id,repository,expected_state,run_idempotency_key,transition_sequence,action_type,requester_login,requester_database_id,requester_node_id,requester_actor_type,reason_code,attention_event_key,status,result_status,resulting_state,resulting_transition_sequence,evidence_digest,outcome_digest,next_eligible_at,received_at,validated_at,applied_at,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, action.ActionID, action.IdempotencyKey, action.PayloadDigest, action.RequestDigest, action.ExpectedAuthorityDigest, action.RunID, action.Repository, string(action.ExpectedState), action.RunIdempotencyKey, action.TransitionSequence, string(action.ActionType), action.Requester.ID, action.Requester.DatabaseID, action.Requester.NodeID, action.Requester.ActorType, action.ReasonCode, action.AttentionEventKey, action.Status, action.ResultStatus, string(action.ResultingState), action.ResultingTransitionSequence, action.EvidenceDigest, action.OutcomeDigest, "", formatTime(action.ReceivedAt), formatTime(action.ValidatedAt), formatTime(action.AppliedAt), formatTime(action.ObservedAt)); err != nil {
		t.Fatal(err)
	}
	insertHistoricalCIWaitReceipt(t, store, action, receipt)
}

func insertHistoricalCIWaitReceipt(t *testing.T, store *admissionTestStore, action application.OperatorActionRecord, receipt application.OperationReceipt) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO operation_receipts(operation_id,authority_key,operation_anchor_digest,operation_type,scope_kind,target_id,requester_login,requester_database_id,requester_node_id,requester_actor_type,request_digest,expected_authority_digest,target_binding_digest,phase,outcome,resulting_authority_digest,resulting_state,resulting_version,evidence_digest,result_digest,accepted_at,applied_at,settled_at,source_action_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, receipt.OperationID, receipt.AuthorityKey, receipt.OperationAnchorDigest, string(receipt.OperationType), string(receipt.Scope), receipt.TargetID, receipt.Requester.Login, receipt.Requester.DatabaseID, receipt.Requester.NodeID, receipt.Requester.ActorType, receipt.RequestDigest, receipt.ExpectedAuthorityDigest, receipt.TargetBindingDigest, string(receipt.Phase), string(receipt.Outcome), receipt.ResultingAuthorityDigest, receipt.ResultingState, receipt.ResultingVersion, receipt.EvidenceDigest, receipt.ResultDigest, formatTime(receipt.AcceptedAt), formatTime(receipt.AppliedAt), formatTime(receipt.SettledAt), action.ActionID); err != nil {
		t.Fatal(err)
	}
}

func assertHistoricalCIWaitAdmissionFailures(t *testing.T, store *Store, run application.Run, action application.OperatorActionRecord, receipt application.OperationReceipt) {
	t.Helper()
	ctx := context.Background()
	blocked := func(label string) {
		t.Helper()
		admission, err := application.EvaluateRunProgressAdmission(ctx, store, run)
		if err != nil || admission.Allowed || admission.Reason != application.RetiredCIWaitRecoveryProgressReason {
			t.Fatalf("%s admission=%+v err=%v", label, admission, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM operation_receipts WHERE operation_id=?`, receipt.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM operator_actions WHERE action_id=?`, action.ActionID); err != nil {
		t.Fatal(err)
	}
	blocked("attention only")
	fixture := &admissionTestStore{Store: store}
	insertHistoricalCIWaitActionAndReceipt(t, fixture, action, receipt)

	if _, err := store.db.ExecContext(ctx, `DELETE FROM operation_receipts WHERE operation_id=?`, receipt.OperationID); err != nil {
		t.Fatal(err)
	}
	blocked("missing receipt")
	insertHistoricalCIWaitReceipt(t, fixture, action, receipt)

	if _, err := store.db.ExecContext(ctx, `UPDATE operation_receipts SET phase=?,outcome=?,settled_at='' WHERE operation_id=?`, application.OperationPhaseApplied, application.OperationOutcomePending, receipt.OperationID); err != nil {
		t.Fatal(err)
	}
	blocked("pending receipt")
	if _, err := store.db.ExecContext(ctx, `UPDATE operation_receipts SET phase=?,outcome=?,settled_at=? WHERE operation_id=?`, receipt.Phase, receipt.Outcome, formatTime(receipt.SettledAt), receipt.OperationID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE operation_receipts SET source_action_id='operator-action-mismatch' WHERE operation_id=?`, receipt.OperationID); err != nil {
		t.Fatal(err)
	}
	blocked("mismatched receipt source")
	if _, err := store.db.ExecContext(ctx, `UPDATE operation_receipts SET source_action_id=? WHERE operation_id=?`, action.ActionID, receipt.OperationID); err != nil {
		t.Fatal(err)
	}
	admission, err := application.EvaluateRunProgressAdmission(ctx, store, run)
	if err != nil || !admission.Allowed {
		t.Fatalf("restored admission=%+v err=%v", admission, err)
	}
}

func historicalCIWaitLegalOffers(t *testing.T, store *Store, run application.Run, requester application.Requester) []application.LegalActionOffer {
	t.Helper()
	identity := domain.GitHubUserIdentity{Login: requester.ID, DatabaseID: requester.DatabaseID, NodeID: requester.NodeID, ActorType: requester.ActorType}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: identity})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewLegalActionService(store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	offers, err := service.ListLegalActionOffers(context.Background(), application.LegalActionOfferQuery{Requester: requester, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	return offers
}

func assertHistoricalCIWaitWriteFences(t *testing.T, store *Store, run application.Run, event application.OperatorAttentionEvent, action application.OperatorActionRecord, receipt application.OperationReceipt) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.AppendOperatorAttention(ctx, event); err == nil {
		t.Fatal("current SQLite store appended retired CI-wait attention")
	}
	if _, _, err := store.BeginOperatorAction(ctx, action); err == nil {
		t.Fatal("current SQLite store began retired CI-wait action")
	}
	if _, _, err := store.beginOperatorActionOnce(ctx, action); err == nil {
		t.Fatal("internal SQLite insertion began retired CI-wait action")
	}
	if _, _, err := store.ApplyOperatorActionResult(ctx, application.OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: application.OperatorActionStatusValidated, ResultStatus: application.OperatorActionResultApplied, ResultingState: run.State, ResultingTransitionSequence: action.TransitionSequence, EvidenceDigest: strings.Repeat("4", 64), At: time.Now().UTC()}); err == nil {
		t.Fatal("current SQLite store applied retired CI-wait action")
	}
	if _, _, err := store.ObserveOperatorActionResult(ctx, application.OperatorActionMutationResult{ActionID: action.ActionID, ExpectedStatus: application.OperatorActionStatusApplied, ResultStatus: application.OperatorActionResultSucceeded, ResultingState: run.State, ResultingTransitionSequence: action.TransitionSequence, EvidenceDigest: strings.Repeat("5", 64), At: time.Now().UTC()}); err == nil {
		t.Fatal("current SQLite store observed retired CI-wait action")
	}
	if _, _, err := store.BeginOperationReceipt(ctx, receipt); err == nil {
		t.Fatal("current SQLite store began retired CI-wait receipt")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOperationReceiptTx(ctx, tx, receipt, action.ActionID); err == nil {
		_ = tx.Rollback()
		t.Fatal("internal SQLite insertion began retired CI-wait receipt")
	}
	_ = tx.Rollback()
	if _, _, err := store.AdvanceOperationReceipt(ctx, application.OperationReceiptMutation{OperationID: receipt.OperationID, ExpectedPhase: application.OperationPhaseAccepted, Phase: application.OperationPhaseAccepted, Outcome: application.OperationOutcomeFailed, ResultDigest: strings.Repeat("6", 64), At: time.Now().UTC()}); err == nil {
		t.Fatal("current SQLite store advanced retired CI-wait receipt")
	}
}
