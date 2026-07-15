package localregistry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
)

const CurrentVersion = 1
const ProfileSnapshotVersion = 1

var referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
var githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
var githubAppProfilePattern = regexp.MustCompile(`^github-app-profile:[a-z0-9][a-z0-9._-]{0,63}$`)
var linearLabelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type OperatorIdentityPolicy struct {
	AllowedLogins []string               `json:"allowed_logins"`
	TrustedActors []TrustedActorIdentity `json:"trusted_actors"`
}

type TrustedActorIdentity struct {
	DatabaseID int64  `json:"database_id"`
	NodeID     string `json:"node_id"`
	Login      string `json:"login"`
	Type       string `json:"type"`
}

type Repository struct {
	Owner       string `json:"owner"`
	Name        string `json:"name"`
	LinearLabel string `json:"linear_label,omitempty"`
	// OriginURL is the preferred explicit GitHub remote binding. OriginPath is
	// retained for existing local bare-repository fixtures and is also accepted
	// as a legacy spelling of an HTTPS or SSH GitHub remote.
	OriginURL              string                 `json:"origin_url,omitempty"`
	OriginPath             string                 `json:"origin_path"`
	SourcePath             string                 `json:"source_path"`
	RunRoot                string                 `json:"run_root"`
	WorktreeRoot           string                 `json:"worktree_root"`
	BaseBranch             string                 `json:"base_branch"`
	VerifierRegistryRef    string                 `json:"verifier_registry_ref"`
	VerifierIDs            []string               `json:"verifier_ids"`
	GitHubAppProfileRef    string                 `json:"github_app_profile_ref"`
	GitHubAppID            int64                  `json:"github_app_id"`
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
	ProfileID               string                 `json:"profile_id"`
	ProfileSnapshotVersion  int                    `json:"profile_snapshot_version"`
	ProfileDigest           string                 `json:"profile_digest"`
	ProfileSnapshotJSON     string                 `json:"-"`
	RegistryVersion         int                    `json:"registry_version"`
	RegistryDigest          string                 `json:"registry_digest"`
	RepositoryBindingDigest string                 `json:"repository_binding_digest"`
	CanonicalRepository     string                 `json:"canonical_repository"`
	LinearLabel             string                 `json:"linear_label"`
	OriginPath              string                 `json:"origin_path"`
	SourcePath              string                 `json:"source_path"`
	RunRoot                 string                 `json:"run_root"`
	WorktreeRoot            string                 `json:"worktree_root"`
	BaseBranch              string                 `json:"base_branch"`
	VerifierRegistryRef     string                 `json:"verifier_registry_ref"`
	VerifierIDs             []string               `json:"verifier_ids"`
	GitHubAppProfileRef     string                 `json:"github_app_profile_ref"`
	GitHubAppID             int64                  `json:"github_app_id"`
	GitHubInstallationID    int64                  `json:"github_installation_id"`
	ExpectedRepositoryID    int64                  `json:"expected_repository_id"`
	OperatorIdentityPolicy  OperatorIdentityPolicy `json:"operator_identity_policy"`
}

// ProfileSnapshot is the canonical, credential-free authority evidence frozen for a run.
// Registry-wide metadata is intentionally excluded so unrelated registry edits do not
// invalidate this repository profile. Local ownership paths remain separately bound.
type ProfileSnapshot struct {
	Version                int                    `json:"version"`
	ProfileID              string                 `json:"profile_id"`
	CanonicalRepository    string                 `json:"canonical_repository"`
	LinearLabel            string                 `json:"linear_label"`
	BaseBranch             string                 `json:"base_branch"`
	VerifierRegistryRef    string                 `json:"verifier_registry_ref"`
	VerifierIDs            []string               `json:"verifier_ids"`
	GitHubAppProfileRef    string                 `json:"github_app_profile_ref"`
	GitHubAppID            int64                  `json:"github_app_id"`
	GitHubInstallationID   int64                  `json:"github_installation_id"`
	ExpectedRepositoryID   int64                  `json:"expected_repository_id"`
	OperatorIdentityPolicy OperatorIdentityPolicy `json:"operator_identity_policy"`
}

