// Package bootstrap owns the controller's composition-root configuration.
package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/githubapp"
	linearadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/linear"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/localregistry"
)

const (
	LegacyVersion  = 1
	CurrentVersion = 2
)

// Error is safe to display to an operator. It deliberately excludes file
// paths, credential references, and underlying parser details.
type Error struct {
	Category string
	Message  string
}

func (e *Error) Error() string { return e.Category + ": " + e.Message }

func invalid(message string) error  { return &Error{Category: "invalid_config", Message: message} }
func missing(message string) error  { return &Error{Category: "missing_reference", Message: message} }
func conflict(message string) error { return &Error{Category: "identity_conflict", Message: message} }
func unsafe(message string) error   { return &Error{Category: "unsafe_path", Message: message} }

type Controller struct {
	DatabasePath string
	CodexBinary  string
	RunTimeout   time.Duration
}

type GitHubProfile struct {
	ID     string
	Config githubapp.Config
	Digest string
}

type Bootstrap struct {
	Version        int
	Digest         string
	Controller     Controller
	Linear         linearadapter.Config
	Registry       localregistry.Registry
	GitHubProfiles map[string]GitHubProfile
	RegistryPath   string
}

type readinessFile struct {
	Version             int                   `json:"version"`
	ConfigurationDigest string                `json:"configuration_digest"`
	Offline             bool                  `json:"offline"`
	Controller          readinessController   `json:"controller"`
	Linear              readinessLinear       `json:"linear"`
	Repositories        []readinessRepository `json:"repository_profiles"`
	GitHubProfiles      []readinessGitHub     `json:"github_app_profiles"`
}

type readinessController struct {
	DatabaseConfigured bool `json:"database_configured"`
	CodexConfigured    bool `json:"codex_configured"`
}

type readinessLinear struct {
	TeamKey string `json:"team_key"`
}

type readinessRepository struct {
	ProfileID     string `json:"profile_id"`
	ProfileDigest string `json:"profile_digest"`
	Repository    string `json:"repository"`
	GitHubProfile string `json:"github_app_profile"`
}

type readinessGitHub struct {
	ProfileID string `json:"profile_id"`
	Digest    string `json:"profile_digest"`
	AppID     int64  `json:"app_id"`
}

