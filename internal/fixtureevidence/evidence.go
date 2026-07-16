package fixtureevidence

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
)

const (
	Marker              = "IFAN_FIXTURE_EVIDENCE "
	ConfigurationDigest = "de01621c17c5d7e03a3ce490bb9d18a8748d51dc4479e9179e635cecd7b48e44"
)

var safeValue = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,160}$`)

type Evidence struct {
	Scenario                   string   `json:"scenario"`
	ConfigurationDigest        string   `json:"configuration_digest"`
	RunIDs                     []string `json:"run_ids"`
	IssueIdentifiers           []string `json:"issue_identifiers"`
	EventActionKeys            []string `json:"event_action_keys"`
	StateSequence              []string `json:"state_sequence"`
	RetryAbandonOutcomes       []string `json:"retry_abandon_outcomes"`
	LeaseEvidence              []string `json:"lease_evidence"`
	CleanupResultClasses       []string `json:"cleanup_result_classes"`
	CandidateOrderingDecisions []string `json:"candidate_ordering_decisions"`
	ExactCandidateBindings     []string `json:"exact_candidate_bindings"`
	FinalWorkerState           string   `json:"final_worker_state"`
}

type Logger interface {
	Helper()
	Logf(string, ...any)
}

func Emit(log Logger, evidence Evidence) {
	log.Helper()
	if evidence.ConfigurationDigest == "" {
		evidence.ConfigurationDigest = ConfigurationDigest
	}
	if err := evidence.Validate(); err != nil {
		panic(err)
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		panic(err)
	}
	log.Logf("%s%s", Marker, raw)
}

func (e Evidence) Validate() error {
	allowed := map[string]bool{"indefinite_restart": true, "park_notify_retry_resume": true, "abandon_complete": true, "abandon_residue": true, "candidate_ordering_handoff": true, "notification_provenance_safety": true}
	if !allowed[e.Scenario] || e.ConfigurationDigest != ConfigurationDigest || e.FinalWorkerState == "" {
		return errors.New("fixture evidence authority is invalid")
	}
	values := [][]string{e.RunIDs, e.IssueIdentifiers, e.EventActionKeys, e.StateSequence, e.RetryAbandonOutcomes, e.LeaseEvidence, e.CleanupResultClasses, e.CandidateOrderingDecisions, e.ExactCandidateBindings, []string{e.FinalWorkerState}}
	for _, group := range values {
		for _, value := range group {
			lower := strings.ToLower(value)
			if !safeValue.MatchString(value) || strings.ContainsAny(value, `/\`) || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "private") {
				return errors.New("fixture evidence contains an unsafe value")
			}
		}
	}
	return nil
}

type Summary struct {
	SchemaVersion       int        `json:"schema_version"`
	FixtureID           string     `json:"fixture_id"`
	ConfigurationDigest string     `json:"configuration_digest"`
	FinalWorkerState    string     `json:"final_worker_state"`
	ExternalBoundaries  string     `json:"external_boundaries"`
	Scenarios           []Evidence `json:"scenarios"`
}

func Aggregate(records []Evidence) (Summary, error) {
	merged := map[string]Evidence{}
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return Summary{}, err
		}
		current := merged[record.Scenario]
		current.Scenario, current.ConfigurationDigest, current.FinalWorkerState = record.Scenario, ConfigurationDigest, record.FinalWorkerState
		merge := func(target *[]string, values []string) { *target = append(*target, values...) }
		merge(&current.RunIDs, record.RunIDs)
		merge(&current.IssueIdentifiers, record.IssueIdentifiers)
		merge(&current.EventActionKeys, record.EventActionKeys)
		merge(&current.StateSequence, record.StateSequence)
		merge(&current.RetryAbandonOutcomes, record.RetryAbandonOutcomes)
		merge(&current.LeaseEvidence, record.LeaseEvidence)
		merge(&current.CleanupResultClasses, record.CleanupResultClasses)
		merge(&current.CandidateOrderingDecisions, record.CandidateOrderingDecisions)
		merge(&current.ExactCandidateBindings, record.ExactCandidateBindings)
		merged[record.Scenario] = current
	}
	if len(merged) != 6 {
		return Summary{}, errors.New("fixture evidence matrix is incomplete")
	}
	result := Summary{SchemaVersion: 1, FixtureID: "continuous-supervisor-v1", ConfigurationDigest: ConfigurationDigest, FinalWorkerState: "stopped", ExternalBoundaries: "deterministic_fakes"}
	for _, record := range merged {
		for _, values := range []*[]string{&record.RunIDs, &record.IssueIdentifiers, &record.EventActionKeys, &record.StateSequence, &record.RetryAbandonOutcomes, &record.LeaseEvidence, &record.CleanupResultClasses, &record.CandidateOrderingDecisions, &record.ExactCandidateBindings} {
			slices.Sort(*values)
			*values = slices.Compact(*values)
		}
		if err := record.Validate(); err != nil {
			return Summary{}, err
		}
		if err := validateCompleteScenario(record); err != nil {
			return Summary{}, err
		}
		result.Scenarios = append(result.Scenarios, record)
	}
	slices.SortFunc(result.Scenarios, func(left, right Evidence) int { return strings.Compare(left.Scenario, right.Scenario) })
	return result, nil
}

func validateCompleteScenario(record Evidence) error {
	if record.FinalWorkerState != "stopped" || len(record.RunIDs) == 0 || len(record.IssueIdentifiers) == 0 || len(record.StateSequence) == 0 {
		return errors.New("fixture scenario lacks bound execution evidence")
	}
	require := func(ok bool) error {
		if !ok {
			return errors.New("fixture scenario evidence is incomplete")
		}
		return nil
	}
	switch record.Scenario {
	case "indefinite_restart":
		return require(len(record.StateSequence) >= 6 && len(record.LeaseEvidence) > 0 && len(record.ExactCandidateBindings) > 0)
	case "park_notify_retry_resume":
		return require(len(record.EventActionKeys) >= 2 && len(record.StateSequence) >= 7 && len(record.RetryAbandonOutcomes) >= 3 && len(record.LeaseEvidence) > 0 && len(record.ExactCandidateBindings) >= 3)
	case "abandon_complete", "abandon_residue":
		return require(len(record.EventActionKeys) > 0 && len(record.RetryAbandonOutcomes) > 0 && len(record.LeaseEvidence) > 0 && len(record.CleanupResultClasses) > 0)
	case "candidate_ordering_handoff":
		return require(len(record.RunIDs) >= 3 && len(record.IssueIdentifiers) >= 3 && len(record.EventActionKeys) > 0 && len(record.RetryAbandonOutcomes) > 0 && len(record.LeaseEvidence) > 0 && len(record.CandidateOrderingDecisions) >= 4 && len(record.ExactCandidateBindings) > 0)
	case "notification_provenance_safety":
		return require(len(record.EventActionKeys) > 0 && len(record.RetryAbandonOutcomes) > 0 && len(record.LeaseEvidence) > 0 && len(record.CleanupResultClasses) >= 4 && len(record.ExactCandidateBindings) > 0)
	default:
		return errors.New("unknown fixture scenario")
	}
}
