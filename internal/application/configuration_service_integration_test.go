package application_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sqlitestore "github.com/ifan0927/Agent-Loop-Controller/internal/adapters/sqlite"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type configurationFilesFixture struct {
	mu             sync.Mutex
	path           string
	database       string
	operator       domain.GitHubUserIdentity
	live           []byte
	raw            map[string][]byte
	replaceMode    string
	baselineSchema int
	retainError    bool
	baselineError  bool
	removeError    bool
	rereadError    bool
	replacementMu  sync.Mutex
	replaceStarted chan struct{}
	replaceRelease chan struct{}
	replaceCalls   int
	removeBlock    sync.Once
	removeStarted  chan string
	removeRelease  chan struct{}
	removeUnlocked bool
}

type configurationFixtureLock struct{ mutex *sync.Mutex }

func (l configurationFixtureLock) Release() error { l.mutex.Unlock(); return nil }

func (f *configurationFilesFixture) AcquireMutation() (application.ConfigurationReplacementLock, bool, error) {
	if !f.replacementMu.TryLock() {
		return nil, false, nil
	}
	return configurationFixtureLock{mutex: &f.replacementMu}, true, nil
}

func (f *configurationFilesFixture) AcquireReplacement(string) (application.ConfigurationReplacementLock, bool, error) {
	return f.AcquireMutation()
}

func (f *configurationFilesFixture) CanonicalConfigPath() string { return f.path }
func (f *configurationFilesFixture) ValidateBaseline(payload []byte) (application.ValidatedConfigurationCandidate, error) {
	schema := f.baselineSchema
	if schema == 0 {
		schema = 5
	}
	return f.candidate(payload, schema), nil
}

func TestLegacyConfigurationBaselineCanAuthorizeCurrentSchemaTransition(t *testing.T) {
	service, store, files, _, requester := configurationServiceFixture(t)
	files.baselineSchema = 4
	authority, err := service.Initialize(context.Background())
	if err != nil || authority.Desired.SchemaVersion != 4 || authority.Desired.ConfiguredOperator.Login != requester.ID {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
	result, err := service.Apply(context.Background(), application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("current schema transition")})
	if err != nil || result.Generation.SchemaVersion != 5 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if generations, err := store.ListConfigurationGenerations(context.Background()); err != nil || len(generations) != 2 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestBaselineInitializationDoesNotRetainRawWithoutMutationAuthority(t *testing.T) {
	service, _, files, _, _ := configurationServiceFixture(t)
	files.replacementMu.Lock()
	_, err := service.Initialize(context.Background())
	files.replacementMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "baseline is still active") {
		t.Fatalf("initialize err=%v", err)
	}
	files.mu.Lock()
	defer files.mu.Unlock()
	if len(files.raw) != 0 {
		t.Fatalf("raw evidence retained without mutation authority: %v", files.raw)
	}
}

func TestBaselineBindingConflictRemovesUnreferencedRawWhileLocked(t *testing.T) {
	service, _, files, _, _ := configurationServiceFixture(t)
	files.baselineError = true
	if _, err := service.Initialize(context.Background()); err == nil || !strings.Contains(err.Error(), "binding conflicts") {
		t.Fatalf("initialize err=%v", err)
	}
	files.mu.Lock()
	defer files.mu.Unlock()
	if len(files.raw) != 0 {
		t.Fatalf("unreferenced baseline raw remained: %v", files.raw)
	}
	if files.removeUnlocked {
		t.Fatal("baseline raw cleanup ran outside mutation authority")
	}
}

func TestConfigurationStartupRemovesRawWithoutDurableAnchor(t *testing.T) {
	service, _, files, _, _ := configurationServiceFixture(t)
	baselineOrphan := []byte("crashed baseline staging")
	files.raw[configurationTestDigest(baselineOrphan)] = baselineOrphan
	authority, err := service.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	files.mu.Lock()
	if len(files.raw) != 1 {
		files.mu.Unlock()
		t.Fatalf("raw after baseline recovery=%v", files.raw)
	}
	applyOrphan := []byte("crashed apply staging")
	files.raw[configurationTestDigest(applyOrphan)] = applyOrphan
	files.mu.Unlock()
	if _, err := service.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	files.mu.Lock()
	defer files.mu.Unlock()
	if len(files.raw) != 1 {
		t.Fatalf("raw after apply recovery=%v", files.raw)
	}
	if _, found := files.raw[authority.Desired.Digest]; !found {
		t.Fatal("durably anchored desired raw was removed")
	}
}

