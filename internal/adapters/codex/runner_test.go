package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	controllercontracts "github.com/ifan0927/Agent-Loop-Controller/contracts"
	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
)

type fakeProcess struct {
	result      processadapter.Result
	lastMessage string
}

func (f fakeProcess) Run(_ context.Context, spec processadapter.Spec) (processadapter.Result, error) {
	for index := 0; index < len(spec.Args)-1; index++ {
		if spec.Args[index] == "--output-last-message" || spec.Args[index] == "-o" {
			if err := os.WriteFile(spec.Args[index+1], []byte(f.lastMessage), 0o600); err != nil {
				return processadapter.Result{}, err
			}
		}
	}
	return f.result, nil
}

func TestRunnerToleratesUnknownJSONLEvents(t *testing.T) {
	artifacts, spec := runnerFixture(t)
	telemetry := "{\"type\":\"future.event\",\"new_field\":true}\n{\"type\":\"thread.started\",\"thread_id\":\"thread-123\"}\n"
	runner := NewRunner(fakeProcess{
		result:      processadapter.Result{ExitCode: 0, Stdout: []byte(telemetry)},
		lastMessage: validAgentOutcome,
	})
	result, err := runner.Implementation(context.Background(), spec, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "thread-123" {
		t.Fatalf("session ID = %q", result.SessionID)
	}
	if _, err := os.Stat(filepath.Join(artifacts, "implementation-session.json")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(artifacts, "implementation-outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("last-message mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRunnerRejectsMissingSessionID(t *testing.T) {
	artifacts, spec := runnerFixture(t)
	runner := NewRunner(fakeProcess{
		result:      processadapter.Result{Stdout: []byte("{\"type\":\"turn.completed\"}\n")},
		lastMessage: validAgentOutcome,
	})
	if _, err := runner.Implementation(context.Background(), spec, artifacts); err == nil || !strings.Contains(err.Error(), "missing explicit session ID") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerRejectsNonZeroExit(t *testing.T) {
	artifacts, spec := runnerFixture(t)
	runner := NewRunner(fakeProcess{result: processadapter.Result{ExitCode: 9}})
	if _, err := runner.Implementation(context.Background(), spec, artifacts); err == nil || !strings.Contains(err.Error(), "code 9") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerRejectsMalformedLastMessage(t *testing.T) {
	artifacts, spec := runnerFixture(t)
	runner := NewRunner(fakeProcess{
		result: processadapter.Result{Stdout: []byte(startedEvent)}, lastMessage: "not-json",
	})
	if _, err := runner.Implementation(context.Background(), spec, artifacts); err == nil {
		t.Fatal("malformed last message must be rejected")
	}
}

func TestRunnerRejectsSemanticallyInvalidOutcome(t *testing.T) {
	artifacts, spec := runnerFixture(t)
	runner := NewRunner(fakeProcess{
		result:      processadapter.Result{Stdout: []byte(startedEvent)},
		lastMessage: `{"status":"completed","summary":"","decision_request":null,"discovered_issues":[],"suggested_checks":[],"implementation_sha":null}`,
	})
	if _, err := runner.Implementation(context.Background(), spec, artifacts); err == nil || !strings.Contains(err.Error(), "semantic") {
		t.Fatalf("error = %v", err)
	}
}

func runnerFixture(t *testing.T) (string, CommandSpec) {
	t.Helper()
	artifacts := t.TempDir()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifacts, "implementation-outcome.schema.json"), []byte(controllercontracts.ImplementationOutcomeSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	return artifacts, NewCommandBuilder("codex").Implementation(testTask(), workspace, artifacts)
}

const startedEvent = "{\"type\":\"thread.started\",\"thread_id\":\"thread-123\"}\n"
const validAgentOutcome = `{"status":"completed","summary":"Implemented","decision_request":null,"discovered_issues":[],"suggested_checks":[],"implementation_sha":null}`
