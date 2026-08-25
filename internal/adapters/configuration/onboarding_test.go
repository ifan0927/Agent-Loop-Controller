package configuration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localregistry"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

func TestPrivateOnboardingInputReplaysWithoutProjectingSourcePath(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	input := domain.ExistingRepositoryOnboardingInput(domain.ExistingCheckoutOnboardingInput{SourcePath: filepath.Join(root, "checkout"), CanonicalRepository: "owner/repository", GitHubAppProfileRef: "github-app-profile:primary", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, LinearLabelSlug: "repository"})
	digest := application.OnboardingPrivateInputDigest(input)
	if err := files.Put("onboarding-private-input", input, digest); err != nil {
		t.Fatal(err)
	}
	if err := files.Put("onboarding-private-input", input, digest); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "authority", "onboarding", "onboarding-private-input", "input.json"))
	if err != nil || bytes.Contains(raw, []byte(`"kind"`)) {
		t.Fatalf("legacy private leaf changed: %q err=%v", raw, err)
	}
	loaded, err := files.Get("onboarding-private-input", digest)
	if err != nil || loaded.Kind != domain.OnboardingExistingCheckout || loaded.SourcePath != input.SourcePath || loaded.CanonicalRepository != input.CanonicalRepository {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	conflict := input
	conflict.BaseBranch = "release"
	if err := files.Put("onboarding-private-input", conflict, application.OnboardingPrivateInputDigest(conflict)); err == nil {
		t.Fatal("conflicting private authority was accepted")
	}
}