func TestLegacyConfigurationWithoutUniversalOperatorStillConverges(t *testing.T) {
	service, _, files, runtime, _ := configurationServiceFixture(t)
	files.baselineSchema = 4
	files.operator = domain.GitHubUserIdentity{}
	authority, err := service.Initialize(context.Background())
	if err != nil || authority.Desired.ConfiguredOperator.Validate() == nil {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
	now := time.Now().UTC()
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, now)
	decision, err := service.CheckNewAdmission(context.Background())
	if err != nil || !decision.Allowed || decision.Reason != application.ConfigurationReasonReady {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}
func (f *configurationFilesFixture) ValidateCurrent(payload []byte) (application.ValidatedConfigurationCandidate, error) {
	return f.candidate(payload, 5), nil
}
func (f *configurationFilesFixture) ReadLive() ([]byte, application.ValidatedConfigurationCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rereadError {
		f.rereadError = false
		return nil, application.ValidatedConfigurationCandidate{}, errors.New("injected exact reread failure")
	}
	payload := append([]byte(nil), f.live...)
	schema := f.baselineSchema
	if schema == 0 {
		schema = 5
	}
	return payload, f.candidate(payload, schema), nil
}
func (f *configurationFilesFixture) RetainRaw(digest string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retainError {
		return errors.New("injected raw retention failure")
	}
	if configurationTestDigest(payload) != digest {
		return errors.New("digest conflict")
	}
	if existing, ok := f.raw[digest]; ok && string(existing) != string(payload) {
		return errors.New("raw conflict")
	}
	f.raw[digest] = append([]byte(nil), payload...)
	return nil
}
func (f *configurationFilesFixture) ReadRaw(digest string, size int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload, ok := f.raw[digest]
	if !ok || int64(len(payload)) != size {
		return nil, errors.New("missing raw")
	}
	return append([]byte(nil), payload...), nil
}
func (f *configurationFilesFixture) HasRaw(digest string, size int64) bool {
	payload, err := f.ReadRaw(digest, size)
	return err == nil && configurationTestDigest(payload) == digest
}
func (f *configurationFilesFixture) ListRawDigests() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	digests := make([]string, 0, len(f.raw))
	for digest := range f.raw {
		digests = append(digests, digest)
	}
	return digests, nil
}
func (f *configurationFilesFixture) ReplaceLive(_ string, expected, payload []byte) error {
	f.mu.Lock()
	if !bytes.Equal(f.live, expected) {
		f.mu.Unlock()
		return errors.New("expected parent changed")
	}
	f.replaceCalls++
	first := f.replaceCalls == 1
	started, release := f.replaceStarted, f.replaceRelease
	f.mu.Unlock()
	if first && started != nil {
		close(started)
	}
	if first && release != nil {
		<-release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.live, expected) {
		return errors.New("expected parent changed")
	}
	switch f.replaceMode {
	case "before", "file_sync":
		return errors.New("before exchange")
	case "after", "directory_sync":
		f.live = append([]byte(nil), payload...)
		f.baselineSchema = 5
		return errors.New("lost response after exchange")
	case "third":
		f.live = []byte("third digest")
		return errors.New("ambiguous replacement")
	case "reread":
		f.live = append([]byte(nil), payload...)
		f.baselineSchema = 5
		f.rereadError = true
		return nil
	default:
		f.live = append([]byte(nil), payload...)
		f.baselineSchema = 5
		return nil
	}
}

