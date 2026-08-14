package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type configurationPersistence interface {
	ConfigurationGenerationStore
	OperationReceiptStore
}

type ConfigurationService struct {
	store   configurationPersistence
	files   ConfigurationFileAuthority
	runtime ConfigurationRuntimeObserver
	now     func() time.Time
}

func NewConfigurationService(store configurationPersistence, files ConfigurationFileAuthority, runtime ConfigurationRuntimeObserver) (*ConfigurationService, error) {
	if store == nil || files == nil {
		return nil, errors.New("configuration authority dependencies are required")
	}
	return &ConfigurationService{store: store, files: files, runtime: runtime, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Initialize adopts one valid unmanaged live configuration or reconciles the
// already-owned authority. It is a production composition operation, never an
// offline config validate/inspect side effect.
func (s *ConfigurationService) Initialize(ctx context.Context) (ConfigurationAuthority, error) {
	authority, found, err := s.store.ConfigurationAuthority(ctx)
	if err != nil {
		return ConfigurationAuthority{}, serviceError(ErrorInternal, "configuration authority is unavailable", nil)
	}
	if found {
		if authority.CanonicalConfigPath != s.files.CanonicalConfigPath() || !s.files.HasRaw(authority.Desired.Digest, authority.Desired.Size) {
			return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration authority conflicts", nil)
		}
		if err := s.files.PublishLocator(authority.DatabasePath); err != nil {
			return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration authority locator conflicts", nil)
		}
		authority, err = s.Reconcile(ctx)
		if err == nil {
			s.prune(ctx)
		}
		return authority, err
	}

	payload, candidate, err := s.files.ReadLive()
	if err != nil {
		return ConfigurationAuthority{}, serviceError(ErrorConflict, "live configuration cannot be adopted", nil)
	}
	if err := s.files.RetainRaw(candidate.Digest, payload); err != nil {
		return ConfigurationAuthority{}, serviceError(ErrorInternal, "configuration baseline evidence could not be retained", nil)
	}
	baseline := ConfigurationBaselineInput{Candidate: candidate, CanonicalConfigPath: s.files.CanonicalConfigPath(), ObservedAt: s.now().UTC()}
	if err := s.files.PublishBaselineBinding(candidate); err != nil {
		return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration baseline binding conflicts", nil)
	}
	if err := s.store.PrepareConfigurationBaseline(ctx, baseline); err != nil {
		return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration baseline binding conflicts", nil)
	}
	if err := s.files.PublishLocator(candidate.DatabasePath); err != nil {
		return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration authority locator conflicts", nil)
	}
	// Reopen and compare exact bytes so a concurrent replacement cannot make
	// SQLite adopt evidence that is no longer the observed live payload.
	current, verified, err := s.files.ReadLive()
	if err != nil || !bytes.Equal(payload, current) || verified.Digest != candidate.Digest || verified.Size != candidate.Size || verified.SchemaVersion != candidate.SchemaVersion {
		return ConfigurationAuthority{}, serviceError(ErrorConflict, "live configuration changed during baseline adoption", nil)
	}
	authority, _, err = s.store.AdoptConfigurationBaseline(ctx, baseline)
	if err != nil {
		if errors.Is(err, ErrConfigurationAuthorityConflict) {
			return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration baseline conflicts", nil)
		}
		return ConfigurationAuthority{}, serviceError(ErrorInternal, "configuration baseline could not be persisted", nil)
	}
	s.prune(ctx)
	return authority, nil
}

// Reconcile deterministically settles one durable apply intent from the exact
// safely reread live bytes. It never guesses from timestamps or process state.
func (s *ConfigurationService) Reconcile(ctx context.Context) (ConfigurationAuthority, error) {
	authority, found, err := s.store.ConfigurationAuthority(ctx)
	if err != nil || !found {
		return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration authority is not initialized", nil)
	}
	if authority.CanonicalConfigPath != s.files.CanonicalConfigPath() || !s.files.HasRaw(authority.Desired.Digest, authority.Desired.Size) {
		return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration authority conflicts", nil)
	}
	if authority.Incomplete == nil {
		return authority, nil
	}
	intent := *authority.Incomplete
	if intent.State == ConfigurationApplyAmbiguous {
		return authority, serviceError(ErrorConflict, "configuration apply is ambiguous", nil)
	}
	replacementLock, acquired, lockErr := s.files.AcquireReplacement(intent.OperationID)
	if lockErr != nil {
		return authority, serviceError(ErrorInternal, "configuration replacement authority is unavailable", nil)
	}
	if !acquired {
		return authority, serviceError(ErrorConflict, "configuration apply is still active", nil)
	}
	defer replacementLock.Release()
	generations, listErr := s.store.ListConfigurationGenerations(ctx)
	if listErr != nil {
		return ConfigurationAuthority{}, serviceError(ErrorInternal, "configuration generation evidence is unavailable", nil)
	}
	target, ok := generationByID(generations, intent.GenerationID)
	if !ok || !s.files.HasRaw(target.Digest, target.Size) {
		return s.settleAmbiguous(ctx, authority, intent, ConfigurationReasonAuthorityConflict)
	}
	targetPayload, targetErr := s.files.ReadRaw(target.Digest, target.Size)
	parentPayload, parentErr := s.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if targetErr != nil || parentErr != nil {
		return s.settleAmbiguous(ctx, authority, intent, ConfigurationReasonAuthorityConflict)
	}
	payload, live, liveErr := s.files.ReconcileReplacement(intent.OperationID, parentPayload, targetPayload)
	settlement := ConfigurationApplySettlement{GenerationID: intent.GenerationID, ParentID: intent.ParentID, OperationID: intent.OperationID, SettledAt: s.now().UTC()}
	switch {
	case liveErr == nil && bytes.Equal(payload, targetPayload) && live.Digest == intent.TargetDigest && live.Size == target.Size && live.SchemaVersion == target.SchemaVersion:
		settlement.Outcome = ConfigurationApplyCommitted
		settlement.Reason = ConfigurationReasonRestartRequired
	case liveErr == nil && bytes.Equal(payload, parentPayload) && live.Digest == intent.ParentDigest && live.Digest == authority.Desired.Digest:
		settlement.Outcome = ConfigurationApplyFailed
		settlement.Reason = ConfigurationReasonAuthorityConflict
	default:
		return s.settleAmbiguous(ctx, authority, intent, ConfigurationReasonAuthorityConflict)
	}
	settlement.EvidenceDigest = configurationDigest("reconcile", strconv.FormatInt(intent.GenerationID, 10), live.Digest, string(settlement.Outcome))
	settled, _, _, err := s.store.SettleConfigurationApply(ctx, settlement)
	if err != nil {
		return ConfigurationAuthority{}, serviceError(ErrorConflict, "configuration reconciliation conflicts", nil)
	}
	if settlement.Outcome == ConfigurationApplyCommitted || settlement.Outcome == ConfigurationApplyFailed {
		s.prune(ctx)
	}
	return settled, nil
}

func (s *ConfigurationService) settleAmbiguous(ctx context.Context, authority ConfigurationAuthority, intent ConfigurationApplyIntent, reason ConfigurationReason) (ConfigurationAuthority, error) {
	evidence := configurationDigest("reconcile-ambiguous", strconv.FormatInt(intent.GenerationID, 10), intent.ParentDigest, intent.TargetDigest)
	settled, _, _, err := s.store.SettleConfigurationApply(ctx, ConfigurationApplySettlement{GenerationID: intent.GenerationID, ParentID: intent.ParentID, OperationID: intent.OperationID, Outcome: ConfigurationApplyAmbiguous, Reason: reason, EvidenceDigest: evidence, SettledAt: s.now().UTC()})
	if err != nil {
		return authority, serviceError(ErrorConflict, "configuration reconciliation is ambiguous", nil)
	}
	return settled, serviceError(ErrorConflict, "configuration reconciliation is ambiguous", nil)
}

func (s *ConfigurationService) Apply(ctx context.Context, command ConfigurationApplyCommand) (ConfigurationApplyResult, error) {
	if len(command.Payload) > 256<<10 {
		return ConfigurationApplyResult{}, serviceError(ErrorInvalidInput, "configuration candidate is too large", nil)
	}
	// An exact settled receipt is evidence that this requester was authorized
	// when the effect was accepted. This read-only path preserves response-loss
	// replay even when that effect changed the configured operator.
	payloadDigest := sha256.Sum256(command.Payload)
	if replay, ok := s.configurationReplay(ctx, command, ValidatedConfigurationCandidate{Digest: hex.EncodeToString(payloadDigest[:])}); ok {
		return replay, nil
	}
	authority, configured, scopes, err := s.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationApplyResult{}, err
	}
	authority, err = s.Reconcile(ctx)
	if err != nil {
		return ConfigurationApplyResult{}, err
	}
	if authority.Incomplete != nil {
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, "configuration apply is unresolved", nil)
	}
	candidate, err := s.files.ValidateCurrent(command.Payload)
	if err != nil {
		return ConfigurationApplyResult{}, serviceError(ErrorInvalidInput, "configuration candidate is invalid", nil)
	}
	proposedReceipt := configurationApplyReceiptFor(command.ExpectedGenerationID, command.ExpectedDigest, configured, scopes, candidate, s.now().UTC())
	if replay, replayErr := s.store.GetAuthorizedOperationReceipt(ctx, proposedReceipt.OperationID, scopes); replayErr == nil {
		generations, listErr := s.store.ListConfigurationGenerations(ctx)
		if listErr != nil {
			return ConfigurationApplyResult{}, serviceError(ErrorInternal, "configuration replay evidence is unavailable", nil)
		}
		for _, generation := range generations {
			if generation.OperationID == replay.OperationID {
				return ConfigurationApplyResult{Generation: generation, Receipt: replay}, nil
			}
		}
		if replay.Outcome == OperationOutcomeSucceeded && replay.ResultingVersion == authority.Desired.GenerationID {
			return ConfigurationApplyResult{Generation: authority.Desired, Receipt: replay, NoOp: true}, nil
		}
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, "configuration replay evidence conflicts", nil)
	}
	if command.ExpectedGenerationID != authority.Desired.GenerationID || command.ExpectedDigest != authority.Desired.Digest {
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, "configuration compare-and-swap authority changed", nil)
	}
	if candidate.DatabasePath != authority.DatabasePath {
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, "configuration authority relocation requires recovery", nil)
	}
	livePayload, live, err := s.files.ReadLive()
	desiredPayload, desiredErr := s.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if err != nil || desiredErr != nil || live.Digest != authority.Desired.Digest || !bytes.Equal(livePayload, desiredPayload) {
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, "live configuration conflicts with desired authority", nil)
	}
	if candidate.Digest == authority.Desired.Digest {
		if !bytes.Equal(command.Payload, livePayload) {
			return ConfigurationApplyResult{}, serviceError(ErrorConflict, "configuration digest evidence conflicts", nil)
		}
		return s.recordNoOp(ctx, authority, configured, scopes, candidate)
	}
	runs, err := s.store.ListNonterminalRuns(ctx)
	if err != nil {
		return ConfigurationApplyResult{}, serviceError(ErrorInternal, "active run authority is unavailable", nil)
	}
	if err := ConfigurationCompatibleWithActiveRuns(authority.Desired.ConfiguredOperator, candidate, runs); err != nil {
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, err.Error(), nil)
	}
	if err := s.files.RetainRaw(candidate.Digest, command.Payload); err != nil {
		return ConfigurationApplyResult{}, serviceError(ErrorInternal, "configuration target evidence could not be retained", nil)
	}
	receipt := proposedReceipt
	replacementLock, acquired, lockErr := s.files.AcquireReplacement(receipt.OperationID)
	if lockErr != nil {
		return ConfigurationApplyResult{}, serviceError(ErrorInternal, "configuration replacement authority is unavailable", nil)
	}
	if !acquired {
		if replay, ok := s.configurationReplay(ctx, command, candidate); ok {
			return replay, nil
		}
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, "configuration apply is still active", nil)
	}
	defer func() {
		if replacementLock != nil {
			_ = replacementLock.Release()
		}
	}()
	generation, acceptedReceipt, created, err := s.store.BeginConfigurationApply(ctx, ConfigurationApplyAcceptance{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, Candidate: candidate, Requester: configured.identity, Receipt: receipt, AcceptedAt: receipt.AcceptedAt})
	if err != nil {
		s.removeUnreferencedRaw(ctx, candidate.Digest)
		return ConfigurationApplyResult{}, classifyConfigurationStoreError(err)
	}
	if generation.State != ConfigurationGenerationAccepted {
		return ConfigurationApplyResult{Generation: generation, Receipt: acceptedReceipt}, nil
	}
	if !created {
		// Another exact caller owns the sole filesystem phase. Startup or a later
		// retry reconciles an interrupted accepted intent before replay reaches
		// this point; a concurrent replay must never touch its deterministic stage.
		return ConfigurationApplyResult{Generation: generation, Receipt: acceptedReceipt}, nil
	}
	if err := s.files.ReplaceLive(generation.OperationID, desiredPayload, command.Payload); err != nil {
		_ = replacementLock.Release()
		replacementLock = nil
		settled, reconcileErr := s.Reconcile(ctx)
		if reconcileErr != nil {
			return ConfigurationApplyResult{}, reconcileErr
		}
		if settled.Desired.OperationID == generation.OperationID && settled.Desired.GenerationID == generation.GenerationID {
			replayed, _ := s.store.GetAuthorizedOperationReceipt(ctx, generation.OperationID, scopes)
			return ConfigurationApplyResult{Generation: settled.Desired, Receipt: replayed}, nil
		}
		return ConfigurationApplyResult{Generation: settled.Desired, Receipt: acceptedReceipt}, serviceError(ErrorConflict, "configuration replacement did not commit", nil)
	}
	verifiedPayload, verified, readErr := s.files.ReadLive()
	if readErr != nil || !bytes.Equal(verifiedPayload, command.Payload) || verified.Digest != candidate.Digest || verified.Size != candidate.Size || verified.SchemaVersion != candidate.SchemaVersion {
		_ = replacementLock.Release()
		replacementLock = nil
		settled, reconcileErr := s.Reconcile(ctx)
		if reconcileErr != nil {
			return ConfigurationApplyResult{}, reconcileErr
		}
		if settled.Desired.OperationID == generation.OperationID && settled.Desired.GenerationID == generation.GenerationID {
			replayed, _ := s.store.GetAuthorizedOperationReceipt(ctx, generation.OperationID, scopes)
			return ConfigurationApplyResult{Generation: settled.Desired, Receipt: replayed}, nil
		}
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, "configuration replacement verification conflicts", nil)
	}
	settlement := ConfigurationApplySettlement{GenerationID: generation.GenerationID, ParentID: generation.ParentID, OperationID: generation.OperationID, Outcome: ConfigurationApplyCommitted, Reason: ConfigurationReasonRestartRequired, EvidenceDigest: configurationDigest("apply-commit", candidate.Digest, generation.OperationID), SettledAt: s.now().UTC()}
	settled, settledReceipt, _, err := s.store.SettleConfigurationApply(ctx, settlement)
	if err != nil {
		return ConfigurationApplyResult{}, serviceError(ErrorConflict, "configuration settlement requires reconciliation", nil)
	}
	s.prune(ctx)
	return ConfigurationApplyResult{Generation: settled.Desired, Receipt: settledReceipt}, nil
}

