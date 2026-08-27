package main

import (
	"strings"
	"testing"
)

func TestControllerIntegrityCommandsAreClosedAndAuthorizationFirst(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"integrity"}, want: "integrity <status|findings|recheck>"},
		{args: []string{"integrity", "unknown"}, want: "integrity <status|findings|recheck>"},
		{args: []string{"integrity", "status"}, want: "complete requester identity"},
		{args: []string{"integrity", "findings", "--family", "storage_schema"}, want: "complete requester identity"},
		{args: []string{"integrity", "recheck", "--request-id", "request-1"}, want: "complete requester identity"},
		{args: []string{"integrity", "recheck", "--force"}, want: "flag provided but not defined"},
		{args: []string{"integrity", "findings", "--sql", "select 1"}, want: "flag provided but not defined"},
	} {
		if err := controller(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("args=%v err=%v want=%q", test.args, err, test.want)
		}
	}
}
