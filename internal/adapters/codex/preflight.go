package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
)

var requiredExecFlags = []string{
	"--ignore-user-config",
	"--sandbox",
	"--cd",
	"--json",
	"--output-schema",
	"--output-last-message",
	"--ephemeral",
}

type PreflightEvidence struct {
	Version       string   `json:"version"`
	RequiredFlags []string `json:"required_flags"`
	MissingFlags  []string `json:"missing_flags"`
}

type Preflighter struct {
	process processadapter.Runner
	binary  string
}

func NewPreflighter(process processadapter.Runner, binary string) Preflighter {
	if strings.TrimSpace(binary) == "" {
		binary = "codex"
	}
	return Preflighter{process: process, binary: binary}
}

func (p Preflighter) Run(ctx context.Context, artifacts string) (PreflightEvidence, error) {
	version, err := p.process.Run(ctx, processadapter.Spec{
		Program: p.binary, Args: []string{"--version"},
		StdoutPath: filepath.Join(artifacts, "codex-version.stdout.txt"),
		StderrPath: filepath.Join(artifacts, "codex-version.stderr.txt"),
	})
	if err != nil {
		return PreflightEvidence{}, fmt.Errorf("codex version: %w", err)
	}
	if version.ExitCode != 0 {
		return PreflightEvidence{}, fmt.Errorf("codex --version exited with code %d", version.ExitCode)
	}
	help, err := p.process.Run(ctx, processadapter.Spec{
		Program: p.binary, Args: []string{"exec", "--help"},
		StdoutPath: filepath.Join(artifacts, "codex-exec-help.stdout.txt"),
		StderrPath: filepath.Join(artifacts, "codex-exec-help.stderr.txt"),
	})
	if err != nil {
		return PreflightEvidence{}, fmt.Errorf("codex exec help: %w", err)
	}
	if help.ExitCode != 0 {
		return PreflightEvidence{}, fmt.Errorf("codex exec --help exited with code %d", help.ExitCode)
	}
	helpOutput, err := boundedStdout(help, 1<<20)
	if err != nil {
		return PreflightEvidence{}, fmt.Errorf("read codex exec help: %w", err)
	}
	helpFlags := declaredLongOptions(string(helpOutput))
	var missing []string
	for _, flag := range requiredExecFlags {
		if _, ok := helpFlags[flag]; !ok {
			missing = append(missing, flag)
		}
	}
	versionOutput, err := boundedStdout(version, 4096)
	if err != nil {
		return PreflightEvidence{}, fmt.Errorf("read codex version: %w", err)
	}
	evidence := PreflightEvidence{
		Version:       strings.TrimSpace(string(versionOutput)),
		RequiredFlags: append([]string(nil), requiredExecFlags...),
		MissingFlags:  missing,
	}
	if evidence.Version == "" {
		return PreflightEvidence{}, fmt.Errorf("codex --version returned an empty version")
	}
	if err := writeJSONExclusive(filepath.Join(artifacts, "codex-preflight.json"), evidence); err != nil {
		return PreflightEvidence{}, err
	}
	if len(missing) > 0 {
		return PreflightEvidence{}, fmt.Errorf("installed Codex CLI lacks required capabilities: %s", strings.Join(missing, ", "))
	}
	return evidence, nil
}

func declaredLongOptions(help string) map[string]struct{} {
	options := make(map[string]struct{})
	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		for _, field := range strings.Fields(trimmed) {
			option := strings.TrimSuffix(field, ",")
			if strings.HasPrefix(option, "--") {
				options[option] = struct{}{}
			}
		}
	}
	return options
}

func boundedStdout(result processadapter.Result, limit int64) ([]byte, error) {
	if result.StdoutPath == "" {
		if int64(len(result.Stdout)) > limit {
			return nil, fmt.Errorf("stdout exceeds %d bytes", limit)
		}
		return result.Stdout, nil
	}
	file, err := os.Open(result.StdoutPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("stdout exceeds %d bytes", limit)
	}
	return data, nil
}

func writeJSONExclusive(path string, value any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create evidence %s: %w", path, err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(value)
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("encode evidence %s: %w", path, encodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close evidence %s: %w", path, closeErr)
	}
	return nil
}
