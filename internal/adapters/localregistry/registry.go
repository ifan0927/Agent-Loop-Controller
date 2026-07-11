package localregistry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
)

const CurrentVersion = 1

var referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
var githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
var githubAppProfilePattern = regexp.MustCompile(`^github-app-profile:[a-z0-9][a-z0-9._-]{0,63}$`)

type OperatorIdentityPolicy struct {
	AllowedLogins []string `json:"allowed_logins"`
}

type Repository struct {
	Owner                  string                 `json:"owner"`
	Name                   string                 `json:"name"`
	OriginPath             string                 `json:"origin_path"`
	SourcePath             string                 `json:"source_path"`
	RunRoot                string                 `json:"run_root"`
	WorktreeRoot           string                 `json:"worktree_root"`
	BaseBranch             string                 `json:"base_branch"`
	VerifierRegistryRef    string                 `json:"verifier_registry_ref"`
	VerifierIDs            []string               `json:"verifier_ids"`
	GitHubAppProfileRef    string                 `json:"github_app_profile_ref"`
	GitHubInstallationID   int64                  `json:"github_installation_id"`
	ExpectedRepositoryID   int64                  `json:"expected_repository_id"`
	OperatorIdentityPolicy OperatorIdentityPolicy `json:"operator_identity_policy"`
}

func (r Repository) CanonicalName() string {
	return strings.ToLower(r.Owner) + "/" + strings.ToLower(r.Name)
}

type File struct {
	Version      int          `json:"version"`
	Repositories []Repository `json:"repositories"`
}

type Binding struct {
	RegistryVersion         int                    `json:"registry_version"`
	RegistryDigest          string                 `json:"registry_digest"`
	RepositoryBindingDigest string                 `json:"repository_binding_digest"`
	CanonicalRepository     string                 `json:"canonical_repository"`
	OriginPath              string                 `json:"origin_path"`
	SourcePath              string                 `json:"source_path"`
	RunRoot                 string                 `json:"run_root"`
	WorktreeRoot            string                 `json:"worktree_root"`
	BaseBranch              string                 `json:"base_branch"`
	VerifierRegistryRef     string                 `json:"verifier_registry_ref"`
	VerifierIDs             []string               `json:"verifier_ids"`
	GitHubAppProfileRef     string                 `json:"github_app_profile_ref"`
	GitHubInstallationID    int64                  `json:"github_installation_id"`
	ExpectedRepositoryID    int64                  `json:"expected_repository_id"`
	OperatorIdentityPolicy  OperatorIdentityPolicy `json:"operator_identity_policy"`
}

type SanitizedBinding struct {
	RegistryVersion         int      `json:"registry_version"`
	RegistryDigest          string   `json:"registry_digest"`
	RepositoryBindingDigest string   `json:"repository_binding_digest"`
	CanonicalRepository     string   `json:"canonical_repository"`
	BaseBranch              string   `json:"base_branch"`
	VerifierRegistryRef     string   `json:"verifier_registry_ref"`
	VerifierIDs             []string `json:"verifier_ids"`
	GitHubAppProfileRef     string   `json:"github_app_profile_ref"`
	GitHubInstallationID    int64    `json:"github_installation_id"`
	ExpectedRepositoryID    int64    `json:"expected_repository_id"`
	AllowedOperatorLogins   []string `json:"allowed_operator_logins"`
}

func (b Binding) Sanitized() SanitizedBinding {
	return SanitizedBinding{RegistryVersion: b.RegistryVersion, RegistryDigest: b.RegistryDigest,
		RepositoryBindingDigest: b.RepositoryBindingDigest, CanonicalRepository: b.CanonicalRepository,
		BaseBranch: b.BaseBranch, VerifierRegistryRef: b.VerifierRegistryRef,
		VerifierIDs: append([]string(nil), b.VerifierIDs...), GitHubAppProfileRef: b.GitHubAppProfileRef,
		GitHubInstallationID: b.GitHubInstallationID, ExpectedRepositoryID: b.ExpectedRepositoryID,
		AllowedOperatorLogins: append([]string(nil), b.OperatorIdentityPolicy.AllowedLogins...)}
}