func TestEmptyOnboardingInputDerivesSourceAndPersistsClosedKind(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	if err := os.WriteFile(configPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := files.DeriveManagedSource("owner/repository")
	if err != nil || source != filepath.Join(root, "repositories", "owner--repository", "source") {
		t.Fatalf("source=%q err=%v", source, err)
	}
	input := domain.ManagedEmptyRepositoryOnboardingInput(domain.EmptyRepositoryOnboardingInput{CanonicalRepository: "owner/repository", GitHubAppProfileRef: "github-app-profile:primary", BaseBranch: "main", VerifierIDs: []string{"fixture-go-test"}, LinearLabelSlug: "repository"}, source)
	digest := application.OnboardingPrivateInputDigest(input)
	if err := files.Put("onboarding-empty-input", input, digest); err != nil {
		t.Fatal(err)
	}
	loaded, err := files.Get("onboarding-empty-input", digest)
	if err != nil || loaded.Kind != domain.OnboardingEmptyRepository || loaded.SourcePath != source {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestRepositoryAdditionPreservesUnrelatedConfigurationAuthority(t *testing.T) {
	root := canonicalTempDirectory(t)
	configPath := filepath.Join(root, "controller.json")
	keyPath := filepath.Join(root, "app.pem")
	if err := os.WriteFile(keyPath, []byte("fixture-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{
		"version":             5,
		"controller":          map[string]any{"database_path": filepath.Join(root, "controller.db"), "codex_binary": "codex", "run_timeout": "30m", "operator": map[string]any{"database_id": 7, "node_id": "USER_7", "login": "operator", "type": "User"}},
		"linear":              map[string]any{"api_url": "https://api.linear.app/graphql", "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN", "authorization_scheme": "bearer", "team_key": "IFAN", "http_timeout": "2s", "max_response_bytes": 4096, "label_page_size": 10, "max_label_pages": 1},
		"repositories":        []any{},
		"github_app_profiles": []map[string]any{{"id": "github-app-profile:primary", "config": map[string]any{"api_base_url": "https://api.github.com", "graphql_url": "https://api.github.com/graphql", "app_id": 11, "installation_id": 12, "repository_owner": "owner", "repository_name": "repository", "repository_id": 13, "private_key_file": keyPath, "http_timeout": "2s", "token_refresh_skew": "5m", "api_version": "2022-11-28"}}},
		"automation":          map[string]any{"linear_todo_admission": map[string]any{"enabled": false, "credential_source_ref": "secret://env/IFAN_LOOP_LINEAR_TOKEN"}},
	}
	base, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "source"), filepath.Join(root, "runs"), filepath.Join(root, "worktrees")}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repository := localregistry.Repository{Owner: "owner", Name: "repository", LinearLabel: "repo:repository", OriginURL: "https://github.com/owner/repository.git", SourcePath: paths[0], RunRoot: paths[1], WorktreeRoot: paths[2], BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"}, GitHubAppProfileRef: "github-app-profile:primary", GitHubAppID: 11, GitHubInstallationID: 12, ExpectedRepositoryID: 13, OperatorIdentityPolicy: localregistry.OperatorIdentityPolicy{AllowedLogins: []string{"operator"}, TrustedActors: []localregistry.TrustedActorIdentity{{DatabaseID: 7, NodeID: "USER_7", Login: "operator", Type: "User"}}}}
	if _, err := localregistry.New([]localregistry.Repository{repository}); err != nil {
		t.Fatalf("repository fixture: %v", err)
	}
	files, err := NewFiles(configPath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, profile, err := files.MaterializeRepositoryAddition(base, repository)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := files.ValidateRepositoryAdditionCandidate(base, candidate, profile)
	if err != nil || len(validated.Repositories) != 1 || validated.Repositories["owner/repository"].RepositoryBindingDigest != profile.RepositoryBindingDigest {
		t.Fatalf("validated=%+v profile=%+v err=%v", validated, profile, err)
	}
	if !bytes.Contains(candidate, []byte("IFAN_LOOP_LINEAR_TOKEN")) || bytes.Contains(base, []byte("owner/repository")) {
		t.Fatal("repository addition changed or pre-existed in unrelated authority")
	}
}

func TestOnboardingRootsArePrivateOwnedAndExactReplayOnly(t *testing.T) {
	root := canonicalTempDirectory(t)
	repositoryRoot := filepath.Join(root, "repositories", "owner--repository")
	runRoot := filepath.Join(repositoryRoot, "runs")
	worktreeRoot := filepath.Join(repositoryRoot, "worktrees")
	first, err := EnsureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, "onboarding-root-owner", "owner/repository")
	if err != nil {
		t.Fatal(err)
	}
	wantLegacyDigest := applicationDigest("onboarding-roots-v1", "onboarding-root-owner", "owner/repository", repositoryRoot, runRoot, worktreeRoot)
	if first.EvidenceDigest != wantLegacyDigest {
		t.Fatalf("legacy evidence digest=%q want=%q", first.EvidenceDigest, wantLegacyDigest)
	}
	replayed, err := EnsureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, "onboarding-root-owner", "owner/repository")
	if err != nil || replayed.EvidenceDigest != first.EvidenceDigest {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	for _, path := range []string{repositoryRoot, runRoot, worktreeRoot} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("path=%s mode=%v err=%v", path, info.Mode(), err)
		}
	}
	if _, err := EnsureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, "onboarding-other-owner", "owner/repository"); err == nil {
		t.Fatal("root ownership conflict was accepted")
	}
}

func TestEmptyOnboardingRootsBindManagedSourceWithoutCreatingCheckout(t *testing.T) {
	root := canonicalTempDirectory(t)
	repositoryRoot := filepath.Join(root, "repositories", "owner--repository")
	sourcePath := filepath.Join(repositoryRoot, "source")
	runRoot := filepath.Join(repositoryRoot, "runs")
	worktreeRoot := filepath.Join(repositoryRoot, "worktrees")
	if err := InspectEmptyOnboardingPaths(repositoryRoot, sourcePath, runRoot, worktreeRoot, nil); err != nil {
		t.Fatal(err)
	}
	created, err := EnsureEmptyOnboardingRoots(repositoryRoot, sourcePath, runRoot, worktreeRoot, "onboarding-empty-roots", "owner/repository")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyEmptyOnboardingRoots(repositoryRoot, sourcePath, runRoot, worktreeRoot, "onboarding-empty-roots", "owner/repository")
	if err != nil || verified.EvidenceDigest != created.EvidenceDigest {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
	if _, err := os.Lstat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed source was created before its step: %v", err)
	}
}
