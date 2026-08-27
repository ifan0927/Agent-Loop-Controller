package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	IntegrityTargetID              = "controller-integrity"
	IntegrityRecheckSchemaVersion  = "v1"
	IntegrityRecheckMaximumRequest = 128
	IntegrityRecheckProgressBound  = 8
)

var (
	ErrIntegrityRecheckActive   = errors.New("integrity recheck is already active")
	ErrIntegrityRecheckConflict = errors.New("integrity recheck evidence conflicts")
)

type IntegrityRecheckState string

const (
	IntegrityRecheckPending   IntegrityRecheckState = "pending"
	IntegrityRecheckSucceeded IntegrityRecheckState = "succeeded"
	IntegrityRecheckConflict  IntegrityRecheckState = "conflict"
	IntegrityRecheckAmbiguous IntegrityRecheckState = "ambiguous"
)

type IntegrityRecheckCommand struct {
	Requester Requester
	RequestID string
	Owner     string
}

type IntegrityRecheckAcceptance struct {
	Requester  domain.GitHubUserIdentity
	RequestID  string
	AcceptedAt time.Time
}

type IntegrityRecheckResult struct {
	Receipt          OperationReceipt      `json:"receipt"`
	RegistryVersion  string                `json:"registry_version"`
	ScanID           string                `json:"scan_id"`
	TargetGeneration int64                 `json:"target_generation"`
	State            IntegrityRecheckState `json:"state"`
	ReasonCode       string                `json:"reason_code"`
	Observation      *IntegrityObservation `json:"observation,omitempty"`
}

type IntegrityRecheckStore interface {
	AcceptIntegrityRecheck(context.Context, IntegrityRecheckAcceptance) (IntegrityRecheckResult, error)
	GetIntegrityRecheck(context.Context, string, string) (IntegrityRecheckResult, error)
}

type IntegrityRecheckService struct {
	store       IntegrityRecheckStore
	authorizer  *AuthorizationService
	maintenance *IntegrityMaintenanceService
	now         func() time.Time
}

func NewIntegrityRecheckService(store IntegrityRecheckStore, authorizer *AuthorizationService, maintenance *IntegrityMaintenanceService) (*IntegrityRecheckService, error) {
	if store == nil || authorizer == nil || maintenance == nil {
		return nil, errors.New("integrity recheck service is unavailable")
	}
	return &IntegrityRecheckService{store: store, authorizer: authorizer, maintenance: maintenance, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *IntegrityRecheckService) Recheck(ctx context.Context, command IntegrityRecheckCommand) (IntegrityRecheckResult, error) {
	configured, err := s.authorizer.ResolveConfiguredRequester(command.Requester)
	if err != nil {
		return IntegrityRecheckResult{}, hiddenTargetError()
	}
	requestID := command.RequestID
	if strings.TrimSpace(requestID) == "" || len(requestID) > IntegrityRecheckMaximumRequest || strings.ContainsRune(requestID, '\x00') || strings.TrimSpace(command.Owner) == "" || strings.ContainsRune(command.Owner, '\x00') {
		return IntegrityRecheckResult{}, serviceError(ErrorInvalidInput, "integrity recheck request is invalid", nil)
	}
	now := s.now().UTC()
	result, err := s.store.AcceptIntegrityRecheck(ctx, IntegrityRecheckAcceptance{Requester: configured.Identity(), RequestID: requestID, AcceptedAt: now})
	if err != nil {
		switch {
		case errors.Is(err, ErrIntegrityRecheckActive):
			return IntegrityRecheckResult{}, serviceError(ErrorConflict, "integrity_recheck_active", err)
		case errors.Is(err, ErrIntegrityRecheckConflict):
			return IntegrityRecheckResult{}, serviceError(ErrorConflict, "integrity recheck authority changed", err)
		default:
			return IntegrityRecheckResult{}, classifyServiceError(err)
		}
	}
	if result.State != IntegrityRecheckPending {
		return result, nil
	}
	for opportunity := 0; opportunity < IntegrityRecheckProgressBound; opportunity++ {
		maintenance, maintenanceErr := s.maintenance.Run(ctx, command.Owner, now.Add(time.Duration(opportunity)*time.Nanosecond))
		if maintenanceErr != nil {
			return IntegrityRecheckResult{}, classifyServiceError(maintenanceErr)
		}
		result, err = s.store.GetIntegrityRecheck(ctx, IntegrityRecheckRequestKey(configured.Identity(), requestID), IntegrityRecheckRequestClaim(requestID))
		if err != nil {
			return IntegrityRecheckResult{}, classifyServiceError(err)
		}
		if result.State != IntegrityRecheckPending || maintenance.Family == "" && !maintenance.Published && !maintenance.Superseded {
			break
		}
	}
	return result, nil
}

func IntegrityRecheckRequestKey(requester domain.GitHubUserIdentity, requestID string) string {
	requester = normalizedOperationRequester(requester)
	return digestText(strings.Join([]string{"integrity-recheck-request-v1", IntegrityRecheckSchemaVersion, IntegrityTargetID, identityDigest(requester), requestID}, "\x00"))
}

func IntegrityRecheckRequestClaim(requestID string) string {
	return digestText(strings.Join([]string{"integrity-recheck-claim-v1", IntegrityTargetID, requestID}, "\x00"))
}

func IntegrityRecheckOperationAnchorDigest(requestKey string) string {
	return digestText("integrity-recheck-anchor-v1\x00" + requestKey)
}

func IntegrityRecheckTargetBindingDigest(requester domain.GitHubUserIdentity) string {
	return identityDigest(normalizedOperationRequester(requester))
}

func NewIntegrityRecheckReceipt(requester domain.GitHubUserIdentity, requestID string, preAcceptanceGeneration int64, acceptedAt time.Time) OperationReceipt {
	requester = normalizedOperationRequester(requester)
	requestKey := IntegrityRecheckRequestKey(requester, requestID)
	authority := IntegrityRecheckAuthorityDigest(preAcceptanceGeneration)
	return NewOperationReceipt(OperationReceiptInput{
		OperationType:           OperationRecheckIntegrity,
		Scope:                   ScopeController,
		TargetID:                IntegrityTargetID,
		Requester:               requester,
		RequestDigest:           digestText(strings.Join([]string{"integrity-recheck-input-v1", IntegrityRecheckSchemaVersion, IntegrityTargetID, requestID}, "\x00")),
		ExpectedAuthorityDigest: authority,
		OperationAnchorDigest:   IntegrityRecheckOperationAnchorDigest(requestKey),
		TargetBindingDigest:     IntegrityRecheckTargetBindingDigest(requester),
		AcceptedAt:              acceptedAt.UTC(),
	})
}

func IntegrityRecheckAuthorityDigest(generation int64) string {
	return digestText(fmt.Sprintf("integrity-recheck-authority-v1\x00%s\x00%d", IntegrityRegistryVersion, generation))
}

func IntegrityRecheckBindingDigest(operationID, scanID string, targetGeneration int64, observationID, observationDigest string, readiness IntegrityState) string {
	return digestText(fmt.Sprintf("integrity-recheck-binding-v1\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s", operationID, scanID, targetGeneration, observationID, observationDigest, readiness))
}

func IntegrityRecheckConflictDigest(operationID, scanID string, targetGeneration, observedGeneration int64, reason string) string {
	return digestText(fmt.Sprintf("integrity-recheck-conflict-v1\x00%s\x00%s\x00%d\x00%d\x00%s", operationID, scanID, targetGeneration, observedGeneration, reason))
}
