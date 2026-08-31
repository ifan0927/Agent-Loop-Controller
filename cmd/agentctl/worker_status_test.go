package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

type controllableWorkerHeartbeatTicker struct {
	ticks chan time.Time
}

func (t *controllableWorkerHeartbeatTicker) C() <-chan time.Time { return t.ticks }
func (t *controllableWorkerHeartbeatTicker) Stop()               {}

func TestWorkerStatusSnapshotIsObservableWhileDispatchIsDriving(t *testing.T) {
	config := filepath.Join(resolvedTempDir(t), "controller.json")
	reporter, err := newWorkerStatusReporter(config, "worker-live", version, strings.Repeat("a", 64))
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
	reporter, err := newWorkerStatusReporter(config, "worker-one", version, strings.Repeat("a", 64))
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
	if err != nil || snapshot.SchemaVersion != application.WorkerHeartbeatSchemaVersion || snapshot.BuildIdentity != version || snapshot.ConfigurationDigest != strings.Repeat("a", 64) || snapshot.Status != workerStatusParked || snapshot.PreviousStatus != workerStatusDriving || snapshot.Cycles != 1 || snapshot.ObservedAt != now {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	raw, err := os.ReadFile(workerStatusPath(config))
	if err != nil || strings.Contains(string(raw), "generation") || strings.Contains(string(raw), "process_start_identity") {
		t.Fatalf("unsafe heartbeat schema raw=%s err=%v", raw, err)
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
	reporter, err := newWorkerStatusReporter(config, "worker-two", version, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Observe(admissionWorkerResult{Status: "unknown"}); err == nil {
		t.Fatal("unknown worker status was accepted")
	}
}

func TestWorkerStatusReporterAcceptsOnlyClosedOnboardingCycleOutcomes(t *testing.T) {
	for _, test := range []struct {
		outcome string
		status  string
	}{
		{outcome: application.RuntimeCycleOnboardingAccepted, status: workerStatusRunning},
		{outcome: application.RuntimeCycleOnboardingRunning, status: workerStatusRunning},
		{outcome: application.RuntimeCycleOnboardingWaitingForOperator, status: workerStatusParked},
		{outcome: application.RuntimeCycleOnboardingConflict, status: workerStatusParked},
		{outcome: application.RuntimeCycleOnboardingReadyDisabled, status: workerStatusRunning},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			config := filepath.Join(resolvedTempDir(t), "controller.json")
			reporter, err := newWorkerStatusReporter(config, "worker-"+test.outcome, version, strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			completedAt := time.Date(2026, 8, 31, 4, 0, 0, 0, time.UTC)
			reporter.now = func() time.Time { return completedAt }
			if err := reporter.Observe(admissionWorkerResult{Status: test.status, Cycles: 1, LastOutcome: test.outcome, LastCycleCompletedAt: completedAt}); err != nil {
				t.Fatal(err)
			}
			snapshot, state := readWorkerStatusEvidence(config, os.Getuid())
			if state != application.RuntimeHeartbeatCurrent || snapshot.LastCycleOutcome != test.outcome || snapshot.Status != test.status {
				t.Fatalf("snapshot=%+v state=%s", snapshot, state)
			}
		})
	}
	config := filepath.Join(resolvedTempDir(t), "controller.json")
	reporter, err := newWorkerStatusReporter(config, "worker-invalid-onboarding", version, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Observe(admissionWorkerResult{Status: workerStatusRunning, Cycles: 1, LastOutcome: "onboarding_opened", LastCycleCompletedAt: time.Now().UTC()}); err == nil {
		t.Fatal("non-dispatch onboarding outcome was accepted")
	}
}

func TestWorkerHeartbeatRemainsPeriodicForQuietAndParkedActivity(t *testing.T) {
	for _, test := range []struct {
		name     string
		outcome  string
		expected string
	}{
		{name: "quiet", outcome: application.LinearTodoDispatchNoCandidate, expected: workerStatusRunning},
		{name: "parked", outcome: application.LinearTodoDispatchAttention, expected: workerStatusParked},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := filepath.Join(resolvedTempDir(t), "controller.json")
			reporter, err := newWorkerStatusReporter(config, "worker-"+test.name, version, strings.Repeat("b", 64))
			if err != nil {
				t.Fatal(err)
			}
			base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
			var clock atomic.Int64
			clock.Store(base.UnixNano())
			reporter.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			ticker := &controllableWorkerHeartbeatTicker{ticks: make(chan time.Time, 1)}
			originalTicker := newWorkerHeartbeatTicker
			newWorkerHeartbeatTicker = func(interval time.Duration) workerHeartbeatTicker {
				if application.WorkerHeartbeatCadence != 15*time.Second {
					t.Fatalf("fixed heartbeat cadence=%s", application.WorkerHeartbeatCadence)
				}
				if interval != application.WorkerHeartbeatCadence {
					t.Fatalf("heartbeat interval=%s", interval)
				}
				return ticker
			}
			t.Cleanup(func() { newWorkerHeartbeatTicker = originalTicker })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, runErr := runBoundedAdmissionWorkerWithHeartbeat(ctx, false, time.Hour, 1, func(context.Context) (application.LinearTodoDispatchResult, error) {
					return application.LinearTodoDispatchResult{Outcome: test.outcome}, nil
				}, reporter, nil)
				done <- runErr
			}()
			var before workerStatusSnapshot
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				before, err = readWorkerStatusSnapshot(config)
				if err == nil && before.Status == test.expected && before.Cycles >= 1 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if err != nil || before.Status != test.expected || before.Cycles < 1 {
				t.Fatalf("before=%+v err=%v", before, err)
			}
			later := base.Add(2 * time.Hour)
			clock.Store(later.UnixNano())
			ticker.ticks <- later
			var after workerStatusSnapshot
			deadline = time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				after, err = readWorkerStatusSnapshot(config)
				if err == nil && after.ObservedAt.Equal(later) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if err != nil || after.Status != test.expected || !after.ObservedAt.Equal(later) || after.WorkerInstanceID != before.WorkerInstanceID {
				t.Fatalf("before=%+v after=%+v err=%v", before, after, err)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("heartbeat worker did not stop")
			}
		})
	}
}

