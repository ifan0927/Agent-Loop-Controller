package application

import (
	"context"
	"errors"
	"strings"
	"time"
)

type RoutineQueueState string

const (
	RoutineQueueCurrent  RoutineQueueState = "current"
	RoutineQueueAbsent   RoutineQueueState = "absent"
	RoutineQueueStale    RoutineQueueState = "stale"
	RoutineQueueDegraded RoutineQueueState = "degraded"
	RoutineQueueUnknown  RoutineQueueState = "unknown"
)

type RoutineQueueCandidate struct {
	Rank             int    `json:"rank"`
	LinearIdentifier string `json:"linear_identifier"`
	Priority         int    `json:"priority"`
	Repository       string `json:"repository,omitempty"`
	Classification   string `json:"classification"`
	ReasonCode       string `json:"reason_code"`
}

type RoutineQueueProjection struct {
	Metadata           RoutineProjectionMetadata `json:"metadata"`
	State              RoutineQueueState         `json:"state"`
	ReasonCode         string                    `json:"reason_code"`
	SnapshotDigest     string                    `json:"snapshot_digest,omitempty"`
	SnapshotObservedAt *time.Time                `json:"snapshot_observed_at,omitempty"`
	Candidates         []RoutineQueueCandidate   `json:"candidates"`
	Collection         RoutineCollectionMetadata `json:"collection"`
}

type RoutineQueueStore interface {
	LatestQueueSnapshot(context.Context) (QueueSnapshot, bool, error)
}

type RoutineQueueAttention struct {
	OccurredAt time.Time
	Degraded   bool
	ReasonCode string
}

type RoutineQueueAttentionSource interface {
	LatestQueueAttention(context.Context, AuthorizedScopeSet) (RoutineQueueAttention, bool, error)
}

type RoutineQueueService struct {
	store      RoutineQueueStore
	authorizer *AuthorizationService
	profiles   RepositoryProfileSource
	attention  RoutineQueueAttentionSource
}

func NewRoutineQueueService(store RoutineQueueStore, authorizer *AuthorizationService, profiles RepositoryProfileSource, attention RoutineQueueAttentionSource) (*RoutineQueueService, error) {
	if store == nil || authorizer == nil || profiles == nil {
		return nil, errors.New("routine queue dependencies are required")
	}
	return &RoutineQueueService{store: store, authorizer: authorizer, profiles: profiles, attention: attention}, nil
}

func (s *RoutineQueueService) Get(ctx context.Context, requester Requester, observedAt time.Time) (RoutineQueueProjection, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return RoutineQueueProjection{}, hiddenTargetError()
	}
	controllerScopes, err := s.authorizer.ControllerScopes(configured)
	if err != nil || !controllerScopes.HasController() {
		return RoutineQueueProjection{}, hiddenTargetError()
	}
	profiles, err := s.profiles.ListRepositoryProfiles(ctx)
	if err != nil {
		return RoutineQueueProjection{}, classifyServiceError(err)
	}
	authorized := map[string]RepositoryProfileAuthority{}
	allScopes := append([]AuthorityScope(nil), controllerScopes.scopes...)
	for _, profile := range profiles {
		scopes, scopeErr := s.authorizer.RepositoryScopes(configured, profile.Authority)
		if scopeErr != nil {
			continue
		}
		authorized[profile.Authority.ProfileID] = profile
		allScopes = append(allScopes, scopes.scopes...)
	}
	scopes, err := newAuthorizedScopeSet(configured.Identity(), allScopes...)
	if err != nil {
		return RoutineQueueProjection{}, classifyServiceError(err)
	}
	snapshot, found, err := s.store.LatestQueueSnapshot(ctx)
	if err != nil {
		return RoutineQueueProjection{}, classifyServiceError(err)
	}
	result := RoutineQueueProjection{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, State: RoutineQueueAbsent, ReasonCode: "snapshot_absent", Candidates: []RoutineQueueCandidate{}}
	if !found {
		result.Metadata.Digest = routineProjectionDigest(result)
		return result, nil
	}
	if snapshot.Validate() != nil {
		return RoutineQueueProjection{}, serviceError(ErrorInternal, "queue snapshot is corrupt", nil)
	}
	result.State, result.ReasonCode, result.SnapshotDigest = RoutineQueueCurrent, "latest_complete_snapshot", snapshot.Digest
	snapshotAt := snapshot.ObservedAt.UTC()
	result.SnapshotObservedAt = &snapshotAt
	for index, candidate := range snapshot.Candidates {
		projected := RoutineQueueCandidate{Rank: index + 1, LinearIdentifier: candidate.TeamKey + "-" + integerString(candidate.IssueSequence), Priority: candidate.Priority, Classification: candidate.Classification, ReasonCode: candidate.ReasonCode}
		if profile, ok := authorized[candidate.RepositoryProfileID]; ok && candidate.RepositoryBindingDigest == profile.Authority.BindingDigest {
			projected.Repository = profile.Authority.Repository
		}
		result.Candidates = append(result.Candidates, projected)
	}
	result.Collection = RoutineCollectionMetadata{Total: len(result.Candidates)}
	if s.attention != nil {
		latest, found, attentionErr := s.attention.LatestQueueAttention(ctx, scopes)
		if attentionErr != nil {
			return RoutineQueueProjection{}, classifyServiceError(attentionErr)
		}
		if found && latest.OccurredAt.After(snapshot.ObservedAt) {
			result.State, result.ReasonCode = RoutineQueueStale, "newer_scheduler_attention"
			if latest.Degraded {
				result.State = RoutineQueueDegraded
			}
			if strings.TrimSpace(latest.ReasonCode) != "" {
				result.ReasonCode = latest.ReasonCode
			}
		}
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func integerString(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