func (s *ConfigurationService) removeUnreferencedRaw(ctx context.Context, digest string) {
	generations, err := s.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return
	}
	for _, generation := range generations {
		if generation.Digest == digest && generation.RawRetained {
			return
		}
	}
	_ = s.files.RemoveRaw(digest)
}

func (s *ConfigurationService) settledConfigurationReplay(ctx context.Context, command ConfigurationApplyCommand, candidate ValidatedConfigurationCandidate) (ConfigurationApplyResult, bool) {
	result, ok := s.configurationReplay(ctx, command, candidate)
	return result, ok && result.Receipt.Phase == OperationPhaseObserved
}

func (s *ConfigurationService) configurationReplay(ctx context.Context, command ConfigurationApplyCommand, candidate ValidatedConfigurationCandidate) (ConfigurationApplyResult, bool) {
	identity, err := command.Requester.githubUserIdentity()
	if err != nil {
		return ConfigurationApplyResult{}, false
	}
	configured := ConfiguredRequester{identity: identity}
	scopes, err := newAuthorizedScopeSet(identity, AuthorityScope{Kind: ScopeController, ID: controllerScopeID, AuthorityDigest: identityDigest(identity)})
	if err != nil {
		return ConfigurationApplyResult{}, false
	}
	proposed := configurationApplyReceiptFor(command.ExpectedGenerationID, command.ExpectedDigest, configured, scopes, candidate, s.now().UTC())
	receipt, err := s.store.GetAuthorizedOperationReceipt(ctx, proposed.OperationID, scopes)
	if err != nil {
		return ConfigurationApplyResult{}, false
	}
	generations, err := s.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return ConfigurationApplyResult{}, false
	}
	for _, generation := range generations {
		if generation.OperationID == receipt.OperationID {
			return ConfigurationApplyResult{Generation: generation, Receipt: receipt}, true
		}
	}
	authority, found, err := s.store.ConfigurationAuthority(ctx)
	if err == nil && found && receipt.Outcome == OperationOutcomeSucceeded && receipt.ResultingVersion == authority.Desired.GenerationID && receipt.RequestDigest == authority.Desired.Digest {
		return ConfigurationApplyResult{Generation: authority.Desired, Receipt: receipt, NoOp: true}, true
	}
	return ConfigurationApplyResult{}, false
}