func TestConcurrentExactConfigurationReplayHasOneFilesystemMutation(t *testing.T) {
	service, _, files, _, requester := configurationServiceFixture(t)
	authority, err := service.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	files.replaceStarted = make(chan struct{})
	files.replaceRelease = make(chan struct{})
	command := application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("concurrent exact replay")}
	firstResult := make(chan application.ConfigurationApplyResult, 1)
	firstError := make(chan error, 1)
	go func() {
		result, applyErr := service.Apply(context.Background(), command)
		firstResult <- result
		firstError <- applyErr
	}()
	select {
	case <-files.replaceStarted:
	case <-time.After(time.Second):
		t.Fatal("first apply did not reach filesystem phase")
	}
	replayed, err := service.Apply(context.Background(), command)
	if err != nil || replayed.Generation.State != application.ConfigurationGenerationAccepted || replayed.Receipt.Phase != application.OperationPhaseAccepted {
		t.Fatalf("accepted replay=%+v err=%v", replayed, err)
	}
	close(files.replaceRelease)
	select {
	case err := <-firstError:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first apply did not finish")
	}
	first := <-firstResult
	if first.Generation.GenerationID != replayed.Generation.GenerationID || first.Receipt.OperationID != replayed.Receipt.OperationID {
		t.Fatalf("first=%+v replay=%+v", first, replayed)
	}
	files.mu.Lock()
	replaceCalls := files.replaceCalls
	files.mu.Unlock()
	if replaceCalls != 1 {
		t.Fatalf("filesystem replacements=%d", replaceCalls)
	}
}
func (f *configurationFilesFixture) ReconcileReplacement(_ string, _, _ []byte) ([]byte, application.ValidatedConfigurationCandidate, error) {
	return f.ReadLive()
}
func (f *configurationFilesFixture) RemoveRaw(digest string) error {
	if f.replacementMu.TryLock() {
		f.replacementMu.Unlock()
		f.mu.Lock()
		f.removeUnlocked = true
		f.mu.Unlock()
	}
	f.mu.Lock()
	if f.removeError {
		f.mu.Unlock()
		return errors.New("injected prune failure")
	}
	delete(f.raw, digest)
	started, release := f.removeStarted, f.removeRelease
	f.mu.Unlock()
	if started != nil && release != nil {
		f.removeBlock.Do(func() {
			started <- digest
			<-release
		})
	}
	return nil
}
func (f *configurationFilesFixture) PublishLocator(string) error { return nil }
func (f *configurationFilesFixture) PublishBaselineBinding(application.ValidatedConfigurationCandidate) error {
	if f.baselineError {
		return errors.New("injected baseline binding conflict")
	}
	return nil
}
func (f *configurationFilesFixture) candidate(payload []byte, schema int) application.ValidatedConfigurationCandidate {
	return application.ValidatedConfigurationCandidate{Digest: configurationTestDigest(payload), Size: int64(len(payload)), SchemaVersion: schema, DatabasePath: f.database, Operator: f.operator, Repositories: map[string]application.ConfigurationRepositoryAuthority{}}
}

type configurationRuntimeFixture struct {
	mu          sync.Mutex
	observation application.RuntimeObservation
}

type configurationFaultStore struct {
	*sqlitestore.Store
	failBegin  bool
	failSettle bool
}

type configurationNoOpRaceStore struct {
	*sqlitestore.Store
	entered chan struct{}
	release chan struct{}
}

func (s *configurationNoOpRaceStore) RecordConfigurationNoOp(ctx context.Context, settlement application.ConfigurationNoOpSettlement) (application.ConfigurationAuthority, application.OperationReceipt, bool, error) {
	close(s.entered)
	<-s.release
	return s.Store.RecordConfigurationNoOp(ctx, settlement)
}

func (s *configurationFaultStore) BeginConfigurationApply(ctx context.Context, input application.ConfigurationApplyAcceptance) (application.ConfigurationGeneration, application.OperationReceipt, bool, error) {
	if s.failBegin {
		s.failBegin = false
		return application.ConfigurationGeneration{}, application.OperationReceipt{}, false, errors.New("injected intent failure")
	}
	return s.Store.BeginConfigurationApply(ctx, input)
}

func (s *configurationFaultStore) SettleConfigurationApply(ctx context.Context, input application.ConfigurationApplySettlement) (application.ConfigurationAuthority, application.OperationReceipt, bool, error) {
	if s.failSettle {
		s.failSettle = false
		return application.ConfigurationAuthority{}, application.OperationReceipt{}, false, errors.New("injected settlement failure")
	}
	return s.Store.SettleConfigurationApply(ctx, input)
}

func (f *configurationRuntimeFixture) ObserveConfigurationRuntime(context.Context, time.Time) (application.RuntimeObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observation, nil
}

