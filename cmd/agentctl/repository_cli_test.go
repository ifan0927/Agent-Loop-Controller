package main

import (
	"strings"
	"testing"
)

func TestRepositoryCommandRequiresClosedSubcommand(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}} {
		if err := repositoryCommand(args); err == nil || !strings.Contains(err.Error(), "repository <list|inspect|recheck|enable|disable>") {
			t.Fatalf("args=%v err=%v", args, err)
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
