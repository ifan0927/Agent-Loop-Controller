package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type StructuredResult[T any] struct {
	SessionID string
	Outcome   T
	Process   processadapter.Result
}

type Runner struct {
	process processadapter.Runner
}

type Executor struct {
	preflight Preflighter
	runner    Runner
}

func NewExecutor(process processadapter.Runner, binary string) Executor {
	return Executor{preflight: NewPreflighter(process, binary), runner: NewRunner(process)}
}

func (e Executor) Preflight(ctx context.Context, artifacts string) (PreflightEvidence, error) {
	return e.preflight.Run(ctx, artifacts)
}

func (e Executor) Implementation(ctx context.Context, spec CommandSpec, artifacts string) (StructuredResult[domain.AgentOutcome], error) {
	return e.runner.Implementation(ctx, spec, artifacts)
}

func (e Executor) Review(ctx context.Context, spec CommandSpec, artifacts string) (StructuredResult[domain.ReviewOutcome], error) {
	return e.runner.Review(ctx, spec, artifacts)
}

func NewRunner(process processadapter.Runner) Runner {
	return Runner{process: process}
}

func (r Runner) Implementation(ctx context.Context, spec CommandSpec, artifacts string) (StructuredResult[domain.AgentOutcome], error) {
	return runStructured(ctx, r.process, spec, artifacts, "implementation", func(outcome domain.AgentOutcome) error {
		return outcome.Validate()
	})
}

func (r Runner) Review(ctx context.Context, spec CommandSpec, artifacts string) (StructuredResult[domain.ReviewOutcome], error) {
	return runStructured(ctx, r.process, spec, artifacts, "review", func(outcome domain.ReviewOutcome) error {
		return outcome.Validate()
	})
}

func runStructured[T any](ctx context.Context, runner processadapter.Runner, command CommandSpec, artifacts, name string, semanticValidate func(T) error) (StructuredResult[T], error) {
	var zero StructuredResult[T]
	result, err := runner.Run(ctx, processadapter.Spec{
		Program: command.Program, Args: command.Args, WorkingDir: command.WorkingDir, Stdin: command.Stdin,
		StdoutPath:   filepath.Join(artifacts, name+".stdout.jsonl"),
		StderrPath:   filepath.Join(artifacts, name+".stderr.txt"),
		MustNotExist: command.MustNotExist,
	})
	if err != nil {
		return zero, err
	}
	if result.ExitCode != 0 {
		return zero, fmt.Errorf("Codex %s exited with code %d", name, result.ExitCode)
	}
	stdout, err := openProcessStdout(result)
	if err != nil {
		return zero, fmt.Errorf("open Codex %s telemetry: %w", name, err)
	}
	sessionID, sessionErr := extractSessionID(stdout)
	closeErr := stdout.Close()
	if sessionErr != nil {
		return zero, fmt.Errorf("Codex %s telemetry: %w", name, sessionErr)
	}
	if closeErr != nil {
		return zero, fmt.Errorf("close Codex %s telemetry: %w", name, closeErr)
	}
	if err := writeJSONExclusive(filepath.Join(artifacts, name+"-session.json"), struct {
		SessionID string `json:"session_id"`
	}{SessionID: sessionID}); err != nil {
		return zero, fmt.Errorf("persist Codex %s session: %w", name, err)
	}
	outputPath, schemaPath, err := structuredPaths(command)
	if err != nil {
		return zero, err
	}
	var outcome T
	if err := validateStructuredFile(outputPath, schemaPath, &outcome); err != nil {
		return zero, fmt.Errorf("Codex %s last message: %w", name, err)
	}
	if err := semanticValidate(outcome); err != nil {
		return zero, fmt.Errorf("Codex %s semantic outcome: %w", name, err)
	}
	return StructuredResult[T]{SessionID: sessionID, Outcome: outcome, Process: result}, nil
}

func openProcessStdout(result processadapter.Result) (io.ReadCloser, error) {
	if result.StdoutPath != "" {
		return os.Open(result.StdoutPath)
	}
	return io.NopCloser(bytes.NewReader(result.Stdout)), nil
}

func extractSessionID(jsonl io.Reader) (string, error) {
	scanner := bufio.NewScanner(jsonl)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var sessionID string
	for scanner.Scan() {
		var event map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return "", fmt.Errorf("invalid JSONL event: %w", err)
		}
		var eventType string
		_ = json.Unmarshal(event["type"], &eventType)
		if eventType != "thread.started" && eventType != "session.started" {
			continue
		}
		for _, field := range []string{"thread_id", "session_id"} {
			var candidate string
			if err := json.Unmarshal(event[field], &candidate); err == nil && strings.TrimSpace(candidate) != "" {
				if sessionID != "" && sessionID != candidate {
					return "", errors.New("conflicting session IDs in JSONL")
				}
				sessionID = candidate
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if sessionID == "" {
		return "", errors.New("missing explicit session ID")
	}
	return sessionID, nil
}

func structuredPaths(command CommandSpec) (outputPath, schemaPath string, err error) {
	for index := 0; index < len(command.Args)-1; index++ {
		switch command.Args[index] {
		case "--output-last-message", "-o":
			outputPath = command.Args[index+1]
		case "--output-schema":
			schemaPath = command.Args[index+1]
		}
	}
	if outputPath == "" || schemaPath == "" {
		return "", "", errors.New("structured command is missing output paths")
	}
	return outputPath, schemaPath, nil
}

func validateStructuredFile(outputPath, schemaPath string, target any) error {
	info, err := os.Lstat(outputPath)
	if err != nil {
		return fmt.Errorf("inspect output: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("structured output must be a regular file, not a symlink or special file")
	}
	if err := os.Chmod(outputPath, 0o600); err != nil {
		return fmt.Errorf("set private output mode: %w", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return err
	}
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	schema, err := jsonschema.Compile(schemaPath)
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	if err := schema.Validate(raw); err != nil {
		return fmt.Errorf("schema validation: %w", err)
	}
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(target); err != nil {
		return fmt.Errorf("decode typed outcome: %w", err)
	}
	return ensureJSONEOF(strict)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("structured output must contain exactly one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
