package application

import (
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
	"testing"
)

func TestReconcileGitHubReadOwnershipAndSHAs(t *testing.T) {
	repo := domain.RepositoryIdentity{ID: 1, NodeID: "R", Owner: "o", Name: "r"}
	pr := domain.PullRequest{Number: 2, DatabaseID: 3, NodeID: "P", URL: "u", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", OwnershipKey: "key", BodyDigest: "body"}
	got := domain.GitHubReadEvidence{Repository: repo, PullRequest: pr}
	if err := ReconcileGitHubRead(repo, pr, "feature", "main", "head", "base", "key", "body", got); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*domain.GitHubReadEvidence){func(e *domain.GitHubReadEvidence) { e.Repository.ID = 9 }, func(e *domain.GitHubReadEvidence) { e.PullRequest.DatabaseID = 9 }, func(e *domain.GitHubReadEvidence) { e.PullRequest.HeadSHA = "wrong" }, func(e *domain.GitHubReadEvidence) { e.PullRequest.BaseSHA = "wrong" }, func(e *domain.GitHubReadEvidence) { e.PullRequest.OwnershipKey = "wrong" }, func(e *domain.GitHubReadEvidence) { e.PullRequest.BodyDigest = "wrong" }}
	for i, mutate := range mutations {
		copy := got
		mutate(&copy)
		if err := ReconcileGitHubRead(repo, pr, "feature", "main", "head", "base", "key", "body", copy); err == nil {
			t.Fatalf("mutation %d accepted", i)
		}
	}
}

func TestReconcileGitHubReadAcceptsCaseInsensitiveRepositoryCoordinates(t *testing.T) {
	expectedRepository := domain.RepositoryIdentity{ID: 1, NodeID: "R", Owner: "ifan0927", Name: "looptest"}
	gotRepository := domain.RepositoryIdentity{ID: 1, NodeID: "R", Owner: "IFAN0927", Name: "LoopTest"}
	pr := domain.PullRequest{Number: 2, DatabaseID: 3, NodeID: "P", URL: "u", HeadBranch: "feature", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base", OwnershipKey: "key", BodyDigest: "body"}
	if err := ReconcileGitHubRead(expectedRepository, pr, "feature", "main", "head", "base", "key", "body", domain.GitHubReadEvidence{Repository: gotRepository, PullRequest: pr}); err != nil {
		t.Fatal(err)
	}
}
