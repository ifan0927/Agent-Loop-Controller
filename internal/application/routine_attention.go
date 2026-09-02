package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	maximumRoutineAttentionCandidates = 1000
	RoutineAttentionSchemaVersion     = "v2"
	routineAttentionCursorVersion     = "v3"
)

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
	RepositoryProfiles  map[string]string
	Limit               int
}

type RoutineAttentionCandidateStore interface {
	ListRoutineAttentionCandidates(context.Context, RoutineAttentionCandidateQuery) ([]OperatorAttentionEvent, error)
}

type ControllerAttentionCandidateQuery struct {
	Authority ControllerReadAuthority
	Limit     int
}

type ControllerAttentionCandidateStore interface {
	ListControllerAttentionCandidates(context.Context, ControllerAttentionCandidateQuery) ([]OperatorAttentionEvent, error)
}

type RoutineAttentionNavigation string

const (
	RoutineAttentionNavigationNone      RoutineAttentionNavigation = "none"
	RoutineAttentionNavigationRunDetail RoutineAttentionNavigation = "run_detail"
)

type RoutineAttentionOfferSummary struct {
	OfferID      string                  `json:"offer_id"`
	Action       OperationType           `json:"action"`
	Reason       string                  `json:"reason"`
	Confirmation LegalActionConfirmation `json:"confirmation"`
	InputKind    LegalActionInputKind    `json:"input_kind"`
	Consequence  LegalActionConsequence  `json:"consequence"`
}

type RoutineAttentionItem struct {
	EventID          string                         `json:"event_id"`
	EventType        string                         `json:"event_type"`
	Scope            AuthorityScopeKind             `json:"scope"`
	TargetID         string                         `json:"target_id"`
	RunID            string                         `json:"run_id,omitempty"`
	LinearIdentifier string                         `json:"linear_identifier,omitempty"`
	Repository       string                         `json:"repository,omitempty"`
	ControllerState  string                         `json:"controller_state"`
	AttentionState   RoutineAttentionState          `json:"attention_state"`
	Severity         string                         `json:"severity"`
	ReasonCode       string                         `json:"reason_code"`
	OccurredAt       time.Time                      `json:"occurred_at"`
	ObservedAt       time.Time                      `json:"observed_at"`
	Offers           []RoutineAttentionOfferSummary `json:"offers"`
	Navigation       RoutineAttentionNavigation     `json:"navigation"`
}

type RoutineAttentionPage struct {
	Metadata   RoutineProjectionMetadata `json:"metadata"`
	Collection RoutineCollectionMetadata `json:"collection"`
	Scope      AuthorityScopeKind        `json:"scope"`
	TargetID   string                    `json:"target_id,omitempty"`
	Items      []RoutineAttentionItem    `json:"items"`
}