type SanitizedBinding struct {
	ProfileID               string                 `json:"profile_id"`
	ProfileSnapshotVersion  int                    `json:"profile_snapshot_version"`
	ProfileDigest           string                 `json:"profile_digest"`
	RegistryVersion         int                    `json:"registry_version"`
	RegistryDigest          string                 `json:"registry_digest"`
	RepositoryBindingDigest string                 `json:"repository_binding_digest"`
	CanonicalRepository     string                 `json:"canonical_repository"`
	LinearLabel             string                 `json:"linear_label"`
	BaseBranch              string                 `json:"base_branch"`
	VerifierRegistryRef     string                 `json:"verifier_registry_ref"`
	VerifierIDs             []string               `json:"verifier_ids"`
	GitHubAppProfileRef     string                 `json:"github_app_profile_ref"`
	GitHubAppID             int64                  `json:"github_app_id"`
	GitHubInstallationID    int64                  `json:"github_installation_id"`
	ExpectedRepositoryID    int64                  `json:"expected_repository_id"`
	AllowedOperatorLogins   []string               `json:"allowed_operator_logins"`
	TrustedOperatorActors   []TrustedActorIdentity `json:"trusted_operator_actors"`
}

func (b Binding) Sanitized() SanitizedBinding {
	return SanitizedBinding{ProfileID: b.ProfileID, ProfileSnapshotVersion: b.ProfileSnapshotVersion, ProfileDigest: b.ProfileDigest,
		RegistryVersion: b.RegistryVersion, RegistryDigest: b.RegistryDigest,
		RepositoryBindingDigest: b.RepositoryBindingDigest, CanonicalRepository: b.CanonicalRepository, LinearLabel: b.LinearLabel,
		BaseBranch: b.BaseBranch, VerifierRegistryRef: b.VerifierRegistryRef,
		VerifierIDs: append([]string(nil), b.VerifierIDs...), GitHubAppProfileRef: b.GitHubAppProfileRef,
		GitHubAppID: b.GitHubAppID, GitHubInstallationID: b.GitHubInstallationID, ExpectedRepositoryID: b.ExpectedRepositoryID,
		AllowedOperatorLogins: append([]string(nil), b.OperatorIdentityPolicy.AllowedLogins...),
		TrustedOperatorActors: append([]TrustedActorIdentity(nil), b.OperatorIdentityPolicy.TrustedActors...)}
}

type Registry struct {
	version      int
	digest       string
	repositories map[string]Binding
	linearLabels map[string]Binding
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
	return build(file)
}

// New validates an inline repository collection using the same authority and
// local-ownership rules as a file-backed registry. It is intended for a
// controller configuration that owns the registry data directly.
func New(repositories []Repository) (Registry, error) {
	return build(File{Version: CurrentVersion, Repositories: repositories})
}

func build(file File) (Registry, error) {
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
	registry := Registry{version: file.Version, digest: registryDigest, repositories: make(map[string]Binding, len(file.Repositories)), linearLabels: make(map[string]Binding, len(file.Repositories))}
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
		if _, exists := registry.linearLabels[binding.LinearLabel]; exists {
			return Registry{}, fmt.Errorf("duplicate Linear repository label: %s", binding.LinearLabel)
		}
		paths := []string{binding.SourcePath, binding.RunRoot, binding.WorktreeRoot}
		if filepath.IsAbs(binding.OriginPath) {
			paths = append(paths, binding.OriginPath)
		}
		for _, path := range paths {
			for _, seen := range seenPaths {
				if overlaps(path, seen.path) {
					return Registry{}, fmt.Errorf("ambiguous repository paths shared by %s and %s", seen.repository, binding.CanonicalRepository)
				}
			}
			seenPaths = append(seenPaths, ownedPath{path, binding.CanonicalRepository})
		}
		registry.repositories[binding.CanonicalRepository] = binding
		registry.linearLabels[binding.LinearLabel] = binding
	}
	return registry, nil
}

