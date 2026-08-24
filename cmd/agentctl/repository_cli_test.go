package main

import (
	"strings"
	"testing"
)

func TestRepositoryCommandRequiresClosedSubcommand(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}} {
		if err := repositoryCommand(args); err == nil || !strings.Contains(err.Error(), "repository <list|inspect|recheck|enable|disable|remove>") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestRepositoryRemovalRequiresExplicitDraftLifecycleAndHasNoBypass(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"remove"}, want: "remove <open|show|validate|preview|apply|discard>"},
		{args: []string{"remove", "open", "owner/repo"}, want: "complete requester identity"},
		{args: []string{"remove", "show"}, want: "draft ID, revision 1"},
		{args: []string{"remove", "validate"}, want: "draft ID, revision 1"},
		{args: []string{"remove", "preview"}, want: "draft ID, revision 1"},
		{args: []string{"remove", "apply"}, want: "complete removal draft"},
		{args: []string{"remove", "discard"}, want: "draft ID, revision 1"},
		{args: []string{"remove", "apply", "--force"}, want: "flag provided but not defined"},
	}
	for _, test := range tests {
		if err := repositoryCommand(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("args=%v err=%v want=%q", test.args, err, test.want)
		}
	}
}

func TestRepositoryCommandsRejectIncompleteAuthorityBeforeOpeningConfiguration(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"list"}, want: "complete requester identity"},
		{args: []string{"inspect", "owner/repo"}, want: "complete requester identity"},
		{args: []string{"recheck", "owner/repo"}, want: "--request-id"},
		{args: []string{"enable", "owner/repo", "--request-id", "request-1"}, want: "complete requester identity"},
		{args: []string{"disable", "owner/repo", "--request-id", "request-1"}, want: "complete requester identity"},
	}
	for _, test := range tests {
		if err := repositoryCommand(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("args=%v err=%v want=%q", test.args, err, test.want)
		}
	}
}