// Readiness is an offline, credential-safe report. It never performs network
// I/O, reads environment variables, opens a database, or reads key contents.
func (b Bootstrap) Readiness() any {
	bindings := b.Registry.Bindings()
	repositories := make([]readinessRepository, 0, len(bindings))
	for _, binding := range bindings {
		repositories = append(repositories, readinessRepository{ProfileID: binding.ProfileID, ProfileDigest: binding.ProfileDigest, Repository: binding.CanonicalRepository, GitHubProfile: binding.GitHubAppProfileRef})
	}
	ids := make([]string, 0, len(b.GitHubProfiles))
	for id := range b.GitHubProfiles {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	profiles := make([]readinessGitHub, 0, len(ids))
	for _, id := range ids {
		profile := b.GitHubProfiles[id]
		profiles = append(profiles, readinessGitHub{ProfileID: profile.ID, Digest: profile.Digest, AppID: profile.Config.AppID})
	}
	return readinessFile{Version: b.Version, ConfigurationDigest: b.Digest, Offline: true,
		Controller: readinessController{DatabaseConfigured: b.Controller.DatabasePath != "", CodexConfigured: b.Controller.CodexBinary != ""},
		Linear:     readinessLinear{TeamKey: b.Linear.TeamKey}, Repositories: repositories, GitHubProfiles: profiles}
}

// GitHubProfileForRepository returns the already cross-checked configuration.
func (b Bootstrap) GitHubProfileForRepository(repository string) (GitHubProfile, error) {
	binding, err := b.Registry.Resolve(repository)
	if err != nil {
		return GitHubProfile{}, missing("repository profile is not configured")
	}
	profile, ok := b.GitHubProfiles[binding.GitHubAppProfileRef]
	if !ok {
		return GitHubProfile{}, missing("GitHub App profile is not configured")
	}
	return profile, nil
}

type configFile struct {
	Version                int             `json:"version"`
	Controller             controllerFile  `json:"controller"`
	Linear                 json.RawMessage `json:"linear"`
	RepositoryRegistryFile json.RawMessage `json:"repository_registry_file"`
	Repositories           json.RawMessage `json:"repositories"`
	GitHubAppProfiles      []profileFile   `json:"github_app_profiles"`
}

type controllerFile struct {
	DatabasePath string `json:"database_path"`
	CodexBinary  string `json:"codex_binary"`
	RunTimeout   string `json:"run_timeout"`
}

type profileFile struct {
	ID     string          `json:"id"`
	Config json.RawMessage `json:"config"`
}

// Load performs strict, offline composition validation. Credential files are
// inspected only as filesystem objects; their contents are never read.
func Load(path string) (Bootstrap, error) {
	data, _, err := readRegularConfig(path)
	if err != nil {
		return Bootstrap{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw configFile
	if err := decoder.Decode(&raw); err != nil {
		return Bootstrap{}, invalid("controller configuration must contain one strict JSON value")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Bootstrap{}, invalid("controller configuration must contain one strict JSON value")
	}
	if raw.Version != LegacyVersion && raw.Version != CurrentVersion {
		return Bootstrap{}, invalid("unsupported controller configuration version")
	}
	controller, err := decodeController(raw.Controller)
	if err != nil {
		return Bootstrap{}, err
	}
	linear, err := linearadapter.DecodeConfig(bytes.NewReader(raw.Linear))
	if err != nil {
		return Bootstrap{}, invalid("Linear profile is invalid")
	}
	registry, registryPath, err := decodeRegistry(raw)
	if err != nil {
		return Bootstrap{}, err
	}
	profiles, err := decodeProfiles(raw.GitHubAppProfiles)
	if err != nil {
		return Bootstrap{}, err
	}
	if err := crossCheck(registry, profiles); err != nil {
		return Bootstrap{}, err
	}
	digest := sha256.Sum256(data)
	return Bootstrap{Version: raw.Version, Digest: hex.EncodeToString(digest[:]), Controller: controller, Linear: linear, Registry: registry, GitHubProfiles: profiles, RegistryPath: registryPath}, nil
}

func decodeRegistry(raw configFile) (localregistry.Registry, string, error) {
	switch raw.Version {
	case LegacyVersion:
		if len(raw.Repositories) != 0 {
			return localregistry.Registry{}, "", invalid("controller configuration version 1 must use repository_registry_file")
		}
		if len(raw.RepositoryRegistryFile) == 0 {
			return localregistry.Registry{}, "", missing("repository registry file is required")
		}
		var registryFile string
		if err := json.Unmarshal(raw.RepositoryRegistryFile, &registryFile); err != nil || strings.TrimSpace(registryFile) == "" {
			return localregistry.Registry{}, "", invalid("repository registry file is invalid")
		}
		registryPath, err := canonicalRegularPath(registryFile)
		if err != nil {
			return localregistry.Registry{}, "", err
		}
		registry, err := localregistry.Load(registryPath)
		if err != nil {
			return localregistry.Registry{}, "", invalid("repository registry is invalid")
		}
		return registry, registryPath, nil
	case CurrentVersion:
		if len(raw.RepositoryRegistryFile) != 0 {
			return localregistry.Registry{}, "", invalid("controller configuration version 2 must use inline repositories")
		}
		if len(raw.Repositories) == 0 {
			return localregistry.Registry{}, "", missing("inline repositories are required")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw.Repositories))
		decoder.DisallowUnknownFields()
		var repositories []localregistry.Repository
		if err := decoder.Decode(&repositories); err != nil {
			return localregistry.Registry{}, "", invalid("inline repositories are invalid")
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF || len(repositories) == 0 {
			return localregistry.Registry{}, "", invalid("inline repositories are invalid")
		}
		registry, err := localregistry.New(repositories)
		if err != nil {
			return localregistry.Registry{}, "", invalid("inline repositories are invalid")
		}
		return registry, "", nil
	default:
		return localregistry.Registry{}, "", invalid("unsupported controller configuration version")
	}
}

func decodeController(raw controllerFile) (Controller, error) {
	databasePath, err := canonicalDatabasePath(raw.DatabasePath)
	if err != nil {
		return Controller{}, err
	}
	if strings.TrimSpace(raw.CodexBinary) == "" || strings.ContainsAny(raw.CodexBinary, "/\\") {
		return Controller{}, invalid("Codex binary must be a simple executable name")
	}
	timeout, err := time.ParseDuration(raw.RunTimeout)
	if err != nil || timeout <= 0 || timeout > 2*time.Hour {
		return Controller{}, invalid("controller run timeout is invalid")
	}
	return Controller{DatabasePath: databasePath, CodexBinary: raw.CodexBinary, RunTimeout: timeout}, nil
}

func decodeProfiles(raw []profileFile) (map[string]GitHubProfile, error) {
	if len(raw) == 0 {
		return nil, missing("at least one GitHub App profile is required")
	}
	profiles := make(map[string]GitHubProfile, len(raw))
	for _, item := range raw {
		if !validProfileID(item.ID) {
			return nil, invalid("GitHub App profile ID is invalid")
		}
		if _, exists := profiles[item.ID]; exists {
			return nil, conflict("duplicate GitHub App profile ID")
		}
		config, err := githubapp.DecodeConfigWithoutPrivateKey(bytes.NewReader(item.Config))
		if err != nil {
			return nil, invalid("GitHub App profile is invalid")
		}
		if err := inspectPrivateKeyPath(config.PrivateKeyFile); err != nil {
			return nil, err
		}
		digest := sha256.Sum256(item.Config)
		profiles[item.ID] = GitHubProfile{ID: item.ID, Config: config, Digest: hex.EncodeToString(digest[:])}
	}
	return profiles, nil
}

func crossCheck(registry localregistry.Registry, profiles map[string]GitHubProfile) error {
	used := make(map[string]struct{})
	for _, binding := range registry.Bindings() {
		profile, ok := profiles[binding.GitHubAppProfileRef]
		if !ok {
			return missing("repository references a missing GitHub App profile")
		}
		used[profile.ID] = struct{}{}
		parts := strings.Split(binding.CanonicalRepository, "/")
		if profile.Config.AppID != binding.GitHubAppID || profile.Config.InstallationID != binding.GitHubInstallationID || profile.Config.RepositoryID != binding.ExpectedRepositoryID || !strings.EqualFold(profile.Config.RepositoryOwner, parts[0]) || !strings.EqualFold(profile.Config.RepositoryName, parts[1]) {
			return conflict("GitHub App profile does not match repository authority")
		}
	}
	if len(used) != len(profiles) {
		return missing("GitHub App profile is not referenced by a repository")
	}
	return nil
}

func validProfileID(value string) bool {
	if !strings.HasPrefix(value, "github-app-profile:") || len(value) > 128 {
		return false
	}
	name := strings.TrimPrefix(value, "github-app-profile:")
	if name == "" {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func readRegularConfig(path string) ([]byte, string, error) {
	canonical, err := canonicalRegularPath(path)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(canonical)
	if err != nil {
		return nil, "", unsafe("controller configuration is unreadable")
	}
	if len(data) > 256<<10 {
		return nil, "", invalid("controller configuration is too large")
	}
	return data, canonical, nil
}

func canonicalRegularPath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", unsafe("configuration path must be absolute and canonical")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", unsafe("configuration file must be a non-symlink regular file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", unsafe("configuration path is ambiguous")
	}
	return path, nil
}

func canonicalDatabasePath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", unsafe("database path must be absolute and canonical")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", unsafe("database parent must be a non-symlink directory")
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return "", unsafe("database parent path is ambiguous")
	}
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return "", unsafe("database path must be absent or a regular file")
	}
	return path, nil
}

func inspectPrivateKeyPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return unsafe("GitHub App credential source is not a private regular file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path || info.Size() > 64<<10 {
		return unsafe("GitHub App credential source path is ambiguous or invalid")
	}
	return nil
}