func validateRepository(version int, registryDigest string, repo Repository) (Binding, error) {
	canonical := repo.CanonicalName()
	if !validGitHubOwner(repo.Owner) || !validGitHubRepository(repo.Name) || strings.Count(canonical, "/") != 1 {
		return Binding{}, errors.New("repository entry has invalid canonical owner/name")
	}
	linearLabel := repo.LinearLabel
	if linearLabel == "" {
		linearLabel = canonical
	} else if !linearLabelPattern.MatchString(linearLabel) {
		return Binding{}, fmt.Errorf("repository %s has invalid Linear repository label", canonical)
	}
	if strings.TrimSpace(repo.OriginURL) != "" && strings.TrimSpace(repo.OriginPath) != "" {
		return Binding{}, fmt.Errorf("repository %s configures both origin_url and origin_path", canonical)
	}
	origin := repo.OriginPath
	if strings.TrimSpace(repo.OriginURL) != "" {
		origin = repo.OriginURL
	}
	canonicalOrigin, localOrigin, err := canonicalOriginBinding(origin)
	if err != nil {
		return Binding{}, fmt.Errorf("repository %s has invalid origin binding: %w", canonical, err)
	}
	if !localOrigin {
		remoteOwner, remoteName, err := githubRemoteOwnerName(canonicalOrigin)
		if err != nil || !strings.EqualFold(remoteOwner, repo.Owner) || !strings.EqualFold(remoteName, repo.Name) {
			return Binding{}, fmt.Errorf("repository %s origin binding targets a different GitHub repository", canonical)
		}
	}
	repo.OriginPath = canonicalOrigin
	repo.OriginURL = ""
	paths := []*string{&repo.SourcePath, &repo.RunRoot, &repo.WorktreeRoot}
	for _, value := range paths {
		resolved, err := canonicalDirectory(*value)
		if err != nil {
			return Binding{}, fmt.Errorf("repository %s has invalid local path: %w", canonical, err)
		}
		*value = resolved
	}
	localPaths := []string{repo.SourcePath, repo.RunRoot, repo.WorktreeRoot}
	if localOrigin {
		localPaths = append(localPaths, repo.OriginPath)
	}
	for i := range localPaths {
		for j := i + 1; j < len(localPaths); j++ {
			if overlaps(localPaths[i], localPaths[j]) {
				return Binding{}, fmt.Errorf("repository %s has overlapping local roots", canonical)
			}
		}
	}
	if !referencePattern.MatchString(repo.BaseBranch) || repo.VerifierRegistryRef != "builtin:v1" || !githubAppProfilePattern.MatchString(repo.GitHubAppProfileRef) || repo.GitHubAppID < 1 || repo.GitHubInstallationID < 1 || repo.ExpectedRepositoryID < 1 {
		return Binding{}, fmt.Errorf("repository %s has incomplete authority binding", canonical)
	}
	if len(repo.VerifierIDs) == 0 || len(repo.OperatorIdentityPolicy.AllowedLogins) == 0 || len(repo.OperatorIdentityPolicy.TrustedActors) == 0 {
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
	for _, actor := range repo.OperatorIdentityPolicy.TrustedActors {
		if actor.DatabaseID < 1 || actor.NodeID == "" || !validGitHubOwner(actor.Login) || actor.Type != "User" {
			return Binding{}, fmt.Errorf("repository %s has invalid trusted operator identity", canonical)
		}
	}
	actorLogins := make([]string, len(repo.OperatorIdentityPolicy.TrustedActors))
	actorNodes := make([]string, len(repo.OperatorIdentityPolicy.TrustedActors))
	actorDatabaseIDs := make(map[int64]struct{}, len(repo.OperatorIdentityPolicy.TrustedActors))
	for i, actor := range repo.OperatorIdentityPolicy.TrustedActors {
		actorLogins[i], actorNodes[i] = actor.Login, actor.NodeID
		if _, exists := actorDatabaseIDs[actor.DatabaseID]; exists {
			return Binding{}, fmt.Errorf("repository %s has duplicate trusted operator identity", canonical)
		}
		actorDatabaseIDs[actor.DatabaseID] = struct{}{}
	}
	if hasDuplicateFold(actorLogins) || hasDuplicate(actorNodes) {
		return Binding{}, fmt.Errorf("repository %s has duplicate trusted operator identity", canonical)
	}
	for _, login := range repo.OperatorIdentityPolicy.AllowedLogins {
		if !slices.ContainsFunc(actorLogins, func(actorLogin string) bool { return strings.EqualFold(login, actorLogin) }) {
			return Binding{}, fmt.Errorf("repository %s operator login lacks immutable trusted identity", canonical)
		}
	}
	if hasDuplicate(repo.VerifierIDs) || hasDuplicateFold(repo.OperatorIdentityPolicy.AllowedLogins) {
		return Binding{}, fmt.Errorf("repository %s has duplicate policy values", canonical)
	}
	for i := range repo.OperatorIdentityPolicy.AllowedLogins {
		repo.OperatorIdentityPolicy.AllowedLogins[i] = strings.ToLower(repo.OperatorIdentityPolicy.AllowedLogins[i])
	}
	for i := range repo.OperatorIdentityPolicy.TrustedActors {
		repo.OperatorIdentityPolicy.TrustedActors[i].Login = strings.ToLower(repo.OperatorIdentityPolicy.TrustedActors[i].Login)
	}
	slices.Sort(repo.VerifierIDs)
	slices.Sort(repo.OperatorIdentityPolicy.AllowedLogins)
	slices.SortFunc(repo.OperatorIdentityPolicy.TrustedActors, func(a, b TrustedActorIdentity) int { return strings.Compare(a.NodeID, b.NodeID) })
	profileID := "repository-profile:" + canonical
	profile := ProfileSnapshot{Version: ProfileSnapshotVersion, ProfileID: profileID, CanonicalRepository: canonical, LinearLabel: linearLabel,
		BaseBranch: repo.BaseBranch, VerifierRegistryRef: repo.VerifierRegistryRef, VerifierIDs: append([]string(nil), repo.VerifierIDs...),
		GitHubAppProfileRef: repo.GitHubAppProfileRef, GitHubAppID: repo.GitHubAppID, GitHubInstallationID: repo.GitHubInstallationID,
		ExpectedRepositoryID: repo.ExpectedRepositoryID, OperatorIdentityPolicy: repo.OperatorIdentityPolicy}
	profileRaw, _ := json.Marshal(profile)
	binding := Binding{ProfileID: profileID, ProfileSnapshotVersion: ProfileSnapshotVersion, ProfileDigest: digest(profileRaw), ProfileSnapshotJSON: string(profileRaw),
		RegistryVersion: version, RegistryDigest: registryDigest, CanonicalRepository: canonical, LinearLabel: linearLabel,
		OriginPath: repo.OriginPath, SourcePath: repo.SourcePath, RunRoot: repo.RunRoot, WorktreeRoot: repo.WorktreeRoot,
		BaseBranch: repo.BaseBranch, VerifierRegistryRef: repo.VerifierRegistryRef, VerifierIDs: repo.VerifierIDs,
		GitHubAppProfileRef: repo.GitHubAppProfileRef, GitHubAppID: repo.GitHubAppID, GitHubInstallationID: repo.GitHubInstallationID,
		ExpectedRepositoryID: repo.ExpectedRepositoryID, OperatorIdentityPolicy: repo.OperatorIdentityPolicy}
	raw, _ := json.Marshal(struct {
		RegistryVersion int        `json:"registry_version"`
		Repository      Repository `json:"repository"`
	}{version, repo})
	binding.RepositoryBindingDigest = digest(raw)
	return binding, nil
}

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("path must be absolute and canonical")
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
	if resolved != path {
		return "", errors.New("path must not traverse a symlink")
	}
	return path, nil
}

