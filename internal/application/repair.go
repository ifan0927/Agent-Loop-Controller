package application

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

const (
	// MaxNormalizedFindingBodyBytes bounds untrusted review text retained for a
	// repair prompt. The limit is intentionally independent of GitHub response
	// limits so controller-owned artifacts remain predictable.
	MaxNormalizedFindingBodyBytes = 16 << 10
	MaxNormalizedFindings         = 50
	MaxRepairPromptBytes          = 64 << 10
	repairDeadline                = 30 * time.Minute
	repairDeadlinePersistenceTTL  = 5 * time.Second
)

type repairFindingReference struct {
	Source     string `json:"source"`
	SourceID   string `json:"source_id"`
	BodyDigest string `json:"body_digest"`
	HeadSHA    string `json:"head_sha"`
}

type repairEvidence struct {
	// Prompt is in-memory only. Raw untrusted bodies belong exclusively in the
	// bounded finding/feedback stores, never in a transition evidence payload.
	Prompt   string                   `json:"-"`
	Hash     string                   `json:"prompt_hash"`
	Findings []repairFindingReference `json:"findings,omitempty"`
}

// RepairableFindings selects only current controller-generated check findings
// and trusted inline findings backed by a selected immutable feedback record.
// The variadic argument preserves the narrow legacy check-only caller contract.
func RepairableFindings(findings []FindingRecord, head string, feedback ...[]TrustedReviewFeedbackRecord) ([]FindingRecord, error) {
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("repair head must not be blank")
	}
	selected := make([]FindingRecord, 0, len(findings))
	trusted := make(map[string]TrustedReviewFeedbackRecord)
	if len(feedback) > 0 {
		for _, item := range feedback[0] {
			trusted[item.RootCommentNodeID] = item
		}
	}
	seen := make(map[string]struct{}, len(findings))
	bodyBytes := 0
	for _, finding := range findings {
		if finding.HeadSHA != head || finding.Resolved || finding.Outdated {
			continue
		}
		if finding.Source != "github_required_check" && finding.Source != "github_human_review_comment" && finding.Source != freshReviewFindingSource {
			return nil, fmt.Errorf("unsupported actionable finding source %q", finding.Source)
		}
		if finding.Source == "github_human_review_comment" {
			item, found := trusted[finding.SourceID]
			if !found || item.Lifecycle != domain.TrustedReviewFeedbackSelectedForRepair || item.OriginalReviewHeadSHA != head || item.Resolved || item.Outdated || item.ThreadNodeID != finding.ThreadID || item.BodyDigest != finding.BodyDigest || item.Body != finding.Body {
				return nil, errors.New("inline finding lacks matching selected trusted feedback")
			}
		}
		if strings.TrimSpace(finding.SourceID) == "" || strings.TrimSpace(finding.Body) == "" {
			return nil, errors.New("actionable finding identity or body is incomplete")
		}
		if len([]byte(finding.Body)) > MaxNormalizedFindingBodyBytes || strings.ContainsRune(finding.Body, '\x00') {
			return nil, errors.New("actionable finding body exceeds controller bounds")
		}
		digest := sha256.Sum256([]byte(finding.Body))
		if finding.BodyDigest != hex.EncodeToString(digest[:]) {
			return nil, errors.New("actionable finding body digest mismatch")
		}
		key := finding.Source + "\x00" + finding.SourceID
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if len(selected) == MaxNormalizedFindings || bodyBytes+len([]byte(finding.Body)) > MaxRepairPromptBytes {
			return nil, errors.New("actionable findings exceed controller aggregate bounds")
		}
		bodyBytes += len([]byte(finding.Body))
		selected = append(selected, finding)
	}
	if len(selected) == 0 {
		return nil, errors.New("no supported actionable findings are available for repair")
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].Source == selected[j].Source {
			return selected[i].SourceID < selected[j].SourceID
		}
		return selected[i].Source < selected[j].Source
	})
	if len([]byte(BuildRepairPrompt(selected))) > MaxRepairPromptBytes {
		return nil, errors.New("repair prompt exceeds controller aggregate bounds")
	}
	return selected, nil
}

