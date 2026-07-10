package codex

import (
	"context"
	"strings"
	"testing"

	processadapter "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/process"
)

type preflightProcess struct {
	missing   string
	extraHelp string
}

func (p preflightProcess) Run(_ context.Context, spec processadapter.Spec) (processadapter.Result, error) {
	if len(spec.Args) == 1 && spec.Args[0] == "--version" {
		return processadapter.Result{Stdout: []byte("codex-cli test\n")}, nil
	}
	lines := make([]string, 0, len(requiredExecFlags))
	for _, flag := range requiredExecFlags {
		lines = append(lines, "      "+flag)
	}
	help := strings.Join(lines, "\n")
	help = strings.ReplaceAll(help, p.missing, "")
	help += "\n" + p.extraHelp
	return processadapter.Result{Stdout: []byte(help)}, nil
}

func TestPreflightRequiresExactDeclaredOption(t *testing.T) {
	process := preflightProcess{missing: "--sandbox"}
	process.extraHelp = "      --sandbox-policy <MODE>\n          description mentions --sandbox"
	_, err := NewPreflighter(process, "codex").Run(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--sandbox") {
		t.Fatalf("error = %v", err)
	}
}

func TestPreflightFailsClosedOnMissingCapability(t *testing.T) {
	_, err := NewPreflighter(preflightProcess{missing: "--ephemeral"}, "codex").Run(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--ephemeral") {
		t.Fatalf("error = %v", err)
	}
}
