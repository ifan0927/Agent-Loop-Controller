package configuration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

type OnboardingRootEvidence struct {
	RepositoryRoot string
	RunRoot        string
	WorktreeRoot   string
	EvidenceDigest string
}

type onboardingRootMarker struct {
	Version      int    `json:"version"`
	OnboardingID string `json:"onboarding_id"`
	Repository   string `json:"repository"`
	RunRoot      string `json:"run_root"`
	WorktreeRoot string `json:"worktree_root"`
	SourcePath   string `json:"source_path,omitempty"`
	Digest       string `json:"digest"`
}

func EnsureEmptyOnboardingRoots(repositoryRoot, sourcePath, runRoot, worktreeRoot, onboardingID, repository string) (OnboardingRootEvidence, error) {
	if filepath.Dir(sourcePath) != repositoryRoot {
		return OnboardingRootEvidence{}, errors.New("managed source authority is invalid")
	}
	evidence, err := ensureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, sourcePath, onboardingID, repository, 2)
	if err != nil {
		return OnboardingRootEvidence{}, err
	}
	marker := onboardingRootMarker{Version: 2, OnboardingID: onboardingID, Repository: repository, RunRoot: runRoot, WorktreeRoot: worktreeRoot, SourcePath: sourcePath, Digest: evidence.EvidenceDigest}
	if err := writeExactOnboardingMarker(filepath.Join(repositoryRoot, ".agentctl-managed-source-owner.json"), marker); err != nil {
		return OnboardingRootEvidence{}, err
	}
	return evidence, nil
}

func EnsureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, onboardingID, repository string) (OnboardingRootEvidence, error) {
	return ensureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, "", onboardingID, repository, 1)
}

func ensureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, sourcePath, onboardingID, repository string, version int) (OnboardingRootEvidence, error) {
	if !filepath.IsAbs(repositoryRoot) || filepath.Clean(repositoryRoot) != repositoryRoot || filepath.Dir(runRoot) != repositoryRoot || filepath.Dir(worktreeRoot) != repositoryRoot || runRoot == worktreeRoot || !validOnboardingPrivateID(onboardingID) || strings.Count(repository, "/") != 1 {
		return OnboardingRootEvidence{}, errors.New("onboarding root authority is invalid")
	}
	parent := filepath.Dir(repositoryRoot)
	if inspectPrivateDirectory(filepath.Dir(parent), os.Getuid(), false) != nil {
		return OnboardingRootEvidence{}, errors.New("onboarding root parent is unsafe")
	}
	if err := ensurePrivateDirectory(parent, os.Getuid()); err != nil {
		return OnboardingRootEvidence{}, errors.New("onboarding root parent is unsafe")
	}
	for _, directory := range []string{repositoryRoot, runRoot, worktreeRoot} {
		if err := ensurePrivateDirectory(directory, os.Getuid()); err != nil {
			return OnboardingRootEvidence{}, errors.New("onboarding root is unsafe")
		}
	}
	digest := applicationDigest("onboarding-roots-v"+strconv.Itoa(version), onboardingID, repository, repositoryRoot, sourcePath, runRoot, worktreeRoot)
	if version == 1 {
		// Preserve the exact evidence identity emitted before empty-repository
		// onboarding introduced a Controller-derived source path.
		digest = applicationDigest("onboarding-roots-v1", onboardingID, repository, repositoryRoot, runRoot, worktreeRoot)
	}
	marker := onboardingRootMarker{Version: version, OnboardingID: onboardingID, Repository: repository, RunRoot: runRoot, WorktreeRoot: worktreeRoot, SourcePath: sourcePath, Digest: digest}
	if err := writeExactOnboardingMarker(filepath.Join(repositoryRoot, ".agentctl-onboarding-owner.json"), marker); err != nil {
		return OnboardingRootEvidence{}, err
	}
	return OnboardingRootEvidence{RepositoryRoot: repositoryRoot, RunRoot: runRoot, WorktreeRoot: worktreeRoot, EvidenceDigest: digest}, nil
}

func VerifyEmptyOnboardingRoots(repositoryRoot, sourcePath, runRoot, worktreeRoot, onboardingID, repository string) (OnboardingRootEvidence, error) {
	digest := applicationDigest("onboarding-roots-v2", onboardingID, repository, repositoryRoot, sourcePath, runRoot, worktreeRoot)
	marker := onboardingRootMarker{Version: 2, OnboardingID: onboardingID, Repository: repository, RunRoot: runRoot, WorktreeRoot: worktreeRoot, SourcePath: sourcePath, Digest: digest}
	for _, path := range []string{filepath.Join(repositoryRoot, ".agentctl-onboarding-owner.json"), filepath.Join(repositoryRoot, ".agentctl-managed-source-owner.json")} {
		payload, err := readPrivateRegular(path, os.Getuid(), 16<<10, true)
		expected, _ := json.Marshal(marker)
		expected = append(expected, '\n')
		if err != nil || !bytes.Equal(payload, expected) {
			return OnboardingRootEvidence{}, errors.New("managed source ownership conflicts")
		}
	}
	for _, directory := range []string{repositoryRoot, runRoot, worktreeRoot} {
		if inspectPrivateDirectory(directory, os.Getuid(), true) != nil {
			return OnboardingRootEvidence{}, errors.New("managed source ownership conflicts")
		}
	}
	return OnboardingRootEvidence{RepositoryRoot: repositoryRoot, RunRoot: runRoot, WorktreeRoot: worktreeRoot, EvidenceDigest: digest}, nil
}

func InspectEmptyOnboardingPaths(repositoryRoot, sourcePath, runRoot, worktreeRoot string, forbidden []string) error {
	if !filepath.IsAbs(repositoryRoot) || filepath.Dir(sourcePath) != repositoryRoot || filepath.Dir(runRoot) != repositoryRoot || filepath.Dir(worktreeRoot) != repositoryRoot {
		return errors.New("managed source path authority is invalid")
	}
	for _, candidate := range []string{repositoryRoot, sourcePath, runRoot, worktreeRoot} {
		if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
			return errors.New("managed source path already exists or is unavailable")
		}
		for _, excluded := range forbidden {
			if excluded != "" && onboardingPathsOverlap(candidate, filepath.Clean(excluded)) {
				return errors.New("managed source path overlaps existing authority")
			}
		}
	}
	parent := filepath.Dir(repositoryRoot)
	if _, err := os.Lstat(parent); err == nil {
		if inspectPrivateDirectory(parent, os.Getuid(), true) != nil {
			return errors.New("managed source parent is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) || inspectPrivateDirectory(filepath.Dir(parent), os.Getuid(), true) != nil {
		return errors.New("managed source parent is unsafe")
	}
	return nil
}

func onboardingPathsOverlap(left, right string) bool {
	rel, err := filepath.Rel(left, right)
	if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return true
	}
	rel, err = filepath.Rel(right, left)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func writeExactOnboardingMarker(path string, marker onboardingRootMarker) error {
	payload, _ := json.Marshal(marker)
	payload = append(payload, '\n')
	if err := atomicPrivateWrite(path, payload, os.Getuid(), true, nil); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := readPrivateRegular(path, os.Getuid(), 16<<10, true)
		if readErr != nil || !bytes.Equal(existing, payload) {
			return errors.New("onboarding root ownership conflicts")
		}
	}
	return nil
}

func applicationDigest(parts ...string) string {
	return application.ConfigurationEvidenceDigest(parts...)
}
