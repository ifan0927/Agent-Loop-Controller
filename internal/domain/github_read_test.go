package domain

import "testing"

func TestRequiredChecksFailClosed(t *testing.T) {
	base := GitHubReadEvidence{PullRequest: PullRequest{HeadSHA: "h"}, Checks: []GitHubCheck{{Name: "test", Required: true, ObservedSHA: "h", State: CheckSuccess}}}
	if got := base.RequiredChecksStatus(); got != ReconciliationPass {
		t.Fatalf("got %s", got)
	}
	base.Checks[0].State = CheckUnknown
	if got := base.RequiredChecksStatus(); got != ReconciliationInfrastructure {
		t.Fatalf("unknown got %s", got)
	}
	base.Checks = nil
	if got := base.RequiredChecksStatus(); got != ReconciliationInfrastructure {
		t.Fatalf("missing got %s", got)
	}
}
