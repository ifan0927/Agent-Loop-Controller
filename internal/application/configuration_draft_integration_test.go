package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type configurationDraftDocumentFixture struct {
	mu       sync.Mutex
	files    *configurationFilesFixture
	base     application.ConfigurationEditableSettings
	settings map[string]application.ConfigurationEditableSettings
}

type configurationDraftSettlementFaultStore struct {
	*sqlitestore.Store
	fail bool
}

func (s *configurationDraftSettlementFaultStore) BindConfigurationDraftApply(ctx context.Context, input application.ConfigurationDraftApplyBinding) (application.ConfigurationDraft, bool, error) {
	if s.fail && input.State == application.ConfigurationDraftApplied {
		s.fail = false
		return application.ConfigurationDraft{}, false, errors.New("injected draft settlement response loss")
	}
	return s.Store.BindConfigurationDraftApply(ctx, input)
}

func (f *configurationDraftDocumentFixture) ProjectEditable(payload []byte) (application.ConfigurationEditableSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if settings, found := f.settings[string(payload)]; found {
		return settings, nil
	}
	return f.base, nil
}

func (f *configurationDraftDocumentFixture) ProjectHistoricalEditable(payload []byte, _ int) (application.ConfigurationEditableSettings, error) {
	return f.ProjectEditable(payload)
}

func (f *configurationDraftDocumentFixture) MaterializeEditable(base []byte, settings application.ConfigurationEditableSettings) ([]byte, error) {
	current, _ := f.ProjectEditable(base)
	if current == settings {
		return append([]byte(nil), base...), nil
	}
	payload, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.settings[string(payload)] = settings
	f.mu.Unlock()
	return payload, nil
}

func (f *configurationDraftDocumentFixture) ValidateEditableCandidate(_, candidate []byte) (application.ValidatedConfigurationCandidate, error) {
	return f.files.candidate(candidate, 5), nil
}

