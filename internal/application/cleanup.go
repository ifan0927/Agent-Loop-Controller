package application

import (
	"context"
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
	for _, resource := range resources {
		if resource.RunID != run.ID || resource.Status == "deleted" {
			continue
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
