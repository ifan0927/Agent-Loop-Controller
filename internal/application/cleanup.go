package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type CleanupPort interface {
	RemoveWorktree(context.Context, string, string) error
	DeleteLocalBranch(context.Context, string, string) error
	DeleteRemoteBranch(context.Context, string, string, string) error
}

func CleanupOwned(ctx context.Context, store DeliveryStore, port CleanupPort, run Run, merge MergeRecord, resources []OwnedResource) error {
	if merge.RunID != run.ID || merge.PreMergeSHA != run.CandidateHead || merge.Method != "squash" || merge.MergeSHA == "" {
		return errors.New("cleanup requires persisted squash-merge evidence for the exact candidate")
	}
	var partial []error
	progress, err := store.CleanupProgress(ctx, run.ID)
	if err != nil {
		return err
	}
	deleted := map[string]bool{}
	for _, item := range progress {
		if item.Status == "deleted" {
			deleted[item.Kind+"\x00"+item.Name] = true
		}
	}
	for _, resource := range resources {
		if resource.RunID != run.ID || resource.Status == "deleted" {
			continue
		}
		if resource.Status != "owned" {
			return fmt.Errorf("cleanup resource %s is not durably owned", resource.Name)
		}
		if deleted[resource.Kind+"\x00"+resource.Name] {
			continue
		}
		if err := validateCleanupEvidence(run, resource); err != nil {
			return err
		}
		result := CleanupRecord{RunID: run.ID, Kind: resource.Kind, Name: resource.Name, Status: "intent"}
		if err := store.UpsertCleanup(ctx, result); err != nil {
			return err
		}
		var err error
		switch resource.Kind {
		case "worktree":
			err = port.RemoveWorktree(ctx, run.Repository, resource.Name)
		case "local_branch":
			if resource.Name == run.BaseBranch {
				err = errors.New("refusing to delete base branch")
			} else if resource.Name != run.WorkingBranch {
				err = errors.New("local branch ownership mismatch")
			} else {
				err = port.DeleteLocalBranch(ctx, run.Repository, resource.Name)
			}
		case "remote_branch":
			if resource.Name == run.BaseBranch {
				err = errors.New("refusing to delete base branch")
			} else if resource.Name != run.WorkingBranch {
				err = errors.New("remote branch ownership mismatch")
			} else {
				err = port.DeleteRemoteBranch(ctx, run.Repository, resource.Name, run.CandidateHead)
			}
		default:
			err = fmt.Errorf("unsupported cleanup resource kind %s", resource.Kind)
		}
		if err != nil {
			result.Status = "failed"
			result.LastError = err.Error()
			partial = append(partial, err)
		} else {
			result.Status = "deleted"
		}
		if saveErr := store.UpsertCleanup(ctx, result); saveErr != nil {
			return saveErr
		}
	}
	return errors.Join(partial...)
}

func validateCleanupEvidence(run Run, resource OwnedResource) error {
	var evidence struct {
		Path       string `json:"path"`
		Branch     string `json:"branch"`
		BaseBranch string `json:"base_branch"`
		BaseSHA    string `json:"base_sha"`
	}
	if err := json.Unmarshal([]byte(resource.CreationEvidence), &evidence); err != nil {
		return fmt.Errorf("decode cleanup ownership evidence: %w", err)
	}
	switch resource.Kind {
	case "worktree":
		if resource.Name != run.WorktreePath || evidence.Path != run.WorktreePath || evidence.Branch != run.WorkingBranch || evidence.BaseBranch != run.BaseBranch || evidence.BaseSHA != run.BaseSHA {
			return errors.New("worktree cleanup ownership evidence mismatch")
		}
	case "local_branch", "remote_branch":
		if resource.Name != run.WorkingBranch || evidence.Branch != run.WorkingBranch || evidence.BaseBranch != run.BaseBranch || evidence.BaseSHA != run.BaseSHA {
			return errors.New("branch cleanup ownership evidence mismatch")
		}
	default:
		return fmt.Errorf("unsupported cleanup resource kind %s", resource.Kind)
	}
	return nil
}
