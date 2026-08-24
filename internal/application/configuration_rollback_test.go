package application

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type rollbackPreviewDocument struct{ settings ConfigurationEditableSettings }

func (d rollbackPreviewDocument) ProjectEditable([]byte) (ConfigurationEditableSettings, error) {
	return d.settings, nil
}
func (d rollbackPreviewDocument) ProjectHistoricalEditable([]byte, int) (ConfigurationEditableSettings, error) {
	return d.settings, nil
}
func (d rollbackPreviewDocument) MaterializeEditable([]byte, ConfigurationEditableSettings) ([]byte, error) {
	return nil, nil
}
func (d rollbackPreviewDocument) ValidateEditableCandidate([]byte, []byte) (ValidatedConfigurationCandidate, error) {
	return ValidatedConfigurationCandidate{}, nil
}

func TestRollbackEligibilityAllowsSafelySupersededGenerationWithoutEffectiveObservation(t *testing.T) {
	created := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	committed := created.Add(time.Minute)
	superseded := committed.Add(time.Minute)
	generation := ConfigurationGeneration{
		GenerationID: 2, ParentID: 1, Digest: strings.Repeat("a", 64), SchemaVersion: 5,
		Origin: ConfigurationOriginApply, Requester: domain.GitHubUserIdentity{Login: "operator", DatabaseID: 1, NodeID: "USER_1", ActorType: "User"},
		OperationID: "operation", State: ConfigurationGenerationSuperseded, RawRetained: true, SettlementEvidenceValid: true,
		CreatedAt: created, CommittedAt: committed, SupersededAt: superseded, SettledAt: superseded,
	}
	if !eligibleRollbackGeneration(generation, 3) {
		t.Fatal("safe superseded generation without an effective observation was excluded")
	}
	generation.SettlementEvidenceValid = false
	if eligibleRollbackGeneration(generation, 3) {
		t.Fatal("generation with contradictory settlement evidence was eligible")
	}
}

func TestNormalApplyIdentityRemainsCompatibleWhileRollbackBindsSource(t *testing.T) {
	requester := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 1, NodeID: "USER_1", ActorType: "User"}
	configured := ConfiguredRequester{identity: requester}
	scopes, err := newAuthorizedScopeSet(requester, AuthorityScope{Kind: ScopeController, ID: controllerScopeID, AuthorityDigest: identityDigest(requester)})
	if err != nil {
		t.Fatal(err)
	}
	candidate := ValidatedConfigurationCandidate{Digest: strings.Repeat("b", 64)}
	at := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	normal := configurationApplyReceiptFor(2, strings.Repeat("a", 64), configured, scopes, candidate, ConfigurationApplyProvenance{Kind: ConfigurationApplyNormal}, at)
	legacy := NewOperationReceipt(OperationReceiptInput{OperationType: OperationApplyConfiguration, Scope: ScopeController, TargetID: ConfigurationTargetID, Requester: requester, RequestDigest: candidate.Digest, ExpectedAuthorityDigest: strings.Repeat("a", 64), OperationAnchorDigest: configurationDigest("configuration-apply", "2", strings.Repeat("a", 64), candidate.Digest), TargetBindingDigest: normal.TargetBindingDigest, AcceptedAt: at})
	if normal.OperationID != legacy.OperationID {
		t.Fatalf("normal identity changed: got %s want %s", normal.OperationID, legacy.OperationID)
	}
	rollback := configurationApplyReceiptFor(2, strings.Repeat("a", 64), configured, scopes, candidate, ConfigurationApplyProvenance{Kind: ConfigurationApplyRollback, RollbackSourceGenerationID: 1, RollbackSourceDigest: strings.Repeat("c", 64)}, at)
	if rollback.OperationID == normal.OperationID {
		t.Fatal("rollback aliased the compatible normal operation identity")
	}
}

func TestNormalPreviewDigestRemainsCompatibleWhileRollbackBindsSource(t *testing.T) {
	settings := ConfigurationEditableSettings{RunTimeout: ConfigurationDuration(30 * time.Minute), Admission: ConfigurationEditableAdmissionSettings{PollInterval: ConfigurationDuration(5 * time.Minute), DeliveryPollInterval: ConfigurationDuration(30 * time.Second), SchedulerLeaseTTL: ConfigurationDuration(time.Minute), SchedulerLeaseRenewalInterval: ConfigurationDuration(20 * time.Second), MaxCandidates: 20, MaxPages: 5, HeavyCapacity: 2}}
	service := ConfigurationDraftService{document: rollbackPreviewDocument{settings: settings}}
	draft := ConfigurationDraft{DraftID: "configuration-draft-00000000000000000000000000000001", BaseGenerationID: 2, BaseDigest: strings.Repeat("a", 64), Revision: 1, DraftOrigin: ConfigurationDraftOriginNormal, Settings: settings}
	authority := ConfigurationAuthority{Desired: ConfigurationGeneration{GenerationID: 2, Digest: draft.BaseDigest}, Version: 7}
	candidate := []byte("candidate")
	preview, err := service.computePreview(draft, []byte("base"), candidate, authority)
	if err != nil {
		t.Fatal(err)
	}
	changesJSON, _ := json.Marshal([]ConfigurationPreviewChange{})
	impactsJSON, _ := json.Marshal([]ConfigurationPreviewImpact{})
	want := configurationDigest("configuration-preview-v1", draft.DraftID, "1", "2", draft.BaseDigest, digestBytes(candidate), string(changesJSON), string(impactsJSON), "2", authority.Desired.Digest, "7")
	if preview.PreviewDigest != want {
		t.Fatalf("normal preview digest changed: got %s want %s", preview.PreviewDigest, want)
	}
	draft.DraftOrigin = ConfigurationDraftOriginRollback
	draft.RollbackSourceGenerationID = 1
	draft.RollbackSourceDigest = strings.Repeat("c", 64)
	rollback, err := service.computePreview(draft, []byte("base"), candidate, authority)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.PreviewDigest == preview.PreviewDigest {
		t.Fatal("rollback preview aliased the compatible normal preview digest")
	}
}