func TestWorkerHeartbeatFailureCancelsAndJoinsActiveDispatch(t *testing.T) {
	config := filepath.Join(resolvedTempDir(t), "controller.json")
	reporter, err := newWorkerStatusReporter(config, "worker-failure", version, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	originalPublish := reporter.publish
	var fail atomic.Bool
	reporter.publish = func(snapshot workerStatusSnapshot) error {
		if fail.Load() {
			return errors.New("raw private write failure")
		}
		return originalPublish(snapshot)
	}
	ticker := &controllableWorkerHeartbeatTicker{ticks: make(chan time.Time, 1)}
	originalTicker := newWorkerHeartbeatTicker
	newWorkerHeartbeatTicker = func(time.Duration) workerHeartbeatTicker { return ticker }
	t.Cleanup(func() { newWorkerHeartbeatTicker = originalTicker })
	dispatchStarted := make(chan struct{})
	dispatchJoined := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, runErr := runBoundedAdmissionWorkerWithHeartbeat(context.Background(), false, time.Hour, 1, func(ctx context.Context) (application.LinearTodoDispatchResult, error) {
			close(dispatchStarted)
			<-ctx.Done()
			close(dispatchJoined)
			return application.LinearTodoDispatchResult{}, ctx.Err()
		}, reporter, nil)
		done <- runErr
	}()
	select {
	case <-dispatchStarted:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not start")
	}
	fail.Store(true)
	ticker.ticks <- time.Now().UTC()
	select {
	case err := <-done:
		if err == nil || err.Error() != "worker heartbeat publication failed" {
			t.Fatalf("unexpected worker failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat failure did not fail worker closed")
	}
	select {
	case <-dispatchJoined:
	default:
		t.Fatal("heartbeat failure returned before active dispatch joined")
	}
}

func TestWorkerStatusReaderRecognizesLegacyWithoutPromotingIt(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	legacy := `{"schema_version":1,"worker_instance_id":"legacy-worker","process_id":1,"process_start_id":"1","status":"parked","cycles":3,"observed_at":"2026-08-13T12:00:00Z"}`
	if err := os.WriteFile(workerStatusPath(config), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, state := readWorkerStatusEvidence(config, os.Getuid())
	if state != application.RuntimeHeartbeatLegacy || snapshot.SchemaVersion != application.WorkerHeartbeatLegacySchemaVersion || snapshot.Status != workerStatusParked {
		t.Fatalf("snapshot=%+v state=%s", snapshot, state)
	}
}

func TestWorkerStatusReaderRejectsUnsafeOrContradictoryCurrentEvidence(t *testing.T) {
	valid := workerStatusSnapshot{
		SchemaVersion:       application.WorkerHeartbeatSchemaVersion,
		WorkerInstanceID:    "worker-safe",
		ProcessID:           os.Getpid(),
		ProcessStartID:      "1:2",
		BuildIdentity:       version,
		ConfigurationDigest: strings.Repeat("e", 64),
		Status:              workerStatusRunning,
		Cycles:              1,
		ObservedAt:          time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name   string
		mutate func(*workerStatusSnapshot)
		extra  bool
	}{
		{name: "missing build", mutate: func(snapshot *workerStatusSnapshot) { snapshot.BuildIdentity = "" }},
		{name: "missing digest", mutate: func(snapshot *workerStatusSnapshot) { snapshot.ConfigurationDigest = "" }},
		{name: "unknown schema", mutate: func(snapshot *workerStatusSnapshot) { snapshot.SchemaVersion++ }},
		{name: "unknown onboarding outcome", mutate: func(snapshot *workerStatusSnapshot) {
			snapshot.LastCycleOutcome = "onboarding_unknown"
			snapshot.LastCycleCompletedAt = snapshot.ObservedAt
		}},
		{name: "schema v2 carrying onboarding cadence", mutate: func(snapshot *workerStatusSnapshot) {
			snapshot.SchemaVersion = application.WorkerHeartbeatPreviousSchemaVersion
			snapshot.LastCycleOutcome = application.RuntimeCycleOnboardingRunning
			snapshot.LastCycleCompletedAt = snapshot.ObservedAt
		}},
		{name: "legacy carrying current authority", mutate: func(snapshot *workerStatusSnapshot) {
			snapshot.SchemaVersion = application.WorkerHeartbeatLegacySchemaVersion
		}},
		{name: "unknown field", extra: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := resolvedTempDir(t)
			config := filepath.Join(root, "controller.json")
			snapshot := valid
			if test.mutate != nil {
				test.mutate(&snapshot)
			}
			raw, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if test.extra {
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"unexpected":true}`)
			}
			if err := os.WriteFile(workerStatusPath(config), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, state := readWorkerStatusEvidence(config, os.Getuid()); state != application.RuntimeHeartbeatInvalid {
				t.Fatalf("state=%s raw=%s", state, raw)
			}
		})
	}
	for _, test := range []struct {
		name string
		mode os.FileMode
		link bool
	}{
		{name: "public mode", mode: 0o644},
		{name: "multiple links", mode: 0o600, link: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := resolvedTempDir(t)
			config := filepath.Join(root, "controller.json")
			raw, _ := json.Marshal(valid)
			if err := os.WriteFile(workerStatusPath(config), raw, test.mode); err != nil {
				t.Fatal(err)
			}
			if test.link {
				if err := os.Link(workerStatusPath(config), filepath.Join(root, "linked-heartbeat")); err != nil {
					t.Fatal(err)
				}
			}
			if _, state := readWorkerStatusEvidence(config, os.Getuid()); state != application.RuntimeHeartbeatUnavailable {
				t.Fatalf("state=%s", state)
			}
		})
	}
}

func TestWorkerHeartbeatTemporaryLeafMustBeExclusive(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	reporter, err := newWorkerStatusReporter(config, "exclusive-worker", version, strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	temporary := workerStatusPath(config) + ".exclusive-worker.tmp"
	if err := os.WriteFile(temporary, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Observe(admissionWorkerResult{Status: workerStatusRunning}); err == nil || err.Error() != "worker heartbeat publication failed" {
		t.Fatalf("unexpected publication error: %v", err)
	}
	if _, state := readWorkerStatusEvidence(config, os.Getuid()); state != application.RuntimeHeartbeatAbsent {
		t.Fatalf("canonical heartbeat state=%s", state)
	}
}

func TestWorkerStartupCallbackFollowsInitialHeartbeatPublication(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	reporter, err := newWorkerStatusReporter(config, "startup-worker", version, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	started := false
	_, err = runBoundedAdmissionWorkerWithHeartbeat(context.Background(), true, time.Minute, 1, func(context.Context) (application.LinearTodoDispatchResult, error) {
		return application.LinearTodoDispatchResult{Outcome: application.LinearTodoDispatchNoCandidate}, nil
	}, reporter, func() {
		snapshot, readErr := readWorkerStatusSnapshot(config)
		if readErr != nil || snapshot.SchemaVersion != application.WorkerHeartbeatSchemaVersion || snapshot.Status != workerStatusRunning {
			t.Fatalf("startup callback preceded heartbeat snapshot=%+v err=%v", snapshot, readErr)
		}
		started = true
	})
	if err != nil || !started {
		t.Fatalf("started=%t err=%v", started, err)
	}

	failingConfig := filepath.Join(root, "failing-controller.json")
	failing, err := newWorkerStatusReporter(failingConfig, "failing-startup", version, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	failing.publish = func(workerStatusSnapshot) error { return errors.New("write failed") }
	started = false
	_, err = runBoundedAdmissionWorkerWithHeartbeat(context.Background(), true, time.Minute, 1, func(context.Context) (application.LinearTodoDispatchResult, error) {
		t.Fatal("dispatch started without initial heartbeat")
		return application.LinearTodoDispatchResult{}, nil
	}, failing, func() { started = true })
	if err == nil || started {
		t.Fatalf("failed heartbeat started=%t err=%v", started, err)
	}
}

func TestForegroundWorkerHeartbeatUsesAuthorizedRuntimeObservation(t *testing.T) {
	root := resolvedTempDir(t)
	config := filepath.Join(root, "controller.json")
	reporter, err := newWorkerStatusReporter(config, "foreground-worker", version, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	reporter.now = func() time.Time { return now }
	if err := reporter.Observe(admissionWorkerResult{Status: workerStatusParked, PreviousStatus: workerStatusDriving, Cycles: 5}); err != nil {
		t.Fatal(err)
	}
	operator := domain.GitHubUserIdentity{Login: "operator", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: operator})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewRuntimeObservationService(workerHeartbeatReader{configPath: config, expectedUID: os.Getuid()}, workerProcessIdentityObserver{}, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	observation, err := service.Observe(context.Background(), requester, now.Add(application.WorkerHeartbeatStaleAfter))
	if err != nil || observation.Liveness != application.RuntimeLivenessFresh || observation.Activity != application.RuntimeActivityParked || observation.WorkerInstanceID != "foreground-worker" || observation.BuildIdentity != version || observation.LoadedConfigurationDigest != strings.Repeat("a", 64) {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
}