func (s *ConfigurationService) recordNoOp(ctx context.Context, authority ConfigurationAuthority, requester ConfiguredRequester, scopes AuthorizedScopeSet, candidate ValidatedConfigurationCandidate) (ConfigurationApplyResult, error) {
	receipt := configurationApplyReceipt(authority, requester, scopes, candidate, s.now().UTC())
	receipts, err := NewOperationReceiptService(s.store)
	if err != nil {
		return ConfigurationApplyResult{}, serviceError(ErrorInternal, "configuration receipt authority is unavailable", nil)
	}
	persisted, _, err := receipts.Accept(ctx, OperationReceiptInput{OperationType: receipt.OperationType, Scope: receipt.Scope, TargetID: receipt.TargetID, Requester: receipt.Requester, RequestDigest: receipt.RequestDigest, ExpectedAuthorityDigest: receipt.ExpectedAuthorityDigest, OperationAnchorDigest: receipt.OperationAnchorDigest, TargetBindingDigest: receipt.TargetBindingDigest, AcceptedAt: receipt.AcceptedAt})
	if err != nil {
		return ConfigurationApplyResult{}, err
	}
	if persisted.Phase == OperationPhaseObserved {
		return ConfigurationApplyResult{Generation: authority.Desired, Receipt: persisted, NoOp: true}, nil
	}
	applied, _, err := receipts.RecordApplied(ctx, OperationReceiptMutation{OperationID: persisted.OperationID, Outcome: OperationOutcomePending, ResultingAuthorityDigest: authority.Desired.Digest, ResultingState: string(authority.Desired.State), ResultingVersion: authority.Desired.GenerationID, EvidenceDigest: configurationDigest("configuration-noop", authority.Desired.Digest), At: s.now().UTC()})
	if err != nil {
		return ConfigurationApplyResult{}, err
	}
	settled, _, err := receipts.RecordSettled(ctx, OperationReceiptMutation{OperationID: applied.OperationID, ExpectedPhase: OperationPhaseApplied, Phase: OperationPhaseObserved, Outcome: OperationOutcomeSucceeded, ResultingAuthorityDigest: authority.Desired.Digest, ResultingState: string(authority.Desired.State), ResultingVersion: authority.Desired.GenerationID, EvidenceDigest: applied.EvidenceDigest, ResultDigest: configurationDigest("configuration-noop-result", authority.Desired.Digest), At: s.now().UTC()})
	if err != nil {
		return ConfigurationApplyResult{}, err
	}
	return ConfigurationApplyResult{Generation: authority.Desired, Receipt: settled, NoOp: true}, nil
}

