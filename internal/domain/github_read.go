package domain

import (
	"strings"
	"time"
)

type RepositoryIdentity struct {
	ID     int64  `json:"database_id"`
	NodeID string `json:"node_id"`
	Owner  string `json:"owner"`
	Name   string `json:"name"`
}

type ActorIdentity struct {
	DatabaseID int64  `json:"database_id"`
	NodeID     string `json:"node_id"`
	Login      string `json:"login"`
	Type       string `json:"type"`
	AppID      int64  `json:"app_id,omitempty"`
}

type CheckState string

const (
	CheckQueued         CheckState = "queued"
	CheckInProgress     CheckState = "in_progress"
	CheckPending        CheckState = "pending"
	CheckRequested      CheckState = "requested"
	CheckWaiting        CheckState = "waiting"
	CheckSuccess        CheckState = "success"
	CheckNeutral        CheckState = "neutral"
	CheckSkipped        CheckState = "skipped"
	CheckFailure        CheckState = "failure"
	CheckActionRequired CheckState = "action_required"
	CheckCancelled      CheckState = "cancelled"
	CheckTimedOut       CheckState = "timed_out"
	CheckStale          CheckState = "stale"
	CheckUnknown        CheckState = "unknown"
)

type GitHubCheck struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Required    bool       `json:"required"`
	Source      string     `json:"source"`
	State       CheckState `json:"state"`
	ObservedSHA string     `json:"observed_sha"`
	SourceAt    time.Time  `json:"source_timestamp"`
	ObservedAt  time.Time  `json:"observation_timestamp"`
}

type CodeRabbitState string

const (
	CodeRabbitAbsent         CodeRabbitState = "absent"
	CodeRabbitPending        CodeRabbitState = "pending"
	CodeRabbitPass           CodeRabbitState = "pass"
	CodeRabbitActionable     CodeRabbitState = "actionable_findings"
	CodeRabbitInfrastructure CodeRabbitState = "infrastructure_failure"
	CodeRabbitUntrusted      CodeRabbitState = "untrusted_lookalike"
	CodeRabbitUnknown        CodeRabbitState = "unknown"
)

type NormalizedFinding struct {
	Source         string    `json:"source"`
	SourceID       string    `json:"source_id"`
	ThreadID       string    `json:"thread_id,omitempty"`
	File           string    `json:"file,omitempty"`
	Line           int       `json:"line,omitempty"`
	Classification string    `json:"classification"`
	BodyDigest     string    `json:"body_digest"`
	Resolved       bool      `json:"resolved"`
	Outdated       bool      `json:"outdated"`
	HeadSHA        string    `json:"observed_head_sha"`
	SourceAt       time.Time `json:"source_timestamp"`
	ObservedAt     time.Time `json:"observation_timestamp"`
}

type GitHubReadEvidence struct {
	Repository     RepositoryIdentity  `json:"repository"`
	PullRequest    PullRequest         `json:"pull_request"`
	Checks         []GitHubCheck       `json:"checks"`
	ReviewDecision string              `json:"review_decision"`
	CodeRabbit     CodeRabbitState     `json:"coderabbit_status"`
	Findings       []NormalizedFinding `json:"findings"`
	UnknownEvents  []string            `json:"unknown_telemetry"`
	ObservedAt     time.Time           `json:"observed_at"`
}

func (e GitHubReadEvidence) RequiredChecksStatus() ReconciliationStatus {
	required := 0
	for _, check := range e.Checks {
		if !check.Required {
			continue
		}
		required++
		if check.ObservedSHA != e.PullRequest.HeadSHA {
			return ReconciliationInfrastructure
		}
		switch check.State {
		case CheckSuccess, CheckNeutral, CheckSkipped:
		case CheckQueued, CheckInProgress, CheckPending, CheckRequested, CheckWaiting:
			return ReconciliationPending
		case CheckFailure, CheckActionRequired:
			return ReconciliationActionable
		case CheckCancelled, CheckTimedOut, CheckStale, CheckUnknown:
			return ReconciliationInfrastructure
		default:
			return ReconciliationInfrastructure
		}
	}
	if required == 0 {
		return ReconciliationInfrastructure
	}
	for _, event := range e.UnknownEvents {
		if strings.HasPrefix(event, "missing_required_check:") {
			return ReconciliationInfrastructure
		}
	}
	return ReconciliationPass
}
