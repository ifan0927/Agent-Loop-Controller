package git

import (
	"context"
	"testing"
)

func TestExternalMergeVerifierAcceptsExactCandidateTreeAndRejectsDrift(t *testing.T) {
	f := sourceSyncFixture(t)
	runGit(t, f.writer, "switch", "-c", "candidate", f.base)
	writeSyncFile(t, f.writer, "candidate.txt")
	runGit(t, f.writer, "add", "candidate.txt")
	runGit(t, f.writer, "commit", "-m", "candidate")
	candidate := stringOutput(t, f.writer, "rev-parse", "HEAD")
	runGit(t, f.writer, "switch", "main")
	runGit(t, f.writer, "reset", "--hard", f.base)
	runGit(t, f.writer, "merge", "--no-ff", "--no-edit", candidate)
	merge := stringOutput(t, f.writer, "rev-parse", "HEAD")
	runGit(t, f.writer, "push", "--force", "origin", "main")

	request := ExternalMergeVerificationRequest{Repository: "owner/repo", SourcePath: f.source, OriginPath: f.origin, BaseBranch: "main", CandidateSHA: candidate, MergeSHA: merge}
	result, err := (ExternalMergeVerifier{}).Verify(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.CandidateSHA != candidate || result.MergeSHA != merge || result.BaseSHA != merge || result.TreeSHA == "" {
		t.Fatalf("result=%+v", result)
	}

	runGit(t, f.writer, "switch", "candidate")
	writeSyncFile(t, f.writer, "drift.txt")
	runGit(t, f.writer, "add", "drift.txt")
	runGit(t, f.writer, "commit", "-m", "drift")
	drift := stringOutput(t, f.writer, "rev-parse", "HEAD")
	request.CandidateSHA = drift
	if _, err := (ExternalMergeVerifier{}).Verify(context.Background(), request); err == nil {
		t.Fatal("tree drift must not be accepted")
	}
}
