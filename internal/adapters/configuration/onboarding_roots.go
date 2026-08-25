package configuration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	Digest       string `json:"digest"`
}

func EnsureOnboardingRoots(repositoryRoot, runRoot, worktreeRoot, onboardingID, repository string) (OnboardingRootEvidence, error) {
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
	digest := applicationDigest("onboarding-roots-v1", onboardingID, repository, repositoryRoot, runRoot, worktreeRoot)
	marker := onboardingRootMarker{Version: 1, OnboardingID: onboardingID, Repository: repository, RunRoot: runRoot, WorktreeRoot: worktreeRoot, Digest: digest}
	payload, _ := json.Marshal(marker)
	payload = append(payload, '\n')
	path := filepath.Join(repositoryRoot, ".agentctl-onboarding-owner.json")
	if err := atomicPrivateWrite(path, payload, os.Getuid(), true, nil); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return OnboardingRootEvidence{}, err
		}
		existing, readErr := readPrivateRegular(path, os.Getuid(), 16<<10, true)
		if readErr != nil || !bytes.Equal(existing, payload) {
			return OnboardingRootEvidence{}, errors.New("onboarding root ownership conflicts")
		}
	}
	return OnboardingRootEvidence{RepositoryRoot: repositoryRoot, RunRoot: runRoot, WorktreeRoot: worktreeRoot, EvidenceDigest: digest}, nil
}

func applicationDigest(parts ...string) string {
	return application.ConfigurationEvidenceDigest(parts...)
}
