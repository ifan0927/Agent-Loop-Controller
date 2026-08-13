package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

func TestWorkerStatusSnapshotIsObservableWhileDispatchIsDriving(t *testing.T) {
	config := filepath.Join(resolvedTempDir(t), "controller.json")
	reporter, err := newWorkerStatusReporter(config, "worker-live")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driving := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, runErr := runAdmissionWorkerObserved(ctx, false, time.Minute, func(dispatchCtx context.Context) (application.LinearTodoDispatchResult, error) {
			close(driving)
			<-dispatchCtx.Done()
			return application.LinearTodoDispatchResult{}, dispatchCtx.Err()
		}, waitAdmissionWorker, reporter.Observe)
		done <- runErr
	}()
	select {
	case <-driving:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter driving state")
	}
	snapshot, err := readWorkerStatusSnapshot(config)
	if err != nil || snapshot.Status != workerStatusDriving || snapshot.PreviousStatus != workerStatusRunning {
		t.Fatalf("live snapshot=%+v err=%v", snapshot, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestCurrentProcessStartIdentityIsStable(t *testing.T) {
	first, err := processStartIdentity(os.Getpid())
	if err != nil || !validProcessStartIdentity(first) {
		t.Fatalf("first=%q err=%v", first, err)
	}
	second, err := processStartIdentity(os.Getpid())
	if err != nil || second != first {
		t.Fatalf("first=%q second=%q err=%v", first, second, err)
	}
}

func TestWorkerStatusReporterAtomicallyReplacesPrivateSnapshot(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	reporter, err := newWorkerStatusReporter(config, "worker-one")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reporter.now = func() time.Time { return now }
	if err := reporter.Observe(admissionWorkerResult{Status: workerStatusDriving, PreviousStatus: workerStatusRunning, Cycles: 1}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := reporter.Observe(admissionWorkerResult{Status: workerStatusParked, PreviousStatus: workerStatusDriving, Cycles: 1}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readWorkerStatusSnapshot(config)
	if err != nil || snapshot.Status != workerStatusParked || snapshot.PreviousStatus != workerStatusDriving || snapshot.Cycles != 1 || snapshot.ObservedAt != now {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	info, err := os.Lstat(workerStatusPath(config))
	if err != nil || info.Mode().Perm() != 0o600 || logLinkCount(info) != 1 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestWorkerStatusReaderRejectsSymlinkAndUnknownState(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, workerStatusPath(config)); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkerStatusSnapshot(config); err == nil {
		t.Fatal("symlink status snapshot was accepted")
	}
	if err := os.Remove(workerStatusPath(config)); err != nil {
		t.Fatal(err)
	}
	reporter, err := newWorkerStatusReporter(config, "worker-two")
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Observe(admissionWorkerResult{Status: "unknown"}); err == nil {
		t.Fatal("unknown worker status was accepted")
	}
}
