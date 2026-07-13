package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// LinearAdmissionRepositoryResolver resolves only controller-configured
// repository labels. Linear labels never supply local paths or verifier commands.
type LinearAdmissionRepositoryResolver interface {
	ResolveLinearAdmissionRepository(string) (LocalRepository, bool)
}

// LinearAdmissionStore supplies the durable uniqueness and drift gate needed
// before a Linear issue can enter the local controller.
type LinearAdmissionStore interface {
	RunStore
	GetRunByIssue(context.Context, string) (Run, bool, error)
	MarkLinearSourceDrift(context.Context, string, domain.State, string, string) (bool, error)
}

type LinearStartCommand struct {
	Requester  Requester `json:"requester"`
	Identifier string    `json:"identifier"`
}

// LinearRevalidateCommand binds a manual continuation to the persisted run and
// makes source drift observable before any Git, Codex, or GitHub action.
type LinearRevalidateCommand struct {
	Requester      Requester
	RunID          string
	Repository     string
	ExpectedState  domain.State
	IdempotencyKey string
}

// LinearAdmissionService composes an authoritative read-only Linear adapter
// with the controller-owned admission and durable-run boundaries.
type LinearAdmissionService struct {
	reader   LinearIssueReader
	resolver LinearAdmissionRepositoryResolver
	store    LinearAdmissionStore
	commands CommandService
}

func NewLinearAdmissionService(reader LinearIssueReader, resolver LinearAdmissionRepositoryResolver, store LinearAdmissionStore, controller LocalRunController) (*LinearAdmissionService, error) {
	if reader == nil || resolver == nil || store == nil || controller == nil {
		return nil, errors.New("Linear admission dependencies are required")
	}
	return &LinearAdmissionService{reader: reader, resolver: resolver, store: store, commands: NewCommandService(controller, store)}, nil
}

func (s *LinearAdmissionService) Start(ctx context.Context, command LinearStartCommand) (CommandResult, []LinearRequestObservation, error) {
	if strings.TrimSpace(command.Identifier) == "" {
		return CommandResult{}, nil, serviceError(ErrorInvalidInput, "Linear issue identifier is required", nil)
	}
	source, observations, err := s.reader.ReadIssue(ctx, command.Identifier)
	if err != nil {
		return CommandResult{}, observations, classifyServiceError(err)
	}
	snapshot, repository, err := admitLinearTask(source, s.resolver)
	if err != nil {
		return CommandResult{}, observations, classifyServiceError(err)
	}
	if err := command.Requester.authorize(repository.AllowedOperatorLogins, repository.TrustedOperatorActors); err != nil {
		return CommandResult{}, observations, err
	}

	existing, found, err := s.store.GetRunByIssue(ctx, snapshot.Task.IssueID)
	if err != nil {
		return CommandResult{}, observations, classifyServiceError(err)
	}
	if found {
		if err := s.requireStableLinearSource(ctx, existing, snapshot, repository); err != nil {
			return CommandResult{}, observations, err
		}
	}

	input := LocalStartInput{Task: snapshot.Task, RawIssueJSON: snapshot.RawJSON, RawIssueHash: snapshot.RawHash,
		NormalizedJSON: snapshot.NormalizedJSON, TaskHash: snapshot.TaskHash, IdempotencyKey: snapshot.IdempotencyKey,
		Repository: repository, RunRoot: repository.RunRoot, WorktreeRoot: repository.WorktreeRoot}
	result, err := s.commands.Start(ctx, StartCommand{Requester: command.Requester, RepositorySelection: repository.CanonicalRepository, IdempotencyKey: snapshot.IdempotencyKey, Input: input})
	if err != nil {
		// A concurrent trigger can create the run after the preflight above. Read
		// the active issue once more so a conflicting revision is durably halted
		// instead of surfacing only as a database uniqueness error.
		existing, foundAfterFailure, lookupErr := s.store.GetRunByIssue(ctx, snapshot.Task.IssueID)
		if !found && lookupErr == nil && foundAfterFailure {
			if driftErr := s.requireStableLinearSource(ctx, existing, snapshot, repository); driftErr != nil {
				return CommandResult{}, observations, driftErr
			}
			return CommandResult{Run: projectRunResult(existing)}, observations, nil
		}
		return CommandResult{}, observations, err
	}
	return result, observations, nil
}

