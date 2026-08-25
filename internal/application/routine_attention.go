package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const maximumRoutineAttentionCandidates = 1000

type RoutineAttentionQuery struct {
	Requester Requester          `json:"requester"`
	Scope     AuthorityScopeKind `json:"scope"`
	TargetID  string             `json:"target_id,omitempty"`
	Limit     int                `json:"limit,omitempty"`
	Cursor    string             `json:"cursor,omitempty"`
}

type RoutineAttentionCandidateQuery struct {
	Scopes              AuthorizedScopeSet
	Scope               AuthorityScopeKind
	TargetID            string
	RepositoryProfileID string
	Limit               int
}

type RoutineAttentionCandidateStore interface {
	ListRoutineAttentionCandidates(context.Context, RoutineAttentionCandidateQuery) ([]OperatorAttentionEvent, error)
}

type RoutineAttentionPage struct {
	Metadata   RoutineProjectionMetadata `json:"metadata"`
	Collection RoutineCollectionMetadata `json:"collection"`
	Scope      AuthorityScopeKind        `json:"scope"`
	TargetID   string                    `json:"target_id,omitempty"`
	Attention  []RoutineAttentionSummary `json:"attention"`
	Offers     []LegalActionOffer        `json:"legal_action_offers"`
}

type routineAttentionCursor struct {
	Version     string             `json:"version"`
	ScopeDigest string             `json:"scope_digest"`
	Scope       AuthorityScopeKind `json:"scope"`
	TargetID    string             `json:"target_id,omitempty"`
	Severity    int                `json:"severity"`
	OccurredAt  time.Time          `json:"occurred_at"`
	EventID     string             `json:"event_id"`
}

type RoutineAttentionQueryService struct {
	store        RoutineAttentionCandidateStore
	queries      QueryStore
	authorizer   *AuthorizationService
	repositories RepositoryProfileSource
	actions      *LegalActionService
}

func NewRoutineAttentionQueryService(store RoutineAttentionCandidateStore, queries QueryStore, authorizer *AuthorizationService, repositories RepositoryProfileSource) (*RoutineAttentionQueryService, error) {
	if store == nil || queries == nil || authorizer == nil || repositories == nil {
		return nil, errors.New("routine attention dependencies are required")
	}
	actionStore, ok := queries.(LegalActionStore)
	if !ok {
		return nil, errors.New("routine attention requires current legal-action authority")
	}
	actions, err := NewLegalActionService(actionStore, authorizer)
	if err != nil {
		return nil, err
	}
	return &RoutineAttentionQueryService{store: store, queries: queries, authorizer: authorizer, repositories: repositories, actions: actions}, nil
}

