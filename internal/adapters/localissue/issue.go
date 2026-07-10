package localissue

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type Issue struct {
	IssueID            string    `json:"issue_id"`
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	Team               string    `json:"team"`
	Labels             []string  `json:"labels"`
	Status             string    `json:"status"`
	CurrentCycle       bool      `json:"current_cycle"`
	CycleID            string    `json:"cycle_id"`
	RepositoryLabel    string    `json:"repository_label"`
	BaseBranch         string    `json:"base_branch"`
	BranchName         string    `json:"branch_name"`
	Goal               string    `json:"goal"`
	AcceptanceCriteria []string  `json:"acceptance_criteria"`
	OutOfScope         []string  `json:"out_of_scope"`
	VerifierIDs        []string  `json:"verifier_ids"`
	SourceRevision     string    `json:"source_revision"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	Comments           []string  `json:"comments"`
}

type Registry interface {
	HasRepository(string) bool
	HasVerifier(string, string) bool
}

type Snapshot struct {
	Issue          Issue
	RawJSON        []byte
	RawHash        string
	Task           domain.CodingTask
	NormalizedJSON []byte
	TaskHash       string
	IdempotencyKey string
}

func Decode(reader io.Reader) (Issue, []byte, error) {
	const limit = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return Issue{}, nil, err
	}
	if len(raw) > limit {
		return Issue{}, nil, errors.New("issue input exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var issue Issue
	if err := decoder.Decode(&issue); err != nil {
		return Issue{}, nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return Issue{}, nil, errors.New("issue input must contain exactly one JSON value")
		}
		return Issue{}, nil, fmt.Errorf("unexpected trailing data: %w", err)
	}
	return issue, raw, nil
}

func Admit(issue Issue, raw []byte, registry Registry) (Snapshot, error) {
	if issue.Team != "IFAN" {
		return Snapshot{}, errors.New("simulated issue team must be IFAN")
	}
	if !slices.Contains(issue.Labels, "agent:codex") {
		return Snapshot{}, errors.New("simulated issue requires agent:codex label")
	}
	if slices.Contains(issue.Labels, "agent:hermes") {
		return Snapshot{}, errors.New("simulated issue must not contain agent:hermes label")
	}
	if issue.Status != "Todo" {
		return Snapshot{}, errors.New("simulated issue status must be Todo")
	}
	if !issue.CurrentCycle || strings.TrimSpace(issue.CycleID) == "" {
		return Snapshot{}, errors.New("simulated issue must be in the current cycle")
	}
	if !registry.HasRepository(issue.RepositoryLabel) {
		return Snapshot{}, fmt.Errorf("unknown repository label: %s", issue.RepositoryLabel)
	}
	if !slices.Contains(issue.Labels, issue.RepositoryLabel) {
		return Snapshot{}, errors.New("repository_label must also be present in labels")
	}
	if err := domain.ValidateGitBranch(issue.BranchName); err != nil {
		return Snapshot{}, fmt.Errorf("invalid branch_name: %w", err)
	}
	if len(issue.AcceptanceCriteria) == 0 {
		return Snapshot{}, errors.New("acceptance_criteria must not be empty")
	}
	for _, verifierID := range issue.VerifierIDs {
		if err := domain.ValidateVerifierID(verifierID); err != nil {
			return Snapshot{}, err
		}
		if !registry.HasVerifier(issue.RepositoryLabel, verifierID) {
			return Snapshot{}, fmt.Errorf("unknown verifier ID %s for %s", verifierID, issue.RepositoryLabel)
		}
	}
	if strings.TrimSpace(issue.SourceRevision) == "" {
		return Snapshot{}, errors.New("source_revision must not be blank")
	}
	if issue.CreatedAt.IsZero() || issue.UpdatedAt.IsZero() {
		return Snapshot{}, errors.New("created_at and updated_at are required")
	}
	if issue.UpdatedAt.Before(issue.CreatedAt) {
		return Snapshot{}, errors.New("updated_at must not precede created_at")
	}
	description := strings.TrimSpace(issue.Description)
	if len(issue.Comments) > 0 {
		description += "\n\nSupplemental specification:\n- " + strings.Join(issue.Comments, "\n- ")
	}
	task := domain.CodingTask{
		IssueID: issue.IssueID, IssueURL: "local://simulated-linear/" + issue.IssueID,
		Title: issue.Title, Description: description, Repository: issue.RepositoryLabel,
		BaseBranch: issue.BaseBranch, WorkingBranch: issue.BranchName, Goal: issue.Goal,
		AcceptanceCriteria: append([]string(nil), issue.AcceptanceCriteria...),
		OutOfScope:         append([]string(nil), issue.OutOfScope...), VerifierIDs: append([]string(nil), issue.VerifierIDs...),
		Policy:         domain.TaskPolicy{HumanApprovalRequired: true, MergeMethod: "squash"},
		SourceRevision: issue.SourceRevision, CreatedAt: issue.CreatedAt,
	}
	keyHash := sha256.Sum256([]byte(issue.IssueID + "\x00" + issue.SourceRevision))
	key := hex.EncodeToString(keyHash[:])
	task.RunID = "run-" + key[:16]
	if err := task.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("normalized CodingTask: %w", err)
	}
	normalized, err := json.Marshal(task)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Issue: issue, RawJSON: append([]byte(nil), raw...), RawHash: hash(raw), Task: task,
		NormalizedJSON: normalized, TaskHash: hash(normalized), IdempotencyKey: key}, nil
}

func hash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
