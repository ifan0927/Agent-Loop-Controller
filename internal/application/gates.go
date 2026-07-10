package application

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type PROpenEvidence struct {
	Review              domain.ReviewOutcome
	CurrentHeadSHA      string
	VerificationHeadSHA string
}

func AuthorizePROpen(from domain.State, evidence PROpenEvidence) error {
	if from != domain.StateFreshReview {
		return fmt.Errorf("PR open requires fresh_review state, got %s", from)
	}
	if err := evidence.Review.Validate(); err != nil {
		return fmt.Errorf("invalid fresh review: %w", err)
	}
	if evidence.Review.Verdict != domain.ReviewPass {
		return errors.New("PR open requires a passing fresh review")
	}
	currentHead := strings.TrimSpace(evidence.CurrentHeadSHA)
	if currentHead == "" {
		return errors.New("current head SHA must not be empty")
	}
	if evidence.Review.ReviewedHeadSHA != currentHead {
		return errors.New("fresh review does not match current head SHA")
	}
	if strings.TrimSpace(evidence.VerificationHeadSHA) != currentHead {
		return errors.New("controller verification does not match current head SHA")
	}
	return nil
}
