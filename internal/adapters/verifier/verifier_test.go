package verifier

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
)

type fakeHead struct {
	heads []string
	index int
}

func (f *fakeHead) Head(context.Context, string) (string, error) {
	value := f.heads[f.index]
	if f.index < len(f.heads)-1 {
		f.index++
	}
	return value, nil
}

type successfulProcess struct{}

func (successfulProcess) Run(context.Context, processadapter.Spec) (processadapter.Result, error) {
	return processadapter.Result{Outcome: processadapter.OutcomeExited}, nil
}

type failingProcess struct{}

func (failingProcess) Run(context.Context, processadapter.Spec) (processadapter.Result, error) {
	return processadapter.Result{Outcome: processadapter.OutcomeExited, ExitCode: 7}, nil
}

type startFailingProcess struct{}

func (startFailingProcess) Run(_ context.Context, spec processadapter.Spec) (processadapter.Result, error) {
	if err := os.WriteFile(spec.StdoutPath, nil, 0o600); err != nil {
		return processadapter.Result{}, err
	}
	if err := os.WriteFile(spec.StderrPath, nil, 0o600); err != nil {
		return processadapter.Result{}, err
	}
	return processadapter.Result{Outcome: processadapter.OutcomeNotStarted, FailureCategory: processadapter.FailureStart, ExitCode: -1}, errors.Join(processadapter.NewFailure(processadapter.FailureStart), errors.New("Authorization: Bearer secret-from-child"))
}

func TestRegistryReturnsFailedCheckEvidence(t *testing.T) {
	artifacts := t.TempDir()
	registry := NewRegistry(map[string]Command{"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}}}, failingProcess{}, &fakeHead{heads: []string{"abc"}})
	evidence, err := registry.Run(context.Background(), []string{"fixture-go-test"}, t.TempDir(), artifacts, "candidate")
	if err == nil {
		t.Fatal("expected verifier failure")
	}
	if len(evidence.Checks) != 1 || evidence.Checks[0].ExitCode != 7 || evidence.VerifiedHeadSHA != "abc" {
		t.Fatalf("evidence=%+v", evidence)
	}
	if _, err := os.Stat(filepath.Join(artifacts, "candidate-verification.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryPersistsSanitizedStartFailureEvidence(t *testing.T) {
	artifacts := t.TempDir()
	registry := NewRegistry(map[string]Command{"missing": {Program: "missing-verifier", Args: []string{"--fixture"}}}, startFailingProcess{}, &fakeHead{heads: []string{"abc"}})
	evidence, err := registry.Run(context.Background(), []string{"missing"}, t.TempDir(), artifacts, "candidate")
	if err == nil || !strings.Contains(err.Error(), "process_start") {
		t.Fatalf("err=%v", err)
	}
	if len(evidence.Checks) != 1 {
		t.Fatalf("evidence=%+v", evidence)
	}
	check := evidence.Checks[0]
	if check.ProcessOutcome != processadapter.OutcomeNotStarted || check.FailureCategory != processadapter.FailureStart || check.ExitCode == 0 {
		t.Fatalf("check=%+v", check)
	}
	data, readErr := os.ReadFile(filepath.Join(artifacts, "candidate-verification.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	contents := string(data)
	if strings.Contains(contents, "Authorization") || strings.Contains(contents, "Bearer") || strings.Contains(contents, "secret-from-child") {
		t.Fatalf("unsanitized process error in evidence: %s", contents)
	}
}

func TestRegistryRejectsUnknownVerifier(t *testing.T) {
	registry := NewRegistry(nil, successfulProcess{}, &fakeHead{heads: []string{"abc"}})
	if _, err := registry.Run(context.Background(), []string{"unknown"}, t.TempDir(), t.TempDir(), "test"); err == nil {
		t.Fatal("unknown verifier must be rejected")
	}
}

func TestRegistryBindsEvidenceToUnchangedHead(t *testing.T) {
	artifacts := t.TempDir()
	registry := NewRegistry(map[string]Command{
		"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}},
	}, successfulProcess{}, &fakeHead{heads: []string{"abc", "abc"}})
	evidence, err := registry.Run(context.Background(), []string{"fixture-go-test"}, t.TempDir(), artifacts, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.VerifiedHeadSHA != "abc" {
		t.Fatalf("verified head = %s", evidence.VerifiedHeadSHA)
	}
	if len(evidence.Checks) != 1 || evidence.Checks[0].Program != "go" {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
	if evidence.Checks[0].StdoutPath != filepath.Join(artifacts, "candidate-verifier-01-fixture-go-test.stdout.txt") {
		t.Fatalf("unexpected stdout path: %s", evidence.Checks[0].StdoutPath)
	}
}

func TestRegistryRejectsVerifierHeadMutation(t *testing.T) {
	registry := NewRegistry(map[string]Command{
		"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}},
	}, successfulProcess{}, &fakeHead{heads: []string{"before", "after"}})
	if _, err := registry.Run(context.Background(), []string{"fixture-go-test"}, t.TempDir(), t.TempDir(), "candidate"); err == nil {
		t.Fatal("verifier changing HEAD must be rejected")
	}
}
