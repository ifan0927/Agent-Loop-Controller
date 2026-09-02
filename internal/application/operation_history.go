package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type OperationHistoryFilter struct {
	Scope         AuthorityScopeKind `json:"scope,omitempty"`
	TargetID      string             `json:"target_id,omitempty"`
	OperationType OperationType      `json:"operation_type,omitempty"`
	Phase         OperationPhase     `json:"phase,omitempty"`
	Outcome       OperationOutcome   `json:"outcome,omitempty"`
}

type OperationHistoryCursor struct {
	Version             string    `json:"v"`
	ScopeDigest         string    `json:"s"`
	FilterDigest        string    `json:"f"`
	AcceptedAt          time.Time `json:"a"`
	OperationID         string    `json:"o"`
	WatermarkAcceptedAt time.Time `json:"wa"`
	WatermarkOperation  string    `json:"wo"`
}

type OperationHistoryStoreQuery struct {
	Scopes AuthorizedScopeSet
	Filter OperationHistoryFilter
	Limit  int
	Cursor *OperationHistoryCursor
}

type OperationHistoryStorePage struct {
	Receipts             []OperationReceipt
	Total                int
	HasMore              bool
	WatermarkAcceptedAt  time.Time
	WatermarkOperationID string
}

type OperationHistoryQuery struct {
	Requester Requester
	Filter    OperationHistoryFilter
	Limit     int
	Cursor    string
}

type OperationHistoryPage struct {
	Metadata   RoutineProjectionMetadata `json:"metadata"`
	Collection RoutineCollectionMetadata `json:"collection"`
	Receipts   []OperationReceipt        `json:"receipts"`
}

func (s *OperationReceiptQueryService) List(ctx context.Context, query OperationHistoryQuery, observedAt time.Time) (OperationHistoryPage, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(query.Requester)
	if err != nil {
		return OperationHistoryPage{}, hiddenTargetError()
	}
	scopes, err := s.authorizer.ControllerScopes(configured)
	if err != nil {
		return OperationHistoryPage{}, hiddenTargetError()
	}
	limit := query.Limit
	if limit == 0 {
		limit = OperationHistoryDefaultLimit
	}
	if limit < 1 || limit > OperationHistoryMaximumLimit {
		return OperationHistoryPage{}, serviceError(ErrorInvalidInput, "operation history limit is invalid", nil)
	}
	if err := validateOperationHistoryFilter(query.Filter); err != nil {
		return OperationHistoryPage{}, serviceError(ErrorInvalidInput, "operation history filter is invalid", err)
	}
	filterDigest := operationHistoryFilterDigest(query.Filter)
	var cursor *OperationHistoryCursor
	if query.Cursor != "" {
		decoded, decodeErr := decodeOperationHistoryCursor(query.Cursor)
		if decodeErr != nil || decoded.Version != ActivitySchemaVersion || decoded.ScopeDigest != scopes.Digest() || decoded.FilterDigest != filterDigest || decoded.AcceptedAt.IsZero() || decoded.OperationID == "" || decoded.WatermarkAcceptedAt.IsZero() || decoded.WatermarkOperation == "" {
			return OperationHistoryPage{}, serviceError(ErrorInvalidInput, "operation history cursor is invalid", nil)
		}
		cursor = &decoded
	}
	page, err := s.store.ListAuthorizedOperationReceipts(ctx, OperationHistoryStoreQuery{Scopes: scopes, Filter: query.Filter, Limit: limit, Cursor: cursor})
	if err != nil {
		return OperationHistoryPage{}, classifyServiceError(err)
	}
	result := OperationHistoryPage{
		Metadata:   RoutineProjectionMetadata{SchemaVersion: ActivitySchemaVersion, ObservedAt: observedAt.UTC()},
		Collection: RoutineCollectionMetadata{Total: page.Total, Truncated: page.HasMore},
		Receipts:   page.Receipts,
	}
	if page.HasMore && len(page.Receipts) != 0 {
		last := page.Receipts[len(page.Receipts)-1]
		result.Collection.NextCursor = encodeOperationHistoryCursor(OperationHistoryCursor{Version: ActivitySchemaVersion, ScopeDigest: scopes.Digest(), FilterDigest: filterDigest, AcceptedAt: last.AcceptedAt, OperationID: last.OperationID, WatermarkAcceptedAt: page.WatermarkAcceptedAt, WatermarkOperation: page.WatermarkOperationID})
	}
	result.Metadata.Digest = operationHistoryProjectionDigest(result)
	return result, nil
}

func validateOperationHistoryFilter(filter OperationHistoryFilter) error {
	if filter.Scope != "" && !validOperationScope(filter.Scope) || filter.TargetID != "" && filter.Scope == "" || strings.ContainsRune(filter.TargetID, '\x00') {
		return errors.New("operation target filter is invalid")
	}
	if filter.OperationType != "" && !validOperationType(filter.OperationType) || filter.Phase != "" && !validOperationPhase(filter.Phase) || filter.Outcome != "" && !validOperationOutcome(filter.Outcome) {
		return errors.New("operation lifecycle filter is invalid")
	}
	return nil
}

func operationHistoryFilterDigest(filter OperationHistoryFilter) string {
	raw, _ := json.Marshal(filter)
	return digestText("operation-history-filter-v1\x00" + string(raw))
}

func encodeOperationHistoryCursor(cursor OperationHistoryCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeOperationHistoryCursor(value string) (OperationHistoryCursor, error) {
	var cursor OperationHistoryCursor
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(raw, &cursor) != nil {
		return OperationHistoryCursor{}, errors.New("operation history cursor is invalid")
	}
	return cursor, nil
}

func operationHistoryProjectionDigest(value OperationHistoryPage) string {
	value.Metadata.Digest = ""
	raw, _ := json.Marshal(value)
	return digestText("operation-history-projection-v1\x00" + string(raw))
}