func TestConfigurationServiceBaselineApplyReplayConvergenceAndDrift(t *testing.T) {
	service, store, files, runtime, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := service.Initialize(ctx)
	if err != nil || authority.Desired.GenerationID != 1 || authority.Desired.Origin != application.ConfigurationOriginBaseline || authority.Desired.Requester.Validate() == nil || authority.Desired.ConfiguredOperator.Login != requester.ID {
		t.Fatalf("authority=%+v err=%v", authority, err)
	}
	now := time.Now().UTC()
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, now)
	projection, err := service.Projection(ctx, requester, now)
	if err != nil || projection.State != application.ConfigurationReady || projection.EffectiveGenerationID != authority.Desired.GenerationID {
		t.Fatalf("projection=%+v err=%v", projection, err)
	}

	target := []byte("target configuration generation")
	result, err := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: target})
	if err != nil || result.NoOp || result.Generation.GenerationID != 2 || result.Generation.State != application.ConfigurationGenerationPendingRestart || result.Receipt.Outcome != application.OperationOutcomeSucceeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	projection, err = service.Projection(ctx, requester, now)
	if err != nil || projection.State != application.ConfigurationRestartRequired {
		t.Fatalf("restart projection=%+v err=%v", projection, err)
	}
	runtime.observation = freshConfigurationRuntime(result.Generation.Digest, now.Add(time.Second))
	projection, err = service.Projection(ctx, requester, now.Add(time.Second))
	if err != nil || projection.State != application.ConfigurationReady || projection.EffectiveGenerationID != result.Generation.GenerationID {
		t.Fatalf("ready projection=%+v err=%v", projection, err)
	}
	replay, err := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: target})
	if err != nil || replay.Generation.GenerationID != result.Generation.GenerationID || replay.Receipt.OperationID != result.Receipt.OperationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	noOp, err := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: result.Generation.GenerationID, ExpectedDigest: result.Generation.Digest, Payload: target})
	if err != nil || !noOp.NoOp || noOp.Generation.GenerationID != result.Generation.GenerationID {
		t.Fatalf("no-op=%+v err=%v", noOp, err)
	}
	if generations, err := store.ListConfigurationGenerations(ctx); err != nil || len(generations) != 2 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
	files.mu.Lock()
	files.live = []byte("external drift")
	files.mu.Unlock()
	projection, err = service.Projection(ctx, requester, now.Add(2*time.Second))
	if err != nil || projection.State != application.ConfigurationConflict || projection.Reason != application.ConfigurationReasonExternalDrift {
		t.Fatalf("drift projection=%+v err=%v", projection, err)
	}
	if decision, err := service.CheckNewAdmission(ctx); err != nil || decision.Allowed {
		t.Fatalf("gate=%+v err=%v", decision, err)
	}
}

func TestConfigurationServiceReconcilesEveryReplacementCrashBoundary(t *testing.T) {
	for _, test := range []struct {
		name        string
		mode        string
		wantError   bool
		wantDesired int64
		wantState   application.ConfigurationGenerationState
		ambiguous   bool
	}{
		{name: "before exchange", mode: "before", wantError: true, wantDesired: 1, wantState: application.ConfigurationGenerationFailed},
		{name: "after exchange lost response", mode: "after", wantDesired: 2, wantState: application.ConfigurationGenerationPendingRestart},
		{name: "third digest", mode: "third", wantError: true, wantDesired: 1, wantState: application.ConfigurationGenerationAmbiguous, ambiguous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store, files, _, requester := configurationServiceFixture(t)
			authority, err := service.Initialize(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			files.replaceMode = test.mode
			result, applyErr := service.Apply(context.Background(), application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("next generation")})
			if (applyErr != nil) != test.wantError {
				t.Fatalf("result=%+v err=%v", result, applyErr)
			}
			current, found, err := store.ConfigurationAuthority(context.Background())
			if err != nil || !found || current.Desired.GenerationID != test.wantDesired || (current.Incomplete != nil) != test.ambiguous {
				t.Fatalf("authority=%+v found=%t err=%v", current, found, err)
			}
			generations, err := store.ListConfigurationGenerations(context.Background())
			if err != nil || len(generations) != 2 || generations[0].State != test.wantState {
				t.Fatalf("generations=%+v err=%v", generations, err)
			}
		})
	}
}

