package application

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"time"
)

func (s *ConfigurationService) RecoveryOffer(ctx context.Context, requester Requester) (ConfigurationRecoveryOffer, bool, error) {
	authority, _, _, err := s.authorize(ctx, requester)
	if err != nil {
		return ConfigurationRecoveryOffer{}, false, err
	}
	authority, err = s.Reconcile(ctx)
	if err != nil {
		var conflict *ServiceError
		if authority.Incomplete != nil || authority.IncompleteRecovery != nil || errors.As(err, &conflict) && conflict.Category == ErrorConflict {
			return ConfigurationRecoveryOffer{}, false, nil
		}
		return ConfigurationRecoveryOffer{}, false, err
	}
	authority, _, _, err = s.authorize(ctx, requester)
	if err != nil {
		return ConfigurationRecoveryOffer{}, false, err
	}
	return s.currentRecoveryOffer(ctx, authority, s.now().UTC())
}

func (s *ConfigurationService) currentRecoveryOffer(ctx context.Context, authority ConfigurationAuthority, observedAt time.Time) (ConfigurationRecoveryOffer, bool, error) {
	if authority.Incomplete != nil || authority.IncompleteRecovery != nil || !authority.Desired.RawRetained {
		return ConfigurationRecoveryOffer{}, false, nil
	}
	if err := s.observeDriftTransition(ctx, authority, observedAt); err != nil && !errors.Is(err, ErrConfigurationAuthorityConflict) {
		return ConfigurationRecoveryOffer{}, false, serviceError(ErrorInternal, "configuration drift evidence could not be persisted", nil)
	}
	current, found, err := s.store.ConfigurationAuthority(ctx)
	if err != nil || !found || current.Incomplete != nil || current.IncompleteRecovery != nil {
		return ConfigurationRecoveryOffer{}, false, serviceError(ErrorConflict, "configuration recovery authority changed", nil)
	}
	desired, desiredErr := s.files.ReadRaw(current.Desired.Digest, current.Desired.Size)
	live, candidate, liveErr := s.files.ReadLive()
	if desiredErr != nil || liveErr != nil || candidate.DatabasePath != current.DatabasePath || candidate.Digest == current.Desired.Digest || bytes.Equal(live, desired) {
		return ConfigurationRecoveryOffer{}, false, nil
	}
	return ConfigurationRecoveryOffer{
		State:                    "available",
		Reason:                   ConfigurationReasonExternalDrift,
		Action:                   ConfigurationRecoveryActionRestore,
		ExpectedGenerationID:     current.Desired.GenerationID,
		ExpectedDigest:           current.Desired.Digest,
		ExpectedAuthorityVersion: current.Version,
		ObservedDigest:           candidate.Digest,
		ObservedAt:               observedAt,
	}, true, nil
}

