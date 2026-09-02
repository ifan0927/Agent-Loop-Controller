package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type operationReceiptQueryFixture struct {
	receipt      OperationReceipt
	runAuthority RunScopeAuthority
}

func (s operationReceiptQueryFixture) GetOperationReceiptTarget(_ context.Context, operationID string) (OperationReceiptTarget, error) {
	if operationID != s.receipt.OperationID {
		return OperationReceiptTarget{}, ErrOperationReceiptNotFound
	}
	return OperationReceiptTarget{Scope: s.receipt.Scope, TargetID: s.receipt.TargetID, TargetBindingDigest: s.receipt.TargetBindingDigest}, nil
}

func (s operationReceiptQueryFixture) GetAuthorizedOperationReceipt(_ context.Context, operationID string, scopes AuthorizedScopeSet) (OperationReceipt, error) {
	if operationID != s.receipt.OperationID || !scopes.AllowsOperationTarget(OperationReceiptTarget{Scope: s.receipt.Scope, TargetID: s.receipt.TargetID, TargetBindingDigest: s.receipt.TargetBindingDigest}) {
		return OperationReceipt{}, ErrOperationReceiptNotFound
	}
	return s.receipt, nil
}

func (s operationReceiptQueryFixture) GetRunScopeAuthority(_ context.Context, runID string) (RunScopeAuthority, error) {
	if runID != s.runAuthority.RunID {
		return RunScopeAuthority{}, ErrRunNotFound
	}
	return s.runAuthority, nil
}

type operationReceiptAuthorityFixture struct {
	repository RepositoryAuthority
	onboarding OnboardingAuthority
}

type retiredOperationReceiptStore struct{ began bool }

func (s *retiredOperationReceiptStore) BeginOperationReceipt(context.Context, OperationReceipt) (OperationReceipt, bool, error) {
	s.began = true
	return OperationReceipt{}, false, nil
}

func (*retiredOperationReceiptStore) AdvanceOperationReceipt(context.Context, OperationReceiptMutation) (OperationReceipt, bool, error) {
	return OperationReceipt{}, false, nil
}

func (*retiredOperationReceiptStore) GetOperationReceiptTarget(context.Context, string) (OperationReceiptTarget, error) {
	return OperationReceiptTarget{}, ErrOperationReceiptNotFound
}

func (*retiredOperationReceiptStore) GetAuthorizedOperationReceipt(context.Context, string, AuthorizedScopeSet) (OperationReceipt, error) {
	return OperationReceipt{}, ErrOperationReceiptNotFound
}

func (s operationReceiptAuthorityFixture) RepositoryAuthority(_ context.Context, repository string) (RepositoryAuthority, bool, error) {
	return s.repository, repository == s.repository.Repository, nil
}

func (s operationReceiptAuthorityFixture) OnboardingAuthority(_ context.Context, onboardingID string) (OnboardingAuthority, bool, error) {
	return s.onboarding, onboardingID == s.onboarding.OnboardingID, nil
}

func TestOperationReceiptIdentityIsScopeNeutralDeterministicAndSanitized(t *testing.T) {
	requester := domain.GitHubUserIdentity{Login: "Operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	now := time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)
	for _, scope := range []AuthorityScopeKind{ScopeController, ScopeRepository, ScopeRun, ScopeOnboarding} {
		input := OperationReceiptInput{OperationType: OperationRetry, Scope: scope, TargetID: string(scope) + "-target", Requester: requester, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("d", 64), TargetBindingDigest: strings.Repeat("c", 64), AcceptedAt: now}
		first := NewOperationReceipt(input)
		second := NewOperationReceipt(input)
		if err := ValidateOperationReceipt(first); err != nil {
			t.Fatalf("scope=%s receipt=%+v err=%v", scope, first, err)
		}
		if first != second || first.OperationID == "" || first.AuthorityKey == "" || first.Requester.Login != "operator" {
			t.Fatalf("scope=%s first=%+v second=%+v", scope, first, second)
		}
		raw, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"AuthorityKey", "authority_key", "OperationAnchorDigest", "operation_anchor_digest", "idempotency_key", "filesystem", "session_id", "instructions"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("scope=%s receipt leaked %q: %s", scope, forbidden, raw)
			}
		}
	}
}

func TestOperationReceiptSeparatesPhaseFromOutcome(t *testing.T) {
	receipt := NewOperationReceipt(OperationReceiptInput{OperationType: OperationDecide, Scope: ScopeRun, TargetID: "run", Requester: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("d", 64), TargetBindingDigest: strings.Repeat("c", 64), AcceptedAt: time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)})
	receipt.Outcome = OperationOutcomeConflict
	receipt.SettledAt = receipt.AcceptedAt.Add(time.Second)
	if err := ValidateOperationReceipt(receipt); err != nil {
		t.Fatalf("accepted conflict should be representable: %v", err)
	}
	receipt.Phase = OperationPhaseObserved
	if err := ValidateOperationReceipt(receipt); err == nil {
		t.Fatal("observed receipt without applied timestamp was accepted")
	}
}

