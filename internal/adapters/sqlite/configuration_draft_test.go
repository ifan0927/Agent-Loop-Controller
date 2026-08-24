package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestConfigurationDraftV33MigratesV31WithoutSyntheticDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 31)
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
	if _, found, err := store.ActiveConfigurationDraft(context.Background()); err != nil || found {
		t.Fatalf("synthetic draft found=%t err=%v", found, err)
	}
	var table int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='configuration_drafts'`).Scan(&table); err != nil || table != 1 {
		t.Fatalf("table=%d err=%v", table, err)
	}
}

func TestConcurrentConfigurationDraftV33MigrationFromV31(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 31)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	errorsSeen := make(chan error, 2)
	for range 2 {
		go func() {
			store, openErr := openPinnedStore(context.Background(), path, schemaVersion, true, application.DatabaseFileIdentity{}, nil, openPinnedStoreHooks{beforeEffects: func() { ready <- struct{}{}; <-release }})
			if openErr == nil {
				openErr = store.Close()
			}
			errorsSeen <- openErr
		}()
	}
	<-ready
	<-ready
	close(release)
	for range 2 {
		if err := <-errorsSeen; err != nil {
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

func TestConfigurationRollbackV33MigratesV32DraftAsNormalAndPreservesGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	legacy, err := openWithSupportedSchema(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC))
	digest := strings.Repeat("a", 64)
	result, err := legacy.db.Exec(`INSERT INTO configuration_generations(digest,target_size,schema_version,origin,configured_operator_login,configured_operator_database_id,configured_operator_node_id,configured_operator_actor_type,lifecycle,raw_retained,created_at,committed_at,effective_at,settled_at,reason_code) VALUES(?,1,5,'baseline','operator',1,'USER_1','User','effective',1,?,?,?,?,?)`, digest, now, now, now, now, string(application.ConfigurationReasonReady))
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ := result.LastInsertId()
	if _, err := legacy.db.Exec(`INSERT INTO configuration_authority(authority_id,canonical_config_path,database_path,desired_generation_id,effective_generation_id,authority_version,created_at,updated_at) VALUES(1,'/tmp/controller.json',?,?,?,1,?,?)`, path, generationID, generationID, now, now); err != nil {
		t.Fatal(err)
	}
	settings := configurationDraftSettings()
	if _, err := legacy.db.Exec(`INSERT INTO configuration_drafts(draft_id,base_generation_id,base_digest,revision,lifecycle,run_timeout_ns,admission_enabled,admission_poll_interval_ns,delivery_poll_interval_ns,scheduler_lease_ttl_ns,scheduler_lease_renewal_interval_ns,max_candidates,max_pages,heavy_capacity,settings_digest,created_at,updated_at) VALUES(?,?,?,1,'open',?,?,?,?,?,?,?,?,?,?,?,?)`, "configuration-draft-00000000000000000000000000000001", generationID, digest, int64(settings.RunTimeout), boolInt(settings.Admission.Enabled), int64(settings.Admission.PollInterval), int64(settings.Admission.DeliveryPollInterval), int64(settings.Admission.SchedulerLeaseTTL), int64(settings.Admission.SchedulerLeaseRenewalInterval), settings.Admission.MaxCandidates, settings.Admission.MaxPages, settings.Admission.HeavyCapacity, application.ConfigurationSettingsDigest(settings), now, now); err != nil {
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
	draft, found, err := store.ConfigurationDraft(context.Background(), "configuration-draft-00000000000000000000000000000001")
	if err != nil || !found || draft.DraftOrigin != application.ConfigurationDraftOriginNormal || draft.RollbackSourceGenerationID != 0 || draft.RollbackSourceDigest != "" {
		t.Fatalf("draft=%+v found=%t err=%v", draft, found, err)
	}
	generations, err := store.ListConfigurationGenerations(context.Background())
	if err != nil || len(generations) != 1 || generations[0].RollbackSourceGenerationID != 0 || generations[0].RollbackSourceDigest != "" {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
	if _, err := store.db.Exec(`UPDATE configuration_drafts SET draft_origin='rollback',rollback_source_generation_id=1,rollback_source_digest=? WHERE draft_id=?`, digest, draft.DraftID); err == nil {
		t.Fatal("draft rollback provenance remained mutable")
	}
}

func TestConfigurationDraftOpenEditReplayDiscardAndIntegrity(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	authority, found, err := store.ConfigurationAuthority(ctx)
	if err != nil || !found {
		t.Fatalf("authority=%+v found=%t err=%v", authority, found, err)
	}
	settings := configurationDraftSettings()
	now := time.Now().UTC()
	input := application.ConfigurationDraftOpenInput{DraftID: "configuration-draft-00000000000000000000000000000001", BaseGenerationID: authority.Desired.GenerationID, BaseDigest: authority.Desired.Digest, Settings: settings, SettingsDigest: application.ConfigurationSettingsDigest(settings), OpenedAt: now}
	draft, created, err := store.OpenConfigurationDraft(ctx, input)
	if err != nil || !created || draft.Revision != 1 {
		t.Fatalf("draft=%+v created=%t err=%v", draft, created, err)
	}
	reopened, created, err := store.OpenConfigurationDraft(ctx, application.ConfigurationDraftOpenInput{DraftID: "configuration-draft-00000000000000000000000000000002", BaseGenerationID: authority.Desired.GenerationID, BaseDigest: authority.Desired.Digest, Settings: settings, SettingsDigest: application.ConfigurationSettingsDigest(settings), OpenedAt: now.Add(time.Second)})
	if err != nil || created || reopened.DraftID != draft.DraftID {
		t.Fatalf("reopened=%+v created=%t err=%v", reopened, created, err)
	}

	settings.Admission.HeavyCapacity = 3
	edit := application.ConfigurationDraftEditInput{DraftID: draft.DraftID, ExpectedRevision: 1, Settings: settings, SettingsDigest: application.ConfigurationSettingsDigest(settings), Field: application.ConfigurationFieldAdmissionHeavyCapacity, EditDigest: strings.Repeat("b", 64), EditedAt: now.Add(2 * time.Second)}
	edited, changed, err := store.EditConfigurationDraft(ctx, edit)
	if err != nil || !changed || edited.Revision != 2 || edited.Settings.Admission.HeavyCapacity != 3 {
		t.Fatalf("edited=%+v changed=%t err=%v", edited, changed, err)
	}
	replay, changed, err := store.EditConfigurationDraft(ctx, edit)
	if err != nil || changed || replay.Revision != 2 {
		t.Fatalf("replay=%+v changed=%t err=%v", replay, changed, err)
	}
	conflict := edit
	conflict.EditDigest = strings.Repeat("c", 64)
	if _, _, err := store.EditConfigurationDraft(ctx, conflict); err == nil {
		t.Fatal("different edit replay did not conflict")
	}
	discarded, changed, err := store.DiscardConfigurationDraft(ctx, application.ConfigurationDraftDiscardInput{DraftID: draft.DraftID, ExpectedRevision: 2, DiscardedAt: now.Add(3 * time.Second)})
	if err != nil || !changed || discarded.State != application.ConfigurationDraftDiscarded {
		t.Fatalf("discarded=%+v changed=%t err=%v", discarded, changed, err)
	}
	replayedDiscard, changed, err := store.DiscardConfigurationDraft(ctx, application.ConfigurationDraftDiscardInput{DraftID: draft.DraftID, ExpectedRevision: 2, DiscardedAt: now.Add(4 * time.Second)})
	if err != nil || changed || replayedDiscard.State != application.ConfigurationDraftDiscarded {
		t.Fatalf("discard replay=%+v changed=%t err=%v", replayedDiscard, changed, err)
	}
	var generations, receipts int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM configuration_generations`).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM operation_receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if generations != 1 || receipts != 0 {
		t.Fatalf("discard mutated authority: generations=%d receipts=%d", generations, receipts)
	}
	if _, err := store.db.Exec(`UPDATE configuration_drafts SET heavy_capacity=99 WHERE draft_id=?`, draft.DraftID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ConfigurationDraft(ctx, draft.DraftID); err == nil || found {
		t.Fatalf("malformed draft found=%t err=%v", found, err)
	}
}

