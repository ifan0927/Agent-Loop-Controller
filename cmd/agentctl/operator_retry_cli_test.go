package main

import (
	"strings"
	"testing"
)

func TestControllerRetryRequiresRunAndCompleteRequesterOnly(t *testing.T) {
	if err := controllerRetry(nil); err == nil || !strings.Contains(err.Error(), "run ID and complete requester identity") {
		t.Fatalf("missing authority err=%v", err)
	}
	identity := []string{"run-1", "--requester", "operator", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User", "--repository", "caller/repo"}
	if err := controllerRetry(identity); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("caller repository authority was accepted: %v", err)
	}
}