func TestConfigurationDraftLifecyclePreviewApplyAndReplay(t *testing.T) {
	configuration, store, files, runtime, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, time.Now().UTC())
	document := newConfigurationDraftDocumentFixture(files)
	service, err := application.NewConfigurationDraftService(configuration, store, document)
	if err != nil {
		t.Fatal(err)
	}

	draft, err := service.Open(ctx, requester)
	if err != nil || draft.Revision != 1 || draft.State != application.ConfigurationDraftOpen {
		t.Fatalf("draft=%+v err=%v", draft, err)
	}
	resumed, err := service.Open(ctx, requester)
	if err != nil || resumed.DraftID != draft.DraftID || resumed.Revision != 1 {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
	denied := requester
	denied.DatabaseID++
	if _, err := service.Show(ctx, application.ConfigurationDraftCommand{Requester: denied, DraftID: draft.DraftID, Revision: 1}); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("unauthorized show err=%v", err)
	}

	capacity := 3
	edit := application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: 1, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}}
	edited, err := service.Edit(ctx, edit)
	if err != nil || edited.Revision != 2 || edited.Settings.Admission.HeavyCapacity != 3 {
		t.Fatalf("edited=%+v err=%v", edited, err)
	}
	replayedEdit, err := service.Edit(ctx, edit)
	if err != nil || replayedEdit.Revision != 2 {
		t.Fatalf("edit replay=%+v err=%v", replayedEdit, err)
	}
	otherCapacity := 4
	edit.Edit.Integer = &otherCapacity
	if _, err := service.Edit(ctx, edit); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("different replay err=%v", err)
	}

	validation, err := service.Validate(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: 2})
	if err != nil || !validation.Valid || len(validation.Findings) != 0 {
		t.Fatalf("validation=%+v err=%v", validation, err)
	}
	preview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: 2})
	if err != nil || len(preview.Changes) != 1 || preview.Changes[0].Category != application.ConfigurationPreviewHeavyCapacityIncreased {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	stablePreview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: 2})
	if err != nil || stablePreview.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("stable preview=%+v err=%v", stablePreview, err)
	}
	applyCommand := application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: draft.DraftID, Revision: 2, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest}
	result, err := service.Apply(ctx, applyCommand)
	if err != nil || result.Apply.NoOp || result.Apply.Generation.GenerationID != authority.Desired.GenerationID+1 || result.Apply.Receipt.OperationID == "" || result.Convergence.State != application.ConfigurationRestartRequired {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	replay, err := service.Apply(ctx, applyCommand)
	if err != nil || replay.Apply.Generation.GenerationID != result.Apply.Generation.GenerationID || replay.Apply.Receipt.OperationID != result.Apply.Receipt.OperationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if generations, err := store.ListConfigurationGenerations(ctx); err != nil || len(generations) != 2 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestConfigurationForwardRollbackLifecycleProvenanceAndNoOpIdentity(t *testing.T) {
	configuration, store, files, runtime, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, time.Now().UTC())
	document := newConfigurationDraftDocumentFixture(files)
	service, err := application.NewConfigurationDraftService(configuration, store, document)
	if err != nil {
		t.Fatal(err)
	}

	normal, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	capacity := 3
	normal, err = service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: normal.DraftID, Revision: normal.Revision, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}})
	if err != nil {
		t.Fatal(err)
	}
	normalPreview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: normal.DraftID, Revision: normal.Revision})
	if err != nil {
		t.Fatal(err)
	}
	normalApplied, err := service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: normal.DraftID, Revision: normal.Revision, PreviewDigest: normalPreview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest})
	if err != nil || normalApplied.Apply.Generation.GenerationID != 2 {
		t.Fatalf("normal apply=%+v err=%v", normalApplied, err)
	}

	sources, err := service.RollbackSources(ctx, requester)
	if err != nil || sources.DesiredGenerationID != 2 || len(sources.Sources) != 1 || sources.Sources[0].GenerationID != 1 || sources.Sources[0].Origin != application.ConfigurationOriginBaseline {
		t.Fatalf("sources=%+v err=%v", sources, err)
	}
	source := sources.Sources[0]
	rollbackCommand := application.ConfigurationRollbackOpenCommand{Requester: requester, SourceGenerationID: source.GenerationID, SourceDigest: source.Digest, ExpectedGenerationID: sources.DesiredGenerationID, ExpectedDigest: sources.DesiredDigest}
	stale := rollbackCommand
	stale.ExpectedGenerationID = 1
	stale.ExpectedDigest = source.Digest
	if _, err := service.OpenRollback(ctx, stale); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("stale rollback open err=%v", err)
	}
	blockingNormal, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.OpenRollback(ctx, rollbackCommand); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("active normal draft did not conflict: %v", err)
	}
	if _, err := service.Discard(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: blockingNormal.DraftID, Revision: blockingNormal.Revision}); err != nil {
		t.Fatal(err)
	}
	rollback, err := service.OpenRollback(ctx, rollbackCommand)
	if err != nil || rollback.DraftOrigin != application.ConfigurationDraftOriginRollback || rollback.Revision != 1 || rollback.BaseGenerationID != 2 || rollback.Settings.Admission.HeavyCapacity != 2 || rollback.RollbackSourceGenerationID != 1 || rollback.RollbackSourceDigest != source.Digest {
		t.Fatalf("rollback=%+v err=%v", rollback, err)
	}
	replayedOpen, err := service.OpenRollback(ctx, rollbackCommand)
	if err != nil || replayedOpen.DraftID != rollback.DraftID {
		t.Fatalf("open replay=%+v err=%v", replayedOpen, err)
	}
	claimed, err := store.ClaimConfigurationRawPrune(ctx, source.Digest)
	if err != nil || !claimed {
		t.Fatalf("source prune claim=%t err=%v", claimed, err)
	}
	if err := files.RemoveRaw(source.Digest); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteConfigurationRawPrune(ctx, source.Digest, true); err != nil {
		t.Fatal(err)
	}
	replayedAfterPrune, err := service.OpenRollback(ctx, rollbackCommand)
	if err != nil || replayedAfterPrune.DraftID != rollback.DraftID {
		t.Fatalf("pruned-source open replay=%+v err=%v", replayedAfterPrune, err)
	}
	resumedNormal, err := service.Open(ctx, requester)
	if err != nil || resumedNormal.DraftID != rollback.DraftID || resumedNormal.DraftOrigin != application.ConfigurationDraftOriginRollback {
		t.Fatalf("normal resume=%+v err=%v", resumedNormal, err)
	}

	preview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: rollback.DraftID, Revision: rollback.Revision})
	if err != nil || preview.DraftOrigin != application.ConfigurationDraftOriginRollback || preview.RollbackSourceGenerationID != 1 || preview.RollbackSourceDigest != source.Digest || len(preview.Changes) != 1 || preview.Changes[0].Category != application.ConfigurationPreviewHeavyCapacityDecreased {
		t.Fatalf("rollback preview=%+v err=%v", preview, err)
	}
	rolledBack, err := service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: rollback.DraftID, Revision: rollback.Revision, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: 2, ExpectedDigest: sources.DesiredDigest})
	if err != nil || rolledBack.Apply.NoOp || rolledBack.Apply.Generation.GenerationID != 3 || rolledBack.Apply.Generation.ParentID != 2 || rolledBack.Apply.Generation.RollbackSourceGenerationID != 1 || rolledBack.Apply.Generation.RollbackSourceDigest != source.Digest {
		t.Fatalf("rollback apply=%+v err=%v", rolledBack, err)
	}
	replayedApply, err := service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: rollback.DraftID, Revision: rollback.Revision, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: 2, ExpectedDigest: sources.DesiredDigest})
	if err != nil || replayedApply.Apply.Receipt.OperationID != rolledBack.Apply.Receipt.OperationID || replayedApply.Apply.Generation.GenerationID != 3 {
		t.Fatalf("rollback replay=%+v err=%v", replayedApply, err)
	}

	current, found, err := store.ConfigurationAuthority(ctx)
	if err != nil || !found || current.Desired.GenerationID != 3 {
		t.Fatalf("authority=%+v found=%t err=%v", current, found, err)
	}
	remainingSources, err := service.RollbackSources(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	var noOpSource application.ConfigurationRollbackSource
	for _, candidate := range remainingSources.Sources {
		if candidate.GenerationID == 2 {
			noOpSource = candidate
		}
	}
	if noOpSource.GenerationID == 0 || noOpSource.EffectiveAt != nil {
		t.Fatalf("superseded source without effective observation was unavailable: %+v", remainingSources)
	}
	noOpRollbackCommand := application.ConfigurationRollbackOpenCommand{Requester: requester, SourceGenerationID: noOpSource.GenerationID, SourceDigest: noOpSource.Digest, ExpectedGenerationID: current.Desired.GenerationID, ExpectedDigest: current.Desired.Digest}
	noOpRollback, err := service.OpenRollback(ctx, noOpRollbackCommand)
	if err != nil {
		t.Fatal(err)
	}
	capacity = 2
	noOpRollback, err = service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: noOpRollback.DraftID, Revision: 1, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}})
	if err != nil || noOpRollback.RollbackSourceGenerationID != noOpSource.GenerationID || noOpRollback.RollbackSourceDigest != noOpSource.Digest {
		t.Fatalf("no-op rollback edit=%+v err=%v", noOpRollback, err)
	}
	noOpRollbackPreview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: noOpRollback.DraftID, Revision: noOpRollback.Revision})
	if err != nil || len(noOpRollbackPreview.Changes) != 0 {
		t.Fatalf("no-op rollback preview=%+v err=%v", noOpRollbackPreview, err)
	}
	noOpRollbackResult, err := service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: noOpRollback.DraftID, Revision: noOpRollback.Revision, PreviewDigest: noOpRollbackPreview.PreviewDigest, ExpectedGenerationID: current.Desired.GenerationID, ExpectedDigest: current.Desired.Digest})
	if err != nil || !noOpRollbackResult.Apply.NoOp || noOpRollbackResult.Apply.Generation.GenerationID != 3 {
		t.Fatalf("no-op rollback=%+v err=%v", noOpRollbackResult, err)
	}

	normalNoOp, err := service.Open(ctx, requester)
	if err != nil || normalNoOp.DraftOrigin != application.ConfigurationDraftOriginNormal {
		t.Fatalf("normal no-op draft=%+v err=%v", normalNoOp, err)
	}
	normalNoOpPreview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: normalNoOp.DraftID, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	normalNoOpResult, err := service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: normalNoOp.DraftID, Revision: 1, PreviewDigest: normalNoOpPreview.PreviewDigest, ExpectedGenerationID: 3, ExpectedDigest: current.Desired.Digest})
	if err != nil || !normalNoOpResult.Apply.NoOp || normalNoOpResult.Apply.Receipt.OperationID == noOpRollbackResult.Apply.Receipt.OperationID {
		t.Fatalf("normal no-op=%+v rollback operation=%s err=%v", normalNoOpResult, noOpRollbackResult.Apply.Receipt.OperationID, err)
	}
	if generations, err := store.ListConfigurationGenerations(ctx); err != nil || len(generations) != 3 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestConfigurationRollbackSourcesExcludeCorruptAndPrunedEvidence(t *testing.T) {
	configuration, store, files, runtime, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, time.Now().UTC())
	document := newConfigurationDraftDocumentFixture(files)
	service, err := application.NewConfigurationDraftService(configuration, store, document)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	capacity := 3
	draft, _ = service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: 1, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}})
	preview, _ := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: 2})
	if _, err := service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: draft.DraftID, Revision: 2, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest}); err != nil {
		t.Fatal(err)
	}
	baselinePayload := append([]byte(nil), files.raw[authority.Desired.Digest]...)
	files.raw[authority.Desired.Digest] = []byte("corrupt retained evidence")
	sources, err := service.RollbackSources(ctx, requester)
	if err != nil || len(sources.Sources) != 0 {
		t.Fatalf("corrupt sources=%+v err=%v", sources, err)
	}
	files.raw[authority.Desired.Digest] = baselinePayload
	sources, err = service.RollbackSources(ctx, requester)
	if err != nil || len(sources.Sources) != 1 {
		t.Fatalf("restored sources=%+v err=%v", sources, err)
	}
	claimed, err := store.ClaimConfigurationRawPrune(ctx, authority.Desired.Digest)
	if err != nil || !claimed {
		t.Fatalf("claimed=%t err=%v", claimed, err)
	}
	if err := files.RemoveRaw(authority.Desired.Digest); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteConfigurationRawPrune(ctx, authority.Desired.Digest, true); err != nil {
		t.Fatal(err)
	}
	sources, err = service.RollbackSources(ctx, requester)
	if err != nil || len(sources.Sources) != 0 {
		t.Fatalf("pruned sources=%+v err=%v", sources, err)
	}
}

