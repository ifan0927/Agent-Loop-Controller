package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestOnboardingResumeAdvancesDurableAttemptsWithoutActivityConflict(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 1, 0, 0, 0, time.UTC)
	digest := func(value string) string { return strings.Repeat(value, 64) }
	started, receipt := startOnboardingRetryFixture(t, store, "onboarding-attempts", now)
	intent := application.OnboardingStepIntent{OnboardingID: started.OnboardingID, Step: domain.OnboardingStepRootsCreated, IntentDigest: digest("4"), IntendedAt: now.Add(time.Second)}
	if changed, beginErr := store.BeginOnboardingStep(ctx, intent); beginErr != nil || !changed {
		t.Fatalf("begin changed=%t err=%v", changed, beginErr)
	}

	outcomes := []application.OperationOutcome{application.OperationOutcomeFailed, application.OperationOutcomePending, application.OperationOutcomeFailed, application.OperationOutcomeSucceeded}
	for index, outcome := range outcomes {
		attempt := int64(index + 1)
		if index > 0 {
			resumed, changed, resumeErr := store.ResumeOnboarding(ctx, started.OnboardingID, now.Add(time.Duration(index*3)*time.Second))
			if resumeErr != nil || !changed || resumed.Status != domain.OnboardingRunning {
				t.Fatalf("attempt %d resume=%+v changed=%t err=%v", attempt, resumed, changed, resumeErr)
			}
			if replayed, replayChanged, replayErr := store.ResumeOnboarding(ctx, started.OnboardingID, now.Add(time.Duration(index*3+1)*time.Second)); replayErr != nil || replayChanged || replayed.Status != domain.OnboardingRunning {
				t.Fatalf("attempt %d replay=%+v changed=%t err=%v", attempt, replayed, replayChanged, replayErr)
			}
			if changed, beginErr := store.BeginOnboardingStep(ctx, intent); beginErr != nil || changed {
				t.Fatalf("attempt %d begin replay changed=%t err=%v", attempt, changed, beginErr)
			}
		}
		var storedAttempt int64
		if err := store.db.QueryRow(`SELECT attempt_number FROM repository_onboarding_steps WHERE onboarding_id=? AND step_name=?`, started.OnboardingID, string(intent.Step)).Scan(&storedAttempt); err != nil || storedAttempt != attempt {
			t.Fatalf("attempt=%d stored=%d err=%v", attempt, storedAttempt, err)
		}
		observation := application.OnboardingStepObservation{Outcome: outcome, ReasonCode: "fixture_attempt", EvidenceDigest: digest(string(rune('5' + index)))}
		settled, settleErr := store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: started.OnboardingID, Step: intent.Step, Observation: observation, ObservedAt: now.Add(time.Duration(index*3+2) * time.Second)})
		if settleErr != nil {
			t.Fatalf("attempt %d settle: %v", attempt, settleErr)
		}
		if outcome == application.OperationOutcomeSucceeded {
			if settled.Status != domain.OnboardingRunning || !slices.Equal(settled.CompletedSteps, []domain.OnboardingStep{intent.Step}) {
				t.Fatalf("attempt %d settled=%+v", attempt, settled)
			}
		} else if settled.Status != domain.OnboardingWaitingForOperator || len(settled.CompletedSteps) != 0 {
			t.Fatalf("attempt %d settled=%+v", attempt, settled)
		}
		if index == 1 {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	defer store.Close()

	rows, err := store.db.Query(`SELECT source_identity FROM activity_events WHERE source_kind='onboarding' AND target_id=? ORDER BY occurred_at`, started.OnboardingID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var identities []string
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			t.Fatal(err)
		}
		identities = append(identities, identity)
	}
	expected := []string{
		started.OnboardingID + ":roots_created",
		started.OnboardingID + ":roots_created:attempt:2",
		started.OnboardingID + ":roots_created:attempt:3",
		started.OnboardingID + ":roots_created:attempt:4",
	}
	if !slices.Equal(identities, expected) {
		t.Fatalf("activity identities=%v", identities)
	}
	var phase, receiptOutcome string
	if err := store.db.QueryRow(`SELECT phase,outcome FROM operation_receipts WHERE operation_id=?`, receipt.OperationID).Scan(&phase, &receiptOutcome); err != nil || phase != string(application.OperationPhaseAccepted) || receiptOutcome != string(application.OperationOutcomePending) {
		t.Fatalf("receipt phase=%s outcome=%s err=%v", phase, receiptOutcome, err)
	}
}

