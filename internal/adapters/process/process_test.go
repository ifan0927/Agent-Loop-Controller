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
	if err != nil {
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
	_, err := (OSRunner{InterruptGrace: 50 * time.Millisecond}).Run(ctx, Spec{
		Program: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore-interrupt"},
		StdoutPath: filepath.Join(directory, "stdout"), StderrPath: filepath.Join(directory, "stderr"),
	})
	if err == nil {
		t.Fatal("cancelled process must return an error")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded termination took %s", elapsed)
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
		}
	}
}
