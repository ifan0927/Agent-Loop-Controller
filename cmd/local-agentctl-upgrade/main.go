package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/localupgrade"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	if args[0] == "successor-recovery-preview" || args[0] == "successor-recover-prepare" {
		return errors.New("managed-upgrade database relocation recovery is retired; replace the complete disposable runtime")
	}
	manager, err := localupgrade.NewManager()
	if err != nil {
		return err
	}
	return runWithManager(context.Background(), args, manager, os.Stdout)
}

type upgradeManager interface {
	Prepare(context.Context, localupgrade.PrepareRequest) (localupgrade.Result, error)
	PrepareSuccessor(context.Context, localupgrade.SuccessorPrepareRequest) (localupgrade.Result, error)
	Status(context.Context, string) (localupgrade.Result, error)
	Replace(context.Context, string, bool) (localupgrade.Result, error)
	Rollback(context.Context, string) (localupgrade.Result, error)
	AuthorizeBootstrap(context.Context, string) (localupgrade.Result, error)
	Observe(context.Context, string) (localupgrade.Result, error)
	Cleanup(context.Context, string) (localupgrade.Result, error)
}

func runWithManager(ctx context.Context, args []string, manager upgradeManager, output io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	var result localupgrade.Result
	var response any
	var err error
	switch args[0] {
	case "prepare":
		flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
		revision := flags.String("revision", "", "exact full Git commit")
		supervisor := flags.String("supervisor", "", "launchagent or launchdaemon")
		binary := flags.String("binary", "", "absolute installed binary path")
		config := flags.String("config", "", "absolute Controller configuration path")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return usageError()
		}
		prepareCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
		defer cancel()
		result, err = manager.Prepare(prepareCtx, localupgrade.PrepareRequest{Revision: *revision, Supervisor: *supervisor, BinaryPath: *binary, ConfigPath: *config})
	case "successor-prepare":
		flags := flag.NewFlagSet("successor-prepare", flag.ContinueOnError)
		id := flags.String("upgrade-id", "", "predecessor managed upgrade identifier")
		revision := flags.String("revision", "", "exact full Git commit")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *id == "" || *revision == "" {
			return errors.New("successor-prepare requires --upgrade-id and --revision")
		}
		prepareCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
		defer cancel()
		result, err = manager.PrepareSuccessor(prepareCtx, localupgrade.SuccessorPrepareRequest{PredecessorUpgradeID: *id, Revision: *revision})
	case "status":
		id, parseErr := parseUpgradeID("status", args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = manager.Status(ctx, id)
	case "replace":
		flags := flag.NewFlagSet("replace", flag.ContinueOnError)
		id := flags.String("upgrade-id", "", "managed upgrade identifier")
		confirmed := flags.Bool("full-backup-confirmed", false, "confirm an external encrypted full backup exists")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *id == "" || !*confirmed {
			return errors.New("replace requires --upgrade-id and --full-backup-confirmed")
		}
		result, err = manager.Replace(ctx, *id, *confirmed)
	case "rollback":
		id, parseErr := parseUpgradeID("rollback", args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = manager.Rollback(ctx, id)
	case "authorize-bootstrap":
		id, parseErr := parseUpgradeID("authorize-bootstrap", args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = manager.AuthorizeBootstrap(ctx, id)
	case "observe":
		id, parseErr := parseUpgradeID("observe", args[1:])
		if parseErr != nil {
			return parseErr
		}
		observeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		result, err = manager.Observe(observeCtx, id)
	case "cleanup":
		id, parseErr := parseUpgradeID("cleanup", args[1:])
		if parseErr != nil {
			return parseErr
		}
		result, err = manager.Cleanup(ctx, id)
	default:
		return usageError()
	}
	if err != nil {
		return err
	}
	if response == nil {
		response = result
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(response)
}

func parseUpgradeID(name string, args []string) (string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	id := flags.String("upgrade-id", "", "managed upgrade identifier")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *id == "" {
		return "", fmt.Errorf("%s requires --upgrade-id", name)
	}
	return *id, nil
}

func usageError() error {
	return errors.New("usage: local-agentctl-upgrade.sh <prepare|successor-prepare|status|replace|rollback|authorize-bootstrap|observe|cleanup> [options]")
}
