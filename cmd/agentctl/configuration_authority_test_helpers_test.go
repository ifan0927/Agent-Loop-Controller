package main

import (
	"context"
	"testing"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite/sqlitetest"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func testConfigurationAuthority(t *testing.T, store *sqlitestore.Store, databasePath string) application.ConfigurationAdmissionAuthority {
	t.Helper()
	authority, err := sqlitetest.EstablishReadyConfigurationAuthority(context.Background(), store, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func testNewAdmissionGate(t *testing.T, store *sqlitestore.Store, databasePath string) application.StaticNewAdmissionGate {
	t.Helper()
	return application.StaticNewAdmissionGate{Decision: application.NewAdmissionDecision{Allowed: true, Reason: application.ConfigurationReasonReady, Authority: testConfigurationAuthority(t, store, databasePath)}}
}

func testExistingNewAdmissionGate(t *testing.T, store *sqlitestore.Store) application.StaticNewAdmissionGate {
	t.Helper()
	authority, found, err := store.ConfigurationAuthority(context.Background())
	if err == nil && found && authority.EffectiveID != authority.Desired.GenerationID {
		authority, _, err = store.ObserveConfigurationEffective(context.Background(), application.ConfigurationEffectiveObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, WorkerInstanceID: "fixture-worker", BuildIdentity: "fixture-build", ObservedAt: time.Now().UTC(), EvidenceDigest: offlineAdmissionDigest("configuration-effective")})
	}
	if err != nil || !found || authority.EffectiveID != authority.Desired.GenerationID {
		t.Fatalf("ready configuration authority is unavailable: authority=%+v found=%t err=%v", authority, found, err)
	}
	decision := application.NewAdmissionDecision{Allowed: true, Reason: application.ConfigurationReasonReady, Authority: application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}}
	return application.StaticNewAdmissionGate{Decision: decision}
}