func TestConfigurationServiceFaultBoundariesRemainReplayable(t *testing.T) {
	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "intent before live replacement", mode: "before"},
		{name: "live exchange response loss", mode: "after"},
		{name: "file sync failure before publication", mode: "file_sync"},
		{name: "directory sync failure after publication", mode: "directory_sync"},
		{name: "exact reread response loss", mode: "reread"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, store, files, _, requester := configurationServiceFixture(t)
			authority, err := service.Initialize(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			files.replaceMode = test.mode
			_, _ = service.Apply(context.Background(), application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("fault target")})
			current, found, err := store.ConfigurationAuthority(context.Background())
			if err != nil || !found || current.Incomplete != nil {
				t.Fatalf("authority=%+v found=%t err=%v", current, found, err)
			}
			beforePublication := test.mode == "before" || test.mode == "file_sync"
			if beforePublication && current.Desired.GenerationID != authority.Desired.GenerationID {
				t.Fatalf("before-rename desired=%d", current.Desired.GenerationID)
			}
			if !beforePublication && current.Desired.GenerationID == authority.Desired.GenerationID {
				t.Fatalf("target was not reconciled: %+v", current)
			}
		})
	}
}

func TestConfigurationServiceCleansFailedPreIntentStagingAndReconcilesSettlementFailure(t *testing.T) {
	baseService, store, files, runtime, requester := configurationServiceFixture(t)
	authority, err := baseService.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	faults := &configurationFaultStore{Store: store, failBegin: true}
	service, err := application.NewConfigurationService(faults, files, runtime)
	if err != nil {
		t.Fatal(err)
	}
	target := []byte("pre-intent target")
	if _, err := service.Apply(context.Background(), application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: target}); err == nil {
		t.Fatal("injected intent failure unexpectedly succeeded")
	}
	if files.HasRaw(configurationTestDigest(target), int64(len(target))) {
		t.Fatal("unreferenced pre-intent staging was retained")
	}
	if generations, err := store.ListConfigurationGenerations(context.Background()); err != nil || len(generations) != 1 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}

	faults.failSettle = true
	target = []byte("settlement target")
	if _, err := service.Apply(context.Background(), application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: target}); err == nil {
		t.Fatal("injected settlement failure unexpectedly succeeded")
	}
	current, err := service.Reconcile(context.Background())
	if err != nil || current.Desired.Digest != configurationTestDigest(target) || current.Incomplete != nil {
		t.Fatalf("reconciled=%+v err=%v", current, err)
	}
}

func TestConfigurationServiceRawRetentionFailureNeverAcceptsIntent(t *testing.T) {
	service, store, files, _, requester := configurationServiceFixture(t)
	authority, err := service.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	files.retainError = true
	if _, err := service.Apply(context.Background(), application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("unretained target")}); err == nil {
		t.Fatal("raw retention failure unexpectedly succeeded")
	}
	if generations, err := store.ListConfigurationGenerations(context.Background()); err != nil || len(generations) != 1 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestConfigurationNoOpCannotSettleAfterAuthorityAdvances(t *testing.T) {
	baseService, store, files, runtime, requester := configurationServiceFixture(t)
	authority, err := baseService.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raceStore := &configurationNoOpRaceStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	raceService, err := application.NewConfigurationService(raceStore, files, runtime)
	if err != nil {
		t.Fatal(err)
	}
	command := application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: append([]byte(nil), files.live...)}
	noOpDone := make(chan error, 1)
	go func() {
		_, noOpErr := raceService.Apply(context.Background(), command)
		noOpDone <- noOpErr
	}()
	select {
	case <-raceStore.entered:
	case <-time.After(time.Second):
		t.Fatal("no-op did not reach transactional settlement")
	}
	advanced, err := baseService.Apply(context.Background(), application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("advanced while no-op waits")})
	if err != nil {
		t.Fatal(err)
	}
	close(raceStore.release)
	select {
	case err := <-noOpDone:
		if err == nil || !strings.Contains(err.Error(), "authority changed") {
			t.Fatalf("stale no-op error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale no-op did not finish")
	}
	current, found, err := store.ConfigurationAuthority(context.Background())
	if err != nil || !found || current.Desired.GenerationID != advanced.Generation.GenerationID || current.Desired.Digest != advanced.Generation.Digest {
		t.Fatalf("authority=%+v found=%t err=%v", current, found, err)
	}
}

