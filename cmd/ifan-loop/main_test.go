package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestDecodeTaskRejectsTrailingJSON(t *testing.T) {
	input := `{"run_id":"one"} {"run_id":"two"}`
	if _, err := decodeTask(strings.NewReader(input)); err == nil {
		t.Fatal("expected trailing JSON to be rejected")
	}
}

func TestLocalCommandsAcceptDocumentedLeadingRunID(t *testing.T) {
	runID, args := splitLeadingRunID([]string{"run-123", "--db", "/tmp/controller.db"})
	if runID != "run-123" || len(args) != 2 || args[0] != "--db" {
		t.Fatalf("runID=%q args=%v", runID, args)
	}
}

func TestDecodeDecisionRejectsTrailingJSON(t *testing.T) {
	if _, err := decodeDecision(strings.NewReader(`{"choice_id":"a","instructions":"go"} {}`)); err == nil {
		t.Fatal("expected trailing decision JSON rejection")
	}
}

func TestLocalStatusOutputsDurableInspection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	store, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	input := application.CreateRunInput{Run: application.Run{ID: "run-1", IssueID: "ISSUE-1", IdempotencyKey: "key", SourceRevision: "v1", RawIssueJSON: "{}", RawIssueHash: "raw-hash", NormalizedTaskJSON: "{}", TaskHash: "task-hash", Repository: "repo:test-project", RepositoryConfigJSON: "{}", BaseBranch: "main", WorkingBranch: "ifan/test", ArtifactRoot: "/tmp/run"}}
	if _, _, err := store.CreateRun(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	store.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	callErr := localInspect("status", []string{"run-1", "--db", path})
	write.Close()
	os.Stdout = original
	if callErr != nil {
		t.Fatal(callErr)
	}
	output, err := io.ReadAll(read)
	read.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"current_state": "received"`, `"state_timeline"`, `"task_snapshot_hash": "task-hash"`, `"attempts"`, `"verifications"`, `"reviews"`, `"owned_resources"`, `"last_durable_error"`} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("status output missing %s: %s", want, output)
		}
	}
}
