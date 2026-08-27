package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type integrityRecheckStoreStub struct {
	acceptCalls int
	getCalls    int
	maintenance int
	accepted    IntegrityRecheckResult
	settled     IntegrityRecheckResult
}

func (s *integrityRecheckStoreStub) AcceptIntegrityRecheck(context.Context, IntegrityRecheckAcceptance) (IntegrityRecheckResult, error) {
	s.acceptCalls++
	return s.accepted, nil
}

func (s *integrityRecheckStoreStub) GetIntegrityRecheck(context.Context, string, string) (IntegrityRecheckResult, error) {
	s.getCalls++
	return s.settled, nil
}

func (s *integrityRecheckStoreStub) RunIntegrityMaintenance(context.Context, string, time.Time) (IntegrityMaintenanceResult, error) {
	s.maintenance++
	return IntegrityMaintenanceResult{ScanID: "scan", Family: IntegrityStorageSchema}, nil
}

func TestIntegrityRecheckAuthorizationPrecedesValidationAndPersistence(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	store := &integrityRecheckStoreStub{}
	maintenance, _ := NewIntegrityMaintenanceService(store)
	service, err := NewIntegrityRecheckService(store, authorizer, maintenance)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID + 1, NodeID: operator.NodeID, ActorType: operator.ActorType}
	if _, err := service.Recheck(context.Background(), IntegrityRecheckCommand{Requester: unauthorized, RequestID: "", Owner: ""}); err == nil || store.acceptCalls != 0 || store.maintenance != 0 {
		t.Fatalf("err=%v accepts=%d maintenance=%d", err, store.acceptCalls, store.maintenance)
	}
	authorized := Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	if _, err := service.Recheck(context.Background(), IntegrityRecheckCommand{Requester: authorized, RequestID: strings.Repeat("x", IntegrityRecheckMaximumRequest+1), Owner: "worker"}); err == nil || store.acceptCalls != 0 {
		t.Fatalf("err=%v accepts=%d", err, store.acceptCalls)
	}
}

func TestIntegrityRecheckUsesBoundedSharedMaintenanceAndSanitizesPrivateIdentity(t *testing.T) {
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	authorizer, _ := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	receipt := NewIntegrityRecheckReceipt(operator, "request-1", 10, time.Now().UTC())
	receipt.Phase, receipt.Outcome, receipt.AppliedAt = OperationPhaseApplied, OperationOutcomePending, receipt.AcceptedAt
	store := &integrityRecheckStoreStub{accepted: IntegrityRecheckResult{Receipt: receipt, RegistryVersion: IntegrityRegistryVersion, ScanID: "scan", TargetGeneration: 12, State: IntegrityRecheckPending, ReasonCode: "scan_pending"}}
	settledReceipt := receipt
	settledReceipt.Phase, settledReceipt.Outcome, settledReceipt.SettledAt = OperationPhaseObserved, OperationOutcomeConflict, receipt.AcceptedAt
	store.settled = IntegrityRecheckResult{Receipt: settledReceipt, RegistryVersion: IntegrityRegistryVersion, ScanID: "scan", TargetGeneration: 12, State: IntegrityRecheckConflict, ReasonCode: "source_generation_advanced"}
	maintenance, _ := NewIntegrityMaintenanceService(store)
	service, _ := NewIntegrityRecheckService(store, authorizer, maintenance)
	result, err := service.Recheck(context.Background(), IntegrityRecheckCommand{Requester: Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}, RequestID: "request-1", Owner: "worker"})
	if err != nil || result.State != IntegrityRecheckConflict || store.acceptCalls != 1 || store.maintenance != 1 || store.getCalls != 1 {
		t.Fatalf("result=%+v err=%v accepts=%d maintenance=%d gets=%d", result, err, store.acceptCalls, store.maintenance, store.getCalls)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"authority_key", "operation_anchor", "request_key", "request_claim", "finalization_guard", "sqlite"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("private value %q escaped in %s", forbidden, raw)
		}
	}
}