func TestConfigurationDraftEditInvalidatesPreviewAndNoOpUsesExactBaseBytes(t *testing.T) {
	configuration, store, files, runtime, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, time.Now().UTC())
	document := newConfigurationDraftDocumentFixture(files)
	service, err := application.NewConfigurationDraftService(configuration, store, document)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: 1})
	if err != nil || len(preview.Changes) != 0 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: draft.DraftID, Revision: 1, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest})
	if err != nil || !result.Apply.NoOp || result.Apply.Generation.GenerationID != authority.Desired.GenerationID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !bytes.Equal(files.live, []byte("baseline configuration")) {
		t.Fatalf("no-op changed exact live bytes: %q", files.live)
	}
	if generations, err := store.ListConfigurationGenerations(ctx); err != nil || len(generations) != 1 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}

	discardedDraft, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	timeout := application.ConfigurationDuration(45 * time.Minute)
	edited, err := service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: discardedDraft.DraftID, Revision: 1, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldRunTimeout, Duration: &timeout}})
	if err != nil || edited.Validation != nil || edited.Preview != nil {
		t.Fatalf("edited=%+v err=%v", edited, err)
	}
	discarded, err := service.Discard(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: edited.DraftID, Revision: edited.Revision})
	if err != nil || discarded.State != application.ConfigurationDraftDiscarded {
		t.Fatalf("discarded=%+v err=%v", discarded, err)
	}
	replay, err := service.Discard(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: edited.DraftID, Revision: edited.Revision})
	if err != nil || replay.State != application.ConfigurationDraftDiscarded {
		t.Fatalf("discard replay=%+v err=%v", replay, err)
	}
}

