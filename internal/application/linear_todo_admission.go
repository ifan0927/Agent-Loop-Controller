package application

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
)

// LinearTodoAdmissionLeaseNamespace is the sole scheduler lease. It is not a
// run lease and must not be replaced by a process manager liveness signal.
const (
	LinearTodoAdmissionLeaseNamespace = "linear_todo_admission"
	MaxLinearTodoAdmissionLeaseTTL    = 10 * time.Minute
)

const (
	LinearTodoAdmissionJournalReserved = "reserved"
)

// LinearTodoAdmissionLease is a compare-and-swap capability. Callers must
// present both OwnerNonce and Version for renewal, release, and reservation.
type LinearTodoAdmissionLease struct {
	Namespace  string    `json:"namespace"`
	OwnerNonce string    `json:"owner_nonce"`
	Version    int64     `json:"version"`
	AcquiredAt time.Time `json:"acquired_at"`
	RenewedAt  time.Time `json:"renewed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LinearTodoAdmissionJournal is deliberately a projection: it contains
// digests and immutable identifiers, never Linear prose, URLs, commands, or
// credentials.
type LinearTodoAdmissionJournal struct {
	IssueUUID         string    `json:"issue_uuid"`
	RunID             string    `json:"run_id"`
	ScanDigest        string    `json:"scan_digest"`
	TaskDigest        string    `json:"task_digest"`
	ProfileDigest     string    `json:"profile_digest"`
	Status            string    `json:"status"`
	MutationIntentRef string    `json:"mutation_intent_ref,omitempty"`
	ReasonCode        string    `json:"reason_code,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// LinearTodoAdmissionReservation carries the already-normalized, existing
// full admission snapshot. Reserving it is persistence only: it does not
// materialize artifacts, provision a worktree, start Codex, or mutate Linear.
type LinearTodoAdmissionReservation struct {
	Lease      LinearTodoAdmissionLease
	ScanDigest string
	IssueUUID  string
	Input      LocalStartInput
}

// LinearTodoAdmissionJournalTransition carries the only mutable journal
// fields. Both the reference and the reason are validated by the store.
type LinearTodoAdmissionJournalTransition struct {
	Lease             LinearTodoAdmissionLease
	RunID             string
	ExpectedStatus    string
	NextStatus        string
	MutationIntentRef string
	ReasonCode        string
}

// LinearTodoAdmissionStore is intentionally narrow. Future scheduling code
// cannot turn it into a generic Linear mutation or controller-driving port.
type LinearTodoAdmissionStore interface {
	AcquireLinearTodoAdmissionLease(context.Context, string, time.Duration, time.Time) (LinearTodoAdmissionLease, bool, error)
	RenewLinearTodoAdmissionLease(context.Context, LinearTodoAdmissionLease, time.Duration, time.Time) (LinearTodoAdmissionLease, bool, error)
	ReleaseLinearTodoAdmissionLease(context.Context, LinearTodoAdmissionLease) (bool, error)
	LinearTodoAdmissionLeaseHeld(context.Context, LinearTodoAdmissionLease, time.Time) (bool, error)
	ListNonterminalRuns(context.Context) ([]Run, error)
	GetLinearTodoAdmissionJournal(context.Context, string) (LinearTodoAdmissionJournal, bool, error)
	ReserveLinearTodoAdmission(context.Context, LinearTodoAdmissionReservation) (Run, LinearTodoAdmissionJournal, bool, error)
	AdoptLinearTodoAdmissionReservation(context.Context, LinearTodoAdmissionReservation) (Run, LinearTodoAdmissionJournal, bool, error)
	AdvanceLinearTodoAdmissionJournal(context.Context, LinearTodoAdmissionJournalTransition) (bool, error)
}

// ReservedRunFromAdmissionSnapshot creates the same durable run record used
// by normal local admission, without starting its controller lifecycle.
func ReservedRunFromAdmissionSnapshot(input LocalStartInput) (Run, error) {
	if err := input.Task.Validate(); err != nil {
		return Run{}, err
	}
	if input.Task.Repository != input.Repository.CanonicalRepository || input.Task.BaseBranch != input.Repository.BaseBranch {
		return Run{}, errors.New("task repository/base does not match the registry snapshot")
	}
	if strings.TrimSpace(input.RunRoot) == "" || strings.TrimSpace(input.WorktreeRoot) == "" {
		return Run{}, errors.New("run and worktree roots are required")
	}
	repositoryJSON, err := json.Marshal(input.Repository)
	if err != nil {
		return Run{}, err
	}
	return Run{ID: input.Task.RunID, IssueID: input.Task.IssueID, IdempotencyKey: input.IdempotencyKey,
		SourceRevision: input.Task.SourceRevision, RawIssueJSON: string(input.RawIssueJSON), RawIssueHash: input.RawIssueHash,
		NormalizedTaskJSON: string(input.NormalizedJSON), TaskHash: input.TaskHash, Repository: input.Task.Repository,
		RepositoryConfigJSON: string(repositoryJSON), BaseBranch: input.Task.BaseBranch,
		ProfileID: input.Repository.ProfileID, ProfileSnapshotVersion: input.Repository.ProfileSnapshotVersion,
		ProfileDigest: input.Repository.ProfileDigest, ProfileSnapshotJSON: input.Repository.ProfileSnapshotJSON,
		RegistryVersion: input.Repository.RegistryVersion, RegistryDigest: input.Repository.RegistryDigest,
		RepositoryBindingDigest: input.Repository.RepositoryBindingDigest, WorkingBranch: input.Task.WorkingBranch,
		WorktreePath: filepath.Join(input.WorktreeRoot, input.Task.RunID), ArtifactRoot: filepath.Join(input.RunRoot, input.Task.RunID),
		ImplementationModel: codex.ImplementationModel, ReviewModel: codex.ReviewModel}, nil
}

// Validate checks only persistence authority. Time is supplied by the caller
// to make boundary behavior explicit and testable.
func (l LinearTodoAdmissionLease) Validate(now time.Time) error {
	if l.Namespace != LinearTodoAdmissionLeaseNamespace || strings.TrimSpace(l.OwnerNonce) == "" || l.Version < 1 || l.AcquiredAt.IsZero() || l.RenewedAt.IsZero() || l.ExpiresAt.IsZero() || !l.ExpiresAt.After(now.UTC()) || l.RenewedAt.Before(l.AcquiredAt) {
		return errors.New("automatic admission lease is invalid")
	}
	return nil
}
