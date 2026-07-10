package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type TriggerAction string

const TriggerActionStart TriggerAction = "start"

type TriggerSignal struct {
	Source      string        `json:"source"`
	IssueID     string        `json:"issue_id"`
	Action      TriggerAction `json:"action"`
	RequestedBy string        `json:"requested_by"`
	RequestedAt time.Time     `json:"requested_at"`
	RequestID   string        `json:"request_id"`
}

type TaskPolicy struct {
	HumanApprovalRequired bool   `json:"human_approval_required"`
	MergeMethod           string `json:"merge_method"`
	MaxRepairAttempts     int    `json:"max_repair_attempts"`
	AllowScopeExpansion   bool   `json:"allow_scope_expansion"`
	CreateDerivedIssues   bool   `json:"create_derived_issues"`
}

type CodingTask struct {
	RunID              string     `json:"run_id"`
	IssueID            string     `json:"issue_id"`
	IssueURL           string     `json:"issue_url"`
	Title              string     `json:"title"`
	Description        string     `json:"description"`
	Repository         string     `json:"repository"`
	BaseBranch         string     `json:"base_branch"`
	WorkingBranch      string     `json:"working_branch"`
	Goal               string     `json:"goal"`
	AcceptanceCriteria []string   `json:"acceptance_criteria"`
	OutOfScope         []string   `json:"out_of_scope"`
	VerifierIDs        []string   `json:"verifier_ids"`
	Policy             TaskPolicy `json:"policy"`
	SourceRevision     string     `json:"source_revision"`
	CreatedAt          time.Time  `json:"created_at"`
}

func (t CodingTask) Validate() error {
	var missing []string
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "run_id", value: t.RunID},
		{name: "issue_id", value: t.IssueID},
		{name: "title", value: t.Title},
		{name: "repository", value: t.Repository},
		{name: "base_branch", value: t.BaseBranch},
		{name: "working_branch", value: t.WorkingBranch},
		{name: "goal", value: t.Goal},
		{name: "source_revision", value: t.SourceRevision},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required task fields: %s", strings.Join(missing, ", "))
	}
	if !validGitBranch(t.BaseBranch) {
		return errors.New("base_branch is not a safe Git branch name")
	}
	if !validGitBranch(t.WorkingBranch) {
		return errors.New("working_branch is not a safe Git branch name")
	}
	if len(t.AcceptanceCriteria) == 0 {
		return errors.New("acceptance_criteria must not be empty")
	}
	if blankItem(t.AcceptanceCriteria) {
		return errors.New("acceptance_criteria items must not be blank")
	}
	if len(t.VerifierIDs) == 0 {
		return errors.New("verifier_ids must not be empty")
	}
	for _, verifierID := range t.VerifierIDs {
		if !validVerifierID(verifierID) {
			return fmt.Errorf("invalid verifier ID: %q", verifierID)
		}
	}
	if !t.Policy.HumanApprovalRequired {
		return errors.New("MVP requires human approval")
	}
	if t.Policy.MergeMethod != "squash" {
		return errors.New("MVP requires squash merge")
	}
	if t.Policy.AllowScopeExpansion {
		return errors.New("MVP does not allow silent scope expansion")
	}
	if t.Policy.MaxRepairAttempts < 0 {
		return errors.New("max_repair_attempts must not be negative")
	}
	return nil
}

func validVerifierID(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if index > 0 && (character == '-' || character == '_') {
			continue
		}
		return false
	}
	return true
}

// ValidateVerifierID validates an untrusted verifier identifier without
// accepting executable command text.
func ValidateVerifierID(value string) error {
	if !validVerifierID(value) {
		return fmt.Errorf("invalid verifier ID: %q", value)
	}
	return nil
}

