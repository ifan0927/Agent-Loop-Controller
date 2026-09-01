package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestRepositoryBaselineAdoptsExactlyOnceAndStartsUnknown(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	at := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	input := application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: at}
	if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), input); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	authority, err := store.RepositoryOperationAuthority(context.Background(), profile.Authority.Repository)
	if err != nil || authority.Lifecycle.Intent != application.RepositoryEnabled || authority.Lifecycle.Version != 1 || authority.Snapshot.Status != domain.RepositoryUnknown || authority.Snapshot.ReasonCode != "initial_recheck_required" || len(authority.Snapshot.Dimensions) != 8 {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
	drift := repositoryProfileFixture("owner/repo", "c", "d")
	if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{drift}, AdoptedAt: at}); !errors.Is(err, application.ErrRepositoryLifecycleConflict) {
		t.Fatalf("drift error=%v", err)
	}
}

func TestRepositoryBaselineAdoptsEmptyAuthorityAndReplaysAfterAuthorityVersionAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	authority, found, err := store.ConfigurationAuthority(ctx)
	if err != nil || !found {
		t.Fatalf("authority=%+v found=%t err=%v", authority, found, err)
	}
	now := time.Date(2026, 8, 31, 7, 0, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	var generationID, authorityVersion int64
	var count int
	var configurationDigest, profilesDigest string
	if err := store.db.QueryRow(`SELECT configuration_generation_id,configuration_digest,configuration_authority_version,repository_count,profiles_digest FROM repository_lifecycle_baseline WHERE authority_id=1`).Scan(&generationID, &configurationDigest, &authorityVersion, &count, &profilesDigest); err != nil {
		t.Fatal(err)
	}
	if generationID != authority.Desired.GenerationID || configurationDigest != authority.Desired.Digest || authorityVersion != authority.Version || count != 0 || profilesDigest != repositoryProfilesDigest(nil) {
		t.Fatalf("baseline generation=%d digest=%s version=%d count=%d profiles=%s authority=%+v", generationID, configurationDigest, authorityVersion, count, profilesDigest, authority)
	}
	for _, table := range []string{"repository_lifecycles", "repository_readiness_snapshots", "repository_readiness_dimensions"} {
		var rows int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("table=%s rows=%d err=%v", table, rows, err)
		}
	}
	changed, err := store.ObserveConfigurationDrift(ctx, application.ConfigurationDriftObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, ObservedDigest: strings.Repeat("f", 64), Drifted: true, Reason: application.ConfigurationReasonExternalDrift, ObservedAt: now.Add(time.Second)})
	advanced, found, authorityErr := store.ConfigurationAuthority(ctx)
	if err != nil || !changed || authorityErr != nil || !found || advanced.Version <= authority.Version {
		t.Fatalf("advanced=%+v changed=%t found=%t err=%v authority_err=%v", advanced, changed, found, err, authorityErr)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{AdoptedAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatalf("same-generation authority-version replay failed: %v", err)
	}
	if changed, err := store.ObserveConfigurationDrift(ctx, application.ConfigurationDriftObservation{ExpectedGenerationID: advanced.Desired.GenerationID, ExpectedDigest: advanced.Desired.Digest, ObservedDigest: advanced.Desired.Digest, Drifted: false, ObservedAt: now.Add(2 * time.Second)}); err != nil || !changed {
		t.Fatalf("drift clear changed=%t err=%v", changed, err)
	}
	advanced, found, err = store.ConfigurationAuthority(ctx)
	if err != nil || !found {
		t.Fatalf("advanced=%+v found=%t err=%v", advanced, found, err)
	}
	descendant, _ := beginRemovalConfigurationApply(t, ctx, store.Store, advanced, advanced.Desired.ConfiguredOperator, path, strings.Repeat("6", 64), now.Add(3*time.Second), "7")
	if _, _, _, err := store.SettleConfigurationApply(ctx, application.ConfigurationApplySettlement{GenerationID: descendant.GenerationID, ParentID: descendant.ParentID, OperationID: descendant.OperationID, Outcome: application.ConfigurationApplyCommitted, Reason: application.ConfigurationReasonRestartRequired, EvidenceDigest: strings.Repeat("8", 64), SettledAt: now.Add(4 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{AdoptedAt: now.Add(5 * time.Second)}); err != nil {
		t.Fatalf("descendant-generation empty replay failed: %v", err)
	}
	var replayGenerationID, replayAuthorityVersion int64
	var replayConfigurationDigest string
	if err := store.db.QueryRow(`SELECT configuration_generation_id,configuration_digest,configuration_authority_version FROM repository_lifecycle_baseline WHERE authority_id=1`).Scan(&replayGenerationID, &replayConfigurationDigest, &replayAuthorityVersion); err != nil || replayGenerationID != generationID || replayConfigurationDigest != configurationDigest || replayAuthorityVersion != authorityVersion {
		t.Fatalf("immutable anchor generation=%d digest=%s version=%d err=%v", replayGenerationID, replayConfigurationDigest, replayAuthorityVersion, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{AdoptedAt: now.Add(6 * time.Second)}); err != nil {
		t.Fatalf("restart replay failed: %v", err)
	}
}

func TestRepositoryEmptyBaselineReplayRejectsCorruptionAndUnmatchedLifecycle(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *admissionTestStore)
	}{
		{name: "blank profiles digest", mutate: func(t *testing.T, store *admissionTestStore) {
			_, err := store.db.Exec(`UPDATE repository_lifecycle_baseline SET profiles_digest=''`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "blank configuration digest", mutate: func(t *testing.T, store *admissionTestStore) {
			_, err := store.db.Exec(`UPDATE repository_lifecycle_baseline SET configuration_digest=''`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "negative repository count", mutate: func(t *testing.T, store *admissionTestStore) {
			rebuildMalformedRepositoryBaseline(t, store)
			if _, err := store.db.Exec(`UPDATE repository_lifecycle_baseline SET repository_count=-1`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non-singleton baseline", mutate: func(t *testing.T, store *admissionTestStore) {
			rebuildMalformedRepositoryBaseline(t, store)
			if _, err := store.db.Exec(`INSERT INTO repository_lifecycle_baseline SELECT 2,configuration_generation_id,configuration_digest,configuration_authority_version,repository_count,profiles_digest,adopted_at FROM repository_lifecycle_baseline WHERE authority_id=1`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong profiles digest", mutate: func(t *testing.T, store *admissionTestStore) {
			_, err := store.db.Exec(`UPDATE repository_lifecycle_baseline SET profiles_digest=?`, strings.Repeat("1", 64))
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing generation anchor", mutate: func(t *testing.T, store *admissionTestStore) {
			_, err := store.db.Exec(`UPDATE repository_lifecycle_baseline SET configuration_generation_id=999`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "generation digest mismatch", mutate: func(t *testing.T, store *admissionTestStore) {
			_, err := store.db.Exec(`UPDATE repository_lifecycle_baseline SET configuration_digest=?`, strings.Repeat("2", 64))
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "future authority version", mutate: func(t *testing.T, store *admissionTestStore) {
			_, err := store.db.Exec(`UPDATE repository_lifecycle_baseline SET configuration_authority_version=configuration_authority_version+100`)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non-ancestral generation anchor", mutate: func(t *testing.T, store *admissionTestStore) {
			now := formatTime(time.Date(2026, 8, 31, 7, 45, 0, 0, time.UTC))
			result, err := store.db.Exec(`INSERT INTO configuration_generations(digest,target_size,schema_version,origin,configured_operator_login,configured_operator_database_id,configured_operator_node_id,configured_operator_actor_type,lifecycle,raw_retained,created_at,committed_at,effective_at,settled_at,reason_code) VALUES(?,1,5,'baseline','fixture-operator',1,'FIXTURE_USER_1','User','effective',1,?,?,?,?,'')`, strings.Repeat("5", 64), now, now, now, now)
			if err != nil {
				t.Fatal(err)
			}
			generationID, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE repository_lifecycle_baseline SET configuration_generation_id=?,configuration_digest=?`, generationID, strings.Repeat("5", 64)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unmatched active lifecycle", mutate: func(t *testing.T, store *admissionTestStore) {
			now := formatTime(time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC))
			_, err := store.db.Exec(`INSERT INTO repository_lifecycles(incarnation_id,repository,profile_id,profile_digest,repository_binding_digest,intent,lifecycle_version,current_snapshot_id,updated_at) VALUES('incarnation-corrupt','owner/corrupt','profile-corrupt',?,?,'disabled',1,'',?)`, strings.Repeat("3", 64), strings.Repeat("4", 64), now)
			if err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), application.RepositoryBaselineInput{AdoptedAt: time.Date(2026, 8, 31, 7, 30, 0, 0, time.UTC)}); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, store)
			if err := store.AdoptRepositoryLifecycleBaseline(context.Background(), application.RepositoryBaselineInput{}); !errors.Is(err, application.ErrRepositoryLifecycleConflict) {
				t.Fatalf("corrupt replay error=%v", err)
			}
		})
	}
}

func rebuildMalformedRepositoryBaseline(t *testing.T, store *admissionTestStore) {
	t.Helper()
	for _, statement := range []string{
		`DROP TRIGGER integrity_track_repository_lifecycle_baseline_insert`,
		`DROP TRIGGER integrity_track_repository_lifecycle_baseline_update`,
		`DROP TRIGGER integrity_track_repository_lifecycle_baseline_delete`,
		`ALTER TABLE repository_lifecycle_baseline RENAME TO repository_lifecycle_baseline_valid_fixture`,
		`CREATE TABLE repository_lifecycle_baseline (authority_id INTEGER,configuration_generation_id INTEGER,configuration_digest TEXT,configuration_authority_version INTEGER,repository_count INTEGER,profiles_digest TEXT,adopted_at TEXT)`,
		`INSERT INTO repository_lifecycle_baseline SELECT * FROM repository_lifecycle_baseline_valid_fixture`,
		`DROP TABLE repository_lifecycle_baseline_valid_fixture`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepositoryBaselineMigrationV47PreservesPositiveAuthorityAndAdoptsIncidentEmpty(t *testing.T) {
	t.Run("positive baseline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "controller.db")
		store, err := openAdmissionTestStore(path)
		if err != nil {
			t.Fatal(err)
		}
		profile := repositoryProfileFixture("owner/migrated", "a", "b")
		now := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
		ctx := context.Background()
		if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
			t.Fatal(err)
		}
		repositoryBefore, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
		if err != nil {
			t.Fatal(err)
		}
		configuration, found, err := store.ConfigurationAuthority(ctx)
		if err != nil || !found {
			t.Fatalf("configuration=%+v found=%t err=%v", configuration, found, err)
		}
		onboardingInput := application.OnboardingOpenInput{OnboardingID: "onboarding-v46-preserved", Kind: domain.OnboardingExistingCheckout, CanonicalRepository: "owner/preserved", Requester: configuration.Desired.ConfiguredOperator, PrivateInputDigest: strings.Repeat("1", 64), SourcePathDigest: strings.Repeat("2", 64), SourceAncestorDigests: []string{strings.Repeat("2", 64)}, RequestDigest: strings.Repeat("3", 64), ConfigurationBaseGenerationID: configuration.Desired.GenerationID, ConfigurationBaseDigest: configuration.Desired.Digest, ConfigurationAuthorityVersion: configuration.Version, OpenedAt: now.Add(time.Second)}
		onboardingBefore, created, err := store.OpenOnboarding(ctx, onboardingInput)
		if err != nil || !created {
			t.Fatalf("onboarding=%+v created=%t err=%v", onboardingBefore, created, err)
		}
		receiptInput := repositoryReceiptFixture(application.OperationDisableRepository, repositoryBefore, profile, now.Add(2*time.Second))
		receiptBefore, created, err := store.BeginOperationReceipt(ctx, receiptInput)
		if err != nil || !created {
			t.Fatalf("receipt=%+v created=%t err=%v", receiptBefore, created, err)
		}
		var before string
		if err := store.db.QueryRow(`SELECT printf('%d|%d|%s|%d|%d|%s|%s',authority_id,configuration_generation_id,configuration_digest,configuration_authority_version,repository_count,profiles_digest,adopted_at) FROM repository_lifecycle_baseline`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		var repositoryActivityBefore, registryBefore string
		var repositoryRevisionBefore int64
		if err := store.db.QueryRow(`SELECT COALESCE(group_concat(event_id,'|'),'') FROM (SELECT event_id FROM activity_events WHERE category='repository' ORDER BY event_id)`).Scan(&repositoryActivityBefore); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow(`SELECT COALESCE(group_concat(family||':'||table_name,'|'),'') FROM (SELECT family,table_name FROM integrity_registry_sources ORDER BY family,table_name)`).Scan(&registryBefore); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow(`SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family='repository_onboarding' AND scope_kind='controller' AND scope_id='local-controller'`).Scan(&repositoryRevisionBefore); err != nil {
			t.Fatal(err)
		}
		downgradeRepositoryBaselineToV46(t, store)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		migrated, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer migrated.Close()
		var after string
		if err := migrated.db.QueryRow(`SELECT printf('%d|%d|%s|%d|%d|%s|%s',authority_id,configuration_generation_id,configuration_digest,configuration_authority_version,repository_count,profiles_digest,adopted_at) FROM repository_lifecycle_baseline`).Scan(&after); err != nil || after != before {
			t.Fatalf("before=%q after=%q err=%v", before, after, err)
		}
		repositoryAfter, err := migrated.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
		if err != nil {
			t.Fatal(err)
		}
		onboardingAfter, found, err := migrated.Onboarding(ctx, onboardingInput.OnboardingID)
		if err != nil || !found {
			t.Fatalf("onboarding=%+v found=%t err=%v", onboardingAfter, found, err)
		}
		receiptAfter, found, err := getOperationReceiptByIDQuery(ctx, migrated.db, receiptBefore.OperationID)
		if err != nil || !found {
			t.Fatalf("receipt=%+v found=%t err=%v", receiptAfter, found, err)
		}
		var repositoryActivityAfter, registryAfter string
		var repositoryRevisionAfter int64
		if err := migrated.db.QueryRow(`SELECT COALESCE(group_concat(event_id,'|'),'') FROM (SELECT event_id FROM activity_events WHERE category='repository' ORDER BY event_id)`).Scan(&repositoryActivityAfter); err != nil {
			t.Fatal(err)
		}
		if err := migrated.db.QueryRow(`SELECT COALESCE(group_concat(family||':'||table_name,'|'),'') FROM (SELECT family,table_name FROM integrity_registry_sources ORDER BY family,table_name)`).Scan(&registryAfter); err != nil {
			t.Fatal(err)
		}
		if err := migrated.db.QueryRow(`SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family='repository_onboarding' AND scope_kind='controller' AND scope_id='local-controller'`).Scan(&repositoryRevisionAfter); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(repositoryAfter, repositoryBefore) || !reflect.DeepEqual(onboardingAfter, onboardingBefore) || !reflect.DeepEqual(receiptAfter, receiptBefore) || repositoryActivityAfter != repositoryActivityBefore || registryAfter != registryBefore || repositoryRevisionAfter != repositoryRevisionBefore {
			t.Fatalf("related evidence changed: repository=%t onboarding=%t receipt=%t activity=%t registry=%t revision_before=%d revision_after=%d", reflect.DeepEqual(repositoryAfter, repositoryBefore), reflect.DeepEqual(onboardingAfter, onboardingBefore), reflect.DeepEqual(receiptAfter, receiptBefore), repositoryActivityAfter == repositoryActivityBefore, registryAfter == registryBefore, repositoryRevisionBefore, repositoryRevisionAfter)
		}
		var triggers, families, violations int
		if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN ('integrity_track_repository_lifecycle_baseline_insert','integrity_track_repository_lifecycle_baseline_update','integrity_track_repository_lifecycle_baseline_delete')`).Scan(&triggers); err != nil {
			t.Fatal(err)
		}
		if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM integrity_registry_families`).Scan(&families); err != nil {
			t.Fatal(err)
		}
		if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || triggers != 3 || families != 7 || violations != 0 {
			t.Fatalf("triggers=%d families=%d violations=%d err=%v", triggers, families, violations, err)
		}
	})

	t.Run("v46 missing baseline incident", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "controller.db")
		store, err := openAdmissionTestStore(path)
		if err != nil {
			t.Fatal(err)
		}
		downgradeRepositoryBaselineToV46(t, store)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		migrated, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer migrated.Close()
		var revisionBefore, revisionAfterInsert, revisionAfterUpdate, revisionAfterDelete int64
		readRevision := func(target *int64) {
			t.Helper()
			if err := migrated.db.QueryRow(`SELECT revision_generation FROM controller_integrity_scope_revisions WHERE family='repository_onboarding' AND scope_kind='controller' AND scope_id='local-controller'`).Scan(target); err != nil {
				t.Fatal(err)
			}
		}
		readRevision(&revisionBefore)
		if err := migrated.AdoptRepositoryLifecycleBaseline(context.Background(), application.RepositoryBaselineInput{AdoptedAt: time.Date(2026, 8, 31, 9, 30, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
		readRevision(&revisionAfterInsert)
		var count int
		if err := migrated.db.QueryRow(`SELECT repository_count FROM repository_lifecycle_baseline WHERE authority_id=1`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("count=%d err=%v", count, err)
		}
		if _, err := migrated.db.Exec(`UPDATE repository_lifecycle_baseline SET adopted_at=adopted_at WHERE authority_id=1`); err != nil {
			t.Fatal(err)
		}
		readRevision(&revisionAfterUpdate)
		if _, err := migrated.db.Exec(`DELETE FROM repository_lifecycle_baseline WHERE authority_id=1`); err != nil {
			t.Fatal(err)
		}
		readRevision(&revisionAfterDelete)
		if revisionAfterInsert <= revisionBefore || revisionAfterUpdate <= revisionAfterInsert || revisionAfterDelete <= revisionAfterUpdate {
			t.Fatalf("repository_onboarding revisions before=%d insert=%d update=%d delete=%d", revisionBefore, revisionAfterInsert, revisionAfterUpdate, revisionAfterDelete)
		}
	})
}

func downgradeRepositoryBaselineToV46(t *testing.T, store *admissionTestStore) {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statements := []string{
		`DROP TRIGGER integrity_track_repository_onboarding_step_claims_insert`,
		`DROP TRIGGER integrity_track_repository_onboarding_step_claims_update`,
		`DROP TRIGGER integrity_track_repository_onboarding_step_claims_delete`,
		`DROP TABLE repository_onboarding_step_claims`,
		`DELETE FROM integrity_registry_sources WHERE table_name='repository_onboarding_step_claims'`,
		`DROP TRIGGER integrity_track_repository_lifecycle_baseline_insert`,
		`DROP TRIGGER integrity_track_repository_lifecycle_baseline_update`,
		`DROP TRIGGER integrity_track_repository_lifecycle_baseline_delete`,
		`ALTER TABLE repository_lifecycle_baseline RENAME TO repository_lifecycle_baseline_v47_fixture`,
		`CREATE TABLE repository_lifecycle_baseline (
			authority_id INTEGER PRIMARY KEY CHECK(authority_id=1),
			configuration_generation_id INTEGER NOT NULL,
			configuration_digest TEXT NOT NULL,
			configuration_authority_version INTEGER NOT NULL CHECK(configuration_authority_version > 0),
			repository_count INTEGER NOT NULL CHECK(repository_count > 0),
			profiles_digest TEXT NOT NULL,
			adopted_at TEXT NOT NULL
		)`,
		`INSERT INTO repository_lifecycle_baseline SELECT * FROM repository_lifecycle_baseline_v47_fixture`,
		`DROP TABLE repository_lifecycle_baseline_v47_fixture`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
		if _, err := tx.Exec(repositoryLifecycleBaselineIntegrityTrigger(operation)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE version IN (47,48)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryLifecyclePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := openAdmissionTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	now := time.Date(2026, 8, 24, 1, 30, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	receipt := repositoryReceiptFixture(application.OperationDisableRepository, authority, profile, now.Add(time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: receipt.OperationID, Expected: authority, Intent: application.RepositoryDisabled, ChangedAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	authority, err = reopened.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil || authority.Lifecycle.Intent != application.RepositoryDisabled || authority.Lifecycle.Version != 2 {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
}

func TestRepositoryRecheckPublishesAtomicallyAndLifecycleFencesAdmission(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	now := time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	receipt := repositoryReceiptFixture(application.OperationRecheckRepository, authority, profile, now)
	if _, _, err := store.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	attempt := application.RepositoryRecheckStart{AttemptID: "repository-recheck-" + receipt.OperationID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(time.Second)}
	if _, created, err := store.BeginRepositoryRecheck(ctx, attempt); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	if decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile); err != nil || decision.Allowed || decision.Reason != "readiness_recheck_in_progress" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	results := repositoryReadyResults(now.Add(2 * time.Second))
	for _, result := range results {
		if err := store.SaveRepositoryRecheckObservation(ctx, attempt.AttemptID, result); err != nil {
			t.Fatal(err)
		}
	}
	projection, settled, err := store.PublishRepositoryRecheck(ctx, application.RepositoryRecheckPublication{AttemptID: attempt.AttemptID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, Results: results, PublishedAt: now.Add(3 * time.Second)})
	if err != nil || projection.Readiness.Status != domain.RepositoryReady || settled.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("projection=%+v receipt=%+v err=%v", projection, settled, err)
	}
	decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile)
	if err != nil || !decision.Allowed || !decision.Token.Valid() {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}

	authority, _ = store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	disable := repositoryReceiptFixture(application.OperationDisableRepository, authority, profile, now.Add(4*time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, disable); err != nil {
		t.Fatal(err)
	}
	projection, settled, err = store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: disable.OperationID, Expected: authority, Intent: application.RepositoryDisabled, ChangedAt: now.Add(5 * time.Second)})
	if err != nil || projection.Lifecycle.Intent != application.RepositoryDisabled || projection.Lifecycle.Version != 2 || settled.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("projection=%+v receipt=%+v err=%v", projection, settled, err)
	}
	if decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile); err != nil || decision.Allowed || decision.Reason != "repository_disabled" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	authority, _ = store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	enable := repositoryReceiptFixture(application.OperationEnableRepository, authority, profile, now.Add(6*time.Second))
	if _, _, err := store.BeginOperationReceipt(ctx, enable); err != nil {
		t.Fatal(err)
	}
	projection, settled, err = store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: enable.OperationID, Expected: authority, Intent: application.RepositoryEnabled, ChangedAt: now.Add(7 * time.Second)})
	if err != nil || projection.Lifecycle.Version != 3 || projection.Readiness.Status != domain.RepositoryReady || settled.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("projection=%+v receipt=%+v err=%v", projection, settled, err)
	}
	if decision, err := store.CheckRepositoryAdmission(ctx, profile.Profile); err != nil || !decision.Allowed || decision.Token.LifecycleVersion != 3 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestRepositoryRecheckFailureSettlesReceiptWithoutPublication(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	now := time.Date(2026, 8, 24, 2, 30, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	receipt := repositoryReceiptFixture(application.OperationRecheckRepository, authority, profile, now)
	if _, _, err := store.BeginOperationReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	attemptID := "repository-recheck-" + receipt.OperationID
	if _, created, err := store.BeginRepositoryRecheck(ctx, application.RepositoryRecheckStart{AttemptID: attemptID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(time.Second)}); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	if err := store.SettleRepositoryRecheckFailure(ctx, application.RepositoryRecheckFailure{AttemptID: attemptID, OperationID: receipt.OperationID, Outcome: application.OperationOutcomeFailed, ReasonCode: "readiness_observation_failed", SettledAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := store.db.QueryRowContext(ctx, `SELECT outcome FROM operation_receipts WHERE operation_id=?`, receipt.OperationID).Scan(&outcome); err != nil || outcome != string(application.OperationOutcomeFailed) {
		t.Fatalf("outcome=%q err=%v", outcome, err)
	}
	current, err := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	if err != nil || current.Recheck != nil || current.Snapshot.SnapshotID != authority.Snapshot.SnapshotID {
		t.Fatalf("authority=%+v err=%v", current, err)
	}
}

func TestRepositoryAdmissionTokenIsRevalidatedWithRunCreation(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repo", "a", "b")
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	receipt := repositoryReceiptFixture(application.OperationRecheckRepository, authority, profile, now)
	_, _, _ = store.BeginOperationReceipt(ctx, receipt)
	attempt := application.RepositoryRecheckStart{AttemptID: "repository-recheck-" + receipt.OperationID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(time.Second)}
	_, _, _ = store.BeginRepositoryRecheck(ctx, attempt)
	results := repositoryReadyResults(now.Add(2 * time.Second))
	for _, result := range results {
		_ = store.SaveRepositoryRecheckObservation(ctx, attempt.AttemptID, result)
	}
	_, _, err = store.PublishRepositoryRecheck(ctx, application.RepositoryRecheckPublication{AttemptID: attempt.AttemptID, OperationID: receipt.OperationID, Expected: authority, Profile: profile.Profile, Results: results, PublishedAt: now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	decision, _ := store.CheckRepositoryAdmission(ctx, profile.Profile)
	run := repositoryRunFixture(profile.Profile, "run-token", "IFAN-121")
	if _, created, err := store.CreateRun(ctx, application.CreateRunInput{Run: run, RepositoryAuthority: decision.Token}); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	stale := decision.Token
	stale.SnapshotDigest = strings.Repeat("f", 64)
	run = repositoryRunFixture(profile.Profile, "run-stale-token", "IFAN-122")
	if _, _, err := store.CreateRun(ctx, application.CreateRunInput{Run: run, RepositoryAuthority: stale}); !errors.Is(err, application.ErrRepositoryAdmissionConflict) {
		t.Fatalf("stale token error=%v", err)
	}
}

func TestRepositoryAdmissionTokenIsRevalidatedWithAutomaticReservation(t *testing.T) {
	store, err := openAdmissionTestStore(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	profile := repositoryProfileFixture("owner/repository", "a", "b")
	now := time.Now().UTC()
	if err := store.AdoptRepositoryLifecycleBaseline(ctx, application.RepositoryBaselineInput{Profiles: []application.RepositoryProfileAuthority{profile}, AdoptedAt: now}); err != nil {
		t.Fatal(err)
	}
	authority, _ := store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	recheck := repositoryReceiptFixture(application.OperationRecheckRepository, authority, profile, now)
	_, _, _ = store.BeginOperationReceipt(ctx, recheck)
	attemptID := "repository-recheck-" + recheck.OperationID
	_, _, _ = store.BeginRepositoryRecheck(ctx, application.RepositoryRecheckStart{AttemptID: attemptID, OperationID: recheck.OperationID, Expected: authority, Profile: profile.Profile, StartedAt: now.Add(time.Second)})
	results := repositoryReadyResults(now.Add(2 * time.Second))
	for _, result := range results {
		_ = store.SaveRepositoryRecheckObservation(ctx, attemptID, result)
	}
	_, _, err = store.PublishRepositoryRecheck(ctx, application.RepositoryRecheckPublication{AttemptID: attemptID, OperationID: recheck.OperationID, Expected: authority, Profile: profile.Profile, Results: results, PublishedAt: now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	decision, _ := store.CheckRepositoryAdmission(ctx, profile.Profile)
	lease, acquired, err := store.AcquireLinearTodoAdmissionLease(ctx, "repository-token-test", time.Minute, now.Add(4*time.Second))
	if err != nil || !acquired {
		t.Fatalf("lease acquired=%t err=%v", acquired, err)
	}
	reservation := automaticAdmissionReservation("123e4567-e89b-42d3-a456-426614174121", "run-repository-token", "IFAN-121", lease)
	reservation.Input.Repository = profile.Profile
	reservation.Input.RepositoryAuthority = decision.Token
	authority, _ = store.RepositoryOperationAuthority(ctx, profile.Authority.Repository)
	disable := repositoryReceiptFixture(application.OperationDisableRepository, authority, profile, now.Add(5*time.Second))
	_, _, _ = store.BeginOperationReceipt(ctx, disable)
	if _, _, err := store.ChangeRepositoryLifecycle(ctx, application.RepositoryLifecycleChange{OperationID: disable.OperationID, Expected: authority, Intent: application.RepositoryDisabled, ChangedAt: now.Add(6 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.ReserveLinearTodoAdmission(ctx, reservation); !errors.Is(err, application.ErrRepositoryAdmissionConflict) {
		t.Fatalf("stale automatic reservation error=%v", err)
	}
}

func repositoryProfileFixture(repository, profileByte, bindingByte string) application.RepositoryProfileAuthority {
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	profileDigest, bindingDigest := strings.Repeat(profileByte, 64), strings.Repeat(bindingByte, 64)
	profile := application.LocalRepository{ProfileID: "repository-profile:" + repository, ProfileSnapshotVersion: 1, ProfileDigest: profileDigest, RegistryVersion: 1, RegistryDigest: strings.Repeat("e", 64), RepositoryBindingDigest: bindingDigest, CanonicalRepository: repository, LinearLabel: "repo", BaseBranch: "main", VerifierRegistryRef: "registry", VerifierIDs: []string{"go-test"}, AllowedOperatorLogins: []string{"operator"}, TrustedOperatorActors: []application.TrustedActorIdentity{{Login: identity.Login, DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, Type: identity.ActorType}}}
	authority := application.RepositoryAuthority{Repository: repository, ProfileID: profile.ProfileID, BindingDigest: bindingDigest, AllowedLogins: []string{"operator"}, TrustedOperators: []domain.GitHubUserIdentity{identity}}
	return application.RepositoryProfileAuthority{Authority: authority, Profile: profile}
}

func repositoryReceiptFixture(operation application.OperationType, authority application.RepositoryOperationAuthority, profile application.RepositoryProfileAuthority, at time.Time) application.OperationReceipt {
	requester := profile.Authority.TrustedOperators[0]
	return application.NewOperationReceipt(application.OperationReceiptInput{OperationType: operation, Scope: application.ScopeRepository, TargetID: profile.Authority.Repository, Requester: requester, RequestDigest: strings.Repeat("c", 64), ExpectedAuthorityDigest: strings.Repeat("d", 64), OperationAnchorDigest: application.ConfigurationEvidenceDigest("anchor", string(operation), profile.Authority.Repository, strings.Repeat("x", int(authority.Lifecycle.Version))), TargetBindingDigest: profile.Authority.BindingDigest, AcceptedAt: at})
}

func repositoryReadyResults(at time.Time) []domain.RepositoryDimensionResult {
	results := make([]domain.RepositoryDimensionResult, 0, len(domain.RepositoryReadinessDimensions))
	for _, dimension := range domain.RepositoryReadinessDimensions {
		results = append(results, domain.RepositoryDimensionResult{Dimension: dimension, Status: domain.RepositoryReady, ReasonCode: "ready", EvidenceDigest: application.ConfigurationEvidenceDigest("ready", string(dimension)), ObservedAt: at})
	}
	return results
}

func repositoryRunFixture(profile application.LocalRepository, runID, issueID string) application.Run {
	raw, _ := json.Marshal(profile)
	return application.Run{ID: runID, IssueID: issueID, IdempotencyKey: "key-" + runID, SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: profile.CanonicalRepository, RepositoryConfigJSON: string(raw), ProfileID: profile.ProfileID, ProfileSnapshotVersion: profile.ProfileSnapshotVersion, ProfileDigest: profile.ProfileDigest, RegistryVersion: profile.RegistryVersion, RegistryDigest: profile.RegistryDigest, RepositoryBindingDigest: profile.RepositoryBindingDigest, BaseBranch: profile.BaseBranch, WorkingBranch: "ifan/121", ArtifactRoot: "/tmp/" + runID, ImplementationModel: "model", ReviewModel: "review", State: domain.StateReceived}
}
