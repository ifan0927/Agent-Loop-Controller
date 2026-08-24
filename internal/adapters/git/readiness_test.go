package git

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type readinessCall struct {
	program string
	args    []string
}
type readinessRunner struct {
	calls   []readinessCall
	outputs [][]byte
	errors  []error
}

func (r *readinessRunner) Output(_ context.Context, program string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, readinessCall{program, slices.Clone(args)})
	index := len(r.calls) - 1
	return r.outputs[index], r.errors[index]
}

func TestReadinessObserverUsesOnlyReadOnlyGitArgv(t *testing.T) {
	runner := &readinessRunner{outputs: [][]byte{[]byte("true\n"), []byte("https://github.com/owner/repo.git\n"), nil}, errors: make([]error, 3)}
	profile := ReadinessProfile{ProfileDigest: strings.Repeat("a", 64), RepositoryBindingDigest: strings.Repeat("b", 64), SourcePath: "/private/repo", OriginPath: "https://github.com/owner/repo.git", BaseBranch: "main"}
	results, err := (ReadinessObserver{Runner: runner, Now: func() time.Time { return time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC) }}).ObserveRepositoryGit(context.Background(), profile)
	if err != nil || results[0].Status != domain.RepositoryReady || results[1].Status != domain.RepositoryReady {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	want := [][]string{{"-C", profile.SourcePath, "rev-parse", "--is-inside-work-tree"}, {"-C", profile.SourcePath, "remote", "get-url", "origin"}, {"-C", profile.SourcePath, "show-ref", "--verify", "--quiet", "refs/heads/main"}}
	for index, call := range runner.calls {
		if call.program != "git" || !slices.Equal(call.args, want[index]) {
			t.Fatalf("call[%d]=%+v", index, call)
		}
	}
}

func TestReadinessObserverNormalizesMissingCheckoutWithoutFurtherCommands(t *testing.T) {
	runner := &readinessRunner{outputs: [][]byte{nil}, errors: []error{errors.New("raw private path")}}
	profile := ReadinessProfile{ProfileDigest: strings.Repeat("a", 64), RepositoryBindingDigest: strings.Repeat("b", 64), SourcePath: "/private/repo", OriginPath: "secret", BaseBranch: "main"}
	results, err := (ReadinessObserver{Runner: runner}).ObserveRepositoryGit(context.Background(), profile)
	if err != nil || len(runner.calls) != 1 || results[0].ReasonCode != "local_checkout_missing" || results[1].ReasonCode != "local_checkout_unavailable" {
		t.Fatalf("calls=%d results=%+v err=%v", len(runner.calls), results, err)
	}
}
