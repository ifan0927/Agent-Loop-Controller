package sqlite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

var admissionTestStoreSeedPath string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "agent-loop-sqlite-test-seed-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create SQLite test seed directory: %v\n", err)
		os.Exit(1)
	}
	seedPath := filepath.Join(root, "controller.db")
	seed, err := Open(seedPath)
	if err == nil {
		var version int
		version, err = seed.SchemaVersion(context.Background())
		if err == nil && version != schemaVersion {
			err = fmt.Errorf("SQLite test seed schema version=%d", version)
		}
		if err == nil {
			_, found, authorityErr := seed.ConfigurationAuthority(context.Background())
			if authorityErr != nil {
				err = authorityErr
			} else if found {
				err = errors.New("SQLite test seed contains configuration authority")
			}
		}
		if closeErr := seed.Close(); err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = validateClosedAdmissionTestStoreSeed(seedPath)
	}
	if err != nil {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "prepare SQLite test seed: %v\n", err)
		os.Exit(1)
	}

	admissionTestStoreSeedPath = seedPath
	code := m.Run()
	admissionTestStoreSeedPath = ""
	if err := os.RemoveAll(root); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove SQLite test seed: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

func openAdmissionTestStore(path string) (*admissionTestStore, error) {
	if admissionTestStoreSeedPath == "" {
		return nil, errors.New("SQLite test seed is unavailable")
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := copyAdmissionTestStoreSeed(path); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	store, err := Open(path)
	if err != nil {
		return nil, err
	}
	return configureAdmissionTestStore(store, path)
}

func copyAdmissionTestStoreSeed(path string) error {
	source, err := os.Open(admissionTestStoreSeedPath)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("SQLite test seed is unsafe")
	}
	target, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = target.Close()
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(target, source); err != nil {
		return err
	}
	if err := target.Chmod(0o600); err != nil {
		return err
	}
	if err := target.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func validateClosedAdmissionTestStoreSeed(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("SQLite test seed is unsafe")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("SQLite test seed has an auxiliary file")
			}
			return err
		}
	}
	return nil
}

func configureAdmissionTestStore(store *Store, path string) (*admissionTestStore, error) {
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
