package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type ReconciliationStatus string

const (
	ReconciliationPending        ReconciliationStatus = "pending"
	ReconciliationPass           ReconciliationStatus = "pass"
	ReconciliationActionable     ReconciliationStatus = "actionable_failure"
	ReconciliationInfrastructure ReconciliationStatus = "infrastructure_failure"
	ReconciliationTimeout        ReconciliationStatus = "timeout"
)

type PullRequest struct {
	Number       int64     `json:"number"`
	DatabaseID   int64     `json:"database_id,omitempty"`
	URL          string    `json:"url"`
	NodeID       string    `json:"node_id"`
	HeadBranch   string    `json:"head_branch"`
	BaseBranch   string    `json:"base_branch"`
	HeadSHA      string    `json:"head_sha"`
	BaseSHA      string    `json:"base_sha"`
	BodyDigest   string    `json:"body_digest"`
	OwnershipKey string    `json:"ownership_key"`
	State        string    `json:"state"`
	Merged       bool      `json:"merged"`
	MergeSHA     string    `json:"merge_sha"`
	MergedAt     time.Time `json:"merged_at,omitempty"`
}

func (p PullRequest) ValidateOwnership(branch, base, head, ownershipKey string) error {
	if p.HeadBranch != branch || p.BaseBranch != base || p.HeadSHA != head {
		return errors.New("pull request head/base evidence does not match the run")
	}
	if strings.TrimSpace(ownershipKey) == "" || p.OwnershipKey != ownershipKey {
		return errors.New("pull request lacks controller ownership evidence")
	}
	if p.Number < 1 || strings.TrimSpace(p.NodeID) == "" || strings.TrimSpace(p.BodyDigest) == "" || strings.TrimSpace(p.BaseSHA) == "" {
		return errors.New("pull request identity evidence is incomplete")
	}
	return nil
}

type Check struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	ObservedSHA string `json:"observed_sha"`
}

type ExternalFinding struct {
	SourceID string `json:"source_id"`
	ThreadID string `json:"thread_id,omitempty"`
	Source   string `json:"source"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
	Resolved bool   `json:"resolved"`
	Outdated bool   `json:"outdated"`
}

func (f ExternalFinding) Validate() error {
	if strings.TrimSpace(f.SourceID) == "" || strings.TrimSpace(f.Source) == "" || strings.TrimSpace(f.Body) == "" {
		return errors.New("review finding identity, source, and body are required")
	}
	if f.Line < 0 || f.Line > 0 && strings.TrimSpace(f.File) == "" {
		return errors.New("review finding line requires a file")
	}
	return nil
}

type ReviewSnapshot struct {
	HeadSHA        string            `json:"head_sha"`
	RequiredChecks []string          `json:"required_checks"`
	Checks         []Check           `json:"checks"`
	Findings       []ExternalFinding `json:"findings"`
	ObservedAt     time.Time         `json:"observed_at"`
	UnknownEvents  []string          `json:"unknown_events,omitempty"`
}

func (s ReviewSnapshot) Classify() ReconciliationStatus {
	if strings.TrimSpace(s.HeadSHA) == "" || len(s.RequiredChecks) == 0 {
		return ReconciliationInfrastructure
	}
	required := make(map[string]bool, len(s.RequiredChecks))
	for _, name := range s.RequiredChecks {
		if strings.TrimSpace(name) == "" {
			return ReconciliationInfrastructure
		}
		required[name] = false
	}
	for _, check := range s.Checks {
		if !check.Required {
			continue
		}
		if check.ObservedSHA != s.HeadSHA {
			return ReconciliationInfrastructure
		}
		if _, ok := required[check.Name]; !ok {
			continue
		}
		required[check.Name] = true
		switch check.Status {
		case "queued", "in_progress", "pending", "requested", "waiting":
			return ReconciliationPending
		}
		switch check.Conclusion {
		case "success", "neutral", "skipped":
		case "failure", "action_required":
			return ReconciliationActionable
		case "cancelled", "timed_out":
			return ReconciliationInfrastructure
		default:
			return ReconciliationPending
		}
	}
	for _, observed := range required {
		if !observed {
			return ReconciliationInfrastructure
		}
	}
	for _, finding := range s.Findings {
		if !finding.Resolved && !finding.Outdated {
			return ReconciliationActionable
		}
	}
	return ReconciliationPass
}

type HumanApproval struct {
	PRNumber         int64         `json:"pr_number"`
	Approver         string        `json:"approver"`
	Actor            ActorIdentity `json:"actor"`
	ReviewDatabaseID int64         `json:"review_database_id"`
	ReviewNodeID     string        `json:"review_node_id"`
	Source           string        `json:"source"`
	ApprovedSHA      string        `json:"approved_sha"`
	CIStatus         string        `json:"ci_status"`
	ReviewSHA        string        `json:"internal_review_sha"`
	ApprovedAt       time.Time     `json:"source_timestamp"`
	ObservedAt       time.Time     `json:"observation_timestamp"`
}

func (a HumanApproval) Authorizes(pr PullRequest, head string) error {
	if a.Source != "github_pull_request_review" || a.Actor.Type != "User" || a.Actor.DatabaseID < 1 || strings.TrimSpace(a.Actor.NodeID) == "" || strings.TrimSpace(a.Actor.Login) == "" || a.ReviewDatabaseID < 1 || strings.TrimSpace(a.ReviewNodeID) == "" {
		return errors.New("human approval lacks immutable trusted review identity")
	}
	if !strings.EqualFold(a.Approver, a.Actor.Login) || a.PRNumber != pr.Number || a.ApprovedSHA != head || a.ReviewSHA != head || a.ApprovedAt.IsZero() || a.ObservedAt.IsZero() {
		return errors.New("human approval is not bound to the exact PR head")
	}
	if a.CIStatus != "pass" {
		return errors.New("human approval lacks passing automated evidence")
	}
	return nil
}

func PRBody(task CodingTask, validation, internalReview, ownershipKey string) (string, error) {
	if !strings.HasPrefix(task.IssueID, "IFAN-") {
		return "", fmt.Errorf("issue %q cannot produce a Linear magic word", task.IssueID)
	}
	return fmt.Sprintf("## Summary\n\n%s\n\n## Scope and rationale\n\n%s\n\n## Validation\n\n%s\n\n## Fresh internal review\n\n%s\n\n## Out of scope\n\n%s\n\nFixes %s\n\n<!-- controller-run:%s -->\n",
		task.Goal, task.Description, validation, internalReview, strings.Join(task.OutOfScope, "\n- "), task.IssueID, ownershipKey), nil
}