func (s *ConfigurationService) RecoverRestore(ctx context.Context, command ConfigurationRecoveryCommand) (ConfigurationRecoveryResult, error) {
	if _, _, _, err := s.authorize(ctx, command.Requester); err != nil {
		return ConfigurationRecoveryResult{}, err
	}
	if command.ExpectedGenerationID <= 0 || command.ExpectedAuthorityVersion <= 0 || !validAuthorityDigest(command.ExpectedDigest) || !validAuthorityDigest(command.ObservedDigest) || command.ExpectedDigest == command.ObservedDigest {
		return ConfigurationRecoveryResult{}, serviceError(ErrorInvalidInput, "configuration recovery authority is invalid", nil)
	}
	replay, replayFound := s.configurationRecoveryReplay(ctx, command)
	authority, err := s.Reconcile(ctx)
	if err != nil {
		if replayFound {
			if refreshed, ok := s.configurationRecoveryReplay(ctx, command); ok && refreshed.Receipt.Phase == OperationPhaseObserved {
				if refreshed.Receipt.Outcome == OperationOutcomeSucceeded {
					currentAuthority, _, _, currentAuthErr := s.authorize(ctx, command.Requester)
					if currentAuthErr != nil {
						return ConfigurationRecoveryResult{}, currentAuthErr
					}
					if current, currentErr := s.configurationRecoveryReplayCurrent(ctx, currentAuthority, refreshed.Recovery); currentErr != nil || !current {
						return ConfigurationRecoveryResult{}, serviceError(ErrorConflict, "configuration recovery authority changed", nil)
					}
					refreshed.Convergence, _ = s.Projection(ctx, command.Requester, s.now().UTC())
					return refreshed, nil
				}
				return refreshed, serviceError(ErrorConflict, "configuration recovery is ambiguous", nil)
			}
			var conflict *ServiceError
			if replay.Receipt.Phase == OperationPhaseAccepted && errors.As(err, &conflict) && conflict.Category == ErrorConflict && conflict.Message == "configuration recovery is still active" {
				return replay, nil
			}
		}
		return ConfigurationRecoveryResult{}, err
	}
	if replay, ok := s.configurationRecoveryReplay(ctx, command); ok && replay.Receipt.Phase == OperationPhaseObserved {
		if current, currentErr := s.configurationRecoveryReplayCurrent(ctx, authority, replay.Recovery); currentErr != nil || !current {
			return ConfigurationRecoveryResult{}, serviceError(ErrorConflict, "configuration recovery authority changed", nil)
		}
		if replay.Receipt.Outcome != OperationOutcomeSucceeded {
			return replay, serviceError(ErrorConflict, "configuration recovery is ambiguous", nil)
		}
		replay.Convergence, _ = s.Projection(ctx, command.Requester, s.now().UTC())
		return replay, nil
	}
	authority, configured, scopes, err := s.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationRecoveryResult{}, err
	}
	offer, eligible, err := s.currentRecoveryOffer(ctx, authority, s.now().UTC())
	if err != nil {
		return ConfigurationRecoveryResult{}, err
	}
	if !eligible || !recoveryCommandMatchesOffer(command, offer) {
		return ConfigurationRecoveryResult{}, serviceError(ErrorConflict, "configuration recovery authority changed", nil)
	}
	receipt := configurationRecoveryReceipt(command, configured, scopes, s.now().UTC())
	lock, acquired, lockErr := s.files.AcquireReplacement(receipt.OperationID)
	if lockErr != nil {
		return ConfigurationRecoveryResult{}, serviceError(ErrorInternal, "configuration replacement authority is unavailable", nil)
	}
	if !acquired {
		if replay, ok := s.configurationRecoveryReplay(ctx, command); ok {
			return replay, nil
		}
		return ConfigurationRecoveryResult{}, serviceError(ErrorConflict, "configuration recovery is still active", nil)
	}
	defer func() {
		if lock != nil {
			_ = lock.Release()
		}
	}()
	// A peer may have accepted or settled this exact command after the initial
	// replay check and before this process acquired the shared mutation lock.
	// Release the lock and reconcile that durable operation instead of treating
	// it as a fresh recovery or returning an unvalidated settled receipt.
	if _, ok := s.configurationRecoveryReplay(ctx, command); ok {
		_ = lock.Release()
		lock = nil
		_, reconcileErr := s.Reconcile(ctx)
		refreshed, refreshedFound := s.configurationRecoveryReplay(ctx, command)
		if !refreshedFound {
			return ConfigurationRecoveryResult{}, serviceError(ErrorConflict, "configuration recovery replay evidence conflicts", nil)
		}
		if refreshed.Receipt.Phase == OperationPhaseObserved {
			if refreshed.Receipt.Outcome != OperationOutcomeSucceeded {
				return refreshed, serviceError(ErrorConflict, "configuration recovery is ambiguous", nil)
			}
			currentAuthority, _, _, currentAuthErr := s.authorize(ctx, command.Requester)
			if currentAuthErr != nil {
				return ConfigurationRecoveryResult{}, currentAuthErr
			}
			if current, currentErr := s.configurationRecoveryReplayCurrent(ctx, currentAuthority, refreshed.Recovery); currentErr != nil || !current {
				return ConfigurationRecoveryResult{}, serviceError(ErrorConflict, "configuration recovery authority changed", nil)
			}
			refreshed.Convergence, _ = s.Projection(ctx, command.Requester, s.now().UTC())
			return refreshed, nil
		}
		if reconcileErr != nil {
			var conflict *ServiceError
			if errors.As(reconcileErr, &conflict) && conflict.Category == ErrorConflict && conflict.Message == "configuration recovery is still active" {
				return refreshed, nil
			}
			return ConfigurationRecoveryResult{}, reconcileErr
		}
		return refreshed, serviceError(ErrorConflict, "configuration recovery settlement requires reconciliation", nil)
	}

	authority, configured, scopes, err = s.authorize(ctx, command.Requester)
	if err != nil {
		return ConfigurationRecoveryResult{}, err
	}
	offer, eligible, err = s.currentRecoveryOffer(ctx, authority, s.now().UTC())
	if err != nil || !eligible || !recoveryCommandMatchesOffer(command, offer) {
		return ConfigurationRecoveryResult{}, serviceError(ErrorConflict, "configuration recovery authority changed", nil)
	}
	receipt = configurationRecoveryReceipt(command, configured, scopes, s.now().UTC())
	if s.recoveryStore == nil {
		return ConfigurationRecoveryResult{}, serviceError(ErrorInternal, "configuration recovery persistence is unavailable", nil)
	}
	intent, acceptedReceipt, created, err := s.recoveryStore.BeginConfigurationRecovery(ctx, ConfigurationRecoveryAcceptance{
		DesiredGenerationID: command.ExpectedGenerationID,
		DesiredDigest:       command.ExpectedDigest,
		AuthorityVersion:    command.ExpectedAuthorityVersion,
		ObservedDigest:      command.ObservedDigest,
		Requester:           configured.identity,
		Receipt:             receipt,
		AcceptedAt:          receipt.AcceptedAt,
	})
	if err != nil {
		return ConfigurationRecoveryResult{}, classifyConfigurationRecoveryStoreError(err)
	}
	if !created {
		return ConfigurationRecoveryResult{Recovery: intent, Receipt: acceptedReceipt}, nil
	}
	desired, rawErr := s.files.ReadRaw(command.ExpectedDigest, authority.Desired.Size)
	observation, restoreErr := s.files.ReconcileRestore(intent.OperationID, command.ObservedDigest, desired)
	outcome, reason := ConfigurationRecoveryCommitted, ConfigurationReasonReady
	if rawErr != nil || restoreErr != nil || observation.State != ConfigurationRestoreFileDesired || observation.Digest != command.ExpectedDigest {
		outcome, reason = ConfigurationRecoveryAmbiguous, ConfigurationReasonRecoveryAmbiguous
	}
	evidence := configurationDigest("configuration-recovery-settlement-v1", intent.OperationID, string(outcome), command.ExpectedDigest, command.ObservedDigest, observation.Digest)
	settledAuthority, settledIntent, settledReceipt, _, settleErr := s.recoveryStore.SettleConfigurationRecovery(ctx, ConfigurationRecoverySettlement{OperationID: intent.OperationID, Outcome: outcome, Reason: reason, EvidenceDigest: evidence, SettledAt: s.now().UTC()})
	if settleErr != nil {
		return ConfigurationRecoveryResult{}, serviceError(ErrorConflict, "configuration recovery settlement requires reconciliation", nil)
	}
	result := ConfigurationRecoveryResult{Recovery: settledIntent, Receipt: settledReceipt}
	if outcome != ConfigurationRecoveryCommitted {
		return result, serviceError(ErrorConflict, "configuration recovery is ambiguous", nil)
	}
	settledAuthority = s.reconcileRuntimeBestEffort(ctx, settledAuthority)
	_ = lock.Release()
	lock = nil
	result.Convergence = s.project(settledAuthority, RuntimeObservation{})
	if projection, projectionErr := s.Projection(ctx, command.Requester, s.now().UTC()); projectionErr == nil {
		result.Convergence = projection
	}
	return result, nil
}

