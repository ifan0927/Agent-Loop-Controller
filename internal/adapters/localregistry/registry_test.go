package localregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistrySelectsTwoRepositoriesDeterministically(t *testing.T) {
	root := t.TempDir()
	first := fixtureRepository(t, root, "owner", "one", 101)
	second := fixtureRepository(t, root, "OWNER", "two", 102)
	registry := loadFixture(t, root, File{Version: CurrentVersion, Repositories: []Repository{first, second}})

	one, err := registry.Resolve("Owner/One")
	if err != nil {
		t.Fatal(err)
	}
	if one.CanonicalRepository != "owner/one" || one.ExpectedRepositoryID != 101 || !registry.HasVerifier("OWNER/ONE", "fixture-go-test") {
		t.Fatalf("unexpected binding: %+v", one.Sanitized())
	}
	two, err := registry.Resolve("owner/two")
	if err != nil || two.ExpectedRepositoryID != 102 || one.RepositoryBindingDigest == two.RepositoryBindingDigest {
		t.Fatalf("second binding=%+v err=%v", two.Sanitized(), err)
	}
	if one.RegistryDigest == "" || one.RepositoryBindingDigest == "" {
		t.Fatal("versioned digests were not frozen")
	}
	legacy, err := registry.ResolveLinearLabel("owner/one")
	if err != nil || legacy.CanonicalRepository != "owner/one" {
		t.Fatalf("legacy=%+v err=%v", legacy.Sanitized(), err)
	}
}

func TestRegistryResolvesConfiguredLinearLabelAndRejectsDuplicates(t *testing.T) {
	root := t.TempDir()
	first := fixtureRepository(t, root, "owner", "one", 101)
	first.LinearLabel = "service-one"
	registry := loadFixture(t, root, File{Version: CurrentVersion, Repositories: []Repository{first}})

	binding, err := registry.ResolveLinearLabel("service-one")
	if err != nil || binding.CanonicalRepository != "owner/one" {
		t.Fatalf("binding=%+v err=%v", binding.Sanitized(), err)
	}
	if _, err := registry.ResolveLinearLabel("owner/one"); err == nil {
		t.Fatal("canonical repository resolved despite an explicit Linear label")
	}

	second := fixtureRepository(t, root, "owner", "two", 102)
	second.LinearLabel = "service-one"
	if _, err := New([]Repository{first, second}); err == nil || !strings.Contains(err.Error(), "duplicate Linear repository label") {
		t.Fatalf("duplicate label err=%v", err)
	}
}

func TestRegistryRejectsUnknownDuplicateIncompleteAndLegacyBindings(t *testing.T) {
	root := t.TempDir()
	valid := fixtureRepository(t, root, "owner", "repo", 101)
	tests := []struct {
		name string
		file any
	}{
		{"legacy", map[string]any{"repositories": []any{map[string]any{"label": "repo:test"}}}},
		{"unknown version", File{Version: 2, Repositories: []Repository{valid}}},
		{"duplicate canonical", File{Version: 1, Repositories: []Repository{valid, valid}}},
		{"incomplete authority", File{Version: 1, Repositories: []Repository{func() Repository { value := valid; value.ExpectedRepositoryID = 0; return value }()}}},
		{"executable verifier", File{Version: 1, Repositories: []Repository{func() Repository { value := valid; value.VerifierIDs = []string{"go test ./..."}; return value }()}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-")+".json")
			raw, _ := json.Marshal(test.file)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid registry accepted")
			}
		})
	}
}

