package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type ReadinessProfile struct {
	ProfileDigest           string
	RepositoryBindingDigest string
	SourcePath              string
	OriginPath              string
	BaseBranch              string
}

type ReadinessOutputRunner interface {
	Output(context.Context, string, ...string) ([]byte, error)
}

type OSReadinessOutputRunner struct{}

func (OSReadinessOutputRunner) Output(ctx context.Context, program string, args ...string) ([]byte, error) {
	if program == "" {
		return nil, errors.New("readiness program is required")
	}
	return exec.CommandContext(ctx, program, args...).Output()
}

type ReadinessObserver struct {
	Runner ReadinessOutputRunner
	Now    func() time.Time
}

func (o ReadinessObserver) ObserveRepositoryGit(ctx context.Context, profile ReadinessProfile) ([2]domain.RepositoryDimensionResult, error) {
	runner := o.Runner
	if runner == nil {
		runner = OSReadinessOutputRunner{}
	}
	now := time.Now().UTC()
	if o.Now != nil {
		now = o.Now().UTC()
	}
	local := repositoryGitResult(domain.ReadinessLocalCheckout, domain.RepositoryReady, "local_checkout_ready", profile, now)
	base := repositoryGitResult(domain.ReadinessBaseBranch, domain.RepositoryReady, "base_branch_ready", profile, now)
	inside, err := runner.Output(ctx, "git", "-C", profile.SourcePath, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		local = repositoryGitResult(domain.ReadinessLocalCheckout, domain.RepositoryNotReady, "local_checkout_missing", profile, now)
		base = repositoryGitResult(domain.ReadinessBaseBranch, domain.RepositoryUnknown, "local_checkout_unavailable", profile, now)
		return [2]domain.RepositoryDimensionResult{local, base}, nil
	}
	remote, err := runner.Output(ctx, "git", "-C", profile.SourcePath, "remote", "get-url", "origin")
	if err != nil {
		local = repositoryGitResult(domain.ReadinessLocalCheckout, domain.RepositoryNotReady, "origin_binding_missing", profile, now)
	} else if strings.TrimSpace(string(remote)) != profile.OriginPath {
		local = repositoryGitResult(domain.ReadinessLocalCheckout, domain.RepositoryConflict, "origin_binding_conflict", profile, now)
	}
	if _, err := runner.Output(ctx, "git", "-C", profile.SourcePath, "show-ref", "--verify", "--quiet", "refs/heads/"+profile.BaseBranch); err != nil {
		base = repositoryGitResult(domain.ReadinessBaseBranch, domain.RepositoryNotReady, "base_branch_missing", profile, now)
	}
	return [2]domain.RepositoryDimensionResult{local, base}, nil
}

func repositoryGitResult(dimension domain.RepositoryReadinessDimension, status domain.RepositoryReadinessStatus, reason string, profile ReadinessProfile, at time.Time) domain.RepositoryDimensionResult {
	identity := ""
	if dimension == domain.ReadinessBaseBranch {
		identity = profile.BaseBranch
	}
	return domain.RepositoryDimensionResult{Dimension: dimension, Status: status, ReasonCode: reason, Identity: identity, EvidenceDigest: readinessDigest("repository-git-readiness-v1", profile.ProfileDigest, profile.RepositoryBindingDigest, string(dimension), string(status), reason, identity), ObservedAt: at}
}

func readinessDigest(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
