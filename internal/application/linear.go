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
	HTTPStatus         int       `json:"http_status"`
	RequestID          string    `json:"request_id,omitempty"`
	RateLimitLimit     int       `json:"rate_limit_limit,omitempty"`
	RateLimitRemaining int       `json:"rate_limit_remaining,omitempty"`
	RateLimitReset     time.Time `json:"rate_limit_reset,omitempty"`
	ResponseDigest     string    `json:"response_digest"`
	ErrorClass         string    `json:"error_class,omitempty"`
	ObservedAt         time.Time `json:"observation_timestamp"`
}
