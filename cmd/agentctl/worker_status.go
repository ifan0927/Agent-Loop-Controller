package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/bootstrap"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const (
	workerStatusSchemaVersion = application.WorkerHeartbeatSchemaVersion
	workerStatusMaxBytes      = 4 << 10
)

type workerStatusSnapshot struct {
	SchemaVersion             int       `json:"schema_version"`
	WorkerInstanceID          string    `json:"worker_instance_id"`
	ProcessID                 int       `json:"process_id"`
	ProcessStartID            string    `json:"process_start_id"`
	BuildIdentity             string    `json:"build_identity,omitempty"`
	ConfigurationDigest       string    `json:"loaded_configuration_digest,omitempty"`
	Status                    string    `json:"status"`
	PreviousStatus            string    `json:"previous_status,omitempty"`
	Cycles                    int       `json:"cycles"`
	ObservedAt                time.Time `json:"observed_at"`
	LastCycleOutcome          string    `json:"last_cycle_outcome,omitempty"`
	LastQueueDecisionReason   string    `json:"last_queue_decision_reason,omitempty"`
	LastCycleCompletedAt      time.Time `json:"last_cycle_completed_at,omitempty"`
	NextAdmissionEvaluationAt time.Time `json:"next_admission_evaluation_at,omitempty"`
}

type workerStatusReporter struct {
	mu                  sync.Mutex
	path                string
	instanceID          string
	buildIdentity       string
	configurationDigest string
	now                 func() time.Time
	processID           int
	processStartID      string
	latest              admissionWorkerResult
	hasLatest           bool
	failed              error
	publish             func(workerStatusSnapshot) error
}

type manualControllerHeartbeat struct {
	cancel   context.CancelFunc
	stopped  chan struct{}
	failure  chan error
	stopOnce sync.Once
	stopErr  error
}

func startManualControllerHeartbeat(parent context.Context, configPath, configurationDigest string) (context.Context, *manualControllerHeartbeat, error) {
	reporter, err := newWorkerStatusReporter(configPath, "manual-"+uuid.NewString(), version, configurationDigest)
	if err != nil {
		return nil, nil, errors.New("manual controller heartbeat is unavailable")
	}
	ticker := newWorkerHeartbeatTicker(application.WorkerHeartbeatCadence)
	if ticker == nil || ticker.C() == nil {
		return nil, nil, errors.New("manual controller heartbeat is unavailable")
	}
	if err := reporter.Observe(admissionWorkerResult{Status: workerStatusRunning}); err != nil {
		ticker.Stop()
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	heartbeat := &manualControllerHeartbeat{cancel: cancel, stopped: make(chan struct{}), failure: make(chan error, 1)}
	go func() {
		defer close(heartbeat.stopped)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C():
				if err := reporter.Heartbeat(); err != nil {
					heartbeat.failure <- err
					cancel()
					return
				}
			}
		}
	}()
	return ctx, heartbeat, nil
}

func (h *manualControllerHeartbeat) Stop() error {
	if h == nil {
		return nil
	}
	h.stopOnce.Do(func() {
		h.cancel()
		<-h.stopped
		select {
		case h.stopErr = <-h.failure:
		default:
		}
	})
	return h.stopErr
}

func newWorkerStatusReporter(configPath, instanceID, buildIdentity, configurationDigest string) (*workerStatusReporter, error) {
	if !validLaunchAgentPath(configPath) || !validWorkerBuildIdentity(instanceID) || !validWorkerBuildIdentity(buildIdentity) || !validWorkerConfigurationDigest(configurationDigest) {
		return nil, errors.New("worker status reporter authority is invalid")
	}
	pid := os.Getpid()
	started, err := processStartIdentity(pid)
	if err != nil {
		return nil, errors.New("worker process identity is unavailable")
	}
	reporter := &workerStatusReporter{path: workerStatusPath(configPath), instanceID: instanceID, buildIdentity: buildIdentity, configurationDigest: configurationDigest, processID: pid, processStartID: started, now: func() time.Time { return time.Now().UTC() }}
	reporter.publish = reporter.writeSnapshot
	return reporter, nil
}

func workerStatusPath(configPath string) string {
	return configPath + ".worker-status.json"
}