func TestConfigurationServiceRetainsCurrentPlusNineSettledRawGenerations(t *testing.T) {
	service, store, files, _, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := service.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 12; index++ {
		payload := []byte("generation-" + time.Unix(int64(index+1), 0).UTC().Format(time.RFC3339Nano))
		result, err := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: payload})
		if err != nil {
			t.Fatalf("apply %d: %v", index, err)
		}
		authority, _, err = store.ConfigurationAuthority(ctx)
		if err != nil || authority.Desired.GenerationID != result.Generation.GenerationID {
			t.Fatalf("authority=%+v err=%v", authority, err)
		}
	}
	files.mu.Lock()
	retained := len(files.raw)
	_, currentPresent := files.raw[authority.Desired.Digest]
	files.mu.Unlock()
	if retained != application.ConfigurationRawRetainCount || !currentPresent {
		t.Fatalf("retained=%d current=%t", retained, currentPresent)
	}
	generations, err := store.ListConfigurationGenerations(ctx)
	if err != nil || len(generations) != 13 {
		t.Fatalf("generations=%d err=%v", len(generations), err)
	}
	retainedMetadata := 0
	for _, generation := range generations {
		if generation.RawRetained {
			retainedMetadata++
		}
	}
	if retainedMetadata != application.ConfigurationRawRetainCount {
		t.Fatalf("retained metadata=%d", retainedMetadata)
	}
	files.removeError = true
	for index := 0; index < 2; index++ {
		payload := []byte("prune-failure-" + string(rune('a'+index)))
		result, applyErr := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: payload})
		if applyErr != nil {
			t.Fatal(applyErr)
		}
		authority = application.ConfigurationAuthority{Desired: result.Generation}
	}
	files.mu.Lock()
	retainedAfterPruneFailure := len(files.raw)
	files.mu.Unlock()
	if retainedAfterPruneFailure != application.ConfigurationRawRetainCount+2 {
		t.Fatalf("retained after prune failure=%d", retainedAfterPruneFailure)
	}
	files.removeError = false
	result, err := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("prune retry")})
	if err != nil {
		t.Fatal(err)
	}
	authority = application.ConfigurationAuthority{Desired: result.Generation}
	files.mu.Lock()
	retainedAfterPruneRetry := len(files.raw)
	files.mu.Unlock()
	if retainedAfterPruneRetry != application.ConfigurationRawRetainCount {
		t.Fatalf("retained after prune retry=%d", retainedAfterPruneRetry)
	}
	files.replaceMode = "third"
	_, err = service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("unresolved generation beyond bound")})
	if err == nil {
		t.Fatal("ambiguous replacement unexpectedly succeeded")
	}
	files.mu.Lock()
	retainedWithUnresolved := len(files.raw)
	files.mu.Unlock()
	if retainedWithUnresolved != application.ConfigurationRawRetainCount+1 {
		t.Fatalf("retained with unresolved=%d", retainedWithUnresolved)
	}
}

func TestConfigurationPruneSerializesDeleteMetadataAndSameDigestRestaging(t *testing.T) {
	service, store, files, _, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := service.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files.removeError = true
	for index := 0; index < application.ConfigurationRawRetainCount+2; index++ {
		payload := []byte("serialized-prune-generation-" + strconv.Itoa(index))
		result, applyErr := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: payload})
		if applyErr != nil {
			t.Fatal(applyErr)
		}
		authority, _, err = store.ConfigurationAuthority(ctx)
		if err != nil || authority.Desired.GenerationID != result.Generation.GenerationID {
			t.Fatalf("authority=%+v err=%v", authority, err)
		}
	}
	files.mu.Lock()
	payloads := make(map[string][]byte, len(files.raw))
	for digest, payload := range files.raw {
		payloads[digest] = append([]byte(nil), payload...)
	}
	files.removeError = false
	files.removeStarted = make(chan string, 1)
	files.removeRelease = make(chan struct{})
	files.mu.Unlock()

	pruneDone := make(chan error, 1)
	go func() {
		_, initializeErr := service.Initialize(ctx)
		pruneDone <- initializeErr
	}()
	var prunedDigest string
	select {
	case prunedDigest = <-files.removeStarted:
	case <-time.After(time.Second):
		t.Fatal("prune did not reach the filesystem deletion boundary")
	}
	prunedPayload := payloads[prunedDigest]
	if len(prunedPayload) == 0 || files.HasRaw(prunedDigest, int64(len(prunedPayload))) {
		t.Fatalf("pruned digest=%q still has raw evidence", prunedDigest)
	}
	_, applyErr := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: prunedPayload})
	if applyErr == nil || !strings.Contains(applyErr.Error(), "still active") {
		t.Fatalf("same-digest restaging crossed active prune: %v", applyErr)
	}
	if files.HasRaw(prunedDigest, int64(len(prunedPayload))) {
		t.Fatal("blocked same-digest apply recreated pruned raw evidence")
	}
	close(files.removeRelease)
	select {
	case err := <-pruneDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("serialized prune did not finish")
	}
	files.mu.Lock()
	retained := len(files.raw)
	removeUnlocked := files.removeUnlocked
	files.mu.Unlock()
	if retained != application.ConfigurationRawRetainCount {
		t.Fatalf("retained raw snapshots=%d", retained)
	}
	if removeUnlocked {
		t.Fatal("raw pruning ran without filesystem mutation authority")
	}
	generations, err := store.ListConfigurationGenerations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retainedMetadata := 0
	for _, generation := range generations {
		if generation.RawRetained {
			retainedMetadata++
		}
	}
	if retainedMetadata != application.ConfigurationRawRetainCount {
		t.Fatalf("retained metadata=%d", retainedMetadata)
	}
}