// canonicalOriginBinding accepts either an existing local, non-symlink
// directory (the fixture-compatible form) or an explicit GitHub SSH/HTTPS
// remote. Remote bindings are normalized without carrying credentials.
func canonicalOriginBinding(value string) (canonical string, local bool, err error) {
	value = strings.TrimSpace(value)
	if filepath.IsAbs(value) {
		canonical, err := canonicalDirectory(value)
		return canonical, true, err
	}
	canonical, err = canonicalGitHubRemoteURL(value)
	return canonical, false, err
}

func canonicalGitHubRemoteURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "git@github.com:") {
		owner, name, err := githubRemotePath(strings.TrimPrefix(value, "git@github.com:"))
		if err != nil {
			return "", err
		}
		return "git@github.com:" + strings.ToLower(owner) + "/" + strings.ToLower(name) + ".git", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", errors.New("remote URL is malformed")
	}
	if (parsed.Scheme != "https" && parsed.Scheme != "ssh") || !strings.EqualFold(parsed.Host, "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", errors.New("remote URL must be a credential-free github.com HTTPS or SSH URL")
	}
	if parsed.Scheme == "https" && parsed.User != nil {
		return "", errors.New("HTTPS remote URL must not contain credentials")
	}
	if parsed.Scheme == "ssh" {
		if parsed.User == nil || parsed.User.Username() != "git" {
			return "", errors.New("SSH remote URL must use the git user")
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return "", errors.New("SSH remote URL must not contain credentials")
		}
	}
	owner, name, err := githubRemotePath(strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "https" {
		return "https://github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(name) + ".git", nil
	}
	return "ssh://git@github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(name) + ".git", nil
}