func TestOnboardingV46MigratesOnlyExactInterruptedResumeAndContinues(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 45)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)
	exactID, exactReceipt, exactEvent := seedV45InterruptedOnboarding(t, legacy, "exact", now, func(*application.ActivityEvent) {})
	seedV45InterruptedOnboarding(t, legacy, "missing-activity", now.Add(time.Hour), func(event *application.ActivityEvent) {
		event.SourceIdentity += ":unrelated"
	})
	seedV45InterruptedOnboarding(t, legacy, "wrong-target", now.Add(2*time.Hour), func(event *application.ActivityEvent) {
		event.TargetID += "-other"
		*event = application.NewActivityEvent(activityInputFromEvent(*event))
	})
	seedV45InterruptedOnboarding(t, legacy, "wrong-binding", now.Add(3*time.Hour), func(event *application.ActivityEvent) {
		event.TargetBindingDigest = application.ConfigurationEvidenceDigest("wrong-binding")
		*event = application.NewActivityEvent(activityInputFromEvent(*event))
	})
	seedV45InterruptedOnboarding(t, legacy, "altered-related", now.Add(4*time.Hour), func(event *application.ActivityEvent) {
		event.RelatedResources[0].ID += "-other"
		*event = application.NewActivityEvent(activityInputFromEvent(*event))
	})
	seedV45InterruptedOnboarding(t, legacy, "wrong-order", now.Add(5*time.Hour), func(event *application.ActivityEvent) {
		if _, err := legacy.db.Exec(`UPDATE repository_onboarding_steps SET step_order=3 WHERE onboarding_id=? AND step_name='linear_label_observed'`, event.TargetID); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.db.Exec(`UPDATE repository_onboardings SET step_index=2 WHERE onboarding_id=?`, event.TargetID); err != nil {
			t.Fatal(err)
		}
	})
	seedV45InterruptedOnboarding(t, legacy, "wrong-prefix", now.Add(6*time.Hour), func(event *application.ActivityEvent) {
		if _, err := legacy.db.Exec(`UPDATE repository_onboarding_steps SET step_name='managed_source_created' WHERE onboarding_id=? AND step_name='roots_created'`, event.TargetID); err != nil {
			t.Fatal(err)
		}
	})
	seedV45InterruptedOnboarding(t, legacy, "wrong-prior-intent", now.Add(7*time.Hour), func(event *application.ActivityEvent) {
		if _, err := legacy.db.Exec(`UPDATE repository_onboarding_steps SET intent_digest=? WHERE onboarding_id=? AND step_name='roots_created'`, application.ConfigurationEvidenceDigest("wrong-prior-intent"), event.TargetID); err != nil {
			t.Fatal(err)
		}
	})
	wrongReceiptID, wrongReceipt, _ := seedV45InterruptedOnboarding(t, legacy, "wrong-receipt", now.Add(8*time.Hour), func(*application.ActivityEvent) {})
	if _, err := legacy.db.Exec(`UPDATE operation_receipts SET target_binding_digest=? WHERE operation_id=?`, application.ConfigurationEvidenceDigest("wrong-receipt-binding"), wrongReceipt.OperationID); err != nil {
		t.Fatal(err)
	}
	seedV45InterruptedOnboarding(t, legacy, "successful", now.Add(9*time.Hour), func(event *application.ActivityEvent) {
		event.ResultingState = string(domain.OnboardingRunning)
		*event = application.NewActivityEvent(activityInputFromEvent(*event))
	})
	seedV45InterruptedOnboarding(t, legacy, "terminal", now.Add(10*time.Hour), func(event *application.ActivityEvent) {
		if _, err := legacy.db.Exec(`UPDATE repository_onboardings SET status='conflict' WHERE onboarding_id=?`, event.TargetID); err != nil {
			t.Fatal(err)
		}
	})
	corruptID, _, corruptEvent := seedV45InterruptedOnboarding(t, legacy, "corrupt-snapshot", now.Add(11*time.Hour), func(*application.ActivityEvent) {})
	if _, err := legacy.db.Exec(`UPDATE activity_events SET snapshot_digest=? WHERE event_id=?`, application.ConfigurationEvidenceDigest("corrupt-snapshot"), corruptEvent.EventID); err != nil {
		t.Fatal(err)
	}
	before, err := scanActivityEvent(legacy.db.QueryRow(activityEventSelect+` WHERE event_id=?`, exactEvent.EventID))
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
	for id, expectedAttempt := range map[string]int64{exactID: 2, "onboarding-v45-missing-activity": 1, "onboarding-v45-wrong-target": 1, "onboarding-v45-wrong-binding": 1, "onboarding-v45-altered-related": 1, "onboarding-v45-wrong-order": 1, "onboarding-v45-wrong-prefix": 1, "onboarding-v45-wrong-prior-intent": 1, wrongReceiptID: 1, "onboarding-v45-successful": 1, "onboarding-v45-terminal": 1, corruptID: 1} {
		var attempt int64
		if err := store.db.QueryRow(`SELECT attempt_number FROM repository_onboarding_steps WHERE onboarding_id=? AND step_name='linear_label_observed'`, id).Scan(&attempt); err != nil || attempt != expectedAttempt {
			t.Fatalf("onboarding=%s attempt=%d want=%d err=%v", id, attempt, expectedAttempt, err)
		}
	}
	after, err := scanActivityEvent(store.db.QueryRow(activityEventSelect+` WHERE event_id=?`, exactEvent.EventID))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("prior activity changed: before=%+v after=%+v err=%v", before, after, err)
	}
	var repositoryRevisionBefore, activityRevisionBefore int64
	if err := store.db.QueryRow(`SELECT
		(SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family='repository_onboarding' AND scope_kind='controller' AND scope_id='local-controller'),
		(SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family='operation_activity' AND scope_kind='controller' AND scope_id='local-controller')`).Scan(&repositoryRevisionBefore, &activityRevisionBefore); err != nil {
		t.Fatal(err)
	}
	settled, err := store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: exactID, Step: domain.OnboardingStepLinearLabelObserved, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "linear_label_ready", EvidenceDigest: strings.Repeat("9", 64), LinearLabelID: "label-recovered"}, ObservedAt: now.Add(10 * time.Minute)})
	if err != nil || settled.Status != domain.OnboardingRunning || !slices.Equal(settled.CompletedSteps, []domain.OnboardingStep{domain.OnboardingStepRootsCreated, domain.OnboardingStepLinearLabelObserved}) {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	var eventCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE source_kind='onboarding' AND target_id=?`, exactID).Scan(&eventCount); err != nil || eventCount != 2 {
		t.Fatalf("event count=%d err=%v", eventCount, err)
	}
	var repositoryRevisionAfter, activityRevisionAfter int64
	if err := store.db.QueryRow(`SELECT
		(SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family='repository_onboarding' AND scope_kind='controller' AND scope_id='local-controller'),
		(SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family='operation_activity' AND scope_kind='controller' AND scope_id='local-controller')`).Scan(&repositoryRevisionAfter, &activityRevisionAfter); err != nil || repositoryRevisionAfter <= repositoryRevisionBefore || activityRevisionAfter <= activityRevisionBefore {
		t.Fatalf("repository revision %d->%d activity revision %d->%d err=%v", repositoryRevisionBefore, repositoryRevisionAfter, activityRevisionBefore, activityRevisionAfter, err)
	}
	var phase, outcome string
	if err := store.db.QueryRow(`SELECT phase,outcome FROM operation_receipts WHERE operation_id=?`, exactReceipt.OperationID).Scan(&phase, &outcome); err != nil || phase != string(application.OperationPhaseAccepted) || outcome != string(application.OperationOutcomePending) {
		t.Fatalf("receipt phase=%s outcome=%s err=%v", phase, outcome, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	value, found, err := store.Onboarding(ctx, exactID)
	if err != nil || !found || len(value.CompletedSteps) != 2 || value.LinearLabelID != "label-recovered" {
		t.Fatalf("reopened=%+v found=%t err=%v", value, found, err)
	}
	var onboardings, roots, minimumAttempt int
	if err := store.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM repository_onboardings WHERE onboarding_id=?),
		(SELECT COUNT(*) FROM repository_onboarding_steps WHERE onboarding_id=? AND step_name='roots_created'),
		(SELECT MIN(attempt_number) FROM repository_onboarding_steps)`, exactID, exactID).Scan(&onboardings, &roots, &minimumAttempt); err != nil || onboardings != 1 || roots != 1 || minimumAttempt < 1 {
		t.Fatalf("onboardings=%d roots=%d minimum_attempt=%d err=%v", onboardings, roots, minimumAttempt, err)
	}
}

