package application

import (
	"context"
	"time"
)

// LinearIssueReader fetches one authoritative issue without making Linear
// mutations. Admission policy belongs to the application service that consumes
// this source record.
type LinearIssueReader interface {
	ReadIssue(context.Context, string) (LinearTaskSource, []LinearRequestObservation, error)
}

// LinearTodoCandidateScanner performs a bounded, read-only pre-admission scan.
// Its output is deliberately insufficient to admit or start an issue: callers
// must later use LinearIssueReader and the full admission contract.
type LinearTodoCandidateScanner interface {
	ListTodoCandidates(context.Context, LinearTodoCandidateAuthority) (LinearTodoCandidateScan, []LinearRequestObservation, error)
}

// LinearTodoCandidateAuthority is the immutable workflow authority validated
// from controller configuration before a scan is attempted.
type LinearTodoCandidateAuthority struct {
	TeamID          string
	TeamKey         string
	TodoState       LinearState
	InProgressState LinearState
	MaxCandidates   int
	MaxPages        int
}

// LinearTodoCandidate contains only metadata needed by a later, separate
// selection policy. It intentionally omits all untrusted issue prose.
type LinearTodoCandidate struct {
	IssueID          string        `json:"issue_id"`
	Identifier       string        `json:"identifier"`
	Priority         int           `json:"priority"`
	State            LinearState   `json:"state"`
	Cycle            LinearCycle   `json:"cycle"`
	Labels           []LinearLabel `json:"labels"`
	RepositoryLabels []LinearLabel `json:"repository_labels"`
	BranchName       string        `json:"branch_name"`
	SourceRevision   string        `json:"source_revision"`
	SourceDigest     string        `json:"source_digest"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

// LinearTodoCandidateScan is a deterministic, sanitized scan snapshot. The
// candidate order is canonical by immutable issue identity, not a selection
// decision or priority policy.
type LinearTodoCandidateScan struct {
	Candidates []LinearTodoCandidate `json:"candidates"`
	Digest     string                `json:"digest"`
	ObservedAt time.Time             `json:"observation_timestamp"`
}

// LinearTaskSource is the sanitized, controller-owned representation of a
// Linear issue before admission freezes it into a CodingTask snapshot.
type LinearTaskSource struct {
	Provider       string        `json:"provider"`
	IssueID        string        `json:"issue_id"`
	Identifier     string        `json:"identifier"`
	URL            string        `json:"url"`
	Title          string        `json:"title"`
	Description    string        `json:"description"`
	Team           LinearTeam    `json:"team"`
	State          LinearState   `json:"state"`
	Labels         []LinearLabel `json:"labels"`
	Cycle          LinearCycle   `json:"cycle"`
	BranchName     string        `json:"branch_name"`
	SourceRevision string        `json:"source_revision"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	ObservedAt     time.Time     `json:"observation_timestamp"`
}

type LinearTeam struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

type LinearState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type LinearLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type LinearCycle struct {
	ID       string    `json:"id"`
	Number   int       `json:"number"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	IsActive bool      `json:"is_active"`
}

// LinearRequestObservation contains bounded metadata only. It deliberately
// excludes Authorization values, request variables, and response bodies.
type LinearRequestObservation struct {
	Operation          string    `json:"operation"`
	Page               int       `json:"page,omitempty"`
	Count              int       `json:"count,omitempty"`
	HTTPStatus         int       `json:"http_status"`
	RequestID          string    `json:"request_id,omitempty"`
	RateLimitLimit     int       `json:"rate_limit_limit,omitempty"`
	RateLimitRemaining int       `json:"rate_limit_remaining,omitempty"`
	RateLimitReset     time.Time `json:"rate_limit_reset,omitempty"`
	ResponseDigest     string    `json:"response_digest"`
	ErrorClass         string    `json:"error_class,omitempty"`
	ObservedAt         time.Time `json:"observation_timestamp"`
}