func TestConfigurationDraftApplyRecoversSettlementResponseLoss(t *testing.T) {
	configuration, store, files, runtime, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, time.Now().UTC())
	faults := &configurationFaultStore{Store: store, failSettle: true}
	faultyConfiguration, err := application.NewConfigurationService(faults, files, runtime)
	if err != nil {
		t.Fatal(err)
	}
	document := newConfigurationDraftDocumentFixture(files)
	service, err := application.NewConfigurationDraftService(faultyConfiguration, store, document)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	capacity := 3
	draft, err = service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	command := application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest}
	if _, err := service.Apply(ctx, command); err == nil {
		t.Fatal("injected response loss unexpectedly succeeded")
	}
	restartedConfiguration, err := application.NewConfigurationService(store, files, runtime)
	if err != nil {
		t.Fatal(err)
	}
	service, err = application.NewConfigurationDraftService(restartedConfiguration, store, document)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Apply(ctx, command)
	if err != nil || result.Apply.Generation.GenerationID != 2 {
		t.Fatalf("recovered=%+v err=%v", result, err)
	}
	replay, err := service.Apply(ctx, command)
	if err != nil || replay.Apply.Receipt.OperationID != result.Apply.Receipt.OperationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if generations, err := store.ListConfigurationGenerations(ctx); err != nil || len(generations) != 2 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestConfigurationDraftStalePreviewConflictsAndCapacityDecreaseUsesDrainImpact(t *testing.T) {
	configuration, store, files, _, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewConfigurationDraftService(configuration, store, newConfigurationDraftDocumentFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	capacity := 1
	draft, err = service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision})
	if err != nil || len(preview.Changes) != 1 || preview.Changes[0].Category != application.ConfigurationPreviewHeavyCapacityDecreased || !slices.Contains(preview.Impacts, application.ConfigurationImpactCapacityReductionUsesDrainSemantics) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	pages := 6
	edited, err := service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionMaxPages, Integer: &pages}})
	if err != nil || edited.Preview != nil || edited.Validation != nil {
		t.Fatalf("edited=%+v err=%v", edited, err)
	}
	_, err = service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: draft.DraftID, Revision: preview.Revision, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest})
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("stale preview err=%v", err)
	}
}