func TestConfigurationApplyPreservesExistingRunAndDatabaseAuthority(t *testing.T) {
	service, store, files, runtime, requester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := service.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, time.Now().UTC())
	decision, err := service.CheckNewAdmission(ctx)
	if err != nil || !decision.Allowed {
		t.Fatalf("admission decision=%+v err=%v", decision, err)
	}
	_, _, err = store.CreateRun(ctx, application.CreateRunInput{Run: application.Run{
		ID: "active-run", IssueID: "IFAN-1", IdempotencyKey: "active-run-key", SourceRevision: "source", TaskHash: "task",
		Repository: "owner/repo", RepositoryConfigJSON: `{}`, BaseBranch: "main", WorkingBranch: "ifan/active-run",
	}, ConfigurationAuthority: decision.Authority})
	if err != nil {
		t.Fatal(err)
	}
	originalLive := append([]byte(nil), files.live...)
	_, err = service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("removes active authority")})
	if err == nil || !strings.Contains(err.Error(), "active run") {
		t.Fatalf("active-run compatibility error=%v", err)
	}
	if string(files.live) != string(originalLive) {
		t.Fatal("rejected active-run apply changed the live file")
	}
	files.operator = domain.GitHubUserIdentity{Login: "future", DatabaseID: 9, NodeID: "USER_9", ActorType: "User"}
	_, err = service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("retargets controller operator")})
	if err == nil || !strings.Contains(err.Error(), "operator authority") {
		t.Fatalf("active-run operator compatibility error=%v", err)
	}
	files.operator = domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}

	files.database = filepath.Join(filepath.Dir(files.database), "relocated.db")
	_, err = service.Apply(ctx, application.ConfigurationApplyCommand{Requester: requester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("database relocation")})
	if err == nil || !strings.Contains(err.Error(), "relocation") {
		t.Fatalf("database relocation error=%v", err)
	}
	if generations, listErr := store.ListConfigurationGenerations(ctx); listErr != nil || len(generations) != 1 {
		t.Fatalf("rejected generations=%+v err=%v", generations, listErr)
	}
}

func TestConfigurationCandidateOperatorCannotAuthorizeOwnApply(t *testing.T) {
	service, store, files, _, currentRequester := configurationServiceFixture(t)
	authority, err := service.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	files.operator = domain.GitHubUserIdentity{Login: "future", DatabaseID: 9, NodeID: "USER_9", ActorType: "User"}
	futureRequester := application.Requester{ID: "future", Kind: "github_login", DatabaseID: 9, NodeID: "USER_9", ActorType: "User"}
	_, err = service.Apply(context.Background(), application.ConfigurationApplyCommand{Requester: futureRequester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("future operator candidate")})
	if err == nil {
		t.Fatal("future candidate operator authorized its own apply")
	}
	if generations, listErr := store.ListConfigurationGenerations(context.Background()); listErr != nil || len(generations) != 1 {
		t.Fatalf("generations=%+v err=%v current=%+v", generations, listErr, currentRequester)
	}
}

