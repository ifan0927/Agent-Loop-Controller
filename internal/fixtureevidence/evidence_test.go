package fixtureevidence

import "testing"

func TestAggregateRejectsIncompleteAndUnsafeFixtureEvidence(t *testing.T) {
	complete := completeEvidenceMatrix()
	if _, err := Aggregate(complete); err != nil {
		t.Fatalf("complete matrix: %v", err)
	}

	incomplete := append([]Evidence(nil), complete[:len(complete)-1]...)
	if _, err := Aggregate(incomplete); err == nil {
		t.Fatal("incomplete matrix was accepted")
	}

	unsafe := append([]Evidence(nil), complete...)
	unsafe[0].RunIDs = []string{"/private/run"}
	if _, err := Aggregate(unsafe); err == nil {
		t.Fatal("private path was accepted")
	}

	missingBinding := append([]Evidence(nil), complete...)
	missingBinding[1].ExactCandidateBindings = nil
	if _, err := Aggregate(missingBinding); err == nil {
		t.Fatal("scenario-specific evidence gap was accepted")
	}
}

func completeEvidenceMatrix() []Evidence {
	common := func(scenario string) Evidence {
		return Evidence{Scenario: scenario, ConfigurationDigest: ConfigurationDigest, RunIDs: []string{"run-1"}, IssueIdentifiers: []string{"IFAN-1"}, StateSequence: []string{"stopped"}, FinalWorkerState: "stopped"}
	}
	restart := common("indefinite_restart")
	restart.StateSequence = []string{"polling", "driving", "deadline", "sigterm", "stopped", "restarted"}
	restart.LeaseEvidence, restart.ExactCandidateBindings = []string{"released"}, []string{"same_run"}
	retry := common("park_notify_retry_resume")
	retry.StateSequence = []string{"parked", "notified", "retried", "resumed", "verified", "reviewed", "stopped"}
	retry.EventActionKeys, retry.RetryAbandonOutcomes, retry.LeaseEvidence, retry.ExactCandidateBindings = []string{"event-1", "action-1"}, []string{"observed", "replayed", "resumed"}, []string{"reacquired"}, []string{"same_run", "auth_required", "exact_head"}
	complete := common("abandon_complete")
	complete.EventActionKeys, complete.RetryAbandonOutcomes, complete.LeaseEvidence, complete.CleanupResultClasses = []string{"action-1"}, []string{"abandoned"}, []string{"released"}, []string{"complete"}
	residue := common("abandon_residue")
	residue.EventActionKeys, residue.RetryAbandonOutcomes, residue.LeaseEvidence, residue.CleanupResultClasses = []string{"event-1"}, []string{"terminal"}, []string{"released"}, []string{"failed"}
	ordering := common("candidate_ordering_handoff")
	ordering.RunIDs, ordering.IssueIdentifiers = []string{"run-1", "run-2", "run-3"}, []string{"IFAN-1", "IFAN-2", "IFAN-3"}
	ordering.EventActionKeys, ordering.RetryAbandonOutcomes, ordering.LeaseEvidence = []string{"event-1"}, []string{"handoff"}, []string{"acquired"}
	ordering.CandidateOrderingDecisions, ordering.ExactCandidateBindings = []string{"priority", "sequence", "uuid", "permutation"}, []string{"distinct_runs"}
	safety := common("notification_provenance_safety")
	safety.EventActionKeys, safety.RetryAbandonOutcomes, safety.LeaseEvidence = []string{"event-1"}, []string{"deduplicated"}, []string{"released"}
	safety.CleanupResultClasses, safety.ExactCandidateBindings = []string{"database", "logs", "cli", "artifacts"}, []string{"stable_key"}
	return []Evidence{restart, retry, complete, residue, ordering, safety}
}
