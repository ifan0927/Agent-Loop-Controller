package codex

import (
	"context"
	"strings"
	"testing"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
)

type preflightProcess struct {
	missing string
}

func (p preflightProcess) Run(_ context.Context, spec processadapter.Spec) (processadapter.Result, error) {
	if len(spec.Args) == 1 && spec.Args[0] == "--version" {
		return processadapter.Result{Stdout: []byte("codex-cli test\n")}, nil
	}
	help := strings.Join(requiredExecFlags, " ")
	help = strings.ReplaceAll(help, p.missing, "")
	return processadapter.Result{Stdout: []byte(help)}, nil
}

func TestPreflightFailsClosedOnMissingCapability(t *testing.T) {
	_, err := NewPreflighter(preflightProcess{missing: "--ephemeral"}, "codex").Run(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--ephemeral") {
		t.Fatalf("error = %v", err)
	}
}