func TestConfigurationApplyReauthorizesAfterReconcileChangesOperator(t *testing.T) {
	baseService, store, files, runtime, oldRequester := configurationServiceFixture(t)
	ctx := context.Background()
	authority, err := baseService.Initialize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	faults := &configurationFaultStore{Store: store, failSettle: true}
	service, err := application.NewConfigurationService(faults, files, runtime)
	if err != nil {
		t.Fatal(err)
	}
	files.operator = domain.GitHubUserIdentity{Login: "future", DatabaseID: 9, NodeID: "USER_9", ActorType: "User"}
	operatorTransition := []byte("operator transition awaiting settlement")
	if _, err := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: oldRequester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: operatorTransition}); err == nil {
		t.Fatal("injected settlement failure unexpectedly succeeded")
	}
	secondTarget := []byte("old operator follow-up after reconcile")
	if _, err := service.Apply(ctx, application.ConfigurationApplyCommand{Requester: oldRequester, ExpectedGenerationID: 2, ExpectedDigest: configurationTestDigest(operatorTransition), Payload: secondTarget}); err == nil {
		t.Fatal("old operator remained authorized after reconcile committed the new operator")
	}
	current, found, err := store.ConfigurationAuthority(ctx)
	if err != nil || !found || current.Desired.GenerationID != 2 || current.Desired.ConfiguredOperator.Login != "future" || current.Incomplete != nil {
		t.Fatalf("authority=%+v found=%t err=%v", current, found, err)
	}
	if generations, err := store.ListConfigurationGenerations(ctx); err != nil || len(generations) != 2 {
		t.Fatalf("generations=%+v err=%v", generations, err)
	}
}

func TestConfigurationResponseLossReplaySurvivesOperatorChange(t *testing.T) {
	service, _, files, _, currentRequester := configurationServiceFixture(t)
	authority, err := service.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	files.operator = domain.GitHubUserIdentity{Login: "future", DatabaseID: 9, NodeID: "USER_9", ActorType: "User"}
	command := application.ConfigurationApplyCommand{Requester: currentRequester, ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Payload: []byte("operator transition")}
	result, err := service.Apply(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Apply(context.Background(), command)
	if err != nil || replay.Generation.GenerationID != result.Generation.GenerationID || replay.Receipt.OperationID != result.Receipt.OperationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestConfigurationProjectionAndHistoryNeverExposeRawOrPrivatePaths(t *testing.T) {
	service, _, files, runtime, requester := configurationServiceFixture(t)
	authority, err := service.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runtime.observation = freshConfigurationRuntime(authority.Desired.Digest, now)
	projection, err := service.Projection(context.Background(), requester, now)
	if err != nil {
		t.Fatal(err)
	}
	history, err := service.History(context.Background(), requester)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		Authority  application.ConfigurationAuthority             `json:"authority"`
		Projection application.ConfigurationConvergenceProjection `json:"projection"`
		History    []application.ConfigurationGeneration          `json:"history"`
	}{authority, projection, history})
	if err != nil {
		t.Fatal(err)
	}
	output := string(encoded)
	for _, secret := range []string{files.path, files.database, string(files.live), "canonical_config_path", "database_path"} {
		if strings.Contains(output, secret) {
			t.Fatalf("configuration output leaked private evidence %q: %s", secret, output)
		}
	}
}

func configurationServiceFixture(t *testing.T) (*application.ConfigurationService, *sqlitestore.Store, *configurationFilesFixture, *configurationRuntimeFixture, application.Requester) {
	t.Helper()
	database := filepath.Join(t.TempDir(), "controller.db")
	store, err := sqlitestore.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "USER_7", ActorType: "User"}
	files := &configurationFilesFixture{path: filepath.Join(filepath.Dir(database), "controller.json"), database: database, operator: operator, live: []byte("baseline configuration"), raw: map[string][]byte{}}
	runtime := &configurationRuntimeFixture{}
	service, err := application.NewConfigurationService(store, files, runtime)
	if err != nil {
		t.Fatal(err)
	}
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	return service, store, files, runtime, requester
}

func freshConfigurationRuntime(digest string, observedAt time.Time) application.RuntimeObservation {
	observedAt = observedAt.UTC()
	return application.RuntimeObservation{Liveness: application.RuntimeLivenessFresh, Activity: application.RuntimeActivityRunning, WorkerInstanceID: "worker-instance", BuildIdentity: "build-v1", LoadedConfigurationDigest: digest, LastObservedAt: &observedAt, Reason: application.RuntimeReasonHeartbeatFresh}
}

func configurationTestDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