type Registry struct {
	version      int
	digest       string
	repositories map[string]Binding
}

func Load(path string) (Registry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Registry{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Registry{}, errors.New("repository registry path must not be a symlink")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Registry{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return Registry{}, fmt.Errorf("decode repository registry: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Registry{}, errors.New("registry must contain exactly one JSON value")
	}
	if file.Version != CurrentVersion {
		return Registry{}, fmt.Errorf("unsupported repository registry version %d", file.Version)
	}
	if len(file.Repositories) == 0 {
		return Registry{}, errors.New("repository registry must not be empty")
	}
	canonical, err := json.Marshal(file)
	if err != nil {
		return Registry{}, err
	}
	registryDigest := digest(canonical)
	registry := Registry{version: file.Version, digest: registryDigest, repositories: make(map[string]Binding, len(file.Repositories))}
	type ownedPath struct{ path, repository string }
	var seenPaths []ownedPath
	for _, repo := range file.Repositories {
		binding, err := validateRepository(file.Version, registryDigest, repo)
		if err != nil {
			return Registry{}, err
		}
		if _, exists := registry.repositories[binding.CanonicalRepository]; exists {
			return Registry{}, fmt.Errorf("duplicate canonical repository: %s", binding.CanonicalRepository)
		}
		for _, path := range []string{binding.OriginPath, binding.SourcePath, binding.RunRoot, binding.WorktreeRoot} {
			for _, seen := range seenPaths {
				if overlaps(path, seen.path) {
					return Registry{}, fmt.Errorf("ambiguous repository paths shared by %s and %s", seen.repository, binding.CanonicalRepository)
				}
			}
			seenPaths = append(seenPaths, ownedPath{path, binding.CanonicalRepository})
		}
		registry.repositories[binding.CanonicalRepository] = binding
	}
	return registry, nil
}

func validateRepository(version int, registryDigest string, repo Repository) (Binding, error) {
	canonical := repo.CanonicalName()
	if !validGitHubOwner(repo.Owner) || !validGitHubRepository(repo.Name) || strings.Count(canonical, "/") != 1 {
		return Binding{}, errors.New("repository entry has invalid canonical owner/name")
	}
	paths := []*string{&repo.OriginPath, &repo.SourcePath, &repo.RunRoot, &repo.WorktreeRoot}
	for _, value := range paths {
		resolved, err := canonicalDirectory(*value)
		if err != nil {
			return Binding{}, fmt.Errorf("repository %s has invalid local path: %w", canonical, err)
		}
		*value = resolved
	}
	localPaths := []string{repo.OriginPath, repo.SourcePath, repo.RunRoot, repo.WorktreeRoot}
	for i := range localPaths {
		for j := i + 1; j < len(localPaths); j++ {
			if overlaps(localPaths[i], localPaths[j]) {
				return Binding{}, fmt.Errorf("repository %s has overlapping local roots", canonical)
			}
		}
	}
	if !referencePattern.MatchString(repo.BaseBranch) || repo.VerifierRegistryRef != "builtin:v1" || !githubAppProfilePattern.MatchString(repo.GitHubAppProfileRef) || repo.GitHubInstallationID < 1 || repo.ExpectedRepositoryID < 1 {
		return Binding{}, fmt.Errorf("repository %s has incomplete authority binding", canonical)
	}
	if len(repo.VerifierIDs) == 0 || len(repo.OperatorIdentityPolicy.AllowedLogins) == 0 {
		return Binding{}, fmt.Errorf("repository %s has incomplete policy", canonical)
	}
	for _, id := range repo.VerifierIDs {
		if _, ok := BuiltinVerifierCommands()[id]; !ok {
			return Binding{}, fmt.Errorf("repository %s references unsupported controller verifier", canonical)
		}
	}
	for _, login := range repo.OperatorIdentityPolicy.AllowedLogins {
		if !validGitHubOwner(login) {
			return Binding{}, fmt.Errorf("repository %s has invalid operator identity policy", canonical)
		}
	}
	if hasDuplicate(repo.VerifierIDs) || hasDuplicateFold(repo.OperatorIdentityPolicy.AllowedLogins) {
		return Binding{}, fmt.Errorf("repository %s has duplicate policy values", canonical)
	}
	slices.Sort(repo.VerifierIDs)
	slices.Sort(repo.OperatorIdentityPolicy.AllowedLogins)
	binding := Binding{RegistryVersion: version, RegistryDigest: registryDigest, CanonicalRepository: canonical,
		OriginPath: repo.OriginPath, SourcePath: repo.SourcePath, RunRoot: repo.RunRoot, WorktreeRoot: repo.WorktreeRoot,
		BaseBranch: repo.BaseBranch, VerifierRegistryRef: repo.VerifierRegistryRef, VerifierIDs: repo.VerifierIDs,
		GitHubAppProfileRef: repo.GitHubAppProfileRef, GitHubInstallationID: repo.GitHubInstallationID,
		ExpectedRepositoryID: repo.ExpectedRepositoryID, OperatorIdentityPolicy: repo.OperatorIdentityPolicy}
	raw, _ := json.Marshal(struct {
		RegistryVersion int        `json:"registry_version"`
		Repository      Repository `json:"repository"`
	}{version, repo})
	binding.RepositoryBindingDigest = digest(raw)
	return binding, nil
}

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("path must be a non-symlink directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func overlaps(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) || func() bool {
		rel, err := filepath.Rel(b, a)
		return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}()
}

func validGitHubOwner(value string) bool {
	return githubOwnerPattern.MatchString(value) && !strings.Contains(value, "--")
}
func validGitHubRepository(value string) bool {
	return githubRepositoryPattern.MatchString(value) && value != "." && value != ".."
}
func hasDuplicate(values []string) bool {
	seen := map[string]struct{}{}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}
func hasDuplicateFold(values []string) bool {
	folded := make([]string, len(values))
	for i, value := range values {
		folded[i] = strings.ToLower(value)
	}
	return hasDuplicate(folded)
}
func digest(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }

func (r Registry) HasRepository(name string) bool {
	_, ok := r.repositories[strings.ToLower(name)]
	return ok
}
func (r Registry) HasVerifier(name, verifierID string) bool {
	repo, ok := r.repositories[strings.ToLower(name)]
	return ok && slices.Contains(repo.VerifierIDs, verifierID)
}
func (r Registry) Resolve(name string) (Binding, error) {
	repo, ok := r.repositories[strings.ToLower(name)]
	if !ok {
		return Binding{}, fmt.Errorf("unknown canonical repository: %s", name)
	}
	return repo, nil
}
func (r Registry) VerifyPersisted(binding Binding) error {
	current, err := r.Resolve(binding.CanonicalRepository)
	if err != nil {
		return err
	}
	if !sameBinding(binding, current) || binding.RegistryVersion != r.version || binding.RegistryDigest != r.digest {
		return errors.New("repository registry drift changes persisted authority")
	}
	return nil
}

func sameBinding(a, b Binding) bool {
	return a.RegistryVersion == b.RegistryVersion && a.RegistryDigest == b.RegistryDigest &&
		a.RepositoryBindingDigest == b.RepositoryBindingDigest && a.CanonicalRepository == b.CanonicalRepository &&
		a.OriginPath == b.OriginPath && a.SourcePath == b.SourcePath && a.RunRoot == b.RunRoot && a.WorktreeRoot == b.WorktreeRoot &&
		a.BaseBranch == b.BaseBranch && a.VerifierRegistryRef == b.VerifierRegistryRef && slices.Equal(a.VerifierIDs, b.VerifierIDs) &&
		a.GitHubAppProfileRef == b.GitHubAppProfileRef && a.GitHubInstallationID == b.GitHubInstallationID &&
		a.ExpectedRepositoryID == b.ExpectedRepositoryID && slices.Equal(a.OperatorIdentityPolicy.AllowedLogins, b.OperatorIdentityPolicy.AllowedLogins)
}

func BuiltinVerifierCommands() map[string]verifier.Command {
	return map[string]verifier.Command{"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}}}
}
