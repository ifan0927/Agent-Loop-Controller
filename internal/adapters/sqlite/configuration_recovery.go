package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

// configurationIncompleteRecovery is the read-only compatibility boundary for
// recovery intents written before in-place configuration restore was retired.
// Current code may hydrate this evidence to fence startup and mutation, but it
// must never create, resume, settle, or rewrite an intent.
func configurationIncompleteRecovery(ctx context.Context, query queryRower) (application.ConfigurationRecoveryIntent, bool, error) {
	return configurationRecoveryIntentQuery(ctx, query, `WHERE status IN ('accepted','ambiguous') ORDER BY accepted_at DESC,operation_id DESC LIMIT 1`)
}

func configurationRecoveryIntentQuery(ctx context.Context, query queryRower, clause string, args ...any) (application.ConfigurationRecoveryIntent, bool, error) {
	var intent application.ConfigurationRecoveryIntent
	var state, accepted, settled, reason string
	err := query.QueryRowContext(ctx, `SELECT desired_generation_id,desired_digest,authority_version,observed_digest,operation_id,requester_login,requester_database_id,requester_node_id,requester_actor_type,status,accepted_at,settled_at,reason_code,evidence_digest FROM configuration_recovery_intents `+clause, args...).Scan(
		&intent.DesiredGenerationID, &intent.DesiredDigest, &intent.AuthorityVersion, &intent.ObservedDigest, &intent.OperationID,
		&intent.Requester.Login, &intent.Requester.DatabaseID, &intent.Requester.NodeID, &intent.Requester.ActorType,
		&state, &accepted, &settled, &reason, &intent.EvidenceDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return application.ConfigurationRecoveryIntent{}, false, nil
	}
	if err != nil {
		return application.ConfigurationRecoveryIntent{}, false, err
	}
	intent.State = application.ConfigurationRecoveryState(state)
	intent.AcceptedAt, intent.SettledAt = parseTime(accepted), parseTime(settled)
	intent.Reason = application.ConfigurationReason(reason)
	if intent.DesiredGenerationID <= 0 || intent.AuthorityVersion <= 0 || !validConfigurationDigest(intent.DesiredDigest) || !validConfigurationDigest(intent.ObservedDigest) || intent.DesiredDigest == intent.ObservedDigest || intent.Requester.Validate() != nil || intent.AcceptedAt.IsZero() || intent.State != application.ConfigurationRecoveryAccepted && intent.State != application.ConfigurationRecoveryCommitted && intent.State != application.ConfigurationRecoveryAmbiguous {
		return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery intent is corrupt")
	}
	switch intent.State {
	case application.ConfigurationRecoveryAccepted:
		if !intent.SettledAt.IsZero() || intent.Reason != "" || intent.EvidenceDigest != "" {
			return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery intent is corrupt")
		}
	case application.ConfigurationRecoveryCommitted:
		if intent.SettledAt.IsZero() || intent.Reason != application.ConfigurationReasonReady || !validConfigurationDigest(intent.EvidenceDigest) {
			return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery intent is corrupt")
		}
	case application.ConfigurationRecoveryAmbiguous:
		if intent.SettledAt.IsZero() || intent.Reason != application.ConfigurationReasonRecoveryAmbiguous || !validConfigurationDigest(intent.EvidenceDigest) {
			return application.ConfigurationRecoveryIntent{}, false, errors.New("configuration recovery intent is corrupt")
		}
	}
	return intent, true, nil
}