func TestConfigurationDraftReplaySettlesAfterGenerationCommittedBeforeDraft(t *testing.T) {
	configuration, store, files, runtime, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, time.Now().UTC())
	document := newConfigurationDraftDocumentFixture(files)
	faults := &configurationDraftSettlementFaultStore{Store: store, fail: true}
	service, err := application.NewConfigurationDraftService(configuration, faults, document)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	capacity := 3
	draft, err = service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	command := application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest}
	if _, err := service.Apply(ctx, command); err == nil || !strings.Contains(err.Error(), "settlement") {
		t.Fatalf("injected draft settlement err=%v", err)
	}
	current, found, err := store.ConfigurationAuthority(ctx)
	if err != nil || !found || current.Desired.GenerationID != 2 || current.Incomplete != nil {
		t.Fatalf("authority=%+v found=%t err=%v", current, found, err)
	}
	restarted, err := application.NewConfigurationDraftService(configuration, store, document)
	if err != nil {
		t.Fatal(err)
	}
	result, err := restarted.Apply(ctx, command)
	if err != nil || result.Apply.Generation.GenerationID != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored, found, err := store.ConfigurationDraft(ctx, draft.DraftID)
	if err != nil || !found || stored.State != application.ConfigurationDraftApplied || stored.ResultOperationID != result.Apply.Receipt.OperationID {
		t.Fatalf("draft=%+v found=%t err=%v", stored, found, err)
	}
	if generations, err := store.ListConfigurationGenerations(ctx); err != nil || len(generations) != 2 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestConfigurationDraftPreAcceptanceConflictReturnsDraftToEditableState(t *testing.T) {
	configuration, store, files, _, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewConfigurationDraftService(configuration, store, newConfigurationDraftDocumentFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	capacity := 3
	draft, err = service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	files.live = []byte("external drift before acceptance")
	_, err = service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest})
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("apply err=%v", err)
	}
	stored, found, err := store.ConfigurationDraft(ctx, draft.DraftID)
	if err != nil || !found || stored.State != application.ConfigurationDraftOpen || stored.Preview != nil || stored.Validation != nil || stored.ResultOperationID != "" {
		t.Fatalf("draft=%+v found=%t err=%v", stored, found, err)
	}
	if generations, err := store.ListConfigurationGenerations(ctx); err != nil || len(generations) != 1 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestConfigurationDraftAmbiguousApplyPreservesOperationBinding(t *testing.T) {
	configuration, store, files, _, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := configuration.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewConfigurationDraftService(configuration, store, newConfigurationDraftDocumentFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	draft, err := service.Open(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	capacity := 3
	draft, err = service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision})
	if err != nil {
		t.Fatal(err)
	}
	files.replaceMode = "third"
	_, err = service.Apply(ctx, application.ConfigurationDraftApplyCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, PreviewDigest: preview.PreviewDigest, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("apply err=%v", err)
	}
	stored, found, err := store.ConfigurationDraft(ctx, draft.DraftID)
	if err != nil || !found || stored.State != application.ConfigurationDraftAmbiguous || stored.ResultOperationID == "" || stored.ResultGenerationID <= 0 {
		t.Fatalf("draft=%+v found=%t err=%v", stored, found, err)
	}
	if _, err := service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: application.ConfigurationEdit{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity}}); err == nil {
		t.Fatal("ambiguous draft remained editable")
	}
	if sources, err := service.RollbackSources(ctx, requester); err != nil || len(sources.Sources) != 0 {
		t.Fatalf("ambiguous generation became rollback source: %+v err=%v", sources, err)
	}
	resumed, err := service.Open(ctx, requester)
	if err != nil || resumed.DraftID != draft.DraftID || resumed.State != application.ConfigurationDraftAmbiguous {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
}

