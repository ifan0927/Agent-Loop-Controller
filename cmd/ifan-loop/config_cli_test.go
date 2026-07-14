package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigPathUsesApplicationSupport(t *testing.T) {
	home := filepath.Join(t.TempDir(), "operator")
	withTestHome(t, home)

	path, err := defaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Library", "Application Support", "agent-loop-controller", "controller.json")
	if path != want {
		t.Fatalf("path=%q want=%q", path, want)
	}
}

func TestResolveConfigPathPreservesExplicitOverride(t *testing.T) {
	withTestHome(t, t.TempDir())
	path, err := resolveConfigPath("/tmp/operator-controller.json")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/operator-controller.json" {
		t.Fatalf("path=%q", path)
	}
}

func TestConfigInitCreatesExclusiveSecretFreeV3Template(t *testing.T) {
	home := resolvedTempDir(t)
	withTestHome(t, home)

	output, err := captureConfigOutput(func() error { return configCommand([]string{"init"}) })
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "Library", "Application Support", "agent-loop-controller", "controller.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%#o want=0600", info.Mode().Perm())
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode=%#o want=0700", directory.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var template configTemplate
	if err := json.Unmarshal(raw, &template); err != nil {
		t.Fatalf("template JSON: %v", err)
	}
	if template.Version != 3 || len(template.GitHubAppProfiles) != 0 || len(template.Repositories) != 0 || template.Automation.LinearTodoAdmission.Enabled {
		t.Fatalf("unexpected template: %#v", template)
	}
	if template.Controller.DatabasePath != filepath.Join(filepath.Dir(path), "controller.db") {
		t.Fatalf("database path=%q", template.Controller.DatabasePath)
	}
	for _, forbidden := range []string{"private_key", "BEGIN PRIVATE KEY", "github_pat_", "ghp_"} {
		if strings.Contains(string(raw), forbidden) || strings.Contains(output, forbidden) {
			t.Fatalf("template or output contains secret material marker %q", forbidden)
		}
	}
	for _, required := range []string{`"created": true`, `"setup_required": true`, `"secret_free": true`} {
		if !strings.Contains(output, required) {
			t.Fatalf("init output missing %s: %s", required, output)
		}
	}

	before := string(raw)
	if _, err := captureConfigOutput(func() error { return configCommand([]string{"init"}) }); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second init error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatal("exclusive init changed existing configuration")
	}
}

func TestConfigInitRefusesSymlinkedConfigurationDirectory(t *testing.T) {
	root := resolvedTempDir(t)
	configRoot := filepath.Join(root, "config-root")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, configRoot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configRoot, "controller.json")
	if _, err := captureConfigOutput(func() error { return configCommand([]string{"init", "--config", path}) }); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink directory error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "controller.json")); !os.IsNotExist(err) {
		t.Fatalf("configuration was created through symlink, stat error=%v", err)
	}
}

func TestConfigInitRefusesSymlinkedConfigurationAncestor(t *testing.T) {
	root := resolvedTempDir(t)
	target := filepath.Join(root, "target")
	ancestor := filepath.Join(root, "linked-ancestor")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ancestor); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ancestor, "nested", "controller.json")
	if _, err := captureConfigOutput(func() error { return configCommand([]string{"init", "--config", path}) }); err == nil || !strings.Contains(err.Error(), "must not include symbolic links") {
		t.Fatalf("symlink ancestor error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "nested", "controller.json")); !os.IsNotExist(err) {
		t.Fatalf("configuration was created through symlinked ancestor, stat error=%v", err)
	}
}

func withTestHome(t *testing.T, home string) {
	t.Helper()
	original := userHomeDirectory
	userHomeDirectory = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDirectory = original })
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func captureConfigOutput(run func() error) (string, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return "", err
	}
	original := os.Stdout
	os.Stdout = write
	err = run()
	closeErr := write.Close()
	os.Stdout = original
	output, readErr := io.ReadAll(read)
	read.Close()
	if err != nil {
		return string(output), err
	}
	if closeErr != nil {
		return string(output), closeErr
	}
	return string(output), readErr
}
