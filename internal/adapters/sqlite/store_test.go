package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestMigrationAndRunIdempotency(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key-1", SourceRevision: "v1",
		RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run-1"}}
	if _, created, err := store.CreateRun(context.Background(), input); err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if _, created, err := store.CreateRun(context.Background(), input); err != nil || created {
		t.Fatalf("repeat: created=%v err=%v", created, err)
	}
	other := input
	other.Run.ID = "run-2"
	other.Run.IdempotencyKey = "key-2"
	if _, _, err := store.CreateRun(context.Background(), other); err == nil {
		t.Fatal("active issue uniqueness must reject second run")
	}
}

func TestDatabaseIsPrivateAndMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode=%o", info.Mode().Perm())
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestTransitionUsesExpectedStateCompare(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "admit", "snapshot", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "duplicate", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateReceived, domain.StateAdmitting, "stale", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateAdmitting, domain.StateProvisioning, "provision", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(context.Background(), "run-1", domain.StateAdmitting, domain.StateFailed, "stale", "", ""); err == nil {
		t.Fatal("stale expected state must fail")
	}
}

func TestAttemptArtifactDirectoryCannotBeReused(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginAttempt(context.Background(), "run-1", "implementation", "/tmp/run/attempt-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginAttempt(context.Background(), "run-1", "resume", "/tmp/run/attempt-1"); err == nil {
		t.Fatal("artifact directory reuse must fail")
	}
}

func TestOwnedResourceCannotBeClaimedByAnotherRun(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for index := 1; index <= 2; index++ {
		id := fmt.Sprintf("run-%d", index)
		input := application.CreateRunInput{Run: application.Run{ID: id, IssueID: fmt.Sprintf("ISSUE-%d", index), IdempotencyKey: fmt.Sprintf("key-%d", index), SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw", NormalizedTaskJSON: "{}", TaskHash: "task", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/shared", ArtifactRoot: "/tmp/" + id}}
		if _, _, err := store.CreateRun(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	resource := application.OwnedResource{RunID: "run-1", Kind: "branch", Name: "ifan/shared", CreationEvidence: "{}", Status: "reserved"}
	if err := store.AddOwnedResource(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	resource.RunID = "run-2"
	if err := store.AddOwnedResource(context.Background(), resource); err == nil {
		t.Fatal("second run must not claim an owned branch")
	}
}
