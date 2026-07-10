package localregistry

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryLoadsPathsButUsesOnlyBuiltinVerifierCommands(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	source := filepath.Join(root, "source")
	if err := os.Mkdir(origin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "registry.json")
	data := fmt.Sprintf(`{"repositories":[{"label":"repo:test-project","origin_path":%q,"source_path":%q,"base_branch":"main","verifier_ids":["fixture-go-test"]}]}`, origin, source)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !registry.HasRepository("repo:test-project") || !registry.HasVerifier("repo:test-project", "fixture-go-test") {
		t.Fatal("registered repository/verifier missing")
	}
	command := BuiltinVerifierCommands()["fixture-go-test"]
	if command.Program != "go" || len(command.Args) != 2 {
		t.Fatalf("unexpected controller command: %+v", command)
	}
}

func TestRegistryRejectsExecutableOrUnknownVerifierConfiguration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "registry.json")
	data := fmt.Sprintf(`{"repositories":[{"label":"repo:test-project","origin_path":%q,"source_path":%q,"base_branch":"main","verifier_ids":["go test ./..."]}]}`, root, root)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("registry-provided executable text must be rejected")
	}
}
