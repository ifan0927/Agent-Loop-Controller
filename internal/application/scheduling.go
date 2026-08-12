package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	DefaultHeavyCapacity    = 2
	MaxHeavyCapacity        = 32
	MaxSchedulingQueryItems = 100

	RepositoryEligibilityEligible = "eligible"
	RepositoryEligibilityDisabled = "repository_disabled"

	QueueCandidateSelected                  = "selected"
	QueueCandidateWaiting                   = "waiting"
	QueueCandidateBlockedByActiveRepository = "blocked_by_active_repository"
	QueueCandidateRepositoryDisabled        = "repository_disabled"
	QueueCandidateInvalid                   = "invalid"
	QueueCandidateDrift                     = "source_drift"
	QueueCandidateAmbiguous                 = "ambiguous"
)

var ErrHeavyPermitProcessReconciliationRequired = errors.New("heavy permit ownership requires process reconciliation")

type heavyPermitOwnerContextKey struct{}
type stopAfterHumanDecisionContextKey struct{}

type heavyPermitOwnerContext struct {
	owner           string
	retainForDriver bool
}

// WithHeavyPermitOwner binds one process-fenced supervisor identity to a
// command path that may resume existing heavy work.
func WithHeavyPermitOwner(ctx context.Context, owner string) context.Context {
	if strings.TrimSpace(owner) == "" {
		return ctx
	}
	return context.WithValue(ctx, heavyPermitOwnerContextKey{}, heavyPermitOwnerContext{owner: owner, retainForDriver: true})
}

// WithManualHeavyPermitOwner binds a unique process-fenced manual supervisor
// identity whose permit is released when the command returns.
func WithManualHeavyPermitOwner(ctx context.Context, owner string) context.Context {
	if strings.TrimSpace(owner) == "" {
		return ctx
	}
	return context.WithValue(ctx, heavyPermitOwnerContextKey{}, heavyPermitOwnerContext{owner: owner})
}

func withHeavyPermitOwner(ctx context.Context, owner string) context.Context {
	return WithHeavyPermitOwner(ctx, owner)
}

func heavyPermitOwnerFromContext(ctx context.Context) (string, bool, bool) {
	value, ok := ctx.Value(heavyPermitOwnerContextKey{}).(heavyPermitOwnerContext)
	return value.owner, value.retainForDriver, ok && strings.TrimSpace(value.owner) != ""
}

func withStopAfterHumanDecision(ctx context.Context) context.Context {
	return context.WithValue(ctx, stopAfterHumanDecisionContextKey{}, true)
}

func stopAfterHumanDecision(ctx context.Context) bool {
	value, _ := ctx.Value(stopAfterHumanDecisionContextKey{}).(bool)
	return value
}

// RepositoryEligibility is a typed application result. Repository lifecycle
// may implement the disabled result later without changing admission policy.
type RepositoryEligibility struct {
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code,omitempty"`
}

func (e RepositoryEligibility) Validate() error {
	switch e.Status {
	case RepositoryEligibilityEligible:
		if e.ReasonCode != "" {
			return errors.New("eligible repository has a reason code")
		}
	case RepositoryEligibilityDisabled:
		if !operatorAttentionScope.MatchString(e.ReasonCode) {
			return errors.New("disabled repository reason code is required")
		}
	default:
		return errors.New("repository eligibility status is invalid")
	}
	return nil
}

// LinearAdmissionRepositoryEligibility is optional until repository lifecycle
// exists. Resolvers that do not implement it are treated as enabled.
type LinearAdmissionRepositoryEligibility interface {
	RepositoryEligibility(LocalRepository) RepositoryEligibility
}

// HeavyWorkRequired is the controller-owned boundary for local Codex,
// verification, fresh review, and repair work. External and human waits do not
// consume a permit.
func HeavyWorkRequired(state domain.State) bool {
	switch state {
	case domain.StateReceived, domain.StateAdmitting, domain.StateProvisioning,
		domain.StateExecuting, domain.StateVerifying, domain.StateFreshReview,
		domain.StateRepairing:
		return true
	default:
		return false
	}
}

func TerminalRunState(state domain.State) bool {
	switch state {
	case domain.StateRejected, domain.StateFailed, domain.StateCompleted:
		return true
	default:
		return false
	}
}