func (s *ConfigurationService) configurationRecoveryReplayCurrent(ctx context.Context, authority ConfigurationAuthority, intent ConfigurationRecoveryIntent) (bool, error) {
	if s.recoveryStore == nil {
		return false, errors.New("configuration recovery persistence is unavailable")
	}
	if authority.Incomplete != nil || authority.IncompleteRecovery != nil || !authority.Desired.RawRetained || authority.Desired.GenerationID != intent.DesiredGenerationID || authority.Desired.Digest != intent.DesiredDigest || authority.Version <= intent.AuthorityVersion {
		return false, nil
	}
	desired, desiredErr := s.files.ReadRaw(authority.Desired.Digest, authority.Desired.Size)
	live, candidate, liveErr := s.files.ReadLive()
	if desiredErr != nil || liveErr != nil || candidate.DatabasePath != authority.DatabasePath || candidate.Digest != authority.Desired.Digest || !bytes.Equal(live, desired) {
		return false, nil
	}
	return s.recoveryStore.ConfigurationRecoveryIsLatest(ctx, intent.OperationID, intent.DesiredGenerationID)
}

func classifyConfigurationRecoveryStoreError(err error) error {
	switch {
	case errors.Is(err, ErrConfigurationAuthorityConflict), errors.Is(err, ErrConfigurationApplyInProgress), errors.Is(err, ErrConfigurationRecoveryInProgress), errors.Is(err, ErrOperationReceiptConflict):
		return serviceError(ErrorConflict, "configuration recovery authority changed", nil)
	default:
		return serviceError(ErrorInternal, "configuration recovery could not be persisted", nil)
	}
}

