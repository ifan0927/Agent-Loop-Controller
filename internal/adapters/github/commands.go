package github

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type Command struct {
	Program string
	Args    []string
	Stdin   string
}

func CreatePRCommand(repository, head, base, title, body string) (Command, error) {
	if strings.TrimSpace(repository) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" {
		return Command{}, errors.New("repository, title, and body are required")
	}
	if err := domain.ValidateGitBranch(head); err != nil {
		return Command{}, err
	}
	if err := domain.ValidateGitBranch(base); err != nil {
		return Command{}, err
	}
	return Command{Program: "gh", Args: []string{"pr", "create", "--repo", repository, "--head", head, "--base", base, "--title", title, "--body-file", "-"}, Stdin: body}, nil
}

func SquashMergeCommand(repository string, number int64, expectedHead string) (Command, error) {
	if strings.TrimSpace(repository) == "" || number < 1 || strings.TrimSpace(expectedHead) == "" {
		return Command{}, errors.New("repository, PR number, and expected head are required")
	}
	return Command{Program: "gh", Args: []string{"pr", "merge", strconv.FormatInt(number, 10), "--repo", repository, "--squash", "--match-head-commit", expectedHead}}, nil
}

func DeleteRemoteBranchCommand(branch, expectedSHA string) (Command, error) {
	if err := domain.ValidateGitBranch(branch); err != nil {
		return Command{}, err
	}
	if branch == "main" || branch == "master" {
		return Command{}, fmt.Errorf("refusing to delete base branch %s", branch)
	}
	if strings.TrimSpace(expectedSHA) == "" {
		return Command{}, errors.New("expected remote branch SHA is required")
	}
	return Command{Program: "git", Args: []string{"push", "--force-with-lease=refs/heads/" + branch + ":" + expectedSHA, "origin", "--delete", "refs/heads/" + branch}}, nil
}
