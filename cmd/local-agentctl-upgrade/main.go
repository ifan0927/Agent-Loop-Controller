package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	manager, err := localupgrade.NewManager()
	if err != nil {
		return err
	}
	ctx := context.Background()
	var result localupgrade.Result
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
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || !*confirmed {
			return errors.New("replace requires --upgrade-id and --full-backup-confirmed")
		}
		result, err = manager.Replace(ctx, *id)
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
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
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
	return errors.New("usage: local-agentctl-upgrade.sh <prepare|status|replace|rollback|authorize-bootstrap|observe|cleanup> [options]")
}