func startOnboardingRetryFixture(t *testing.T, store *Store, id string, now time.Time) (application.Onboarding, application.OperationReceipt) {
	t.Helper()
	ctx := context.Background()
	digest := func(value string) string { return strings.Repeat(value, 64) }
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	opened, _, err := store.OpenOnboarding(ctx, application.OnboardingOpenInput{OnboardingID: id, Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/" + id, Requester: requester, PrivateInputDigest: digest("a"), SourcePathDigest: digest("b"), SourceAncestorDigests: []string{digest("b")}, RequestDigest: digest("c"), ConfigurationBaseGenerationID: 1, ConfigurationBaseDigest: digest("d"), ConfigurationAuthorityVersion: 1, OpenedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: id, ExpectedStatus: opened.Status, PreflightDigest: digest("e"), EvidenceDigest: digest("f"), ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: id, Requester: requester, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: opened.ConfigurationBaseDigest, OperationAnchorDigest: digest("1"), TargetBindingDigest: digest("2"), AcceptedAt: now})
	started, _, _, err := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: id, Expected: ready, PreflightDigest: ready.PreflightDigest, PreviewDigest: digest("3"), Profile: application.LocalRepository{CanonicalRepository: opened.CanonicalRepository}, Receipt: receipt, AcceptedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return started, receipt
}

func seedV45InterruptedOnboarding(t *testing.T, store *Store, suffix string, now time.Time, mutate func(*application.ActivityEvent)) (string, application.OperationReceipt, application.ActivityEvent) {
	t.Helper()
	id := "onboarding-v45-" + suffix
	digest := func(value string) string {
		return application.ConfigurationEvidenceDigest("v45-onboarding", suffix, value)
	}
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	preflight, preview := digest("preflight"), digest("preview")
	startAnchor := digestBytes([]byte("onboarding-start-v1\x00" + id + "\x00" + preflight + "\x00" + preview))
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: id, Requester: requester, RequestDigest: digest("request"), ExpectedAuthorityDigest: digest("authority"), OperationAnchorDigest: startAnchor, TargetBindingDigest: onboardingV46IdentityDigest(requester), AcceptedAt: now})
	if _, _, err := store.BeginOperationReceipt(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	binding := digest("repository-binding")
	labelIntent := digestBytes([]byte("onboarding-step-intent-v1\x00" + id + "\x00" + string(domain.OnboardingStepLinearLabelObserved) + "\x00" + receipt.RequestDigest))
	_, err := store.db.Exec(`INSERT INTO repository_onboardings(onboarding_id,onboarding_kind,canonical_repository,private_input_digest,source_path_digest,request_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,base_generation_id,base_digest,configuration_authority_version,status,step_index,reason_code,preflight_digest,preview_digest,operation_id,repository_binding_digest,accepted_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'running',1,'',?,?,?,?,?,?,?)`, id, "existing_checkout", "owner/"+suffix, digest("private"), digest("source"), receipt.RequestDigest, requester.Login, requester.DatabaseID, requester.NodeID, requester.ActorType, 1, receipt.ExpectedAuthorityDigest, 1, preflight, preview, receipt.OperationID, binding, formatTime(now), formatTime(now), formatTime(now.Add(3*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO repository_onboarding_steps(onboarding_id,step_name,step_order,intent_digest,status,outcome,reason_code,evidence_digest,intended_at,observed_at) VALUES
		(?,'roots_created',1,?,'observed','succeeded','roots_ready',?,?,?),
		(?,'linear_label_observed',2,?,'intended','pending','','',?,'')`, id, digestBytes([]byte("onboarding-step-intent-v1\x00"+id+"\x00"+string(domain.OnboardingStepRootsCreated)+"\x00"+receipt.RequestDigest)), digest("roots-evidence"), formatTime(now.Add(time.Minute)), formatTime(now.Add(90*time.Second)), id, labelIntent, formatTime(now.Add(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	event, valid := newOnboardingActivityEvent(id, domain.OnboardingStepLinearLabelObserved, 2, 1, application.OperationOutcomeFailed, "linear_label_outcome_unknown", application.ConfigurationEvidenceDigest("onboarding-linear-unknown-v1", id), now.Add(2*time.Minute), binding, receipt.OperationID, application.ActivityIngestionCurrent)
	if !valid {
		t.Fatal("v45 event fixture is invalid")
	}
	mutate(&event)
	if event.SourceIdentity == id+":linear_label_observed:unrelated" {
		return id, receipt, event
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := appendActivityEventTx(context.Background(), tx, event)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("append event=%+v err=%v", stored, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit event=%+v err=%v", stored, err)
	}
	return id, receipt, stored
}

func activityInputFromEvent(event application.ActivityEvent) application.ActivityEventInput {
	return application.ActivityEventInput{SourceKind: event.SourceKind, SourceIdentity: event.SourceIdentity, SourceEvidenceDigest: event.SourceEvidenceDigest, Category: event.Category, EventKind: event.EventKind, Actor: event.Actor, Scope: event.Scope, TargetID: event.TargetID, TargetBindingDigest: event.TargetBindingDigest, ReasonCode: event.ReasonCode, PriorState: event.PriorState, ResultingState: event.ResultingState, PriorVersion: event.PriorVersion, ResultingVersion: event.ResultingVersion, OccurredAt: event.OccurredAt, ObservedAt: event.ObservedAt, SettledAt: event.SettledAt, RelatedResources: event.RelatedResources, OperationIDs: event.OperationIDs, EvidenceDigests: event.EvidenceDigests, Coverage: event.Coverage}
}

func TestOnboardingSagaReplaysAndResumesAfterStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC)
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	digest := func(character string) string { return strings.Repeat(character, 64) }
	baseline := application.ConfigurationBaselineInput{Candidate: application.ValidatedConfigurationCandidate{Digest: digest("d"), Size: 1, SchemaVersion: 5, DatabasePath: path, Operator: requester}, CanonicalConfigPath: path + ".json", ObservedAt: now.Add(-time.Second)}
	if err := store.PrepareConfigurationBaseline(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	configuration, _, err := store.AdoptConfigurationBaseline(ctx, baseline)
	if err != nil {
		t.Fatal(err)
	}
	configuration, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: configuration.Desired.GenerationID, ExpectedDigest: configuration.Desired.Digest, WorkerInstanceID: "onboarding-fixture-worker", BuildIdentity: "onboarding-fixture-build", ObservedAt: now, EvidenceDigest: digest("3")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	openInput := application.OnboardingOpenInput{OnboardingID: "onboarding-restart-safe", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/repository", Requester: requester, PrivateInputDigest: digest("a"), SourcePathDigest: digest("b"), SourceAncestorDigests: []string{digest("0"), digest("b")}, RequestDigest: digest("c"), ConfigurationBaseGenerationID: configuration.Desired.GenerationID, ConfigurationBaseDigest: configuration.Desired.Digest, ConfigurationAuthorityVersion: configuration.Version, OpenedAt: now}
	opened, created, err := store.OpenOnboarding(ctx, openInput)
	if err != nil || !created || opened.Status != domain.OnboardingOpened {
		t.Fatalf("opened=%+v created=%t err=%v", opened, created, err)
	}
	if replayed, changed, replayErr := store.OpenOnboarding(ctx, openInput); replayErr != nil || changed || replayed.OnboardingID != opened.OnboardingID {
		t.Fatalf("open replay=%+v changed=%t err=%v", replayed, changed, replayErr)
	}
	runtimeVersionReplay := openInput
	runtimeVersionReplay.ConfigurationAuthorityVersion++
	if replayed, changed, replayErr := store.OpenOnboarding(ctx, runtimeVersionReplay); replayErr != nil || changed || replayed.ConfigurationAuthorityVersion != openInput.ConfigurationAuthorityVersion {
		t.Fatalf("runtime-version replay=%+v changed=%t err=%v", replayed, changed, replayErr)
	}
	changedClaims := openInput
	changedClaims.SourceAncestorDigests = []string{digest("b")}
	if _, _, conflictErr := store.OpenOnboarding(ctx, changedClaims); !errors.Is(conflictErr, application.ErrOnboardingConflict) {
		t.Fatalf("changed path claims conflict=%v", conflictErr)
	}
	conflict := openInput
	conflict.OnboardingID = "onboarding-conflicting-source"
	conflict.RequestDigest = digest("e")
	if _, _, conflictErr := store.OpenOnboarding(ctx, conflict); !errors.Is(conflictErr, application.ErrOnboardingConflict) {
		t.Fatalf("duplicate source conflict=%v", conflictErr)
	}
	ancestorConflict := openInput
	ancestorConflict.OnboardingID = "onboarding-ancestor-conflict"
	ancestorConflict.CanonicalRepository = "owner/other"
	ancestorConflict.SourcePathDigest = digest("0")
	ancestorConflict.SourceAncestorDigests = []string{digest("0")}
	ancestorConflict.RequestDigest = digest("7")
	if _, _, conflictErr := store.OpenOnboarding(ctx, ancestorConflict); !errors.Is(conflictErr, application.ErrOnboardingConflict) {
		t.Fatalf("ancestor source conflict=%v", conflictErr)
	}
	preflight := digest("f")
	ready, err := store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: opened.OnboardingID, ExpectedStatus: domain.OnboardingOpened, PreflightDigest: preflight, EvidenceDigest: digest("1"), ObservedAt: now.Add(time.Second)})
	if err != nil || ready.Status != domain.OnboardingPreflightReady {
		t.Fatalf("preflight=%+v err=%v", ready, err)
	}
	profile := application.LocalRepository{CanonicalRepository: opened.CanonicalRepository}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: opened.OnboardingID, Requester: requester, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: opened.ConfigurationBaseDigest, OperationAnchorDigest: digest("2"), TargetBindingDigest: digest("3"), AcceptedAt: now.Add(2 * time.Second)})
	preview := digest("4")
	started, acceptedReceipt, changed, err := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: opened.OnboardingID, Expected: ready, PreflightDigest: preflight, PreviewDigest: preview, Profile: profile, Receipt: receipt, AcceptedAt: receipt.AcceptedAt})
	if err != nil || !changed || started.Status != domain.OnboardingAccepted || acceptedReceipt.OperationID != receipt.OperationID {
		t.Fatalf("started=%+v receipt=%+v changed=%t err=%v", started, acceptedReceipt, changed, err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO configuration_drafts DEFAULT VALUES`); err == nil || !strings.Contains(err.Error(), "configuration mutation is already active") {
		t.Fatalf("parallel configuration draft error=%v", err)
	}
	if replayed, replayReceipt, replayChanged, replayErr := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: opened.OnboardingID, Expected: ready, PreflightDigest: preflight, PreviewDigest: preview, Profile: profile, Receipt: receipt, AcceptedAt: receipt.AcceptedAt}); replayErr != nil || replayChanged || replayed.OperationID != receipt.OperationID || replayReceipt.OperationID != receipt.OperationID {
		t.Fatalf("start replay=%+v receipt=%+v changed=%t err=%v", replayed, replayReceipt, replayChanged, replayErr)
	}
	intent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepRootsCreated, IntentDigest: digest("5"), IntendedAt: now.Add(3 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, intent); err != nil || !changed {
		t.Fatalf("begin changed=%t err=%v", changed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runnable, err := store.ListRunnableOnboardings(ctx, 10)
	if err != nil || len(runnable) != 1 || runnable[0] != opened.OnboardingID {
		t.Fatalf("runnable=%v err=%v", runnable, err)
	}
	settled, err := store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepRootsCreated, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "roots_ready", EvidenceDigest: digest("6")}, ObservedAt: now.Add(4 * time.Second)})
	if err != nil || settled.Status != domain.OnboardingRunning || len(settled.CompletedSteps) != 1 || settled.CompletedSteps[0] != domain.OnboardingStepRootsCreated {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	labelIntent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepLinearLabelObserved, IntentDigest: digest("8"), IntendedAt: now.Add(5 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, labelIntent); err != nil || !changed {
		t.Fatalf("label begin changed=%t err=%v", changed, err)
	}
	settled, err = store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepLinearLabelObserved, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "linear_label_ready", EvidenceDigest: digest("9"), LinearLabelID: "label-immutable-1"}, ObservedAt: now.Add(6 * time.Second)})
	if err != nil || settled.LinearLabelID != "label-immutable-1" || len(settled.CompletedSteps) != 2 {
		t.Fatalf("label settled=%+v err=%v", settled, err)
	}
	profileAuthority := repositoryProfileFixture(opened.CanonicalRepository, "e", "f")
	configurationIntent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepConfigurationApplied, IntentDigest: digest("a"), IntendedAt: now.Add(7 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, configurationIntent); err != nil || !changed {
		t.Fatalf("configuration begin changed=%t err=%v", changed, err)
	}
	candidate := application.ValidatedConfigurationCandidate{Digest: digest("4"), Size: 2, SchemaVersion: 5, DatabasePath: path, Operator: requester}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: requester})
	if err != nil {
		t.Fatal(err)
	}
	configured, err := authorizer.ResolveConfiguredRequester(application.Requester{ID: requester.Login, Kind: "github_login", DatabaseID: requester.DatabaseID, NodeID: requester.NodeID, ActorType: requester.ActorType})
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := authorizer.ControllerScopes(configured)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := scopes.ControllerOperationTarget()
	if !ok {
		t.Fatal("controller operation target is unavailable")
	}
	configurationAnchor := application.ConfigurationEvidenceDigest("configuration-apply-v3", fmt.Sprint(configuration.Desired.GenerationID), configuration.Desired.Digest, candidate.Digest, string(application.ConfigurationApplyOnboarding), opened.OnboardingID, opened.RequestDigest)
	configurationReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: target.TargetID, Requester: requester, RequestDigest: candidate.Digest, ExpectedAuthorityDigest: configuration.Desired.Digest, OperationAnchorDigest: configurationAnchor, TargetBindingDigest: target.TargetBindingDigest, AcceptedAt: now.Add(8 * time.Second)})
	apply := application.ConfigurationApplyAcceptance{ExpectedGenerationID: configuration.Desired.GenerationID, ExpectedDigest: configuration.Desired.Digest, Candidate: candidate, Requester: requester, Receipt: configurationReceipt, Provenance: application.ConfigurationApplyProvenance{Kind: application.ConfigurationApplyOnboarding, OnboardingSourceID: opened.OnboardingID, OnboardingSourceDigest: opened.RequestDigest}, AcceptedAt: configurationReceipt.AcceptedAt}
	normalReceipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationApplyConfiguration, Scope: application.ScopeController, TargetID: application.ConfigurationTargetID, Requester: requester, RequestDigest: candidate.Digest, ExpectedAuthorityDigest: configuration.Desired.Digest, OperationAnchorDigest: digest("0"), TargetBindingDigest: digest("6"), AcceptedAt: now.Add(8 * time.Second)})
	normalApply := apply
	normalApply.Receipt, normalApply.Provenance = normalReceipt, application.ConfigurationApplyProvenance{Kind: application.ConfigurationApplyNormal}
	if _, _, _, err := store.BeginConfigurationApply(ctx, normalApply); !errors.Is(err, application.ErrConfigurationApplyInProgress) {
		t.Fatalf("parallel normal configuration apply error=%v", err)
	}
	generation, _, changed, err := store.BeginConfigurationApply(ctx, apply)
	if err != nil || !changed {
		t.Fatalf("configuration apply generation=%+v changed=%t err=%v", generation, changed, err)
	}
	if replayed, _, replayChanged, replayErr := store.BeginConfigurationApply(ctx, apply); replayErr != nil || replayChanged || replayed.GenerationID != generation.GenerationID {
		t.Fatalf("configuration replay=%+v changed=%t err=%v", replayed, replayChanged, replayErr)
	}
	var settledConfigurationReceipt application.OperationReceipt
	configuration, settledConfigurationReceipt, _, err = store.SettleConfigurationApply(ctx, application.ConfigurationApplySettlement{GenerationID: generation.GenerationID, ParentID: generation.ParentID, OperationID: generation.OperationID, Outcome: application.ConfigurationApplyCommitted, Reason: application.ConfigurationReasonRestartRequired, EvidenceDigest: digest("7"), SettledAt: now.Add(9 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	configuration, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: generation.GenerationID, ExpectedDigest: generation.Digest, WorkerInstanceID: "replacement-worker", BuildIdentity: "replacement-build", ObservedAt: now.Add(10 * time.Second), EvidenceDigest: digest("8")})
	if err != nil {
		t.Fatal(err)
	}
	configurationEvidence := application.ConfigurationEvidenceDigest("onboarding-configuration-v1", opened.OnboardingID, generation.Digest, fmt.Sprint(generation.GenerationID), settledConfigurationReceipt.EvidenceDigest)
	settled, err = store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepConfigurationApplied, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "configuration_applied", EvidenceDigest: configurationEvidence, ProfileID: profileAuthority.Profile.ProfileID, ProfileDigest: profileAuthority.Profile.ProfileDigest, RepositoryBindingDigest: profileAuthority.Profile.RepositoryBindingDigest, ConfigurationGenerationID: generation.GenerationID}, ObservedAt: now.Add(11 * time.Second)})
	if err != nil || settled.ConfigurationGenerationID != generation.GenerationID {
		t.Fatalf("configuration settled=%+v err=%v", settled, err)
	}
	profiles := []application.RepositoryProfileAuthority{profileAuthority}
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: profiles, AdoptedAt: now.Add(11 * time.Second)}); err != nil {
		t.Fatalf("running post-configuration bridge failed: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET status='waiting_for_operator',reason_code='worker_restart_required' WHERE onboarding_id=?`, opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: profiles, AdoptedAt: now.Add(11 * time.Second)}); err != nil {
		t.Fatalf("waiting post-configuration bridge failed: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET status='running',reason_code='' WHERE onboarding_id=?`, opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict := func(label string) {
		t.Helper()
		if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: profiles, AdoptedAt: now.Add(11 * time.Second)}); !errors.Is(err, application.ErrRepositoryLifecycleConflict) {
			t.Fatalf("%s bridge error=%v", label, err)
		}
	}
	for _, status := range []string{"accepted", "conflict", "ready_disabled", "cancelled"} {
		if _, err := store.db.Exec(`UPDATE repository_onboardings SET status=? WHERE onboarding_id=?`, status, opened.OnboardingID); err != nil {
			t.Fatal(err)
		}
		expectBridgeConflict(status)
		if _, err := store.db.Exec(`UPDATE repository_onboardings SET status='running' WHERE onboarding_id=?`, opened.OnboardingID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET step_index=? WHERE onboarding_id=?`, onboardingStepOrder(opened.Kind, domain.OnboardingStepLifecycleCreated), opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("later lifecycle step index")
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET step_index=? WHERE onboarding_id=?`, onboardingStepOrder(opened.Kind, domain.OnboardingStepConfigurationApplied), opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE repository_onboarding_steps SET reason_code='wrong_reason' WHERE onboarding_id=? AND step_name='configuration_applied'`, opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("wrong step reason")
	if _, err := store.db.Exec(`UPDATE repository_onboarding_steps SET reason_code='configuration_applied',evidence_digest=? WHERE onboarding_id=? AND step_name='configuration_applied'`, digest("0"), opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("wrong step evidence")
	if _, err := store.db.Exec(`UPDATE repository_onboarding_steps SET evidence_digest=? WHERE onboarding_id=? AND step_name='configuration_applied'`, configurationEvidence, opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET profile_digest=? WHERE onboarding_id=?`, digest("0"), opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("wrong profile digest")
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET profile_digest=? WHERE onboarding_id=?`, profileAuthority.Profile.ProfileDigest, opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		label, column, value, restore string
	}{
		{"wrong profile id", "profile_id", "profile-wrong", profileAuthority.Profile.ProfileID},
		{"wrong binding", "repository_binding_digest", digest("0"), profileAuthority.Profile.RepositoryBindingDigest},
		{"zero generation", "configuration_generation_id", "0", fmt.Sprint(generation.GenerationID)},
	} {
		if _, err := store.db.Exec(`UPDATE repository_onboardings SET `+mutation.column+`=? WHERE onboarding_id=?`, mutation.value, opened.OnboardingID); err != nil {
			t.Fatal(err)
		}
		expectBridgeConflict(mutation.label)
		if _, err := store.db.Exec(`UPDATE repository_onboardings SET `+mutation.column+`=? WHERE onboarding_id=?`, mutation.restore, opened.OnboardingID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE repository_onboarding_steps SET status='intended',outcome='pending',reason_code='',evidence_digest='',observed_at='' WHERE onboarding_id=? AND step_name='configuration_applied'`, opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("intended pending step")
	for _, outcome := range []string{"failed", "ambiguous"} {
		if _, err := store.db.Exec(`UPDATE repository_onboarding_steps SET status='observed',outcome=?,reason_code='configuration_applied',evidence_digest=?,observed_at=? WHERE onboarding_id=? AND step_name='configuration_applied'`, outcome, configurationEvidence, formatTime(now.Add(11*time.Second)), opened.OnboardingID); err != nil {
			t.Fatal(err)
		}
		expectBridgeConflict(outcome + " step")
	}
	if _, err := store.db.Exec(`UPDATE repository_onboarding_steps SET status='observed',outcome='succeeded',reason_code='configuration_applied',evidence_digest=?,observed_at=? WHERE onboarding_id=? AND step_name='configuration_applied'`, configurationEvidence, formatTime(now.Add(11*time.Second)), opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET base_digest=? WHERE onboarding_id=?`, digest("0"), opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("wrong onboarding base")
	if _, err := store.db.Exec(`UPDATE repository_onboardings SET base_digest=? WHERE onboarding_id=?`, opened.ConfigurationBaseDigest, opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE configuration_apply_intents SET parent_digest=? WHERE generation_id=?`, digest("0"), generation.GenerationID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("wrong apply intent")
	if _, err := store.db.Exec(`UPDATE configuration_apply_intents SET parent_digest=? WHERE generation_id=?`, baseline.Candidate.Digest, generation.GenerationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE operation_receipts SET outcome='failed' WHERE operation_id=?`, generation.OperationID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("wrong configuration receipt")
	if _, err := store.db.Exec(`UPDATE operation_receipts SET outcome='succeeded' WHERE operation_id=?`, generation.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX repository_onboarding_one_active_repository`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO repository_onboardings(onboarding_id,onboarding_kind,canonical_repository,private_input_digest,source_path_digest,request_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,base_generation_id,base_digest,configuration_authority_version,status,step_index,reason_code,preflight_digest,preflight_evidence_digest,preview_digest,operation_id,profile_id,profile_digest,repository_binding_digest,configuration_generation_id,incarnation_id,readiness_snapshot_id,linear_label_id,initial_revision_sha,accepted_at,created_at,updated_at,settled_at)
		SELECT 'onboarding-duplicate-bridge',onboarding_kind,canonical_repository,private_input_digest,?, ?,requester_login,requester_database_id,requester_node_id,requester_actor_type,base_generation_id,base_digest,configuration_authority_version,status,step_index,reason_code,preflight_digest,preflight_evidence_digest,preview_digest,NULL,profile_id,profile_digest,repository_binding_digest,configuration_generation_id,incarnation_id,readiness_snapshot_id,linear_label_id,initial_revision_sha,accepted_at,created_at,updated_at,settled_at FROM repository_onboardings WHERE onboarding_id=?`, digest("8"), digest("9"), opened.OnboardingID); err != nil {
		t.Fatal(err)
	}
	expectBridgeConflict("duplicate candidate")
	if _, err := store.db.Exec(`DELETE FROM repository_onboardings WHERE onboarding_id='onboarding-duplicate-bridge'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE UNIQUE INDEX repository_onboarding_one_active_repository ON repository_onboardings(canonical_repository) WHERE status NOT IN ('cancelled','conflict','ready_disabled')`); err != nil {
		t.Fatal(err)
	}
	convergenceIntent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepConfigurationConverged, IntentDigest: digest("c"), IntendedAt: now.Add(12 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, convergenceIntent); err != nil || !changed {
		t.Fatalf("convergence begin changed=%t err=%v", changed, err)
	}
	settled, err = store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepConfigurationConverged, Observation: application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "configuration_converged", EvidenceDigest: digest("d")}, ObservedAt: now.Add(13 * time.Second)})
	if err != nil || len(settled.CompletedSteps) != 4 {
		t.Fatalf("convergence settled=%+v err=%v", settled, err)
	}
	lifecycleIntent := application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepLifecycleCreated, IntentDigest: digest("e"), IntendedAt: now.Add(14 * time.Second)}
	if changed, err := store.BeginOnboardingStep(ctx, lifecycleIntent); err != nil || !changed {
		t.Fatalf("lifecycle begin changed=%t err=%v", changed, err)
	}
	projection, created, err := store.CreateOnboardingRepositoryLifecycle(ctx, opened.OnboardingID, profileAuthority.Profile, now.Add(14*time.Second))
	if err != nil || !created || projection.Lifecycle.Intent != application.RepositoryDisabled || projection.Readiness.Status != domain.RepositoryUnknown || projection.Readiness.ReasonCode != "initial_recheck_required" {
		t.Fatalf("projection=%+v created=%t err=%v", projection, created, err)
	}
	if replayed, changed, replayErr := store.CreateOnboardingRepositoryLifecycle(ctx, opened.OnboardingID, profileAuthority.Profile, now.Add(14*time.Second)); replayErr != nil || changed || replayed.Lifecycle.IncarnationID != projection.Lifecycle.IncarnationID {
		t.Fatalf("lifecycle replay=%+v changed=%t err=%v", replayed, changed, replayErr)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: profiles, AdoptedAt: now.Add(15 * time.Second)}); err != nil {
		t.Fatalf("post-lifecycle replay failed: %v", err)
	}
}

func TestOnboardingV37MigrationPreservesV36AndAddsReceiptScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 36)
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
	var tables int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('repository_onboardings','repository_onboarding_path_claims','repository_onboarding_steps')`).Scan(&tables); err != nil || tables != 3 {
		t.Fatalf("tables=%d err=%v", tables, err)
	}
	now := time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC)
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: "onboarding-migrated", Requester: requester, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("c", 64), TargetBindingDigest: strings.Repeat("d", 64), AcceptedAt: now})
	if _, created, err := store.BeginOperationReceipt(context.Background(), receipt); err != nil || !created {
		t.Fatalf("onboarding receipt created=%t err=%v", created, err)
	}
}