func (s *ConfigurationService) reconcileRecovery(ctx context.Context, authority ConfigurationAuthority, intent ConfigurationRecoveryIntent) (ConfigurationAuthority, error) {
	if intent.State == ConfigurationRecoveryAmbiguous {
		return authority, serviceError(ErrorConflict, "configuration recovery is ambiguous", nil)
	}
	if s.recoveryStore == nil {
		return authority, serviceError(ErrorInternal, "configuration recovery persistence is unavailable", nil)
	}
	lock, acquired, lockErr := s.files.AcquireReplacement(intent.OperationID)
	if lockErr != nil {
		return authority, serviceError(ErrorInternal, "configuration replacement authority is unavailable", nil)
	}
	if !acquired {
		return authority, serviceError(ErrorConflict, "configuration recovery is still active", nil)
	}
	defer lock.Release()
	desired, rawErr := s.files.ReadRaw(intent.DesiredDigest, authority.Desired.Size)
	observation, restoreErr := s.files.ReconcileRestore(intent.OperationID, intent.ObservedDigest, desired)
	outcome, reason := ConfigurationRecoveryCommitted, ConfigurationReasonReady
	if rawErr != nil || restoreErr != nil || observation.State != ConfigurationRestoreFileDesired || observation.Digest != intent.DesiredDigest {
		outcome, reason = ConfigurationRecoveryAmbiguous, ConfigurationReasonRecoveryAmbiguous
	}
	evidence := configurationDigest("configuration-recovery-reconcile-v1", intent.OperationID, string(outcome), intent.DesiredDigest, intent.ObservedDigest, observation.Digest)
	settled, _, _, _, err := s.recoveryStore.SettleConfigurationRecovery(ctx, ConfigurationRecoverySettlement{OperationID: intent.OperationID, Outcome: outcome, Reason: reason, EvidenceDigest: evidence, SettledAt: s.now().UTC()})
	if err != nil {
		return authority, serviceError(ErrorConflict, "configuration recovery reconciliation conflicts", nil)
	}
	if outcome == ConfigurationRecoveryAmbiguous {
		return settled, serviceError(ErrorConflict, "configuration recovery is ambiguous", nil)
	}
	return settled, nil
}