func (r *workerStatusReporter) Observe(result admissionWorkerResult) error {
	if r == nil {
		return errors.New("worker heartbeat publisher is unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return r.failed
	}
	r.latest, r.hasLatest = result, true
	return r.publishLatestLocked()
}

func (r *workerStatusReporter) Heartbeat() error {
	if r == nil {
		return errors.New("worker heartbeat publisher is unavailable")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil {
		return r.failed
	}
	if !r.hasLatest {
		return errors.New("worker heartbeat activity is unavailable")
	}
	return r.publishLatestLocked()
}

func (r *workerStatusReporter) publishLatestLocked() error {
	queueReason := ""
	if r.latest.QueueDecision != nil {
		queueReason = r.latest.QueueDecision.Reason
	}
	snapshot := workerStatusSnapshot{SchemaVersion: workerStatusSchemaVersion, WorkerInstanceID: r.instanceID, ProcessID: r.processID, ProcessStartID: r.processStartID, BuildIdentity: r.buildIdentity, ConfigurationDigest: r.configurationDigest, Status: r.latest.Status, PreviousStatus: r.latest.PreviousStatus, Cycles: r.latest.Cycles, ObservedAt: r.now().UTC(), LastCycleOutcome: r.latest.LastOutcome, LastQueueDecisionReason: queueReason, LastCycleCompletedAt: r.latest.LastCycleCompletedAt, NextAdmissionEvaluationAt: r.latest.NextAdmissionEvaluationAt}
	if err := validateWorkerStatusSnapshot(snapshot); err != nil {
		return err
	}
	if err := r.publish(snapshot); err != nil {
		r.failed = errors.New("worker heartbeat publication failed")
		return r.failed
	}
	return nil
}

func (r *workerStatusReporter) writeSnapshot(snapshot workerStatusSnapshot) error {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temporary := r.path + "." + r.instanceID + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return errors.New("worker status snapshot could not be created")
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(raw); err != nil {
		return errors.New("worker status snapshot could not be written")
	}
	written, err := file.Stat()
	if err != nil || !written.Mode().IsRegular() || written.Mode().Perm() != 0o600 || !ownedByCurrentUser(written) || logLinkCount(written) != 1 || written.Size() != int64(len(raw)) || written.Size() > workerStatusMaxBytes {
		return errors.New("worker status snapshot is unsafe")
	}
	if err := file.Sync(); err != nil {
		return errors.New("worker status snapshot could not be synchronized")
	}
	if err := file.Close(); err != nil {
		return errors.New("worker status snapshot could not be closed")
	}
	if err := os.Rename(temporary, r.path); err != nil {
		return errors.New("worker status snapshot could not be published")
	}
	cleanup = false
	return nil
}

func readWorkerStatusSnapshot(configPath string) (workerStatusSnapshot, error) {
	snapshot, state := readWorkerStatusEvidence(configPath, os.Getuid())
	if state != application.RuntimeHeartbeatCurrent && state != application.RuntimeHeartbeatLegacy {
		return workerStatusSnapshot{}, errors.New("worker status snapshot is unavailable")
	}
	return snapshot, nil
}

func readWorkerStatusEvidence(configPath string, expectedUID int) (workerStatusSnapshot, application.RuntimeHeartbeatReadState) {
	if !validLaunchAgentPath(configPath) || expectedUID < 0 {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatUnavailable
	}
	path := workerStatusPath(configPath)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatAbsent
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByUID(info, expectedUID) || logLinkCount(info) != 1 || info.Size() < 1 || info.Size() > workerStatusMaxBytes {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatUnavailable
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatUnavailable
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || !ownedByUID(opened, expectedUID) || logLinkCount(opened) != 1 {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(file, workerStatusMaxBytes+1))
	if err != nil || len(raw) > workerStatusMaxBytes {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatUnavailable
	}
	var snapshot workerStatusSnapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatInvalid
	}
	if err := validateWorkerStatusSnapshot(snapshot); err != nil {
		return workerStatusSnapshot{}, application.RuntimeHeartbeatInvalid
	}
	if snapshot.SchemaVersion == application.WorkerHeartbeatLegacySchemaVersion {
		return snapshot, application.RuntimeHeartbeatLegacy
	}
	return snapshot, application.RuntimeHeartbeatCurrent
}

func validateWorkerStatusSnapshot(snapshot workerStatusSnapshot) error {
	if snapshot.SchemaVersion != workerStatusSchemaVersion && snapshot.SchemaVersion != application.WorkerHeartbeatPreviousSchemaVersion && snapshot.SchemaVersion != application.WorkerHeartbeatLegacySchemaVersion || strings.TrimSpace(snapshot.WorkerInstanceID) == "" || snapshot.ProcessID < 1 || !validProcessStartIdentity(snapshot.ProcessStartID) || snapshot.Cycles < 0 || snapshot.ObservedAt.IsZero() {
		return errors.New("worker status snapshot is invalid")
	}
	if !validWorkerStatus(snapshot.Status) || snapshot.PreviousStatus != "" && !validWorkerStatus(snapshot.PreviousStatus) {
		return errors.New("worker status snapshot is invalid")
	}
	if snapshot.SchemaVersion == application.WorkerHeartbeatLegacySchemaVersion {
		if snapshot.BuildIdentity != "" || snapshot.ConfigurationDigest != "" || hasWorkerCadence(snapshot) {
			return errors.New("worker status snapshot is invalid")
		}
		return nil
	}
	if !validWorkerBuildIdentity(snapshot.BuildIdentity) || !validWorkerConfigurationDigest(snapshot.ConfigurationDigest) {
		return errors.New("worker status snapshot is invalid")
	}
	if snapshot.SchemaVersion == application.WorkerHeartbeatPreviousSchemaVersion {
		if hasWorkerCadence(snapshot) {
			return errors.New("worker status snapshot is invalid")
		}
		return nil
	}
	if snapshot.LastCycleOutcome == "" {
		if snapshot.LastQueueDecisionReason != "" || !snapshot.LastCycleCompletedAt.IsZero() {
			return errors.New("worker status snapshot is invalid")
		}
		return nil
	}
	if snapshot.Cycles < 1 || !application.ValidRuntimeCycleOutcome(snapshot.LastCycleOutcome) || snapshot.LastQueueDecisionReason != "" && !application.ValidRuntimeQueueDecisionReason(snapshot.LastQueueDecisionReason) || snapshot.LastCycleCompletedAt.IsZero() {
		return errors.New("worker status snapshot is invalid")
	}
	return nil
}

func hasWorkerCadence(snapshot workerStatusSnapshot) bool {
	return snapshot.LastCycleOutcome != "" || snapshot.LastQueueDecisionReason != "" || !snapshot.LastCycleCompletedAt.IsZero() || !snapshot.NextAdmissionEvaluationAt.IsZero()
}

func validProcessStartIdentity(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character != ':' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validWorkerStatus(status string) bool {
	switch status {
	case workerStatusRunning, workerStatusParked, workerStatusDriving, workerStatusStopping:
		return true
	default:
		return false
	}
}

func validWorkerBuildIdentity(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_.:+", character) {
			continue
		}
		return false
	}
	return true
}

func validWorkerConfigurationDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

type workerHeartbeatReader struct {
	configPath                string
	expectedUID               int
	expectedProcessID         int
	supervisorProcessRequired bool
}

func (r workerHeartbeatReader) ReadRuntimeHeartbeat(ctx context.Context) (application.RuntimeHeartbeatEvidence, application.RuntimeHeartbeatReadState) {
	if ctx.Err() != nil {
		return application.RuntimeHeartbeatEvidence{}, application.RuntimeHeartbeatUnavailable
	}
	snapshot, state := readWorkerStatusEvidence(r.configPath, r.expectedUID)
	if state != application.RuntimeHeartbeatCurrent && state != application.RuntimeHeartbeatLegacy {
		return application.RuntimeHeartbeatEvidence{}, state
	}
	return application.RuntimeHeartbeatEvidence{
		SchemaVersion:                snapshot.SchemaVersion,
		WorkerInstanceID:             snapshot.WorkerInstanceID,
		ProcessID:                    snapshot.ProcessID,
		ProcessStartIdentity:         snapshot.ProcessStartID,
		BuildIdentity:                snapshot.BuildIdentity,
		LoadedConfigurationDigest:    snapshot.ConfigurationDigest,
		Activity:                     application.RuntimeActivity(snapshot.Status),
		PreviousActivity:             application.RuntimeActivity(snapshot.PreviousStatus),
		Cycles:                       snapshot.Cycles,
		LastCycleOutcome:             snapshot.LastCycleOutcome,
		LastQueueDecisionReason:      snapshot.LastQueueDecisionReason,
		LastCycleCompletedAt:         snapshot.LastCycleCompletedAt,
		NextAdmissionEvaluationAt:    snapshot.NextAdmissionEvaluationAt,
		ObservedAt:                   snapshot.ObservedAt,
		SupervisorProcessUnavailable: r.supervisorProcessRequired && r.expectedProcessID < 1,
		SupervisorProcessConflict:    r.supervisorProcessRequired && r.expectedProcessID > 0 && snapshot.ProcessID != r.expectedProcessID,
	}, state
}

type workerProcessIdentityObserver struct{}

var loadWorkerRuntimeConfiguration = bootstrap.Load

func (workerProcessIdentityObserver) ObserveRuntimeProcess(_ context.Context, pid int) application.RuntimeProcessObservation {
	identity, err := processStartIdentity(pid)
	if err == nil {
		return application.RuntimeProcessObservation{State: application.RuntimeProcessPresent, StartIdentity: identity}
	}
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return application.RuntimeProcessObservation{State: application.RuntimeProcessAbsent}
	}
	return application.RuntimeProcessObservation{State: application.RuntimeProcessUnavailable}
}

func observeConfiguredWorkerRuntime(ctx context.Context, configPath string, expectedUID, expectedProcessID int, now time.Time) (application.RuntimeObservation, error) {
	loaded, err := loadWorkerRuntimeConfiguration(configPath)
	if err != nil {
		return application.RuntimeObservation{}, err
	}
	authorizer, err := application.NewAuthorizationService(application.ConfiguredOperatorIdentity{User: loaded.Controller.Operator})
	if err != nil {
		return application.RuntimeObservation{}, err
	}
	service, err := application.NewRuntimeObservationService(workerHeartbeatReader{configPath: configPath, expectedUID: expectedUID, expectedProcessID: expectedProcessID, supervisorProcessRequired: true}, workerProcessIdentityObserver{}, authorizer)
	if err != nil {
		return application.RuntimeObservation{}, err
	}
	operator := loaded.Controller.Operator
	requester := application.Requester{ID: operator.Login, Kind: "github_login", DatabaseID: operator.DatabaseID, NodeID: operator.NodeID, ActorType: operator.ActorType}
	return service.Observe(ctx, requester, now.UTC())
}