func TestEmptyRepositoryOnboardingPersistsKindSpecificOrderAndInitialSHA(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 25, 5, 0, 0, 123456789, time.UTC)
	digest := func(character string) string { return strings.Repeat(character, 64) }
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	input := application.OnboardingOpenInput{OnboardingID: "onboarding-empty-order", Kind: domain.OnboardingEmptyRepository, CanonicalRepository: "owner/empty", Requester: requester, PrivateInputDigest: digest("a"), SourcePathDigest: digest("b"), SourceAncestorDigests: []string{digest("0"), digest("b")}, RequestDigest: digest("c"), ConfigurationBaseGenerationID: 1, ConfigurationBaseDigest: digest("d"), ConfigurationAuthorityVersion: 1, OpenedAt: now}
	opened, _, err := store.OpenOnboarding(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.SaveOnboardingPreflight(ctx, application.OnboardingPreflightInput{OnboardingID: opened.OnboardingID, ExpectedStatus: domain.OnboardingOpened, PreflightDigest: digest("e"), EvidenceDigest: digest("f"), ObservedAt: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: opened.OnboardingID, Requester: requester, RequestDigest: opened.RequestDigest, ExpectedAuthorityDigest: opened.ConfigurationBaseDigest, OperationAnchorDigest: digest("1"), TargetBindingDigest: digest("2"), AcceptedAt: now.Add(2 * time.Second)})
	started, _, _, err := store.StartOnboarding(ctx, application.OnboardingStartAcceptance{OnboardingID: opened.OnboardingID, Expected: ready, PreflightDigest: ready.PreflightDigest, PreviewDigest: digest("3"), Profile: application.LocalRepository{CanonicalRepository: opened.CanonicalRepository}, Receipt: receipt, AcceptedAt: receipt.AcceptedAt})
	if err != nil || !started.AcceptedAt.Equal(receipt.AcceptedAt.UTC().Truncate(time.Second)) {
		t.Fatalf("started=%+v err=%v", started, err)
	}
	if _, err := store.BeginOnboardingStep(ctx, application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: domain.OnboardingStepLinearLabelObserved, IntentDigest: digest("4"), IntendedAt: now.Add(3 * time.Second)}); !errors.Is(err, application.ErrOnboardingConflict) {
		t.Fatalf("out-of-order step error=%v", err)
	}
	sha := strings.Repeat("1", 40)
	for index, step := range domain.EmptyRepositoryOnboardingSteps[:4] {
		if _, err := store.BeginOnboardingStep(ctx, application.OnboardingStepIntent{OnboardingID: opened.OnboardingID, Step: step, IntentDigest: digest(string(rune('5' + index))), IntendedAt: now.Add(time.Duration(3+index*2) * time.Second)}); err != nil {
			t.Fatalf("begin %s: %v", step, err)
		}
		observation := application.OnboardingStepObservation{Outcome: application.OperationOutcomeSucceeded, ReasonCode: "step_ready", EvidenceDigest: digest(string(rune('a' + index)))}
		if step == domain.OnboardingStepInitialRevisionCreated {
			observation.InitialRevisionSHA = sha
		}
		started, err = store.SettleOnboardingStep(ctx, application.OnboardingStepSettlement{OnboardingID: opened.OnboardingID, Step: step, Observation: observation, ObservedAt: now.Add(time.Duration(4+index*2) * time.Second)})
		if err != nil {
			t.Fatalf("settle %s: %v", step, err)
		}
	}
	if started.InitialRevisionSHA != sha || !slices.Equal(started.CompletedSteps, domain.EmptyRepositoryOnboardingSteps[:4]) {
		t.Fatalf("onboarding=%+v", started)
	}
}