func TestOperationReceiptServiceRejectsRetiredCleanupSourceProducer(t *testing.T) {
	store := &retiredOperationReceiptStore{}
	service, err := NewOperationReceiptService(store)
	if err != nil {
		t.Fatal(err)
	}
	input := OperationReceiptInput{OperationType: OperationRecoverCleanupSource, Scope: ScopeRun, TargetID: "run", Requester: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("d", 64), TargetBindingDigest: strings.Repeat("c", 64), AcceptedAt: time.Date(2026, 8, 31, 1, 0, 0, 0, time.UTC)}
	if _, _, err := service.Accept(context.Background(), input); err == nil || store.began {
		t.Fatalf("retired operation accepted=%t err=%v", store.began, err)
	}
	if err := ValidateOperationReceipt(NewOperationReceipt(input)); err != nil {
		t.Fatalf("historical receipt is no longer readable: %v", err)
	}
}

func TestEveryLegalOperatorActionProjectsToTheCommonReceiptContract(t *testing.T) {
	requester := Requester{ID: "operator", Kind: "github_login", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	authority := strings.Repeat("b", 64)
	binding := strings.Repeat("c", 64)
	now := time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)
	for _, operationType := range []OperationType{OperationDecide, OperationRetry, OperationAbandon, OperationRecoverCIWait, OperationRecoverOwnedPush, OperationAcceptExternalMerge} {
		action := newOperatorActionRecord(OperatorActionInput{Requester: requester, RunID: "run", Repository: "owner/repo", ExpectedState: domain.StateManualIntervention, RunIdempotencyKey: "run-key", TransitionSequence: 9, ActionType: OperatorActionType(operationType), ReasonCode: "operator_attention", AttentionEventKey: "attention-key", RequestDigest: NoOperationInputDigest(), ExpectedAuthorityDigest: authority}, now)
		receipt, err := OperationReceiptForOperatorAction(action, binding)
		if err != nil || receipt.OperationType != operationType || receipt.Scope != ScopeRun || receipt.TargetID != action.RunID || receipt.Phase != OperationPhaseAccepted || receipt.Outcome != OperationOutcomePending {
			t.Fatalf("operation=%s receipt=%+v err=%v", operationType, receipt, err)
		}
	}
}

func TestOperationReceiptQueryReauthorizesEverySupportedScope(t *testing.T) {
	identity := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	requester := Requester{ID: identity.Login, Kind: "github_login", DatabaseID: identity.DatabaseID, NodeID: identity.NodeID, ActorType: identity.ActorType}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: identity})
	if err != nil {
		t.Fatal(err)
	}
	repository := RepositoryAuthority{Repository: "owner/repo", ProfileID: "repository-profile:owner/repo", BindingDigest: strings.Repeat("c", 64), AllowedLogins: []string{identity.Login}, TrustedOperators: []domain.GitHubUserIdentity{identity}, Enabled: true}
	onboarding := OnboardingAuthority{OnboardingID: "onboarding-1"}
	runAuthority := RunScopeAuthority{RunID: "run-1", Repository: repository.Repository, BindingDigest: repository.BindingDigest, PersistenceBindingValue: repository.BindingDigest, AllowedLogins: repository.AllowedLogins, TrustedOperators: repository.TrustedOperators}
	authorities := operationReceiptAuthorityFixture{repository: repository, onboarding: onboarding}
	tests := []struct {
		scope   AuthorityScopeKind
		target  string
		binding string
	}{
		{ScopeController, controllerScopeID, identityDigest(identity)},
		{ScopeRepository, repository.Repository, repository.BindingDigest},
		{ScopeRun, runAuthority.RunID, runAuthority.PersistenceBindingValue},
		{ScopeOnboarding, onboarding.OnboardingID, identityDigest(identity)},
	}
	for _, test := range tests {
		receipt := NewOperationReceipt(OperationReceiptInput{OperationType: OperationRetry, Scope: test.scope, TargetID: test.target, Requester: identity, RequestDigest: strings.Repeat("a", 64), ExpectedAuthorityDigest: strings.Repeat("b", 64), OperationAnchorDigest: strings.Repeat("d", 64), TargetBindingDigest: test.binding, AcceptedAt: time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)})
		store := operationReceiptQueryFixture{receipt: receipt, runAuthority: runAuthority}
		query, err := NewOperationReceiptQueryService(store, authorizer, authorities, authorities)
		if err != nil {
			t.Fatal(err)
		}
		got, err := query.Get(context.Background(), requester, receipt.OperationID)
		if err != nil || got != receipt {
			t.Fatalf("scope=%s receipt=%+v err=%v", test.scope, got, err)
		}
		denied := requester
		denied.NodeID = "USER_other"
		_, deniedErr := query.Get(context.Background(), denied, receipt.OperationID)
		var serviceErr *ServiceError
		if !errors.As(deniedErr, &serviceErr) || serviceErr.Category != ErrorNotFound {
			t.Fatalf("scope=%s denied=%v", test.scope, deniedErr)
		}
	}
}