func (s *LinearAdmissionService) Revalidate(ctx context.Context, command LinearRevalidateCommand) (Run, error) {
	if command.RunID == "" || command.Repository == "" || command.ExpectedState == "" || command.IdempotencyKey == "" {
		return Run{}, serviceError(ErrorInvalidInput, "run, expected state, repository, and idempotency key are required", nil)
	}
	run, err := s.store.GetRun(ctx, command.RunID)
	if err != nil {
		return Run{}, classifyServiceError(err)
	}
	if run.Repository != command.Repository || run.State != command.ExpectedState || run.IdempotencyKey != command.IdempotencyKey {
		return Run{}, serviceError(ErrorConflict, "run authority or state changed before reconciliation", nil)
	}
	if err := authorizePersistedRequester(run, command.Requester); err != nil {
		return Run{}, err
	}
	source, _, err := s.reader.ReadIssue(ctx, run.IssueID)
	if err != nil {
		return Run{}, classifyServiceError(err)
	}
	snapshot, repository, err := admitLinearTask(source, s.resolver)
	if err != nil {
		return Run{}, classifyServiceError(err)
	}
	if err := command.Requester.authorize(repository.AllowedOperatorLogins, repository.TrustedOperatorActors); err != nil {
		return Run{}, err
	}
	if snapshot.Task.IssueID != run.IssueID {
		return Run{}, serviceError(ErrorConflict, "Linear source does not match the persisted run", nil)
	}
	if err := s.requireStableLinearSource(ctx, run, snapshot, repository); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *LinearAdmissionService) requireStableLinearSource(ctx context.Context, existing Run, snapshot linearAdmissionSnapshot, repository LocalRepository) error {
	if existing.State == domain.StateManualIntervention {
		return serviceError(ErrorConflict, "existing run requires a human decision", nil)
	}
	if existing.SourceRevision == snapshot.Task.SourceRevision && existing.Repository == repository.CanonicalRepository && existing.WorkingBranch == snapshot.Task.WorkingBranch && existing.TaskHash == snapshot.TaskHash {
		return nil
	}
	evidence := "linear-source-drift:" + snapshot.RawHash
	marked, err := s.store.MarkLinearSourceDrift(ctx, existing.ID, existing.State, existing.SourceRevision, evidence)
	if err != nil {
		return classifyServiceError(err)
	}
	if marked {
		return serviceError(ErrorConflict, "Linear source drift requires a human decision", nil)
	}
	return serviceError(ErrorConflict, "Linear source conflicts with an existing run", nil)
}

type linearAdmissionSnapshot struct {
	Task           domain.CodingTask
	RawJSON        []byte
	RawHash        string
	NormalizedJSON []byte
	TaskHash       string
	IdempotencyKey string
}

