package localissue

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type testRegistry struct{ verifier bool }

func (testRegistry) HasRepository(label string) bool { return label == "repo:test-project" }
func (r testRegistry) HasVerifier(_, id string) bool { return r.verifier && id == "fixture-go-test" }

func TestAdmitSimulatedIssue(t *testing.T) {
	issue := validIssue()
	raw, _ := json.Marshal(issue)
	snapshot, err := Admit(issue, raw, testRegistry{verifier: true})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Task.Repository != issue.RepositoryLabel || snapshot.TaskHash == "" || snapshot.RawHash == "" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if !strings.Contains(snapshot.Task.Description, "Supplemental specification") {
		t.Fatal("comments were not normalized into the immutable task")
	}
}

func TestAdmissionRejectsEligibilityAndUnknownVerifier(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Issue)
		reg  testRegistry
	}{
		{"wrong team", func(i *Issue) { i.Team = "OTHER" }, testRegistry{true}},
		{"hermes", func(i *Issue) { i.Labels = append(i.Labels, "agent:hermes") }, testRegistry{true}},
		{"not current", func(i *Issue) { i.CurrentCycle = false }, testRegistry{true}},
		{"unsafe branch", func(i *Issue) { i.BranchName = "--bad" }, testRegistry{true}},
		{"unknown verifier", func(*Issue) {}, testRegistry{false}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issue := validIssue()
			test.edit(&issue)
			raw, _ := json.Marshal(issue)
			if _, err := Admit(issue, raw, test.reg); err == nil {
				t.Fatal("expected admission rejection")
			}
		})
	}
}

func validIssue() Issue {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	return Issue{IssueID: "IFAN-LAB-1", Title: "Add Clamp", Description: "Add a pure function.", Team: "IFAN",
		Labels: []string{"agent:codex", "repo:test-project"}, Status: "Todo", CurrentCycle: true, CycleID: "lab-cycle",
		RepositoryLabel: "repo:test-project", BaseBranch: "main", BranchName: "ifan/ifan-lab-1-clamp",
		Goal: "Add Clamp", AcceptanceCriteria: []string{"Tests pass"}, OutOfScope: []string{"Network"},
		VerifierIDs: []string{"fixture-go-test"}, SourceRevision: "v1", CreatedAt: now, UpdatedAt: now,
		Comments: []string{"Keep it deterministic."}}
}