func repairableEvidenceFindings(evidence domain.GitHubReadEvidence, head string, feedback ...[]TrustedReviewFeedbackRecord) ([]domain.NormalizedFinding, []FindingRecord, error) {
	findings := append([]domain.NormalizedFinding(nil), evidence.Findings...)
	for _, check := range evidence.Checks {
		if !check.Required || check.ObservedSHA != head || (check.State != domain.CheckFailure && check.State != domain.CheckActionRequired) {
			continue
		}
		if strings.TrimSpace(check.ID) == "" || strings.TrimSpace(check.Name) == "" || strings.TrimSpace(check.Source) == "" {
			return nil, nil, errors.New("actionable required check lacks trusted identity evidence")
		}
		body := fmt.Sprintf("Required CI check %q from trusted GitHub source %q reported %q. Repair only the controller-owned worktree and re-run the controller verifier.", check.Name, check.Source, check.State)
		digest := sha256.Sum256([]byte(body))
		findings = append(findings, domain.NormalizedFinding{Source: "github_required_check", SourceID: check.ID, Classification: "required_check_failure", BodyDigest: hex.EncodeToString(digest[:]), Body: body, HeadSHA: head, SourceAt: check.SourceAt, ObservedAt: check.ObservedAt})
	}
	records := make([]FindingRecord, 0, len(findings))
	for _, finding := range findings {
		records = append(records, FindingRecord{SourceID: finding.SourceID, ThreadID: finding.ThreadID, Source: finding.Source,
			File: finding.File, Line: finding.Line, Severity: finding.Classification, BodyDigest: finding.BodyDigest, Body: finding.Body,
			Resolved: finding.Resolved, Outdated: finding.Outdated, HeadSHA: finding.HeadSHA, ObservedAt: finding.ObservedAt})
	}
	selected, err := RepairableFindings(records, head, feedback...)
	if err != nil {
		return nil, nil, err
	}
	return findings, selected, nil
}

func repairEvidenceFor(findings []FindingRecord) repairEvidence {
	references := make([]repairFindingReference, 0, len(findings))
	for _, finding := range findings {
		references = append(references, repairFindingReference{Source: finding.Source, SourceID: finding.SourceID, BodyDigest: finding.BodyDigest, HeadSHA: finding.HeadSHA})
	}
	prompt := BuildRepairPrompt(findings)
	return repairEvidence{Prompt: prompt, Hash: bytesHash([]byte(prompt)), Findings: references}
}

func repairDeadlineExceeded(timeline []Transition, now time.Time) bool {
	_, expired := repairDeadlineAt(timeline, now)
	return expired
}

func repairDeadlineAt(timeline []Transition, now time.Time) (time.Time, bool) {
	deadline, found, err := persistedRepairDeadline(timeline)
	if err != nil {
		return time.Time{}, true
	}
	if !found {
		return now.Add(repairDeadline), false
	}
	return deadline, !now.Before(deadline)
}

func persistedRepairDeadline(timeline []Transition) (time.Time, bool, error) {
	for _, transition := range timeline {
		if transition.From == domain.StateRepairing && transition.To == domain.StateExecuting {
			if transition.CreatedAt.IsZero() {
				return time.Time{}, true, errors.New("persisted repair deadline anchor has no timestamp")
			}
			return transition.CreatedAt.Add(repairDeadline), true, nil
		}
	}
	return time.Time{}, false, nil
}

func repairDeadlineAnchorIsValid(timeline []Transition) bool {
	_, found, err := persistedRepairDeadline(timeline)
	return found && err == nil
}

func repairDeadlineAnchorInvalid(timeline []Transition) bool {
	_, found, err := persistedRepairDeadline(timeline)
	return found && err != nil
}

func repairDeadlineAnchorRequired(state domain.State, timeline []Transition) bool {
	switch state {
	case domain.StateRepairing:
		// A run may legitimately wait in repairing before its first
		// repairing -> executing transition creates the global anchor.
		return countTransitionsToState(timeline, domain.StateRepairing) != 1
	case domain.StateExecuting, domain.StateVerifying, domain.StateFreshReview:
		if hasRepairLifecycleTransition(timeline) {
			return true
		}
		return !hasInitialRepairFreePath(state, timeline)
	default:
		return false
	}
}

func hasRepairLifecycleTransition(timeline []Transition) bool {
	for _, transition := range timeline {
		if transition.From == domain.StateRepairing || transition.To == domain.StateRepairing {
			return true
		}
	}
	return false
}

func countTransitionsToState(timeline []Transition, state domain.State) int {
	count := 0
	for _, transition := range timeline {
		if transition.To == state {
			count++
		}
	}
	return count
}

func hasInitialRepairFreePath(state domain.State, timeline []Transition) bool {
	if !hasTransition(timeline, domain.StateProvisioning, domain.StateExecuting) {
		return false
	}
	switch state {
	case domain.StateExecuting:
		return true
	case domain.StateVerifying:
		return hasTransition(timeline, domain.StateExecuting, domain.StateVerifying)
	case domain.StateFreshReview:
		return hasTransition(timeline, domain.StateExecuting, domain.StateVerifying) && hasTransition(timeline, domain.StateVerifying, domain.StateFreshReview)
	default:
		return false
	}
}

func hasTransition(timeline []Transition, from, to domain.State) bool {
	for _, transition := range timeline {
		if transition.From == from && transition.To == to {
			return true
		}
	}
	return false
}
