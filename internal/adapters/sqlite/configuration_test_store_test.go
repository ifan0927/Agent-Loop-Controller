package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// admissionTestStore is a test adapter, not a production Store mode. It gives
// legacy storage/unit fixtures an explicit ready configuration authority while
// production Store.CreateRun and reservation paths remain fail closed.
type admissionTestStore struct {
	*Store
	authority application.ConfigurationAdmissionAuthority
}

func openAdmissionTestStore(path string) (*admissionTestStore, error) {
	store, err := Open(path)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	authority, found, err := store.ConfigurationAuthority(ctx)
	if err != nil {
		store.Close()
		return nil, err
	}
	if !found {
		observedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		input := application.ConfigurationBaselineInput{
			Candidate: application.ValidatedConfigurationCandidate{
				Digest: strings.Repeat("a", 64), Size: 1, SchemaVersion: 5,
				DatabasePath: path,
				Operator:     domain.GitHubUserIdentity{Login: "fixture-operator", DatabaseID: 1, NodeID: "FIXTURE_USER_1", ActorType: "User"},
			},
			CanonicalConfigPath: filepath.Clean(path) + ".test-controller.json",
			ObservedAt:          observedAt,
		}
		if err := store.PrepareConfigurationBaseline(ctx, input); err != nil {
			store.Close()
			return nil, err
		}
		authority, _, err = store.AdoptConfigurationBaseline(ctx, input)
		if err != nil {
			store.Close()
			return nil, err
		}
	}
	if authority.EffectiveID != authority.Desired.GenerationID || authority.Desired.State != application.ConfigurationGenerationEffective {
		authority, _, err = store.ObserveConfigurationEffective(ctx, application.ConfigurationEffectiveObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, WorkerInstanceID: "fixture-worker", BuildIdentity: "fixture-build", ObservedAt: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), EvidenceDigest: strings.Repeat("e", 64)})
		if err != nil {
			store.Close()
			return nil, err
		}
	}
	return &admissionTestStore{Store: store, authority: application.ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}}, nil
}

func (s *admissionTestStore) CreateRun(ctx context.Context, input application.CreateRunInput) (application.Run, bool, error) {
	if !input.ConfigurationAuthority.Valid() {
		input.ConfigurationAuthority = s.authority
	}
	return s.Store.CreateRun(ctx, input)
}

func (s *admissionTestStore) ReserveLinearTodoAdmission(ctx context.Context, reservation application.LinearTodoAdmissionReservation) (application.Run, application.LinearTodoAdmissionJournal, bool, error) {
	if !reservation.ConfigurationAuthority.Valid() {
		reservation.ConfigurationAuthority = s.authority
	}
	return s.Store.ReserveLinearTodoAdmission(ctx, reservation)
}
