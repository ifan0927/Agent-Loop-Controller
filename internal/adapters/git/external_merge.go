package git

import (
	"context"
	"errors"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type ExternalMergeVerifier struct{ Workspace }

type ExternalMergeVerificationRequest struct {
	Repository   string
	SourcePath   string
	OriginPath   string
	BaseBranch   string
	CandidateSHA string
	MergeSHA     string
}

type ExternalMergeVerification struct {
	CandidateSHA string
	MergeSHA     string
	BaseSHA      string
	TreeSHA      string
}

func (v ExternalMergeVerifier) Verify(ctx context.Context, request ExternalMergeVerificationRequest) (ExternalMergeVerification, error) {
	if err := validateSourceSyncRequest(SourceSyncRequest{Repository: request.Repository, SourcePath: request.SourcePath, OriginPath: request.OriginPath, BaseBranch: request.BaseBranch, MergeSHA: request.MergeSHA}); err != nil || domain.ValidateGitBranch(request.BaseBranch) != nil || !fullSHA.MatchString(request.CandidateSHA) || request.CandidateSHA != strings.ToLower(request.CandidateSHA) {
		return ExternalMergeVerification{}, errors.New("external merge Git authority is invalid")
	}
	remote, err := v.run(ctx, request.SourcePath, "remote", "get-url", "origin")
	if err != nil || !sameOriginBinding(strings.TrimSpace(remote), request.OriginPath) {
		return ExternalMergeVerification{}, errors.New("external merge source origin authority mismatch")
	}
	if _, err := v.run(ctx, request.SourcePath, "fetch", "--no-tags", "origin", "refs/heads/"+request.BaseBranch); err != nil {
		return ExternalMergeVerification{}, errors.New("fetch remote base for external merge verification")
	}
	base, err := v.run(ctx, request.SourcePath, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return ExternalMergeVerification{}, errors.New("resolve fetched remote base")
	}
	base = strings.TrimSpace(base)
	for _, sha := range []string{request.CandidateSHA, request.MergeSHA} {
		kind, err := v.run(ctx, request.SourcePath, "cat-file", "-t", sha)
		if err != nil || strings.TrimSpace(kind) != "commit" {
			return ExternalMergeVerification{}, errors.New("external merge commit authority is missing")
		}
	}
	if _, err := v.run(ctx, request.SourcePath, "merge-base", "--is-ancestor", request.MergeSHA, base); err != nil {
		return ExternalMergeVerification{}, errors.New("external merge is not contained by remote base")
	}
	if _, err := v.run(ctx, request.SourcePath, "merge-base", "--is-ancestor", request.CandidateSHA, request.MergeSHA); err != nil {
		return ExternalMergeVerification{}, errors.New("candidate is not contained by external merge")
	}
	candidateTree, err := v.run(ctx, request.SourcePath, "rev-parse", request.CandidateSHA+"^{tree}")
	if err != nil {
		return ExternalMergeVerification{}, errors.New("resolve candidate tree")
	}
	mergeTree, err := v.run(ctx, request.SourcePath, "rev-parse", request.MergeSHA+"^{tree}")
	if err != nil || strings.TrimSpace(candidateTree) != strings.TrimSpace(mergeTree) {
		return ExternalMergeVerification{}, errors.New("external merge tree differs from candidate tree")
	}
	return ExternalMergeVerification{CandidateSHA: request.CandidateSHA, MergeSHA: request.MergeSHA, BaseSHA: base, TreeSHA: strings.TrimSpace(candidateTree)}, nil
}