func (s *ConfigurationService) ReconcileRuntime(ctx context.Context, now time.Time) (ConfigurationAuthority, RuntimeObservation, error) {
	authority, found, err := s.store.ConfigurationAuthority(ctx)
	if err != nil || !found {
		return ConfigurationAuthority{}, RuntimeObservation{}, serviceError(ErrorConflict, "configuration authority is not initialized", nil)
	}
	if authority.Incomplete == nil {
		if err := s.observeDriftTransition(ctx, authority, now.UTC()); err != nil {
			if !errors.Is(err, ErrConfigurationAuthorityConflict) {
				return authority, RuntimeObservation{}, serviceError(ErrorInternal, "configuration drift evidence could not be persisted", nil)
			}
			current, currentFound, currentErr := s.store.ConfigurationAuthority(ctx)
			if currentErr != nil || !currentFound {
				return authority, RuntimeObservation{}, serviceError(ErrorInternal, "configuration drift evidence could not be persisted", nil)
			}
			if current.Incomplete == nil && s.observeDriftTransition(ctx, current, now.UTC()) != nil {
				return authority, RuntimeObservation{}, serviceError(ErrorInternal, "configuration drift evidence could not be persisted", nil)
			}
			authority = current
		}
	}
	if s.runtime == nil {
		return authority, RuntimeObservation{}, serviceError(ErrorInternal, "runtime observation is unavailable", nil)
	}
	runtime, err := s.runtime.ObserveConfigurationRuntime(ctx, now.UTC())
	if err != nil {
		return authority, RuntimeObservation{}, err
	}
	if authority.Incomplete != nil || runtime.Liveness != RuntimeLivenessFresh || runtime.LoadedConfigurationDigest != authority.Desired.Digest || runtime.LastObservedAt == nil {
		return authority, runtime, nil
	}
	live, liveCandidate, err := s.files.ReadLive()
	desired, desiredErr := s.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	if err != nil || desiredErr != nil || liveCandidate.Digest != authority.Desired.Digest || !bytes.Equal(live, desired) {
		return authority, runtime, nil
	}
	evidence := configurationDigest("effective", strconv.FormatInt(authority.Desired.GenerationID, 10), authority.Desired.Digest, runtime.WorkerInstanceID, runtime.BuildIdentity, runtime.LastObservedAt.UTC().Format(time.RFC3339Nano))
	authority, _, err = s.store.ObserveConfigurationEffective(ctx, ConfigurationEffectiveObservation{ExpectedGenerationID: authority.Desired.GenerationID, ExpectedDigest: authority.Desired.Digest, WorkerInstanceID: runtime.WorkerInstanceID, BuildIdentity: runtime.BuildIdentity, ObservedAt: runtime.LastObservedAt.UTC(), EvidenceDigest: evidence})
	if errors.Is(err, ErrConfigurationAuthorityConflict) {
		current, found, currentErr := s.store.ConfigurationAuthority(ctx)
		if currentErr != nil || !found {
			return ConfigurationAuthority{}, RuntimeObservation{}, serviceError(ErrorInternal, "effective configuration authority could not be reloaded", nil)
		}
		return current, runtime, nil
	}
	if err != nil {
		return ConfigurationAuthority{}, RuntimeObservation{}, serviceError(ErrorInternal, "effective configuration observation could not be persisted", nil)
	}
	return authority, runtime, nil
}