func TestConfigurationRollbackDraftReplayRejectsStaleCurrentAuthority(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	authority, found, err := store.ConfigurationAuthority(ctx)
	if err != nil || !found {
		t.Fatalf("authority=%+v found=%t err=%v", authority, found, err)
	}
	now := time.Now().UTC()
	sourceID, sourceDigest := authority.Desired.GenerationID, authority.Desired.Digest
	if _, err := store.db.Exec(`UPDATE configuration_generations SET lifecycle='superseded',superseded_at=?,settled_at=? WHERE generation_id=?`, formatTime(now), formatTime(now), sourceID); err != nil {
		t.Fatal(err)
	}
	currentDigest := strings.Repeat("b", 64)
	result, err := store.db.Exec(`INSERT INTO configuration_generations(digest,target_size,schema_version,origin,configured_operator_login,configured_operator_database_id,configured_operator_node_id,configured_operator_actor_type,lifecycle,raw_retained,created_at,committed_at,effective_at,settled_at,reason_code) VALUES(?,1,5,'baseline',?,?,?,?,'effective',1,?,?,?,?,?)`, currentDigest, authority.Desired.ConfiguredOperator.Login, authority.Desired.ConfiguredOperator.DatabaseID, authority.Desired.ConfiguredOperator.NodeID, authority.Desired.ConfiguredOperator.ActorType, formatTime(now), formatTime(now), formatTime(now), formatTime(now), string(application.ConfigurationReasonReady))
	if err != nil {
		t.Fatal(err)
	}
	currentID, _ := result.LastInsertId()
	if _, err := store.db.Exec(`UPDATE configuration_authority SET desired_generation_id=?,effective_generation_id=? WHERE authority_id=1`, currentID, currentID); err != nil {
		t.Fatal(err)
	}
	settings := configurationDraftSettings()
	input := application.ConfigurationDraftOpenInput{DraftID: "configuration-draft-00000000000000000000000000000021", BaseGenerationID: currentID, BaseDigest: currentDigest, Settings: settings, SettingsDigest: application.ConfigurationSettingsDigest(settings), DraftOrigin: application.ConfigurationDraftOriginRollback, RollbackSourceGenerationID: sourceID, RollbackSourceDigest: sourceDigest, OpenedAt: now}
	draft, created, err := store.OpenConfigurationDraft(ctx, input)
	if err != nil || !created || draft.DraftOrigin != application.ConfigurationDraftOriginRollback {
		t.Fatalf("draft=%+v created=%t err=%v", draft, created, err)
	}
	if _, err := store.db.Exec(`UPDATE configuration_authority SET desired_generation_id=? WHERE authority_id=1`, sourceID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.OpenConfigurationDraft(ctx, input); err == nil {
		t.Fatal("rollback draft replay accepted stale current authority")
	}
}

func TestConcurrentConfigurationDraftOpenCreatesOneActiveDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	first, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	authority, _, err := first.ConfigurationAuthority(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings := configurationDraftSettings()
	stores := []*admissionTestStore{first, second}
	results := make(chan application.ConfigurationDraft, 2)
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	for index, store := range stores {
		wait.Add(1)
		go func(index int, store *admissionTestStore) {
			defer wait.Done()
			id := "configuration-draft-0000000000000000000000000000000" + string(rune('1'+index))
			draft, _, openErr := store.OpenConfigurationDraft(context.Background(), application.ConfigurationDraftOpenInput{DraftID: id, BaseGenerationID: authority.Desired.GenerationID, BaseDigest: authority.Desired.Digest, Settings: settings, SettingsDigest: application.ConfigurationSettingsDigest(settings), OpenedAt: time.Now().UTC()})
			results <- draft
			errorsSeen <- openErr
		}(index, store)
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for draft := range results {
		if id == "" {
			id = draft.DraftID
		} else if draft.DraftID != id {
			t.Fatalf("different active drafts %q and %q", id, draft.DraftID)
		}
	}
	var count int
	if err := first.db.QueryRow(`SELECT COUNT(*) FROM configuration_drafts WHERE lifecycle IN ('open','applying','ambiguous')`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("active=%d err=%v", count, err)
	}
}

func configurationDraftSettings() application.ConfigurationEditableSettings {
	return application.ConfigurationEditableSettings{RunTimeout: application.ConfigurationDuration(30 * time.Minute), Admission: application.ConfigurationEditableAdmissionSettings{Enabled: true, PollInterval: application.ConfigurationDuration(5 * time.Minute), DeliveryPollInterval: application.ConfigurationDuration(30 * time.Second), SchedulerLeaseTTL: application.ConfigurationDuration(time.Minute), SchedulerLeaseRenewalInterval: application.ConfigurationDuration(20 * time.Second), MaxCandidates: 20, MaxPages: 5, HeavyCapacity: 2}}
}
