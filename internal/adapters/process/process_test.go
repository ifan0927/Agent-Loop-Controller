package process

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"testing"
	"time"
)

func TestOSRunnerRejectsExistingOutputLeaf(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "outcome.json")
	if err := os.WriteFile(output, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		MustNotExist: []string{output},
	})
	if err == nil {
		t.Fatal("existing output leaf must be rejected")
	}
}

func TestOSRunnerRejectsSymlinkOutputLeaf(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	output := filepath.Join(directory, "outcome.json")
	if err := os.WriteFile(target, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, output); err != nil {
		t.Fatal(err)
	}
	_, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		MustNotExist: []string{output},
	})
	if err == nil {
		t.Fatal("symlink output leaf must be rejected")
	}
}

func TestOSRunnerKeepsProductionOutputOnlyInArtifactFiles(t *testing.T) {
	directory := t.TempDir()
	stdoutPath := filepath.Join(directory, "stdout")
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "exit"},
		StdoutPath: stdoutPath, StderrPath: filepath.Join(directory, "stderr"),
	})
	if err != nil || result.Outcome != OutcomeExited || !result.Succeeded() {
		t.Fatal(err)
	}
	if len(result.Stdout) != 0 || result.StdoutPath != stdoutPath {
		t.Fatalf("production output was copied into memory: %+v", result)
	}
	data, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "done\n" {
		t.Fatalf("stdout artifact = %q", data)
	}
}

func TestOSRunnerCancelsProcessGroupWithBoundedTermination(t *testing.T) {
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(ctx, Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-interrupt"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
	})
	if err == nil {
		t.Fatal("cancelled process must return an error")
	}
	if result.Outcome != OutcomeInterrupted || result.ExitCode == 0 || result.FailureCategory != FailureInterrupted {
		t.Fatalf("interrupted result=%+v", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded termination took %s", elapsed)
	}
}

func TestOSRunnerRecordsMissingExecutableAsNotStarted(t *testing.T) {
	directory := t.TempDir()
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program:    "definitely-missing-verifier",
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
	})
	if err == nil {
		t.Fatal("missing executable must fail")
	}
	if result.Outcome != OutcomeNotStarted || result.FailureCategory != FailureStart || result.ExitCode == 0 {
		t.Fatalf("start failure result=%+v", result)
	}
	if got := SanitizeError(err); got != "managed process failure: process_start" {
		t.Fatalf("sanitized error=%q", got)
	}
	for _, path := range []string{result.StdoutPath, result.StderrPath} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || len(data) != 0 {
			t.Fatalf("capture path=%q data=%q err=%v", path, data, readErr)
		}
	}
}

func TestResultCannotTreatNotStartedZeroAsSuccess(t *testing.T) {
	result := Result{Outcome: OutcomeNotStarted, FailureCategory: FailureStart, ExitCode: 0}
	if NormalizeResult(result, NewFailure(FailureStart)).Succeeded() {
		t.Fatal("not-started process must never be successful")
	}
}

func TestOSRunnerExcludesConfiguredEnvironment(t *testing.T) {
	t.Setenv("IFAN_LOOP_LINEAR_TOKEN", "secret")
	directory := t.TempDir()
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "print-token"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		ExcludedEnv: []string{"IFAN_LOOP_LINEAR_TOKEN"},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(data) != "absent\n" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestOSRunnerAppliesManagedEnvironmentOverrides(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "ambient-config")
	t.Setenv("GIT_AUTHOR_NAME", "ambient-author")
	directory := t.TempDir()
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "managed-environment"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
		Environment: []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=managed-author"},
	})
	if err != nil || !result.Succeeded() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(data) != "/dev/null\nmanaged-author\n" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestOSRunnerRestrictsEnvironmentToAllowlist(t *testing.T) {
	t.Setenv("HOME", "/managed-home")
	t.Setenv("RETAINED_SECRET", "must-not-enter")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	directory := t.TempDir()
	result, err := (OSRunner{}).Run(context.Background(), Spec{
		Program:              os.Args[0],
		Args:                 []string{"-test.run=TestProcessHelper", "--", "allowlist-environment"},
		StdoutPath:           filepath.Join(directory, "stdout"),
		StderrPath:           filepath.Join(directory, "stderr"),
		EnvironmentAllowlist: []string{"HOME"},
	})
	if err != nil || !result.Succeeded() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(data) != "/managed-home\nabsent\nabsent\n"+ManagedCommandPath+"\n" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestControllerEnvironmentPrependsManagedCommandPath(t *testing.T) {
	environment := controllerEnvironment([]string{"PATH=/custom/bin:/usr/bin", "RETAIN=1"}, nil)
	if got, want := environment[0], "PATH="+ManagedCommandPath+":/custom/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if got := environment[1]; got != "RETAIN=1" {
		t.Fatalf("retained environment = %q", got)
	}
}

func TestEnvironmentOverridesCannotReplaceManagedPath(t *testing.T) {
	environment := applyEnvironmentOverrides([]string{"PATH=" + ManagedCommandPath, "VALUE=old"}, nil, []string{"PATH=/unsafe", "VALUE=new"})
	if got := environment[0]; got != "PATH="+ManagedCommandPath {
		t.Fatalf("PATH override escaped managed runtime: %q", got)
	}
	if got := environment[1]; got != "VALUE=new" {
		t.Fatalf("environment override = %q", got)
	}
}

func TestOSRunnerResolvesSimpleProgramFromControllerEnvironment(t *testing.T) {
	directory := t.TempDir()
	program := "controller-managed-fixture"
	if err := os.WriteFile(filepath.Join(directory, program), []byte("#!/bin/sh\nprintf 'managed\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (OSRunner{}).run(context.Background(), Spec{
		Program:     program,
		StdoutPath:  filepath.Join(directory, "stdout"),
		StderrPath:  filepath.Join(directory, "stderr"),
		WorkingDir:  directory,
		ExcludedEnv: []string{"IFAN_LOOP_LINEAR_TOKEN"},
	}, []string{"PATH=" + directory, "IFAN_LOOP_LINEAR_TOKEN=secret"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout"))
	if err != nil || string(data) != "managed\n" {
		t.Fatalf("stdout=%q err=%v", data, err)
	}
}

func TestProcessHelper(t *testing.T) {
	for index, arg := range os.Args {
		if arg != "--" || index+1 >= len(os.Args) {
			continue
		}
		switch os.Args[index+1] {
		case "exit":
			fmt.Println("done")
			os.Exit(0)
		case "ignore-interrupt":
			signal.Ignore(os.Interrupt)
			time.Sleep(10 * time.Second)
			os.Exit(0)
		case "print-token":
			if _, found := os.LookupEnv("IFAN_LOOP_LINEAR_TOKEN"); found {
				fmt.Println("present")
			} else {
				fmt.Println("absent")
			}
			os.Exit(0)
		case "managed-environment":
			fmt.Println(os.Getenv("GIT_CONFIG_GLOBAL"))
			fmt.Println(os.Getenv("GIT_AUTHOR_NAME"))
			os.Exit(0)
		case "allowlist-environment":
			fmt.Println(os.Getenv("HOME"))
			if _, found := os.LookupEnv("RETAINED_SECRET"); found {
				fmt.Println("present")
			} else {
				fmt.Println("absent")
			}
			if _, found := os.LookupEnv("GIT_CONFIG_COUNT"); found {
				fmt.Println("present")
			} else {
				fmt.Println("absent")
			}
			fmt.Println(os.Getenv("PATH"))
			os.Exit(0)
		}
	}
}
