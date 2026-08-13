package main

import (
	"strings"
	"testing"
)

func TestControllerRecoverCIWaitRequiresRunAndCompleteRequesterOnly(t *testing.T) {
	if err := controllerRecoverCIWait(nil); err == nil || !strings.Contains(err.Error(), "run ID and complete requester identity") {
		t.Fatalf("missing authority err=%v", err)
	}
	identity := []string{"run-1", "--requester", "operator", "--requester-database-id", "33", "--requester-node-id", "MDQ6VXNlcjMz", "--requester-type", "User", "--expected-head", "caller-head"}
	if err := controllerRecoverCIWait(identity); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("caller GitHub authority was accepted: %v", err)
	}
}