func (s *ConfigurationService) configurationRecoveryReplay(ctx context.Context, command ConfigurationRecoveryCommand) (ConfigurationRecoveryResult, bool) {
	identity, err := command.Requester.githubUserIdentity()
	if err != nil {
		return ConfigurationRecoveryResult{}, false
	}
	configured := ConfiguredRequester{identity: identity}
	scopes, err := newAuthorizedScopeSet(identity, AuthorityScope{Kind: ScopeController, ID: controllerScopeID, AuthorityDigest: identityDigest(identity)})
	if err != nil {
		return ConfigurationRecoveryResult{}, false
	}
	receipt := configurationRecoveryReceipt(command, configured, scopes, s.now().UTC())
	persisted, err := s.store.GetAuthorizedOperationReceipt(ctx, receipt.OperationID, scopes)
	if err != nil {
		return ConfigurationRecoveryResult{}, false
	}
	intent, found, err := s.recoveryIntent(ctx, receipt.OperationID)
	if err != nil || !found || !sameConfigurationRecoveryIntentCommand(intent, command) {
		return ConfigurationRecoveryResult{}, false
	}
	return ConfigurationRecoveryResult{Recovery: intent, Receipt: persisted}, true
}

func (s *ConfigurationService) recoveryIntent(ctx context.Context, operationID string) (ConfigurationRecoveryIntent, bool, error) {
	if s.recoveryStore == nil {
		return ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery persistence is unavailable")
	}
	return s.recoveryStore.ConfigurationRecoveryIntent(ctx, operationID)
}

func configurationRecoveryReceipt(command ConfigurationRecoveryCommand, requester ConfiguredRequester, scopes AuthorizedScopeSet, at time.Time) OperationReceipt {
	target, _ := scopes.ControllerOperationTarget()
	authorityDigest := configurationDigest("configuration-recovery-authority-v1", strconv.FormatInt(command.ExpectedGenerationID, 10), command.ExpectedDigest, strconv.FormatInt(command.ExpectedAuthorityVersion, 10), command.ObservedDigest)
	requestDigest := configurationDigest("configuration-recovery-request-v1", authorityDigest)
	anchor := configurationDigest("configuration-recovery-occurrence-v1", authorityDigest)
	return NewOperationReceipt(OperationReceiptInput{OperationType: OperationRestoreConfiguration, Scope: ScopeController, TargetID: target.TargetID, Requester: requester.identity, RequestDigest: requestDigest, ExpectedAuthorityDigest: authorityDigest, OperationAnchorDigest: anchor, TargetBindingDigest: target.TargetBindingDigest, AcceptedAt: at})
}

func recoveryCommandMatchesOffer(command ConfigurationRecoveryCommand, offer ConfigurationRecoveryOffer) bool {
	return command.ExpectedGenerationID == offer.ExpectedGenerationID && command.ExpectedDigest == offer.ExpectedDigest && command.ExpectedAuthorityVersion == offer.ExpectedAuthorityVersion && command.ObservedDigest == offer.ObservedDigest
}

func sameConfigurationRecoveryIntentCommand(intent ConfigurationRecoveryIntent, command ConfigurationRecoveryCommand) bool {
	return intent.DesiredGenerationID == command.ExpectedGenerationID && intent.DesiredDigest == command.ExpectedDigest && intent.AuthorityVersion == command.ExpectedAuthorityVersion && intent.ObservedDigest == command.ObservedDigest
}