func TestRegistryRejectsSymlinkAmbiguityAndAuthorityDrift(t *testing.T) {
	root := t.TempDir()
	repo := fixtureRepository(t, root, "owner", "repo", 101)
	registry := loadFixture(t, root, File{Version: 1, Repositories: []Repository{repo}})
	persisted, _ := registry.Resolve("owner/repo")

	repo.ExpectedRepositoryID = 202
	drifted := loadFixtureAt(t, filepath.Join(root, "drift.json"), File{Version: 1, Repositories: []Repository{repo}})
	if err := drifted.VerifyPersisted(persisted); err == nil {
		t.Fatal("authority-changing config drift accepted")
	}
	repo.ExpectedRepositoryID = 101
	repo.OperatorIdentityPolicy.TrustedActors[0].DatabaseID++
	actorDrift := loadFixtureAt(t, filepath.Join(root, "actor-drift.json"), File{Version: 1, Repositories: []Repository{repo}})
	if err := actorDrift.VerifyPersisted(persisted); err == nil {
		t.Fatal("trusted actor identity drift accepted")
	}
	tampered := persisted
	tampered.OriginPath = tampered.SourcePath
	if err := registry.VerifyPersisted(tampered); err == nil {
		t.Fatal("tampered persisted binding accepted with unchanged digests")
	}

	link := filepath.Join(root, "source-link")
	if err := os.Symlink(repo.SourcePath, link); err != nil {
		t.Fatal(err)
	}
	repo.SourcePath = link
	path := filepath.Join(root, "symlink.json")
	raw, _ := json.Marshal(File{Version: 1, Repositories: []Repository{repo}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("symlink-ambiguous path accepted")
	}
}

func TestRegistryRejectsNonCanonicalAndSymlinkAncestorPaths(t *testing.T) {
	root := t.TempDir()
	valid := fixtureRepository(t, root, "owner", "repo", 101)
	nonCanonical := valid
	nonCanonical.RunRoot = valid.RunRoot + "/../runs"
	path := filepath.Join(root, "noncanonical.json")
	raw, _ := json.Marshal(File{Version: 1, Repositories: []Repository{nonCanonical}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("non-canonical path accepted")
	}
	alias := filepath.Join(root, "repo-alias")
	if err := os.Symlink(filepath.Dir(valid.SourcePath), alias); err != nil {
		t.Fatal(err)
	}
	symlinkAncestor := valid
	symlinkAncestor.SourcePath = filepath.Join(alias, "source")
	path = filepath.Join(root, "ancestor-symlink.json")
	raw, _ = json.Marshal(File{Version: 1, Repositories: []Repository{symlinkAncestor}})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("symlink ancestor path accepted")
	}
}

func TestProfileDigestIsCanonicalAndScopedToResolvedRepository(t *testing.T) {
	root := t.TempDir()
	primary := fixtureRepository(t, root, "owner", "primary", 101)
	other := fixtureRepository(t, root, "owner", "other", 102)
	registry := loadFixtureAt(t, filepath.Join(root, "first.json"), File{Version: CurrentVersion, Repositories: []Repository{primary}})
	persisted, err := registry.Resolve("owner/primary")
	if err != nil {
		t.Fatal(err)
	}
	var repositoryFields map[string]any
	encodedPrimary, _ := json.Marshal(primary)
	if err := json.Unmarshal(encodedPrimary, &repositoryFields); err != nil {
		t.Fatal(err)
	}
	reorderedRaw, _ := json.Marshal(map[string]any{"repositories": []any{repositoryFields}, "version": CurrentVersion})
	reorderedPath := filepath.Join(root, "reordered.json")
	if err := os.WriteFile(reorderedPath, reorderedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	reorderedRegistry, err := Load(reorderedPath)
	if err != nil {
		t.Fatal(err)
	}
	reorderedBinding, _ := reorderedRegistry.Resolve("owner/primary")
	if reorderedBinding.ProfileDigest != persisted.ProfileDigest || reorderedBinding.ProfileSnapshotJSON != persisted.ProfileSnapshotJSON {
		t.Fatal("source JSON field order changed the canonical profile evidence")
	}
	caseChanged := primary
	caseChanged.OperatorIdentityPolicy.AllowedLogins[0] = "IFAN0927"
	caseChanged.OperatorIdentityPolicy.TrustedActors[0].Login = "Ifan0927"
	caseRegistry := loadFixtureAt(t, filepath.Join(root, "case-changed.json"), File{Version: CurrentVersion, Repositories: []Repository{caseChanged}})
	caseBinding, _ := caseRegistry.Resolve("owner/primary")
	if caseBinding.ProfileDigest != persisted.ProfileDigest {
		t.Fatal("GitHub login casing changed the canonical profile digest")
	}

	// Adding or changing another repository changes the registry file digest, but
	// must not change the immutable authority evidence for this resolved profile.
	other.ExpectedRepositoryID = 202
	expanded := loadFixtureAt(t, filepath.Join(root, "expanded.json"), File{Version: CurrentVersion, Repositories: []Repository{other, primary}})
	current, err := expanded.Resolve("owner/primary")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.RegistryDigest == current.RegistryDigest || persisted.ProfileDigest != current.ProfileDigest {
		t.Fatalf("registry/profile digests are not independently scoped: old=%+v new=%+v", persisted.Sanitized(), current.Sanitized())
	}
	if err := expanded.VerifyPersisted(persisted); err != nil {
		t.Fatalf("unrelated registry edit invalidated profile: %v", err)
	}

	var snapshot ProfileSnapshot
	if err := json.Unmarshal([]byte(persisted.ProfileSnapshotJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ProfileID != "repository-profile:owner/primary" || strings.Contains(persisted.ProfileSnapshotJSON, root) {
		t.Fatalf("profile snapshot is unstable or contains local paths: %s", persisted.ProfileSnapshotJSON)
	}
}

func TestProfileDigestRejectsTamperedCanonicalSnapshot(t *testing.T) {
	root := t.TempDir()
	repository := fixtureRepository(t, root, "owner", "repo", 101)
	registry := loadFixture(t, root, File{Version: CurrentVersion, Repositories: []Repository{repository}})
	persisted, _ := registry.Resolve("owner/repo")

	var fields map[string]any
	if err := json.Unmarshal([]byte(persisted.ProfileSnapshotJSON), &fields); err != nil {
		t.Fatal(err)
	}
	reordered, _ := json.Marshal(fields)
	if digest(reordered) == persisted.ProfileDigest && string(reordered) != persisted.ProfileSnapshotJSON {
		t.Fatal("test did not produce a different JSON field order")
	}
	persisted.ProfileSnapshotJSON = string(reordered)
	if err := registry.VerifyPersisted(persisted); err == nil {
		t.Fatal("non-canonical persisted profile snapshot accepted")
	}
}

func TestRegistryRejectsInvalidGitHubIdentities(t *testing.T) {
	root := t.TempDir()
	base := fixtureRepository(t, root, "owner", "repo", 101)
	tests := []Repository{
		func() Repository { value := base; value.Owner = "owner:admin"; return value }(),
		func() Repository { value := base; value.Name = "repo:name"; return value }(),
		func() Repository {
			value := base
			value.OperatorIdentityPolicy.AllowedLogins = []string{strings.Repeat("a", 40)}
			return value
		}(),
		func() Repository {
			value := base
			value.GitHubAppProfileRef = "ghp_fixtureSecretMaterial"
			return value
		}(),
	}
	for index, repo := range tests {
		path := filepath.Join(root, fmt.Sprintf("invalid-identity-%d.json", index))
		raw, _ := json.Marshal(File{Version: 1, Repositories: []Repository{repo}})
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("invalid GitHub identity accepted")
		}
	}
}

func TestRegistryAcceptsExplicitGitHubRemoteBindingAndRejectsWrongOrUnsafeRemote(t *testing.T) {
	root := t.TempDir()
	base := fixtureRepository(t, root, "owner", "repo", 101)
	remote := base
	remote.OriginPath = ""
	remote.OriginURL = "https://github.com/OWNER/REPO.git"
	registry := loadFixture(t, root, File{Version: CurrentVersion, Repositories: []Repository{remote}})
	binding, err := registry.Resolve("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if binding.OriginPath != "https://github.com/owner/repo.git" {
		t.Fatalf("remote origin was not canonically bound: %q", binding.OriginPath)
	}
	legacy := base
	legacy.OriginPath = "git@github.com:OWNER/REPO.git"
	legacyRegistry := loadFixtureAt(t, filepath.Join(root, "legacy-remote.json"), File{Version: CurrentVersion, Repositories: []Repository{legacy}})
	legacyBinding, err := legacyRegistry.Resolve("owner/repo")
	if err != nil || legacyBinding.OriginPath != "git@github.com:owner/repo.git" {
		t.Fatalf("legacy remote origin was not canonically bound: binding=%q err=%v", legacyBinding.OriginPath, err)
	}

	tests := []struct {
		name string
		repo Repository
	}{
		{"wrong repository", func() Repository { value := remote; value.OriginURL = "git@github.com:owner/other.git"; return value }()},
		{"unsafe host", func() Repository {
			value := remote
			value.OriginURL = "https://github.example.invalid/owner/repo.git"
			return value
		}()},
		{"HTTPS credential", func() Repository {
			value := remote
			value.OriginURL = "https://token@github.com/owner/repo.git"
			return value
		}()},
		{"SSH credential", func() Repository {
			value := remote
			value.OriginURL = "ssh://git:token@github.com/owner/repo.git"
			return value
		}()},
		{"both spellings", func() Repository { value := remote; value.OriginPath = base.OriginPath; return value }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-")+".json")
			raw, _ := json.Marshal(File{Version: CurrentVersion, Repositories: []Repository{test.repo}})
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("unsafe or mismatched remote binding accepted")
			}
		})
	}
}

func TestRegistryErrorsDoNotEchoSecretValues(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "registry.json")
	secret := "super-secret-private-key-material"
	raw := `{"version":1,"private_key":"` + secret + `","repositories":[]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("secret-bearing invalid registry error=%v", err)
	}
}

func fixtureRepository(t *testing.T, root, owner, name string, id int64) Repository {
	t.Helper()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	root = canonicalRoot
	base := filepath.Join(root, strings.ToLower(owner+"-"+name))
	paths := make([]string, 4)
	for i, part := range []string{"origin", "source", "runs", "worktrees"} {
		paths[i] = filepath.Join(base, part)
		if err := os.MkdirAll(paths[i], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return Repository{Owner: owner, Name: name, OriginPath: paths[0], SourcePath: paths[1], RunRoot: paths[2], WorktreeRoot: paths[3],
		BaseBranch: "main", VerifierRegistryRef: "builtin:v1", VerifierIDs: []string{"fixture-go-test"},
		GitHubAppProfileRef: "github-app-profile:fixture", GitHubAppID: 11, GitHubInstallationID: 22, ExpectedRepositoryID: id,
		OperatorIdentityPolicy: OperatorIdentityPolicy{AllowedLogins: []string{"ifan0927"}, TrustedActors: []TrustedActorIdentity{{DatabaseID: 33, NodeID: "MDQ6VXNlcjMz", Login: "ifan0927", Type: "User"}}}}
}

func loadFixture(t *testing.T, root string, file File) Registry {
	t.Helper()
	return loadFixtureAt(t, filepath.Join(root, "registry.json"), file)
}

func loadFixtureAt(t *testing.T, path string, file File) Registry {
	t.Helper()
	raw, _ := json.Marshal(file)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
