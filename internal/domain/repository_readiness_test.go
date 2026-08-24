package domain

import (
	"strings"
	"testing"
	"time"
)

func TestRepositoryReadinessAggregationPrecedence(t *testing.T) {
	results := completeReadiness(RepositoryReady)
	for _, test := range []struct {
		statuses map[RepositoryReadinessDimension]RepositoryReadinessStatus
		want     RepositoryReadinessStatus
	}{
		{map[RepositoryReadinessDimension]RepositoryReadinessStatus{}, RepositoryReady},
		{map[RepositoryReadinessDimension]RepositoryReadinessStatus{ReadinessLinearLabel: RepositoryNotReady}, RepositoryNotReady},
		{map[RepositoryReadinessDimension]RepositoryReadinessStatus{ReadinessLinearLabel: RepositoryNotReady, ReadinessGitHubApp: RepositoryUnknown}, RepositoryUnknown},
		{map[RepositoryReadinessDimension]RepositoryReadinessStatus{ReadinessLinearLabel: RepositoryNotReady, ReadinessGitHubApp: RepositoryUnknown, ReadinessBaseBranch: RepositoryConflict}, RepositoryConflict},
		{map[RepositoryReadinessDimension]RepositoryReadinessStatus{ReadinessLinearLabel: RepositoryNotApplicable}, RepositoryReady},
	} {
		copy := append([]RepositoryDimensionResult(nil), results...)
		for dimension, status := range test.statuses {
			for index := range copy {
				if copy[index].Dimension == dimension {
					copy[index].Status = status
				}
			}
		}
		got, err := AggregateRepositoryReadiness(copy)
		if err != nil || got != test.want {
			t.Fatalf("statuses=%v got=%s want=%s err=%v", test.statuses, got, test.want, err)
		}
	}
}

func TestRepositoryReadinessRequiresEveryDimensionExactlyOnce(t *testing.T) {
	results := completeReadiness(RepositoryReady)
	if err := ValidateCompleteRepositoryReadiness(results[:len(results)-1]); err == nil {
		t.Fatal("incomplete snapshot accepted")
	}
	results[len(results)-1] = results[0]
	if err := ValidateCompleteRepositoryReadiness(results); err == nil {
		t.Fatal("duplicate snapshot dimension accepted")
	}
}

func completeReadiness(status RepositoryReadinessStatus) []RepositoryDimensionResult {
	results := make([]RepositoryDimensionResult, 0, len(RepositoryReadinessDimensions))
	for _, dimension := range RepositoryReadinessDimensions {
		results = append(results, RepositoryDimensionResult{Dimension: dimension, Status: status, ReasonCode: "fixture", EvidenceDigest: strings.Repeat("a", 64), ObservedAt: time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)})
	}
	return results
}
