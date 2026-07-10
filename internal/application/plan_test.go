package application

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestDirectoriesOverlapByFileIdentity(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	artifacts := filepath.Join(workspace, "runs", "one")
	if err := os.MkdirAll(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	overlapped, err := directoriesOverlap(workspace, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if !overlapped {
		t.Fatal("nested directories must overlap")
	}
}

func TestDirectoriesOverlapThroughSymlinkAlias(t *testing.T) {
	workspace := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	overlapped, err := directoriesOverlap(workspace, alias)
	if err != nil {
		t.Fatal(err)
	}
	if !overlapped {
		t.Fatal("symlink aliases to the same directory must overlap")
	}
}

func TestExistingDirectoryRejectsMissingAndParentTraversal(t *testing.T) {
	root := t.TempDir()
	if _, err := existingDirectory(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing directory must be rejected")
	}
	path := root + string(filepath.Separator) + ".." + string(filepath.Separator) + "other"
	if _, err := existingDirectory(path); err == nil {
		t.Fatal("parent traversal components must be rejected")
	}
}

func TestPlanMaterializesValidSchemasOutsideWorkspace(t *testing.T) {
	task := validTask()
	workspace := t.TempDir()
	artifacts := t.TempDir()
	plan, err := NewPlanner("codex").Build(task, workspace, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Artifacts) != 2 {
		t.Fatalf("artifact count = %d, want 2", len(plan.Artifacts))
	}
	for _, artifact := range plan.Artifacts {
		if !json.Valid([]byte(artifact.Content)) {
			t.Fatalf("artifact %s does not contain valid JSON", artifact.Path)
		}
		overlapped, err := directoriesOverlap(workspace, filepath.Dir(artifact.Path))
		if err != nil {
			t.Fatal(err)
		}
		if overlapped {
			t.Fatalf("artifact %s overlaps workspace", artifact.Path)
		}
		if !artifact.CreateExclusive {
			t.Fatalf("artifact %s must use exclusive creation", artifact.Path)
		}
	}
}

func TestPlanRejectsReusedArtifactDirectory(t *testing.T) {
	workspace := t.TempDir()
	artifacts := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifacts, "stale-output.json"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPlanner("codex").Build(validTask(), workspace, artifacts); err == nil {
		t.Fatal("non-empty artifact directory must be rejected")
	}
}

func validTask() domain.CodingTask {
	return domain.CodingTask{
		RunID:              "run-1",
		IssueID:            "IFAN-1",
		Title:              "Example",
		Repository:         "owner/repo",
		BaseBranch:         "dev",
		WorkingBranch:      "ifan/ifan-1-example",
		Goal:               "Implement example",
		AcceptanceCriteria: []string{"Behavior works"},
		VerifierIDs:        []string{"go-test-all"},
		Policy: domain.TaskPolicy{
			HumanApprovalRequired: true,
			MergeMethod:           "squash",
		},
		SourceRevision: "revision-1",
	}
}