func (s *ConfigurationService) observeDriftTransition(ctx context.Context, authority ConfigurationAuthority, observedAt time.Time) error {
	_, live, err := s.files.ReadLive()
	observation := ConfigurationDriftObservation{
		ExpectedGenerationID: authority.Desired.GenerationID,
		ExpectedDigest:       authority.Desired.Digest,
		ObservedAt:           observedAt,
	}
	if err != nil {
		observation.Drifted = true
		observation.Reason = ConfigurationReasonUnsafeLiveFile
	} else if live.Digest != authority.Desired.Digest {
		observation.Drifted = true
		observation.Reason = ConfigurationReasonExternalDrift
		observation.ObservedDigest = live.Digest
	} else {
		observation.Reason = ConfigurationReasonReady
		observation.ObservedDigest = live.Digest
	}
	_, err = s.store.ObserveConfigurationDrift(ctx, observation)
	return err
}

func (s *ConfigurationService) Projection(ctx context.Context, requester Requester, now time.Time) (ConfigurationConvergenceProjection, error) {
	if _, _, _, err := s.authorize(ctx, requester); err != nil {
		return ConfigurationConvergenceProjection{}, err
	}
	authority, runtime, err := s.ReconcileRuntime(ctx, now)
	if err != nil {
		return ConfigurationConvergenceProjection{}, err
	}
	return s.project(authority, runtime), nil
}