func validGitBranch(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") ||
		strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") ||
		strings.HasSuffix(value, ".lock") || strings.Contains(value, "..") ||
		strings.Contains(value, "@{") || strings.Contains(value, "//") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || strings.ContainsRune(" ~^:?*[\\", character) {
			return false
		}
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

// ValidateGitBranch validates an untrusted branch name before it is passed to
// Git as a positional argument.
func ValidateGitBranch(value string) error {
	if !validGitBranch(value) {
		return errors.New("unsafe Git branch name")
	}
	return nil
}

func blankItem(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}

type AgentStatus string

const (
	AgentCompleted          AgentStatus = "completed"
	AgentNeedsHumanDecision AgentStatus = "needs_human_decision"
	AgentBlocked            AgentStatus = "blocked"
	AgentFailed             AgentStatus = "failed"
)

type DecisionOption struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type DecisionRequest struct {
	Question       string           `json:"question"`
	Context        string           `json:"context"`
	Options        []DecisionOption `json:"options"`
	Recommendation string           `json:"recommendation"`
	BlockingReason string           `json:"blocking_reason"`
}

type AgentOutcome struct {
	Status            AgentStatus      `json:"status"`
	Summary           string           `json:"summary"`
	DecisionRequest   *DecisionRequest `json:"decision_request"`
	DiscoveredIssues  []string         `json:"discovered_issues"`
	SuggestedChecks   []string         `json:"suggested_checks"`
	ImplementationSHA *string          `json:"implementation_sha"`
}

func (o AgentOutcome) Validate() error {
	switch o.Status {
	case AgentCompleted, AgentBlocked, AgentFailed:
		if o.DecisionRequest != nil {
			return errors.New("decision_request is only valid for needs_human_decision")
		}
	case AgentNeedsHumanDecision:
		if o.DecisionRequest == nil {
			return errors.New("needs_human_decision requires decision_request")
		}
		if err := o.DecisionRequest.Validate(); err != nil {
			return fmt.Errorf("invalid decision_request: %w", err)
		}
	default:
		return fmt.Errorf("unknown agent status: %q", o.Status)
	}
	if strings.TrimSpace(o.Summary) == "" {
		return errors.New("agent outcome summary must not be empty")
	}
	return nil
}

func (r DecisionRequest) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "question", value: r.Question},
		{name: "context", value: r.Context},
		{name: "recommendation", value: r.Recommendation},
		{name: "blocking_reason", value: r.BlockingReason},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must not be blank", field.name)
		}
	}
	if len(r.Options) < 2 {
		return errors.New("at least two decision options are required")
	}
	seen := make(map[string]struct{}, len(r.Options))
	for _, option := range r.Options {
		id := strings.TrimSpace(option.ID)
		if id == "" || strings.TrimSpace(option.Description) == "" {
			return errors.New("decision option ID and description must not be blank")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate decision option ID: %s", id)
		}
		seen[id] = struct{}{}
	}
	if _, exists := seen[strings.TrimSpace(r.Recommendation)]; !exists {
		return errors.New("recommendation must reference a decision option ID")
	}
	return nil
}

type ReviewVerdict string

const (
	ReviewPass     ReviewVerdict = "pass"
	ReviewFindings ReviewVerdict = "findings"
	ReviewFailed   ReviewVerdict = "failed"
)

type ReviewFinding struct {
	ID       string  `json:"id"`
	Severity string  `json:"severity"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	File     *string `json:"file"`
	Line     *int    `json:"line"`
}

type ReviewOutcome struct {
	Verdict         ReviewVerdict   `json:"verdict"`
	Summary         string          `json:"summary"`
	ReviewedHeadSHA string          `json:"reviewed_head_sha"`
	Findings        []ReviewFinding `json:"findings"`
}

func (o ReviewOutcome) Validate() error {
	if strings.TrimSpace(o.ReviewedHeadSHA) == "" {
		return errors.New("reviewed_head_sha must not be empty")
	}
	if strings.TrimSpace(o.Summary) == "" {
		return errors.New("review summary must not be empty")
	}
	switch o.Verdict {
	case ReviewPass:
		if len(o.Findings) != 0 {
			return errors.New("pass verdict must not contain findings")
		}
	case ReviewFindings:
		if len(o.Findings) == 0 {
			return errors.New("findings verdict requires at least one finding")
		}
		for index, finding := range o.Findings {
			if err := finding.Validate(); err != nil {
				return fmt.Errorf("invalid finding %d: %w", index, err)
			}
		}
	case ReviewFailed:
	default:
		return fmt.Errorf("unknown review verdict: %q", o.Verdict)
	}
	return nil
}

func (f ReviewFinding) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "id", value: f.ID},
		{name: "severity", value: f.Severity},
		{name: "title", value: f.Title},
		{name: "body", value: f.Body},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must not be blank", field.name)
		}
	}
	switch f.Severity {
	case "critical", "high", "medium", "low":
	default:
		return fmt.Errorf("unknown severity: %q", f.Severity)
	}
	if f.File != nil && strings.TrimSpace(*f.File) == "" {
		return errors.New("file must be null or non-blank")
	}
	if f.Line != nil {
		if *f.Line < 1 {
			return errors.New("line must be positive")
		}
		if f.File == nil {
			return errors.New("line requires file")
		}
	}
	return nil
}