type routineAttentionCursor struct {
	Version         string             `json:"version"`
	AuthorityDigest string             `json:"authority_digest"`
	Scope           AuthorityScopeKind `json:"scope"`
	TargetID        string             `json:"target_id,omitempty"`
	Severity        int                `json:"severity"`
	OccurredAt      time.Time          `json:"occurred_at"`
	EventID         string             `json:"event_id"`
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
	if query.Scope == ScopeController {
		return RoutineAttentionPage{}, serviceError(ErrorInternal, "controller attention requires stable read authority", nil)
	}
	scopes, profileID, repositoryTargets, err := s.attentionScopes(ctx, configured, query.Scope, query.TargetID)
	if err != nil {
		return RoutineAttentionPage{}, err
	}
	cursor, err := decodeRoutineAttentionCursor(query.Cursor)
	if err != nil {
		return RoutineAttentionPage{}, err
	}
	if query.Cursor != "" && (cursor.AuthorityDigest != scopes.Digest() || cursor.Scope != query.Scope || cursor.TargetID != query.TargetID) {
		return RoutineAttentionPage{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	candidates, err := s.store.ListRoutineAttentionCandidates(ctx, RoutineAttentionCandidateQuery{Scopes: scopes, Scope: query.Scope, TargetID: query.TargetID, RepositoryProfileID: profileID, RepositoryProfiles: repositoryTargets, Limit: maximumRoutineAttentionCandidates + 1})
	if err != nil {
		return RoutineAttentionPage{}, classifyServiceError(err)
	}
	if len(candidates) > maximumRoutineAttentionCandidates {
		return RoutineAttentionPage{}, serviceError(ErrorInternal, "active attention candidate bound exceeded", nil)
	}
	return s.projectPage(ctx, query, observedAt, scopes.Digest(), candidates, repositoryTargets)
}

// ListController reads the complete local Controller Attention collection with
// the stable collection-only reader. Direct run offer derivation still uses
// the requester's existing target-specific authorization path.
func (s *RoutineAttentionQueryService) ListController(ctx context.Context, authority ControllerReadAuthority, query RoutineAttentionQuery, observedAt time.Time) (RoutineAttentionPage, error) {
	if s == nil {
		return RoutineAttentionPage{}, serviceError(ErrorInternal, "controller attention authority is unavailable", nil)
	}
	limit := query.Limit
	if limit == 0 {
		limit = RoutineQueryDefaultLimit
	}
	store, ok := s.store.(ControllerAttentionCandidateStore)
	if !ok || !authority.Valid() {
		return RoutineAttentionPage{}, serviceError(ErrorInternal, "controller attention authority is unavailable", nil)
	}
	if query.Scope != ScopeController || query.TargetID != "" && query.TargetID != controllerScopeID || limit < 1 || limit > RoutineQueryMaximumLimit || len(query.Cursor) > 1024 {
		return RoutineAttentionPage{}, serviceError(ErrorInvalidInput, "attention collection bounds are invalid", nil)
	}
	query.Limit = limit
	cursor, err := decodeRoutineAttentionCursor(query.Cursor)
	if err != nil {
		return RoutineAttentionPage{}, err
	}
	if query.Cursor != "" && (cursor.AuthorityDigest != authority.Digest() || cursor.Scope != ScopeController || cursor.TargetID != query.TargetID) {
		return RoutineAttentionPage{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	candidates, err := store.ListControllerAttentionCandidates(ctx, ControllerAttentionCandidateQuery{Authority: authority, Limit: maximumRoutineAttentionCandidates + 1})
	if err != nil {
		return RoutineAttentionPage{}, classifyServiceError(err)
	}
	if len(candidates) > maximumRoutineAttentionCandidates {
		return RoutineAttentionPage{}, serviceError(ErrorInternal, "active attention candidate bound exceeded", nil)
	}
	return s.projectPage(ctx, query, observedAt, authority.Digest(), candidates, nil)
}

func (s *RoutineAttentionQueryService) projectPage(ctx context.Context, query RoutineAttentionQuery, observedAt time.Time, authorityDigest string, candidates []OperatorAttentionEvent, repositoryTargets map[string]string) (RoutineAttentionPage, error) {
	limit := query.Limit
	if limit == 0 {
		limit = RoutineQueryDefaultLimit
	}
	cursor, err := decodeRoutineAttentionCursor(query.Cursor)
	if err != nil {
		return RoutineAttentionPage{}, err
	}
	var active []RoutineAttentionItem
	eventsByID := make(map[string]OperatorAttentionEvent, len(candidates))
	for _, event := range candidates {
		state := RoutineAttentionUnknown
		navigation := RoutineAttentionNavigationNone
		repository := repositoryTargets[event.RepositoryProfileID]
		if query.Scope == ScopeController && repository == "" {
			repository = event.RepositoryProfileName
		}
		linearIdentifier := event.LinearIdentifier
		if event.RunID != "" {
			inspection, inspectErr := s.queries.Inspect(ctx, event.RunID)
			if inspectErr != nil {
				state = RoutineAttentionUnknown
			} else {
				state = classifyRoutineAttention(inspection, event)
				if state == "" {
					continue
				}
				navigation = RoutineAttentionNavigationRunDetail
				repository = inspection.Run.Repository
				if linearIdentifier == "" {
					linearIdentifier = inspection.Run.IssueID
				}
			}
		} else if ValidateOperatorAttentionEvent(event) == nil || ValidatePreviousOperatorAttentionEvent(event) == nil || ValidateLegacyOperatorAttentionEvent(event) == nil {
			state = RoutineAttentionActive
		} else {
			state = RoutineAttentionConflict
		}
		scope, target := routineAttentionScope(event), routineAttentionTarget(event)
		if query.Scope == ScopeRepository {
			scope, target = ScopeRepository, query.TargetID
			repository = query.TargetID
		} else if query.Scope == ScopeRun {
			scope, target = ScopeRun, query.TargetID
		} else if scope == ScopeRepository {
			if repository != "" {
				target = repository
			} else {
				continue
			}
		}
		eventID := routineAttentionEventID(event.EventKey)
		active = append(active, RoutineAttentionItem{EventID: eventID, EventType: event.EventType, Scope: scope, TargetID: target, RunID: event.RunID, LinearIdentifier: linearIdentifier, Repository: repository, ControllerState: event.ControllerState, AttentionState: state, Severity: event.Severity, ReasonCode: event.ReasonCode, OccurredAt: event.OccurredAt.UTC(), ObservedAt: event.ObservedAt.UTC(), Offers: []RoutineAttentionOfferSummary{}, Navigation: navigation})
		eventsByID[eventID] = event
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
	result := RoutineAttentionPage{Metadata: RoutineProjectionMetadata{SchemaVersion: RoutineAttentionSchemaVersion, ObservedAt: observedAt.UTC()}, Collection: RoutineCollectionMetadata{Total: len(active)}, Scope: query.Scope, TargetID: query.TargetID}
	end := start + limit
	if end > len(active) {
		end = len(active)
	}
	result.Items = append(make([]RoutineAttentionItem, 0, end-start), active[start:end]...)
	result.Collection.Truncated = end < len(active)
	if result.Collection.Truncated && len(result.Items) != 0 {
		last := result.Items[len(result.Items)-1]
		result.Collection.NextCursor = encodeRoutineAttentionCursor(routineAttentionCursor{Version: routineAttentionCursorVersion, AuthorityDigest: authorityDigest, Scope: query.Scope, TargetID: query.TargetID, Severity: routineSeverityRank(last.Severity), OccurredAt: last.OccurredAt, EventID: last.EventID})
	}
	current, currentStore := s.queries.(CurrentOperatorAttentionQuery)
	for index := range result.Items {
		item := &result.Items[index]
		event := eventsByID[item.EventID]
		if event.RunID == "" || item.AttentionState == RoutineAttentionConflict || !currentStore {
			continue
		}
		currentEvent, found, currentErr := current.CurrentOperatorAttention(ctx, event.RunID)
		if currentErr != nil {
			return RoutineAttentionPage{}, classifyServiceError(currentErr)
		}
		if !found || currentEvent.EventKey != event.EventKey {
			continue
		}
		offers, offerErr := s.actions.ListLegalActionOffers(ctx, LegalActionOfferQuery{Requester: query.Requester, RunID: event.RunID})
		if offerErr != nil {
			return RoutineAttentionPage{}, offerErr
		}
		for _, offer := range offers {
			item.Offers = append(item.Offers, routineAttentionOfferSummary(offer))
		}
		sort.Slice(item.Offers, func(i, j int) bool { return item.Offers[i].OfferID < item.Offers[j].OfferID })
	}
	result.Metadata.Digest = routineProjectionDigest(result)
	return result, nil
}

func routineAttentionOfferSummary(offer LegalActionOffer) RoutineAttentionOfferSummary {
	return RoutineAttentionOfferSummary{OfferID: offer.OfferID, Action: offer.Action, Reason: offer.Reason, Confirmation: offer.Confirmation, InputKind: offer.InputKind, Consequence: offer.Consequence}
}

func routineAttentionEventID(eventKey string) string {
	digest := sha256.Sum256([]byte("routine-attention-item:" + eventKey))
	return "attention-" + hex.EncodeToString(digest[:])
}

func (s *RoutineAttentionQueryService) attentionScopes(ctx context.Context, configured ConfiguredRequester, scope AuthorityScopeKind, target string) (AuthorizedScopeSet, string, map[string]string, error) {
	switch scope {
	case ScopeRepository:
		profile, found, err := s.repositories.RepositoryProfile(ctx, target)
		if err != nil {
			return AuthorizedScopeSet{}, "", nil, classifyServiceError(err)
		}
		if !found || profile.Authority.Repository != target {
			return AuthorizedScopeSet{}, "", nil, hiddenTargetError()
		}
		scopes, err := s.authorizer.RepositoryScopes(configured, profile.Authority)
		if err != nil {
			return AuthorizedScopeSet{}, "", nil, hiddenTargetError()
		}
		return scopes, profile.Authority.ProfileID, map[string]string{profile.Authority.ProfileID: profile.Authority.Repository}, nil
	case ScopeRun:
		authority, err := s.queries.GetRunScopeAuthority(ctx, target)
		if err != nil {
			return AuthorizedScopeSet{}, "", nil, hiddenTargetError()
		}
		scopes, err := s.authorizer.RunScopes(configured, authority)
		if err != nil {
			return AuthorizedScopeSet{}, "", nil, hiddenTargetError()
		}
		return scopes, "", nil, nil
	default:
		return AuthorizedScopeSet{}, "", nil, serviceError(ErrorInvalidInput, "attention scope is invalid", nil)
	}
}

func routineAttentionAfter(value RoutineAttentionItem, cursor routineAttentionCursor) bool {
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
	if json.Unmarshal(raw, &cursor) != nil || cursor.Version != routineAttentionCursorVersion || !validAuthorityDigest(cursor.AuthorityDigest) || cursor.OccurredAt.IsZero() || strings.TrimSpace(cursor.EventID) == "" || cursor.Severity < 0 || cursor.Severity > 4 {
		return routineAttentionCursor{}, serviceError(ErrorInvalidInput, "cursor is invalid", nil)
	}
	return cursor, nil
}

func encodeRoutineAttentionCursor(cursor routineAttentionCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}