func githubRemoteOwnerName(value string) (string, string, error) {
	if strings.HasPrefix(value, "git@github.com:") {
		return githubRemotePath(strings.TrimPrefix(value, "git@github.com:"))
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", "", err
	}
	return githubRemotePath(strings.TrimPrefix(parsed.EscapedPath(), "/"))
}

func githubRemotePath(value string) (string, string, error) {
	if !strings.HasSuffix(value, ".git") {
		return "", "", errors.New("remote URL path must end in .git")
	}
	parts := strings.Split(strings.TrimSuffix(value, ".git"), "/")
	if len(parts) != 2 || !validGitHubOwner(parts[0]) || !validGitHubRepository(parts[1]) {
		return "", "", errors.New("remote URL path must contain one valid owner and repository")
	}
	return parts[0], parts[1], nil
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

// ResolveLinearLabel resolves the controller-configured short label from a
// Linear issue. A missing linear_label retains the legacy canonical name.
func (r Registry) ResolveLinearLabel(label string) (Binding, error) {
	repo, ok := r.linearLabels[label]
	if !ok {
		return Binding{}, fmt.Errorf("unknown Linear repository label: %s", label)
	}
	return repo, nil
}

// Bindings returns a stable copy of every configured repository binding.
func (r Registry) Bindings() []Binding {
	keys := make([]string, 0, len(r.repositories))
	for key := range r.repositories {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	bindings := make([]Binding, 0, len(keys))
	for _, key := range keys {
		binding := r.repositories[key]
		binding.VerifierIDs = append([]string(nil), binding.VerifierIDs...)
		binding.OperatorIdentityPolicy.AllowedLogins = append([]string(nil), binding.OperatorIdentityPolicy.AllowedLogins...)
		binding.OperatorIdentityPolicy.TrustedActors = append([]TrustedActorIdentity(nil), binding.OperatorIdentityPolicy.TrustedActors...)
		bindings = append(bindings, binding)
	}
	return bindings
}
func (r Registry) VerifyPersisted(binding Binding) error {
	current, err := r.Resolve(binding.CanonicalRepository)
	if err != nil {
		return err
	}
	if binding.ProfileSnapshotVersion != ProfileSnapshotVersion || binding.ProfileID == "" || binding.ProfileDigest == "" {
		return errors.New("persisted repository profile evidence is legacy-insufficient")
	}
	if digest([]byte(binding.ProfileSnapshotJSON)) != binding.ProfileDigest || binding.ProfileSnapshotJSON != current.ProfileSnapshotJSON {
		return errors.New("persisted repository profile snapshot is invalid")
	}
	if !sameBinding(binding, current) {
		return errors.New("repository registry drift changes persisted authority")
	}
	return nil
}

func sameBinding(a, b Binding) bool {
	return a.ProfileID == b.ProfileID && a.ProfileSnapshotVersion == b.ProfileSnapshotVersion && a.ProfileDigest == b.ProfileDigest && a.ProfileSnapshotJSON == b.ProfileSnapshotJSON &&
		a.CanonicalRepository == b.CanonicalRepository && a.LinearLabel == b.LinearLabel && a.OriginPath == b.OriginPath && a.SourcePath == b.SourcePath &&
		a.RunRoot == b.RunRoot && a.WorktreeRoot == b.WorktreeRoot && a.BaseBranch == b.BaseBranch &&
		a.VerifierRegistryRef == b.VerifierRegistryRef && slices.Equal(a.VerifierIDs, b.VerifierIDs) &&
		a.GitHubAppProfileRef == b.GitHubAppProfileRef && a.GitHubAppID == b.GitHubAppID && a.GitHubInstallationID == b.GitHubInstallationID &&
		a.ExpectedRepositoryID == b.ExpectedRepositoryID && slices.Equal(a.OperatorIdentityPolicy.AllowedLogins, b.OperatorIdentityPolicy.AllowedLogins) &&
		slices.Equal(a.OperatorIdentityPolicy.TrustedActors, b.OperatorIdentityPolicy.TrustedActors)
}

func BuiltinVerifierCommands() map[string]verifier.Command {
	return map[string]verifier.Command{"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}}}
}
