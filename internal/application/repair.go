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
)

type repairFindingReference struct {
	Source     string `json:"source"`
	SourceID   string `json:"source_id"`
	BodyDigest string `json:"body_digest"`
	HeadSHA    string `json:"head_sha"`
}

type repairEvidence struct {
	Prompt   string                   `json:"normalized_prompt"`
	Hash     string                   `json:"prompt_hash"`
	Findings []repairFindingReference `json:"findings,omitempty"`
}

// RepairableFindings selects only current, unresolved CodeRabbit findings.
// The GitHub adapter establishes CodeRabbit's immutable identity; this
// application boundary accepts only that normalized source and rechecks every
// persisted body before it can reach a Terra resume prompt.
func RepairableFindings(findings []FindingRecord, head string) ([]FindingRecord, error) {
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("repair head must not be blank")
	}
	selected := make([]FindingRecord, 0, len(findings))
	seen := make(map[string]struct{}, len(findings))
	bodyBytes := 0
	for _, finding := range findings {
		if finding.HeadSHA != head || finding.Resolved || finding.Outdated {
			continue
		}
		if finding.Source != "coderabbit_review_comment" && finding.Source != "github_required_check" {
			return nil, fmt.Errorf("unsupported actionable finding source %q", finding.Source)
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

func repairableEvidenceFindings(evidence domain.GitHubReadEvidence, head string) ([]domain.NormalizedFinding, []FindingRecord, error) {
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
	selected, err := RepairableFindings(records, head)
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
	for _, transition := range timeline {
		if transition.From == domain.StateRepairing && transition.To == domain.StateExecuting {
			deadline := transition.CreatedAt.Add(repairDeadline)
			return deadline, !transition.CreatedAt.IsZero() && !now.Before(deadline)
		}
	}
	return now.Add(repairDeadline), false
}
