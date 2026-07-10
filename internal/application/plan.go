package application

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	controllercontracts "github.com/ifan0927/Agent-Loop-Controller/contracts"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/codex"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type DeliveryPlan struct {
	RunID          string            `json:"run_id"`
	IssueID        string            `json:"issue_id"`
	Artifacts      []ArtifactSpec    `json:"artifacts"`
	Implementation codex.CommandSpec `json:"implementation"`
	FreshReview    codex.CommandSpec `json:"fresh_review"`
}

type ArtifactSpec struct {
	Path            string `json:"path"`
	Content         string `json:"content"`
	Mode            uint32 `json:"mode"`
	CreateExclusive bool   `json:"create_exclusive"`
}

type Planner struct {
	commands codex.CommandBuilder
}

func NewPlanner(binary string) Planner {
	return Planner{commands: codex.NewCommandBuilder(binary)}
}

func (p Planner) Build(task domain.CodingTask, workspace, artifacts string) (DeliveryPlan, error) {
	if err := task.Validate(); err != nil {
		return DeliveryPlan{}, err
	}
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(artifacts) {
		return DeliveryPlan{}, fmt.Errorf("workspace and artifacts must be absolute paths")
	}
	canonicalWorkspace, err := existingDirectory(workspace)
	if err != nil {
		return DeliveryPlan{}, fmt.Errorf("resolve workspace: %w", err)
	}
	canonicalArtifacts, err := existingDirectory(artifacts)
	if err != nil {
		return DeliveryPlan{}, fmt.Errorf("resolve artifacts: %w", err)
	}
	overlapped, err := directoriesOverlap(canonicalWorkspace, canonicalArtifacts)
	if err != nil {
		return DeliveryPlan{}, fmt.Errorf("compare workspace and artifacts: %w", err)
	}
	if overlapped {
		return DeliveryPlan{}, fmt.Errorf("workspace and artifacts must not overlap")
	}
	if err := requireEmptyDirectory(canonicalArtifacts); err != nil {
		return DeliveryPlan{}, fmt.Errorf("artifacts directory: %w", err)
	}
	return DeliveryPlan{
		RunID:   task.RunID,
		IssueID: task.IssueID,
		Artifacts: []ArtifactSpec{
			{
				Path:            filepath.Join(canonicalArtifacts, "implementation-outcome.schema.json"),
				Content:         controllercontracts.ImplementationOutcomeSchema,
				Mode:            0o600,
				CreateExclusive: true,
			},
			{
				Path:            filepath.Join(canonicalArtifacts, "review-outcome.schema.json"),
				Content:         controllercontracts.ReviewOutcomeSchema,
				Mode:            0o600,
				CreateExclusive: true,
			},
		},
		Implementation: p.commands.Implementation(task, canonicalWorkspace, canonicalArtifacts),
		FreshReview:    p.commands.FreshReview(task, canonicalWorkspace, canonicalArtifacts),
	}, nil
}

func requireEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("must be a new empty attempt directory")
	}
	return nil
}

func existingDirectory(path string) (string, error) {
	if hasParentTraversal(path) {
		return "", errors.New("parent traversal path components are not allowed")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", path)
	}
	return resolved, nil
}

func hasParentTraversal(path string) bool {
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return true
		}
	}
	return false
}

func directoriesOverlap(first, second string) (bool, error) {
	firstContainsSecond, err := sameOrAncestor(first, second)
	if err != nil || firstContainsSecond {
		return firstContainsSecond, err
	}
	return sameOrAncestor(second, first)
}

func sameOrAncestor(parent, child string) (bool, error) {
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return false, err
	}
	current := child
	for {
		currentInfo, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		if os.SameFile(parentInfo, currentInfo) {
			return true, nil
		}
		next := filepath.Dir(current)
		if next == current {
			return false, nil
		}
		current = next
	}
}