func TestOnboardingV38MigrationPreservesVersion37RowsExactly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 37)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	digest := func(character string) string { return strings.Repeat(character, 64) }
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	receipt := application.NewOperationReceipt(application.OperationReceiptInput{OperationType: application.OperationOnboardRepository, Scope: application.ScopeOnboarding, TargetID: "onboarding-version-37", Requester: requester, RequestDigest: digest("a"), ExpectedAuthorityDigest: digest("b"), OperationAnchorDigest: digest("c"), TargetBindingDigest: digest("d"), AcceptedAt: now})
	if _, _, err := legacy.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	_, err = legacy.db.ExecContext(ctx, `INSERT INTO repository_onboardings(onboarding_id,onboarding_kind,canonical_repository,private_input_digest,source_path_digest,request_digest,requester_login,requester_database_id,requester_node_id,requester_actor_type,base_generation_id,base_digest,configuration_authority_version,status,step_index,preflight_digest,preview_digest,operation_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'running',1,?,?,?,?,?)`, "onboarding-version-37", "existing_checkout", "owner/legacy", digest("e"), digest("f"), receipt.RequestDigest, requester.Login, requester.DatabaseID, requester.NodeID, requester.ActorType, 1, receipt.ExpectedAuthorityDigest, 1, digest("1"), digest("2"), receipt.OperationID, formatTime(now.Add(-time.Hour)), formatTime(now.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.ExecContext(ctx, `INSERT INTO repository_onboarding_path_claims(onboarding_id,path_digest) VALUES(?,?)`, "onboarding-version-37", digest("f")); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.ExecContext(ctx, `INSERT INTO repository_onboarding_steps(onboarding_id,step_name,step_order,intent_digest,status,outcome,evidence_digest,intended_at,observed_at) VALUES(?,'roots_created',1,?,'observed','succeeded',?,?,?)`, "onboarding-version-37", digest("3"), digest("4"), formatTime(now.Add(2*time.Minute)), formatTime(now.Add(3*time.Minute))); err != nil {
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
	value, found, err := store.Onboarding(ctx, "onboarding-version-37")
	if err != nil || !found || value.Kind != domain.OnboardingExistingCheckout || value.InitialRevisionSHA != "" || !value.AcceptedAt.Equal(now) || !slices.Equal(value.CompletedSteps, []domain.OnboardingStep{domain.OnboardingStepRootsCreated}) || !value.CreatedAt.Equal(now.Add(-time.Hour)) || !value.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("value=%+v found=%t err=%v", value, found, err)
	}
}
