// Package sqlitetest provides explicit SQLite test and development-fixture
// composition helpers. Production composition must establish configuration
// authority from the canonical live configuration.
package sqlitetest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func EstablishReadyConfigurationAuthority(ctx context.Context, store *sqlitestore.Store, databasePath string) (application.ConfigurationAdmissionAuthority, error) {
	return EstablishReadyFixtureConfigurationAuthority(ctx, store, databasePath)
}

func EstablishReadyFixtureConfigurationAuthority(ctx context.Context, store *sqlitestore.Store, databasePath string) (application.ConfigurationAdmissionAuthority, error) {
	authority, found, err := store.ConfigurationAuthority(ctx)
	if err != nil {
		return application.ConfigurationAdmissionAuthority{}, err
	}
	expectedConfigPath := filepath.Clean(databasePath) + ".test-controller.json"
	if found && !isDevelopmentFixtureAuthority(authority, expectedConfigPath, databasePath) {
		return application.ConfigurationAdmissionAuthority{}, errors.New("existing configuration authority is not a development fixture")
	}
	if !found {
		observedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		input := application.ConfigurationBaselineInput{
			Candidate: application.ValidatedConfigurationCandidate{
				Digest: strings.Repeat("a", 64), Size: 1, SchemaVersion: 5,
				DatabasePath: databasePath,
				Operator:     domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"},
			},
			CanonicalConfigPath: expectedConfigPath,
			ObservedAt:          observedAt,
		}
		if err := store.PrepareConfigurationBaseline(ctx, input); err != nil {
			return application.ConfigurationAdmissionAuthority{}, err
		}
		authority, _, err = store.AdoptConfigurationBaseline(ctx, input)
		if err != nil {
			return application.ConfigurationAdmissionAuthority{}, err
		}
	}
	if authority.EffectiveID != authority.Desired.GenerationID || authority.Desired.State != application.ConfigurationGenerationEffective {
		authority, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, WorkerInstanceID: "fixture-worker", BuildIdentity: "fixture-build", ObservedAt: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), EvidenceDigest: strings.Repeat("e", 64)})
		if err != nil {
			return application.ConfigurationAdmissionAuthority{}, err
		}
	}
	return application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func isDevelopmentFixtureAuthority(authority application.ConfigurationAuthority, configPath, databasePath string) bool {
	operator := authority.Desired.ConfiguredOperator
	return authority.CanonicalConfigPath == configPath && authority.DatabasePath == databasePath &&
		authority.Desired.Origin == application.ConfigurationOriginBaseline && authority.Desired.Digest == strings.Repeat("a", 64) &&
		authority.Desired.Size == 1 && authority.Desired.SchemaVersion == 5 &&
		operator.Login == "fixture-operator" && operator.DatabaseID == 1 && operator.NodeID == "FIXTURE_USER_1" && operator.ActorType == "User"
}
