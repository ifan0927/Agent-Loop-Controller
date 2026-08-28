package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ifan0927/Agent-Loop-Controller/internal/localupgrade"
)

type fakeUpgradeManager struct {
	successorRequest localupgrade.SuccessorPrepareRequest
	previewRequest   localupgrade.SuccessorRecoveryPreviewRequest
	recoveryRequest  localupgrade.SuccessorRecoverPrepareRequest
	replaceID        string
	replaceConfirmed bool
}

func (f *fakeUpgradeManager) PreviewSuccessorRecovery(_ context.Context, request localupgrade.SuccessorRecoveryPreviewRequest) (localupgrade.SuccessorRecoveryPreview, error) {
	f.previewRequest = request
	return localupgrade.SuccessorRecoveryPreview{UpgradeID: request.PredecessorUpgradeID, State: "eligible", Reason: "authorized_database_relocation_verified", SuccessorRevision: request.Revision, PreviewDigest: strings.Repeat("b", 64), RequiredConfirmations: []string{"database_relocation_confirmed", "full_backup_confirmed"}}, nil
}

func (f *fakeUpgradeManager) RecoverPrepareSuccessor(_ context.Context, request localupgrade.SuccessorRecoverPrepareRequest) (localupgrade.Result, error) {
	f.recoveryRequest = request
	return localupgrade.Result{UpgradeID: "upgrade-22222222222222222222222222222222", State: "prepared", Reason: "verified_recovered_successor_activated", NextAction: "replace", UpgradeHealth: "pending", ControllerReadiness: "unknown", PredecessorUpgradeID: request.PredecessorUpgradeID}, nil
}

func (f *fakeUpgradeManager) Prepare(context.Context, localupgrade.PrepareRequest) (localupgrade.Result, error) {
	return localupgrade.Result{}, errors.New("unexpected prepare")
}

func (f *fakeUpgradeManager) PrepareSuccessor(_ context.Context, request localupgrade.SuccessorPrepareRequest) (localupgrade.Result, error) {
	f.successorRequest = request
	return localupgrade.Result{UpgradeID: "upgrade-22222222222222222222222222222222", State: "prepared", Reason: "verified_successor_activated", NextAction: "bootout_selected_supervisor", UpgradeHealth: "pending", ControllerReadiness: "unknown", PredecessorUpgradeID: request.PredecessorUpgradeID}, nil
}

func (f *fakeUpgradeManager) Status(context.Context, string) (localupgrade.Result, error) {
	return localupgrade.Result{}, errors.New("unexpected status")
}

func (f *fakeUpgradeManager) Replace(_ context.Context, id string, confirmed bool) (localupgrade.Result, error) {
	f.replaceID, f.replaceConfirmed = id, confirmed
	return localupgrade.Result{UpgradeID: id, State: "replaced", Reason: "candidate_installed", NextAction: "authorize-bootstrap", UpgradeHealth: "pending", ControllerReadiness: "unknown"}, nil
}

func (f *fakeUpgradeManager) Rollback(context.Context, string) (localupgrade.Result, error) {
	return localupgrade.Result{}, errors.New("unexpected rollback")
}

func (f *fakeUpgradeManager) AuthorizeBootstrap(context.Context, string) (localupgrade.Result, error) {
	return localupgrade.Result{}, errors.New("unexpected authorize-bootstrap")
}

func (f *fakeUpgradeManager) Observe(context.Context, string) (localupgrade.Result, error) {
	return localupgrade.Result{}, errors.New("unexpected observe")
}

func (f *fakeUpgradeManager) Cleanup(context.Context, string) (localupgrade.Result, error) {
	return localupgrade.Result{}, errors.New("unexpected cleanup")
}

func TestSuccessorPrepareCLIRequiresOnlyBoundIdentifiersAndEmitsSanitizedJSON(t *testing.T) {
	manager := &fakeUpgradeManager{}
	predecessor := "upgrade-11111111111111111111111111111111"
	revision := strings.Repeat("a", 40)
	var output bytes.Buffer
	if err := runWithManager(context.Background(), []string{"successor-prepare", "--upgrade-id", predecessor, "--revision", revision}, manager, &output); err != nil {
		t.Fatal(err)
	}
	if manager.successorRequest.PredecessorUpgradeID != predecessor || manager.successorRequest.Revision != revision {
		t.Fatalf("request=%+v", manager.successorRequest)
	}
	text := output.String()
	if !strings.Contains(text, `"predecessor_upgrade_id":"`+predecessor+`"`) || strings.Contains(text, "/Users/") || strings.Contains(text, "controller.json") {
		t.Fatalf("output=%s", text)
	}
}

