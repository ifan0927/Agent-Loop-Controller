package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "plan":
		err = plan(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func plan(args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	taskPath := flags.String("task", "", "path to a CodingTask JSON snapshot")
	workspace := flags.String("workspace", "", "absolute dedicated worktree path")
	artifacts := flags.String("artifacts", "", "absolute run artifact directory")
	codexBinary := flags.String("codex-binary", "codex", "Codex CLI binary")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *taskPath == "" {
		return fmt.Errorf("--task is required")
	}

	file, err := os.Open(*taskPath)
	if err != nil {
		return fmt.Errorf("open task: %w", err)
	}
	defer file.Close()

	task, err := decodeTask(file)
	if err != nil {
		return fmt.Errorf("decode task: %w", err)
	}

	deliveryPlan, err := application.NewPlanner(*codexBinary).Build(task, *workspace, *artifacts)
	if err != nil {
		return err
	}
	output, err := json.MarshalIndent(deliveryPlan, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plan: %w", err)
	}
	fmt.Println(string(output))
	return nil
}

func decodeTask(reader io.Reader) (domain.CodingTask, error) {
	var task domain.CodingTask
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&task); err != nil {
		return domain.CodingTask{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return domain.CodingTask{}, fmt.Errorf("task input must contain exactly one JSON value")
		}
		return domain.CodingTask{}, fmt.Errorf("unexpected trailing data: %w", err)
	}
	return task, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ifan-loop <version|plan> [options]")
}