func (s *ConfigurationService) CheckNewAdmission(ctx context.Context) (NewAdmissionDecision, error) {
	now := s.now().UTC()
	authority, runtime, err := s.ReconcileRuntime(ctx, now)
	if err != nil {
		return NewAdmissionDecision{Allowed: false, Reason: ConfigurationReasonRuntimeUnknown}, nil
	}
	projection := s.project(authority, runtime)
	decision := NewAdmissionDecision{Allowed: projection.State == ConfigurationReady, Reason: projection.Reason}
	if decision.Allowed && runtime.LastObservedAt != nil {
		decision.Authority = ConfigurationAdmissionAuthority{GenerationID: authority.Desired.GenerationID, Digest: authority.Desired.Digest, AuthorityVersion: authority.Version, ValidThrough: runtime.LastObservedAt.UTC().Add(WorkerHeartbeatStaleAfter)}
	}
	return decision, nil
}

func (s *ConfigurationService) History(ctx context.Context, requester Requester) ([]ConfigurationGeneration, error) {
	if _, _, _, err := s.authorize(ctx, requester); err != nil {
		return nil, err
	}
	generations, err := s.store.ListConfigurationGenerations(ctx)
	if err != nil {
		return nil, serviceError(ErrorInternal, "configuration history is unavailable", nil)
	}
	return generations, nil
}