func admitLinearTask(source LinearTaskSource, resolver LinearAdmissionRepositoryResolver) (linearAdmissionSnapshot, LocalRepository, error) {
	if source.Provider != "linear" || source.Team.Key != "IFAN" || source.Identifier == "" || source.IssueID == "" || source.URL == "" || strings.TrimSpace(source.Title) == "" || strings.TrimSpace(source.Description) == "" || strings.TrimSpace(source.SourceRevision) == "" {
		return linearAdmissionSnapshot{}, LocalRepository{}, errors.New("Linear issue source is incomplete")
	}
	if source.State.Name != "Todo" || !source.Cycle.IsActive || source.Cycle.ID == "" {
		return linearAdmissionSnapshot{}, LocalRepository{}, errors.New("Linear issue is not coding-ready for admission")
	}
	labels := make([]string, 0, len(source.Labels))
	for _, label := range source.Labels {
		if label.ID == "" || strings.TrimSpace(label.Name) == "" {
			return linearAdmissionSnapshot{}, LocalRepository{}, errors.New("Linear issue contains an incomplete label")
		}
		labels = append(labels, label.Name)
	}
	if !slices.Contains(labels, "agent:codex") || slices.Contains(labels, "agent:hermes") {
		return linearAdmissionSnapshot{}, LocalRepository{}, errors.New("Linear issue is not eligible for Codex admission")
	}
	repository, err := resolveLinearRepository(labels, resolver)
	if err != nil {
		return linearAdmissionSnapshot{}, LocalRepository{}, err
	}
	if err := domain.ValidateGitBranch(source.BranchName); err != nil {
		return linearAdmissionSnapshot{}, LocalRepository{}, fmt.Errorf("invalid Linear branch name: %w", err)
	}
	goal, acceptance, outOfScope, err := parseLinearSpecification(source.Description)
	if err != nil {
		return linearAdmissionSnapshot{}, LocalRepository{}, err
	}
	if len(repository.VerifierIDs) == 0 {
		return linearAdmissionSnapshot{}, LocalRepository{}, errors.New("repository has no controller-owned verifier policy")
	}
	for _, verifierID := range repository.VerifierIDs {
		if err := domain.ValidateVerifierID(verifierID); err != nil {
			return linearAdmissionSnapshot{}, LocalRepository{}, fmt.Errorf("invalid controller-owned verifier ID: %w", err)
		}
	}

	raw, err := json.Marshal(source)
	if err != nil {
		return linearAdmissionSnapshot{}, LocalRepository{}, errors.New("encode sanitized Linear source")
	}
	keyHash := sha256.Sum256([]byte(source.Identifier + "\x00" + source.SourceRevision))
	idempotencyKey := hex.EncodeToString(keyHash[:])
	task := domain.CodingTask{RunID: "run-" + idempotencyKey[:16], IssueID: source.Identifier, IssueURL: source.URL,
		Title: source.Title, Description: source.Description, Repository: repository.CanonicalRepository, BaseBranch: repository.BaseBranch,
		WorkingBranch: source.BranchName, Goal: goal, AcceptanceCriteria: acceptance, OutOfScope: outOfScope,
		VerifierIDs: append([]string(nil), repository.VerifierIDs...), Policy: domain.TaskPolicy{HumanApprovalRequired: true, MergeMethod: "squash", MaxRepairAttempts: domain.DefaultMaxRepairAttempts},
		SourceRevision: source.SourceRevision, CreatedAt: source.CreatedAt}
	if err := task.Validate(); err != nil {
		return linearAdmissionSnapshot{}, LocalRepository{}, fmt.Errorf("normalized Linear CodingTask: %w", err)
	}
	normalized, err := json.Marshal(task)
	if err != nil {
		return linearAdmissionSnapshot{}, LocalRepository{}, errors.New("encode normalized Linear task")
	}
	return linearAdmissionSnapshot{Task: task, RawJSON: raw, RawHash: digestLinear(raw), NormalizedJSON: normalized, TaskHash: digestLinear(normalized), IdempotencyKey: idempotencyKey}, repository, nil
}

func resolveLinearRepository(labels []string, resolver LinearAdmissionRepositoryResolver) (LocalRepository, error) {
	var matches []LocalRepository
	for _, label := range labels {
		repository, ok := resolver.ResolveLinearAdmissionRepository(label)
		if ok {
			matches = append(matches, repository)
		}
	}
	if len(matches) != 1 {
		return LocalRepository{}, errors.New("Linear issue must have exactly one supported repository label")
	}
	return matches[0], nil
}

func parseLinearSpecification(description string) (string, []string, []string, error) {
	sections := make(map[string][]string)
	var current string
	for _, line := range strings.Split(strings.ReplaceAll(description, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			current = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
			if _, exists := sections[current]; exists {
				return "", nil, nil, fmt.Errorf("duplicate Linear specification section: %s", current)
			}
			sections[current] = nil
			continue
		}
		if current != "" {
			sections[current] = append(sections[current], line)
		}
	}
	goal := sectionText(sections, "goal", "outcome")
	acceptance := sectionItems(sections, "acceptance criteria")
	outOfScope := sectionItems(sections, "out of scope")
	if goal == "" || len(acceptance) == 0 {
		return "", nil, nil, errors.New("Linear issue must contain Goal or Outcome and Acceptance Criteria sections")
	}
	return goal, acceptance, outOfScope, nil
}

func sectionText(sections map[string][]string, names ...string) string {
	for _, name := range names {
		if lines, ok := sections[name]; ok {
			return strings.TrimSpace(strings.Join(lines, "\n"))
		}
	}
	return ""
}

func sectionItems(sections map[string][]string, name string) []string {
	lines, ok := sections[name]
	if !ok {
		return nil
	}
	var items []string
	for _, line := range lines {
		value := strings.TrimSpace(line)
		value = strings.TrimSpace(strings.TrimPrefix(value, "- "))
		value = strings.TrimSpace(strings.TrimPrefix(value, "* "))
		if value != "" {
			items = append(items, value)
		}
	}
	return items
}

func digestLinear(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
