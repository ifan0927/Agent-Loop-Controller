package application

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeArtifactsUsesExclusiveCreateAndPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.json")
	spec := ArtifactSpec{Path: path, Content: "{}", Mode: 0o600, CreateExclusive: true}
	if err := MaterializeArtifacts([]ArtifactSpec{spec}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if err := MaterializeArtifacts([]ArtifactSpec{spec}); err == nil {
		t.Fatal("existing artifact must be rejected")
	}
}