func (s *ConfigurationService) project(authority ConfigurationAuthority, runtime RuntimeObservation) ConfigurationConvergenceProjection {
	projection := ConfigurationConvergenceProjection{DesiredGenerationID: authority.Desired.GenerationID, DesiredDigest: authority.Desired.Digest, EffectiveGenerationID: authority.EffectiveID, LoadedConfigurationDigest: runtime.LoadedConfigurationDigest, LastMeaningfulObservation: runtime.LastObservedAt}
	if authority.Incomplete != nil {
		projection.State, projection.NextAction = ConfigurationConflict, ConfigurationActionRecoverAuthority
		if authority.Incomplete.State == ConfigurationApplyAmbiguous {
			projection.Reason = ConfigurationReasonApplyAmbiguous
		} else {
			projection.Reason = ConfigurationReasonApplyIncomplete
		}
		return projection
	}
	_, live, liveErr := s.files.ReadLive()
	if liveErr != nil {
		projection.State, projection.Reason, projection.NextAction = ConfigurationConflict, ConfigurationReasonUnsafeLiveFile, ConfigurationActionRecoverAuthority
		return projection
	}
	if live.Digest != authority.Desired.Digest {
		projection.State, projection.Reason, projection.NextAction = ConfigurationConflict, ConfigurationReasonExternalDrift, ConfigurationActionRecoverAuthority
		return projection
	}
	switch runtime.Liveness {
	case RuntimeLivenessFresh:
		if runtime.LoadedConfigurationDigest != authority.Desired.Digest {
			projection.State, projection.Reason, projection.NextAction = ConfigurationRestartRequired, ConfigurationReasonRestartRequired, ConfigurationActionRestartWorker
		} else if authority.EffectiveID != authority.Desired.GenerationID || authority.Desired.EffectiveAt.IsZero() {
			projection.State, projection.Reason, projection.NextAction = ConfigurationStarting, ConfigurationReasonEffectiveUnsettled, ConfigurationActionWaitForWorker
		} else {
			projection.State, projection.Reason, projection.NextAction = ConfigurationReady, ConfigurationReasonReady, ConfigurationActionNone
		}
	case RuntimeLivenessStale:
		projection.State, projection.Reason, projection.NextAction = ConfigurationStale, ConfigurationReasonRuntimeStale, ConfigurationActionInspectRuntime
	case RuntimeLivenessOffline:
		projection.State, projection.Reason, projection.NextAction = ConfigurationOffline, ConfigurationReasonRuntimeOffline, ConfigurationActionRestartWorker
	case RuntimeLivenessConflict:
		projection.State, projection.Reason, projection.NextAction = ConfigurationConflict, ConfigurationReasonRuntimeConflict, ConfigurationActionInspectRuntime
	default:
		projection.State, projection.Reason, projection.NextAction = ConfigurationUnknown, ConfigurationReasonRuntimeUnknown, ConfigurationActionInspectRuntime
	}
	return projection
}

