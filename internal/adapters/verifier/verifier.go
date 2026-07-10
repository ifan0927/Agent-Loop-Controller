package verifier

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
)

type Command struct {
	Program string
	Args    []string
}

type HeadReader interface {
	Head(context.Context, string) (string, error)
}

type CheckEvidence struct {
	VerifierID string   `json:"verifier_id"`
	Program    string   `json:"program"`
	Args       []string `json:"args"`
	ExitCode   int      `json:"exit_code"`
	StdoutPath string   `json:"stdout_path"`
	StderrPath string   `json:"stderr_path"`
	RunError   string   `json:"run_error,omitempty"`
}

type Evidence struct {
	VerifiedHeadSHA string          `json:"verified_head_sha"`
	Checks          []CheckEvidence `json:"checks"`
}

type Registry struct {
	commands map[string]Command
	process  processadapter.Runner
	git      HeadReader
}

func NewRegistry(commands map[string]Command, process processadapter.Runner, git HeadReader) Registry {
	copy := make(map[string]Command, len(commands))
	for id, command := range commands {
		copy[id] = Command{Program: command.Program, Args: append([]string(nil), command.Args...)}
	}
	return Registry{commands: copy, process: process, git: git}
}

func (r Registry) Run(ctx context.Context, ids []string, workspace, artifacts, label string) (Evidence, error) {
	for _, id := range ids {
		if _, ok := r.commands[id]; !ok {
			return Evidence{}, fmt.Errorf("unknown verifier ID: %s", id)
		}
	}
	head, err := r.git.Head(ctx, workspace)
	if err != nil {
		return Evidence{}, fmt.Errorf("read verification head: %w", err)
	}
	evidence := Evidence{VerifiedHeadSHA: head}
	for index, id := range ids {
		command := r.commands[id]
		stem := fmt.Sprintf("%s-verifier-%02d-%s", label, index+1, id)
		stdoutPath := filepath.Join(artifacts, stem+".stdout.txt")
		stderrPath := filepath.Join(artifacts, stem+".stderr.txt")
		result, runErr := r.process.Run(ctx, processadapter.Spec{
			Program: command.Program, Args: command.Args, WorkingDir: workspace,
			StdoutPath: stdoutPath, StderrPath: stderrPath,
		})
		check := CheckEvidence{
			VerifierID: id, Program: command.Program, Args: append([]string(nil), command.Args...),
			ExitCode: result.ExitCode, StdoutPath: stdoutPath, StderrPath: stderrPath,
		}
		if runErr != nil {
			check.RunError = runErr.Error()
		}
		evidence.Checks = append(evidence.Checks, check)
		if runErr != nil {
			if writeErr := writeEvidence(filepath.Join(artifacts, label+"-verification.json"), evidence); writeErr != nil {
				return Evidence{}, fmt.Errorf("run verifier %s: %v; persist evidence: %w", id, runErr, writeErr)
			}
			return evidence, fmt.Errorf("run verifier %s: %w", id, runErr)
		}
		if result.ExitCode != 0 {
			reason := fmt.Errorf("verifier %s exited with code %d", id, result.ExitCode)
			if writeErr := writeEvidence(filepath.Join(artifacts, label+"-verification.json"), evidence); writeErr != nil {
				return Evidence{}, fmt.Errorf("%v; persist evidence: %w", reason, writeErr)
			}
			return evidence, reason
		}
		after, err := r.git.Head(ctx, workspace)
		if err != nil {
			return Evidence{}, fmt.Errorf("read head after verifier %s: %w", id, err)
		}
		if strings.TrimSpace(after) != head {
			reason := fmt.Errorf("verifier %s changed HEAD", id)
			if writeErr := writeEvidence(filepath.Join(artifacts, label+"-verification.json"), evidence); writeErr != nil {
				return Evidence{}, fmt.Errorf("%v; persist evidence: %w", reason, writeErr)
			}
			return evidence, reason
		}
	}
	path := filepath.Join(artifacts, label+"-verification.json")
	if err := writeEvidence(path, evidence); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

func writeEvidence(path string, evidence Evidence) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create verification evidence: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(evidence)
	closeErr := file.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}