func TestConfigurationDraftClosedEditableFieldsAndBounds(t *testing.T) {
	configuration, store, files, _, requester := configurationServiceFixture(t)
	ctx := context.Background()
	if _, err := configuration.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := application.NewConfigurationDraftService(configuration, store, newConfigurationDraftDocumentFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	trueValue, falseValue := true, false
	runTimeout, poll, delivery, ttl, renewal := application.ConfigurationDuration(45*time.Minute), application.ConfigurationDuration(10*time.Minute), application.ConfigurationDuration(time.Minute), application.ConfigurationDuration(2*time.Minute), application.ConfigurationDuration(30*time.Second)
	maxCandidates, maxPages, capacity := 30, 10, 4
	tests := []application.ConfigurationEdit{
		{Field: application.ConfigurationFieldRunTimeout, Duration: &runTimeout},
		{Field: application.ConfigurationFieldAdmissionEnabled, Boolean: &falseValue},
		{Field: application.ConfigurationFieldAdmissionPollInterval, Duration: &poll},
		{Field: application.ConfigurationFieldDeliveryPollInterval, Duration: &delivery},
		{Field: application.ConfigurationFieldSchedulerLeaseTTL, Duration: &ttl},
		{Field: application.ConfigurationFieldSchedulerLeaseRenewalInterval, Duration: &renewal},
		{Field: application.ConfigurationFieldAdmissionMaxCandidates, Integer: &maxCandidates},
		{Field: application.ConfigurationFieldAdmissionMaxPages, Integer: &maxPages},
		{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &capacity},
	}
	for _, edit := range tests {
		draft, err := service.Open(ctx, requester)
		if err != nil {
			t.Fatalf("field=%s open: %v", edit.Field, err)
		}
		edited, err := service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: edit})
		if err != nil || edited.Revision != 2 {
			t.Fatalf("field=%s edited=%+v err=%v", edit.Field, edited, err)
		}
		if _, err := service.Discard(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: edited.DraftID, Revision: edited.Revision}); err != nil {
			t.Fatalf("field=%s discard: %v", edit.Field, err)
		}
	}
	invalidDuration := application.ConfigurationDuration(3 * time.Hour)
	invalidInteger := 33
	invalid := []application.ConfigurationEdit{
		{Field: application.ConfigurationFieldRunTimeout, Duration: &invalidDuration},
		{Field: application.ConfigurationFieldAdmissionEnabled, Integer: &maxPages},
		{Field: application.ConfigurationFieldAdmissionHeavyCapacity, Integer: &invalidInteger},
		{Field: application.ConfigurationFieldID("controller.raw"), Boolean: &trueValue},
	}
	for _, edit := range invalid {
		draft, err := service.Open(ctx, requester)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Edit(ctx, application.ConfigurationDraftEditCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision, Edit: edit}); err == nil || !strings.Contains(err.Error(), "invalid_input") {
			t.Fatalf("invalid field=%s err=%v", edit.Field, err)
		}
		if _, err := service.Discard(ctx, application.ConfigurationDraftCommand{Requester: requester, DraftID: draft.DraftID, Revision: draft.Revision}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigurationDraftOpenRequiresCurrentSchemaWithoutCreatingDraft(t *testing.T) {
	configuration, store, files, _, requester := configurationServiceFixture(t)
	files.baselineSchema = 4
	if _, err := configuration.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := application.NewConfigurationDraftService(configuration, store, newConfigurationDraftDocumentFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Open(context.Background(), requester); err == nil || !strings.Contains(err.Error(), "schema upgrade") {
		t.Fatalf("open err=%v", err)
	}
	if _, found, err := store.ActiveConfigurationDraft(context.Background()); err != nil || found {
		t.Fatalf("draft found=%t err=%v", found, err)
	}
}

func newConfigurationDraftDocumentFixture(files *configurationFilesFixture) *configurationDraftDocumentFixture {
	base := application.ConfigurationEditableSettings{RunTimeout: application.ConfigurationDuration(30 * time.Minute), Admission: application.ConfigurationEditableAdmissionSettings{Enabled: true, PollInterval: application.ConfigurationDuration(5 * time.Minute), DeliveryPollInterval: application.ConfigurationDuration(30 * time.Second), SchedulerLeaseTTL: application.ConfigurationDuration(time.Minute), SchedulerLeaseRenewalInterval: application.ConfigurationDuration(20 * time.Second), MaxCandidates: 20, MaxPages: 5, HeavyCapacity: 2}}
	return &configurationDraftDocumentFixture{files: files, base: base, settings: map[string]application.ConfigurationEditableSettings{string(files.live): base}}
}