func TestSuccessorPrepareCLIRejectsMissingOrExpandedArguments(t *testing.T) {
	for _, args := range [][]string{
		{"successor-prepare", "--upgrade-id", "upgrade-11111111111111111111111111111111"},
		{"successor-prepare", "--revision", strings.Repeat("a", 40)},
		{"successor-prepare", "--upgrade-id", "upgrade-11111111111111111111111111111111", "--revision", strings.Repeat("a", 40), "--binary", "/private/path"},
	} {
		manager := &fakeUpgradeManager{}
		if err := runWithManager(context.Background(), args, manager, &bytes.Buffer{}); err == nil {
			t.Fatalf("args=%v", args)
		}
		if manager.successorRequest != (localupgrade.SuccessorPrepareRequest{}) {
			t.Fatalf("invalid arguments reached manager: %+v", manager.successorRequest)
		}
	}
}

func TestSuccessorRecoveryCLIRequiresDigestAndBothConfirmations(t *testing.T) {
	predecessor := "upgrade-11111111111111111111111111111111"
	revision := strings.Repeat("a", 40)
	digest := strings.Repeat("b", 64)
	manager := &fakeUpgradeManager{}
	var previewOutput bytes.Buffer
	if err := runWithManager(context.Background(), []string{"successor-recovery-preview", "--upgrade-id", predecessor, "--revision", revision}, manager, &previewOutput); err != nil {
		t.Fatal(err)
	}
	if manager.previewRequest.PredecessorUpgradeID != predecessor || manager.previewRequest.Revision != revision || !strings.Contains(previewOutput.String(), `"preview_digest":"`+digest+`"`) || strings.Contains(previewOutput.String(), "/private/") {
		t.Fatalf("request=%+v output=%s", manager.previewRequest, previewOutput.String())
	}
	for _, args := range [][]string{
		{"successor-recover-prepare", "--upgrade-id", predecessor, "--revision", revision, "--preview-digest", digest, "--full-backup-confirmed"},
		{"successor-recover-prepare", "--upgrade-id", predecessor, "--revision", revision, "--preview-digest", digest, "--database-relocation-confirmed"},
		{"successor-recover-prepare", "--upgrade-id", predecessor, "--revision", revision, "--preview-digest", digest, "--database-relocation-confirmed", "--full-backup-confirmed", "--database", "/private/controller.db"},
	} {
		candidate := &fakeUpgradeManager{}
		if err := runWithManager(context.Background(), args, candidate, &bytes.Buffer{}); err == nil {
			t.Fatalf("args=%v", args)
		}
		if candidate.recoveryRequest != (localupgrade.SuccessorRecoverPrepareRequest{}) {
			t.Fatalf("invalid arguments reached manager: %+v", candidate.recoveryRequest)
		}
	}
	var output bytes.Buffer
	args := []string{"successor-recover-prepare", "--upgrade-id", predecessor, "--revision", revision, "--preview-digest", digest, "--database-relocation-confirmed", "--full-backup-confirmed"}
	if err := runWithManager(context.Background(), args, manager, &output); err != nil {
		t.Fatal(err)
	}
	if manager.recoveryRequest.PredecessorUpgradeID != predecessor || manager.recoveryRequest.Revision != revision || manager.recoveryRequest.PreviewDigest != digest || !manager.recoveryRequest.DatabaseRelocationConfirmed || !manager.recoveryRequest.FullBackupConfirmed {
		t.Fatalf("request=%+v", manager.recoveryRequest)
	}
	if strings.Contains(output.String(), "/private/") || !strings.Contains(output.String(), `"reason":"verified_recovered_successor_activated"`) {
		t.Fatalf("output=%s", output.String())
	}
}

func TestReplaceCLIRequiresFreshBackupConfirmation(t *testing.T) {
	manager := &fakeUpgradeManager{}
	id := "upgrade-22222222222222222222222222222222"
	if err := runWithManager(context.Background(), []string{"replace", "--upgrade-id", id}, manager, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "full-backup-confirmed") {
		t.Fatalf("replace err=%v", err)
	}
	if manager.replaceID != "" {
		t.Fatal("replacement without confirmation reached manager")
	}
	if err := runWithManager(context.Background(), []string{"replace", "--upgrade-id", id, "--full-backup-confirmed"}, manager, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if manager.replaceID != id || !manager.replaceConfirmed {
		t.Fatalf("id=%q confirmed=%t", manager.replaceID, manager.replaceConfirmed)
	}
}