func (s *RoutineAttentionQueryService) List(ctx context.Context, query RoutineAttentionQuery, observedAt time.Time) (RoutineAttentionPage, error) {
	limit := query.Limit
	if limit == 0 {
		limit = RoutineQueryDefaultLimit
	}
	if limit < 1 || limit > RoutineQueryMaximumLimit || len(query.Cursor) > 1024 {
		return RoutineAttentionPage{}, serviceError(ErrorInvalidInput, "attention collection bounds are invalid", nil)
	}
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return RoutineAttentionPage{}, hiddenTargetError()
	}
	scopes, profileID, err := s.attentionScopes(ctx, configured, query.Scope, query.TargetID)
	if err != nil {
		return RoutineAttentionPage{}, err
	}
	cursor, err := decodeRoutineAttentionCursor(query.Cursor)
	if err != nil {
		return RoutineAttentionPage{}, err
	}
	if query.Cursor != "" && (cursor.ScopeDigest != scopes.Digest() || cursor.Scope != query.Scope || cursor.TargetID != query.TargetID) {
		return RoutineAttentionPage{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	candidates, err := s.store.ListRoutineAttentionCandidates(ctx, RoutineAttentionCandidateQuery{Scopes: scopes, Scope: query.Scope, TargetID: query.TargetID, RepositoryProfileID: profileID, Limit: maximumRoutineAttentionCandidates + 1})
	if err != nil {
		return RoutineAttentionPage{}, classifyServiceError(err)
	}
	if len(candidates) > maximumRoutineAttentionCandidates {
		return RoutineAttentionPage{}, serviceError(ErrorInternal, "active attention candidate bound exceeded", nil)
	}
	var active []RoutineAttentionSummary
	offersByID := map[string]LegalActionOffer{}
	repositoryTargets := map[string]string{}
	if query.Scope == ScopeController {
		profiles, profileErr := s.repositories.ListRepositoryProfiles(ctx)
		if profileErr != nil {
			return RoutineAttentionPage{}, classifyServiceError(profileErr)
		}
		for _, profile := range profiles {
			if repositoryScopes, scopeErr := s.authorizer.RepositoryScopes(configured, profile.Authority); scopeErr == nil && !repositoryScopes.Empty() {
				repositoryTargets[profile.Authority.ProfileID] = profile.Authority.Repository
			}
		}
	}
	for _, event := range candidates {
		state := RoutineAttentionUnknown
		if event.RunID != "" {
			inspection, inspectErr := s.queries.Inspect(ctx, event.RunID)
			if inspectErr != nil {
				state = RoutineAttentionUnknown
			} else {
				state = classifyRoutineAttention(inspection, event)
				if state == "" {
					continue
				}
				currentOffers, offerErr := s.actions.ListLegalActionOffers(ctx, LegalActionOfferQuery{Requester: query.Requester, RunID: event.RunID})
				if offerErr != nil {
					return RoutineAttentionPage{}, offerErr
				}
				for _, offer := range currentOffers {
					offersByID[offer.OfferID] = offer
				}
			}
		}
		scope, target := routineAttentionScope(event), routineAttentionTarget(event)
		if query.Scope == ScopeRepository {
			scope, target = ScopeRepository, query.TargetID
		} else if query.Scope == ScopeRun {
			scope, target = ScopeRun, query.TargetID
		} else if scope == ScopeRepository {
			if repository, ok := repositoryTargets[event.RepositoryProfileID]; ok {
				target = repository
			} else {
				scope, target = ScopeController, controllerScopeID
			}
		}
		active = append(active, RoutineAttentionSummary{EventID: event.EventKey, Scope: scope, TargetID: target, Severity: event.Severity, ReasonCode: event.ReasonCode, State: state, OccurredAt: event.OccurredAt.UTC(), ObservedAt: event.ObservedAt.UTC()})
	}
	sort.Slice(active, func(i, j int) bool {
		left, right := routineSeverityRank(active[i].Severity), routineSeverityRank(active[j].Severity)
		if left != right {
			return left < right
		}
		if !active[i].OccurredAt.Equal(active[j].OccurredAt) {
			return active[i].OccurredAt.Before(active[j].OccurredAt)
		}
		return active[i].EventID < active[j].EventID
	})
	start := 0
	if query.Cursor != "" {
		for start < len(active) && !routineAttentionAfter(active[start], cursor) {
			start++
		}
	}
	result := RoutineAttentionPage{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineQuerySchemaVersion, ObservedAt: observedAt.UTC()}, Collection: RoutineCollectionMetadata{Total: len(active)}, Scope: query.Scope, TargetID: query.TargetID}
	end := start + limit
	if end > len(active) {
		end = len(active)
	}
	result.Attention = append([]RoutineAttentionSummary(nil), active[start:end]...)
	result.Collection.Truncated = end < len(active)
	if result.Collection.Truncated && len(result.Attention) != 0 {
		last := result.Attention[len(result.Attention)-1]
		result.Collection.NextCursor = encodeRoutineAttentionCursor(routineAttentionCursor{Version: RoutineQuerySchemaVersion, ScopeDigest: scopes.Digest(), Scope: query.Scope, TargetID: query.TargetID, Severity: routineSeverityRank(last.Severity), OccurredAt: last.OccurredAt, EventID: last.EventID})
	}
	for _, offer := range offersByID {
		result.Offers = append(result.Offers, offer)
	}
	sort.Slice(result.Offers, func(i, j int) bool { return result.Offers[i].OfferID < result.Offers[j].OfferID })
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func (s *RoutineAttentionQueryService) attentionScopes(ctx context.Context, configured ConfiguredRequester, scope AuthorityScopeKind, target string) (AuthorizedScopeSet, string, error) {
	switch scope {
	case ScopeController:
		if target != "" && target != controllerScopeID {
			return AuthorizedScopeSet{}, "", hiddenTargetError()
		}
		scopes, err := s.authorizer.ControllerScopes(configured)
		return scopes, "", err
	case ScopeRepository:
		profile, found, err := s.repositories.RepositoryProfile(ctx, target)
		if err != nil {
			return AuthorizedScopeSet{}, "", classifyServiceError(err)
		}
		if !found || profile.Authority.Repository != target {
			return AuthorizedScopeSet{}, "", hiddenTargetError()
		}
		scopes, err := s.authorizer.RepositoryScopes(configured, profile.Authority)
		if err != nil {
			return AuthorizedScopeSet{}, "", hiddenTargetError()
		}
		return scopes, profile.Authority.ProfileID, nil
	case ScopeRun:
		authority, err := s.queries.GetRunScopeAuthority(ctx, target)
		if err != nil {
			return AuthorizedScopeSet{}, "", hiddenTargetError()
		}
		scopes, err := s.authorizer.RunScopes(configured, authority)
		if err != nil {
			return AuthorizedScopeSet{}, "", hiddenTargetError()
		}
		return scopes, "", nil
	default:
		return AuthorizedScopeSet{}, "", serviceError(ErrorInvalidInput, "attention scope is invalid", nil)
	}
}

func routineAttentionAfter(value RoutineAttentionSummary, cursor routineAttentionCursor) bool {
	rank := routineSeverityRank(value.Severity)
	if rank != cursor.Severity {
		return rank > cursor.Severity
	}
	if !value.OccurredAt.Equal(cursor.OccurredAt) {
		return value.OccurredAt.After(cursor.OccurredAt)
	}
	return value.EventID > cursor.EventID
}

func decodeRoutineAttentionCursor(value string) (routineAttentionCursor, error) {
	if value == "" {
		return routineAttentionCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return routineAttentionCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	var cursor routineAttentionCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Version != RoutineQuerySchemaVersion || !validAuthorityDigest(cursor.ScopeDigest) || cursor.OccurredAt.IsZero() || strings.TrimSpace(cursor.EventID) == "" || cursor.Severity < 0 || cursor.Severity > 4 {
		return routineAttentionCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	return cursor, nil
}

func encodeRoutineAttentionCursor(cursor routineAttentionCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}
