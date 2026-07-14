package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
)

const (
	defaultConfigDirectoryName = "agent-loop-controller"
	defaultConfigFileName      = "controller.json"
)

var userHomeDirectory = os.UserHomeDir

// defaultConfigPath is the stable macOS operator-facing configuration path.
// Tests replace userHomeDirectory so no host configuration is consulted.
func defaultConfigPath() (string, error) {
	home, err := userHomeDirectory()
	if err != nil {
		return "", fmt.Errorf("resolve default configuration path: %w", err)
	}
	home = strings.TrimSpace(home)
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("default configuration home directory is invalid")
	}
	return filepath.Join(home, "Library", "Application Support", defaultConfigDirectoryName, defaultConfigFileName), nil
}

func configPathFlag(flags *flag.FlagSet) *string {
	return flags.String("config", "", "controller composition configuration (default: ~/Library/Application Support/agent-loop-controller/controller.json)")
}

func resolveConfigPath(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	return defaultConfigPath()
}

func configCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ifan-loop config <init|path|validate|inspect> [--config <controller.json>]")
	}
	switch args[0] {
	case "init":
		return configInit(args[1:])
	case "path":
		return configPath(args[1:])
	case "validate", "inspect":
		return configReadiness(args[0], args[1:])
	default:
		return errors.New("usage: ifan-loop config <init|path|validate|inspect> [--config <controller.json>]")
	}
}

func configPath(args []string) error {
	flags := flag.NewFlagSet("config path", flag.ContinueOnError)
	pathFlag := configPathFlag(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config path does not accept positional arguments")
	}
	path, err := resolveConfigPath(*pathFlag)
	if err != nil {
		return err
	}
	return printJSON(struct {
		Path string `json:"path"`
	}{Path: path})
}

func configReadiness(command string, args []string) error {
	flags := flag.NewFlagSet("config "+command, flag.ContinueOnError)
	pathFlag := configPathFlag(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config command does not accept positional arguments")
	}
	path, err := resolveConfigPath(*pathFlag)
	if err != nil {
		return err
	}
	loaded, err := bootstrap.Load(path)
	if err != nil {
		return err
	}
	return printJSON(loaded.Readiness())
}

type configInitOutput struct {
	Path          string `json:"path"`
	Created       bool   `json:"created"`
	SetupRequired bool   `json:"setup_required"`
	SecretFree    bool   `json:"secret_free"`
}

// configTemplate intentionally contains no credentials or private-key paths.
// It remains a strict v3 JSON document, but has no repository or GitHub App
// profiles; an operator must add those before validation and execution.
type configTemplate struct {
	Version           int                      `json:"version"`
	Controller        configTemplateControl    `json:"controller"`
	Linear            configTemplateLinear     `json:"linear"`
	GitHubAppProfiles []json.RawMessage        `json:"github_app_profiles"`
	Repositories      []json.RawMessage        `json:"repositories"`
	Automation        configTemplateAutomation `json:"automation"`
}

type configTemplateAutomation struct {
	LinearTodoAdmission configTemplateLinearTodoAdmission `json:"linear_todo_admission"`
}

type configTemplateLinearTodoAdmission struct {
	Enabled                       bool   `json:"enabled"`
	PollInterval                  string `json:"poll_interval"`
	SchedulerLeaseTTL             string `json:"scheduler_lease_ttl"`
	SchedulerLeaseRenewalInterval string `json:"scheduler_lease_renewal_interval"`
	MaxCandidates                 int    `json:"max_candidates"`
	MaxPages                      int    `json:"max_pages"`
	MaxActiveRuns                 int    `json:"max_active_runs"`
	NotificationMode              string `json:"notification_mode"`
}

type configTemplateControl struct {
	DatabasePath string `json:"database_path"`
	CodexBinary  string `json:"codex_binary"`
	RunTimeout   string `json:"run_timeout"`
}

type configTemplateLinear struct {
	APIURL              string `json:"api_url"`
	CredentialSourceRef string `json:"credential_source_ref"`
	AuthorizationScheme string `json:"authorization_scheme"`
	TeamKey             string `json:"team_key"`
	HTTPTimeout         string `json:"http_timeout"`
	MaxResponseBytes    int64  `json:"max_response_bytes"`
	LabelPageSize       int    `json:"label_page_size"`
	MaxLabelPages       int    `json:"max_label_pages"`
}

func configInit(args []string) error {
	flags := flag.NewFlagSet("config init", flag.ContinueOnError)
	pathFlag := configPathFlag(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config init does not accept positional arguments")
	}
	path, err := resolveConfigPath(*pathFlag)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve configuration path: %w", err)
	}
	if err := createConfigDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	content, err := json.MarshalIndent(newConfigTemplate(filepath.Dir(path)), "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration template: %w", err)
	}
	content = append(content, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errors.New("configuration already exists; refusing to overwrite it")
		}
		return fmt.Errorf("create configuration: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("write configuration template: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close configuration template: %w", err)
	}
	return printJSON(configInitOutput{Path: path, Created: true, SetupRequired: true, SecretFree: true})
}

func createConfigDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect configuration directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("configuration directory must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve configuration directory: %w", err)
	}
	if resolved != path {
		return errors.New("configuration directory must not include symbolic links")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure configuration directory: %w", err)
	}
	return nil
}

func newConfigTemplate(root string) configTemplate {
	return configTemplate{
		Version: 3,
		Controller: configTemplateControl{
			DatabasePath: filepath.Join(root, "controller.db"),
			CodexBinary:  "codex",
			RunTimeout:   "30m",
		},
		Linear: configTemplateLinear{
			APIURL:              "https://api.linear.app/graphql",
			CredentialSourceRef: "secret://env/IFAN_LOOP_LINEAR_TOKEN",
			AuthorizationScheme: "bearer",
			TeamKey:             "IFAN",
			HTTPTimeout:         "30s",
			MaxResponseBytes:    1048576,
			LabelPageSize:       50,
			MaxLabelPages:       10,
		},
		GitHubAppProfiles: []json.RawMessage{},
		Repositories:      []json.RawMessage{},
		Automation: configTemplateAutomation{LinearTodoAdmission: configTemplateLinearTodoAdmission{
			Enabled: false, PollInterval: "5m", SchedulerLeaseTTL: "1m", SchedulerLeaseRenewalInterval: "20s",
			MaxCandidates: 20, MaxPages: 5, MaxActiveRuns: 1, NotificationMode: "local_outbox",
		}},
	}
}