func (s *ConfigurationService) authorize(ctx context.Context, requester Requester) (ConfigurationAuthority, ConfiguredRequester, AuthorizedScopeSet, error) {
	authority, found, err := s.store.ConfigurationAuthority(ctx)
	if err != nil || !found {
		return ConfigurationAuthority{}, ConfiguredRequester{}, AuthorizedScopeSet{}, hiddenTargetError()
	}
	operator, ok := configurationGenerationOperator(authority.Desired)
	if !ok {
		return ConfigurationAuthority{}, ConfiguredRequester{}, AuthorizedScopeSet{}, hiddenTargetError()
	}
	authorizer, err := NewAuthorizationService(ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		return ConfigurationAuthority{}, ConfiguredRequester{}, AuthorizedScopeSet{}, hiddenTargetError()
	}
	configured, err := authorizer.ResolveConfiguredRequester(requester)
	if err != nil {
		return ConfigurationAuthority{}, ConfiguredRequester{}, AuthorizedScopeSet{}, hiddenTargetError()
	}
	scopes, err := authorizer.ControllerScopes(configured)
	if err != nil || !scopes.HasController() {
		return ConfigurationAuthority{}, ConfiguredRequester{}, AuthorizedScopeSet{}, hiddenTargetError()
	}
	return authority, configured, scopes, nil
}

func configurationGenerationOperator(generation ConfigurationGeneration) (domain.GitHubUserIdentity, bool) {
	if generation.ConfiguredOperator.Validate() == nil {
		return generation.ConfiguredOperator, true
	}
	return domain.GitHubUserIdentity{}, false
}

func configurationApplyReceipt(authority ConfigurationAuthority, requester ConfiguredRequester, scopes AuthorizedScopeSet, candidate ValidatedConfigurationCandidate, at time.Time) OperationReceipt {
	return configurationApplyReceiptFor(authority.Desired.GenerationID, authority.Desired.Digest, requester, scopes, candidate, at)
}

func configurationApplyReceiptFor(expectedGenerationID int64, expectedDigest string, requester ConfiguredRequester, scopes AuthorizedScopeSet, candidate ValidatedConfigurationCandidate, at time.Time) OperationReceipt {
	target, _ := scopes.ControllerOperationTarget()
	anchor := configurationDigest("configuration-apply", strconv.FormatInt(expectedGenerationID, 10), expectedDigest, candidate.Digest)
	return NewOperationReceipt(OperationReceiptInput{OperationType: OperationApplyConfiguration, Scope: ScopeController, TargetID: target.TargetID, Requester: requester.identity, RequestDigest: candidate.Digest, ExpectedAuthorityDigest: expectedDigest, OperationAnchorDigest: anchor, TargetBindingDigest: target.TargetBindingDigest, AcceptedAt: at})
}

func classifyConfigurationStoreError(err error) error {
	switch {
	case errors.Is(err, ErrConfigurationAuthorityConflict), errors.Is(err, ErrConfigurationApplyInProgress), errors.Is(err, ErrOperationReceiptConflict):
		return serviceError(ErrorConflict, "configuration apply authority changed", nil)
	default:
		return serviceError(ErrorInternal, "configuration apply could not be persisted", nil)
	}
}

func generationByID(generations []ConfigurationGeneration, id int64) (ConfigurationGeneration, bool) {
	for _, generation := range generations {
		if generation.GenerationID == id {
			return generation, true
		}
	}
	return ConfigurationGeneration{}, false
}

func (s *ConfigurationService) prune(ctx context.Context) {
	claims, err := s.store.ConfigurationRawPruneClaims(ctx)
	if err != nil {
		return
	}
	for _, digest := range claims {
		s.completeRawPrune(ctx, digest)
	}
	digests, err := s.store.ConfigurationRawPruneCandidates(ctx, ConfigurationRawRetainCount)
	if err != nil {
		return
	}
	for _, digest := range digests {
		claimed, claimErr := s.store.ClaimConfigurationRawPrune(ctx, digest)
		if claimErr != nil || !claimed {
			continue
		}
		s.completeRawPrune(ctx, digest)
	}
}

func (s *ConfigurationService) completeRawPrune(ctx context.Context, digest string) {
	removed := s.files.RemoveRaw(digest) == nil
	_ = s.store.CompleteConfigurationRawPrune(ctx, digest, removed)
}