type RepositorySlot struct {
	RepositoryBindingDigest string    `json:"repository_binding_digest"`
	RunID                   string    `json:"run_id"`
	Version                 int64     `json:"version"`
	AcquiredAt              time.Time `json:"acquired_at"`
}

type HeavyPermit struct {
	RunID      string    `json:"run_id"`
	OwnerNonce string    `json:"owner_nonce"`
	Version    int64     `json:"version"`
	AcquiredAt time.Time `json:"acquired_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type SchedulingRun struct {
	RunID                   string       `json:"run_id"`
	RepositoryBindingDigest string       `json:"repository_binding_digest"`
	State                   domain.State `json:"state"`
	RunnableSince           time.Time    `json:"runnable_since,omitempty"`
	SupervisorState         string       `json:"supervisor_state"`
	WaitingForCapacity      bool         `json:"waiting_for_capacity"`
	HasHeavyPermit          bool         `json:"has_heavy_permit"`
	Quarantined             bool         `json:"quarantined"`
}

type CapacityProjection struct {
	ConfiguredCapacity int       `json:"configured_capacity"`
	EffectiveCapacity  int       `json:"effective_capacity"`
	InUse              int       `json:"in_use"`
	Available          int       `json:"available"`
	Draining           bool      `json:"draining"`
	WaitingRunnable    int       `json:"waiting_runnable"`
	EffectiveIdentity  string    `json:"effective_identity"`
	Version            int64     `json:"version"`
	ObservedAt         time.Time `json:"observed_at"`
}

type QueueCandidateProjection struct {
	IssueUUID               string `json:"issue_uuid"`
	TeamKey                 string `json:"team_key"`
	IssueSequence           int    `json:"issue_sequence"`
	Priority                int    `json:"priority"`
	RepositoryProfileID     string `json:"repository_profile_id,omitempty"`
	RepositoryBindingDigest string `json:"repository_binding_digest,omitempty"`
	Classification          string `json:"classification"`
	ReasonCode              string `json:"reason_code"`
}

type QueueSnapshot struct {
	Digest                    string                     `json:"digest"`
	ObservedAt                time.Time                  `json:"observed_at"`
	EffectiveCapacityIdentity string                     `json:"effective_capacity_identity"`
	Candidates                []QueueCandidateProjection `json:"candidates"`
}

func (s QueueSnapshot) Validate() error {
	if !validOperatorAttentionDigest(s.Digest) || s.ObservedAt.IsZero() || !operatorAttentionProfileID.MatchString(s.EffectiveCapacityIdentity) || len(s.Candidates) > 100 {
		return errors.New("queue snapshot authority is invalid")
	}
	for _, candidate := range s.Candidates {
		if !validLinearUUID(candidate.IssueUUID) || candidate.TeamKey != "IFAN" || candidate.IssueSequence < 1 || candidate.Priority < 0 || candidate.Priority > 4 || !validQueueCandidateClassification(candidate.Classification) || !operatorAttentionScope.MatchString(candidate.ReasonCode) || (candidate.RepositoryProfileID != "" && !validSchedulingProfileID(candidate.RepositoryProfileID)) || (candidate.RepositoryBindingDigest != "" && !validOperatorAttentionDigest(candidate.RepositoryBindingDigest)) {
			return errors.New("queue candidate projection is invalid")
		}
	}
	return nil
}

func validSchedulingProfileID(value string) bool {
	if operatorAttentionProfileID.MatchString(value) {
		return true
	}
	const prefix = "repository-profile:"
	return strings.HasPrefix(value, prefix) && operatorAttentionRepository.MatchString(strings.TrimPrefix(value, prefix))
}

func validQueueCandidateClassification(value string) bool {
	switch value {
	case QueueCandidateSelected, QueueCandidateWaiting, QueueCandidateBlockedByActiveRepository,
		QueueCandidateRepositoryDisabled, QueueCandidateInvalid, QueueCandidateDrift,
		QueueCandidateAmbiguous:
		return true
	default:
		return false
	}
}

type SchedulingDecision struct {
	DecisionID              string    `json:"decision_id"`
	SnapshotDigest          string    `json:"snapshot_digest"`
	ObservedAt              time.Time `json:"observed_at"`
	CapacityIdentity        string    `json:"capacity_identity"`
	IssueUUID               string    `json:"issue_uuid"`
	IssueSequence           int       `json:"issue_sequence"`
	Priority                int       `json:"priority"`
	RepositoryProfileID     string    `json:"repository_profile_id"`
	RunID                   string    `json:"run_id"`
	RepositoryBindingDigest string    `json:"repository_binding_digest"`
	Classification          string    `json:"classification"`
	ReasonCode              string    `json:"reason_code"`
	RepositorySlotVersion   int64     `json:"repository_slot_version"`
	HeavyPermitVersion      int64     `json:"heavy_permit_version"`
	AdmissionLeaseVersion   int64     `json:"admission_lease_version"`
}

type SchedulingReservation struct {
	OwnerNonce          string
	CapacityIdentity    string
	Capacity            int
	RunnableSince       time.Time
	DecisionID          string
	IssueSequence       int
	Priority            int
	RepositoryProfileID string
}

func (d SchedulingDecision) Validate() error {
	if !validOperatorAttentionDigest(d.DecisionID) || !validOperatorAttentionDigest(d.SnapshotDigest) || d.ObservedAt.IsZero() || !operatorAttentionProfileID.MatchString(d.CapacityIdentity) || !validLinearUUID(d.IssueUUID) || d.IssueSequence < 1 || d.Priority < 0 || d.Priority > 4 || !validSchedulingProfileID(d.RepositoryProfileID) || !validQueueCandidateClassification(d.Classification) || !operatorAttentionScope.MatchString(d.ReasonCode) || d.RepositorySlotVersion < 0 || d.HeavyPermitVersion < 0 || d.AdmissionLeaseVersion < 0 {
		return errors.New("scheduling decision evidence is invalid")
	}
	if d.RepositoryBindingDigest != "" && !validOperatorAttentionDigest(d.RepositoryBindingDigest) {
		return errors.New("scheduling decision repository binding is invalid")
	}
	return nil
}

func (r SchedulingReservation) Enabled() bool {
	return r.OwnerNonce != "" || r.CapacityIdentity != "" || r.Capacity != 0 || !r.RunnableSince.IsZero() || r.DecisionID != "" || r.IssueSequence != 0 || r.Priority != 0 || r.RepositoryProfileID != ""
}

func (r SchedulingReservation) Validate() error {
	if !r.Enabled() {
		return nil
	}
	if strings.TrimSpace(r.OwnerNonce) == "" || !operatorAttentionProfileID.MatchString(r.CapacityIdentity) || r.Capacity < 1 || r.Capacity > MaxHeavyCapacity || r.RunnableSince.IsZero() || !validOperatorAttentionDigest(r.DecisionID) || r.IssueSequence < 1 || r.Priority < 0 || r.Priority > 4 || !validSchedulingProfileID(r.RepositoryProfileID) {
		return errors.New("scheduling reservation authority is invalid")
	}
	return nil
}

// SchedulingAuthorityStore keeps repository slots, execution scheduling,
// heavy permits, and bounded projections transport-neutral.
type SchedulingAuthorityStore interface {
	ConfigureHeavyCapacity(context.Context, int, string, time.Time) (CapacityProjection, error)
	ReconcileSchedulingAuthorities(context.Context, time.Time) ([]SchedulingRun, error)
	AcquireHeavyPermit(context.Context, string, string, time.Time) (HeavyPermit, bool, error)
	ReleaseHeavyPermit(context.Context, HeavyPermit, string, time.Time) (bool, error)
	DeferSchedulingRun(context.Context, string, time.Time, time.Time) (bool, error)
	Capacity(context.Context, time.Time) (CapacityProjection, error)
	SaveQueueSnapshot(context.Context, QueueSnapshot) error
	LatestQueueSnapshot(context.Context) (QueueSnapshot, bool, error)
	AppendSchedulingDecision(context.Context, SchedulingDecision) (bool, error)
}

// SchedulingProjectionReader exposes bounded, read-only scheduler state for
// later transport adapters. Queries never reconcile authorities or trigger
// external observation.
type SchedulingProjectionReader interface {
	ListSchedulingRuns(context.Context, int) ([]SchedulingRun, error)
	ListSchedulingDecisions(context.Context, int) ([]SchedulingDecision, error)
}
