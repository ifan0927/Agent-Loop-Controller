package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	workerStatusSchemaVersion = 1
	workerStatusMaxBytes      = 4 << 10
)

type workerStatusSnapshot struct {
	SchemaVersion    int       `json:"schema_version"`
	WorkerInstanceID string    `json:"worker_instance_id"`
	ProcessID        int       `json:"process_id"`
	ProcessStartID   string    `json:"process_start_id"`
	Status           string    `json:"status"`
	PreviousStatus   string    `json:"previous_status,omitempty"`
	Cycles           int       `json:"cycles"`
	ObservedAt       time.Time `json:"observed_at"`
}

type workerStatusReporter struct {
	path           string
	instanceID     string
	now            func() time.Time
	processID      int
	processStartID string
}

func newWorkerStatusReporter(configPath, instanceID string) (workerStatusReporter, error) {
	if !filepath.IsAbs(configPath) || strings.TrimSpace(instanceID) == "" {
		return workerStatusReporter{}, errors.New("worker status reporter authority is invalid")
	}
	pid := os.Getpid()
	started, err := processStartIdentity(pid)
	if err != nil {
		return workerStatusReporter{}, errors.New("worker process identity is unavailable")
	}
	return workerStatusReporter{path: workerStatusPath(configPath), instanceID: instanceID, processID: pid, processStartID: started, now: func() time.Time { return time.Now().UTC() }}, nil
}

func workerStatusPath(configPath string) string {
	return configPath + ".worker-status.json"
}

func (r workerStatusReporter) Observe(result admissionWorkerResult) error {
	snapshot := workerStatusSnapshot{SchemaVersion: workerStatusSchemaVersion, WorkerInstanceID: r.instanceID, ProcessID: r.processID, ProcessStartID: r.processStartID, Status: result.Status, PreviousStatus: result.PreviousStatus, Cycles: result.Cycles, ObservedAt: r.now().UTC()}
	if err := validateWorkerStatusSnapshot(snapshot); err != nil {
		return err
	}
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
	path := workerStatusPath(configPath)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByCurrentUser(info) || logLinkCount(info) != 1 || info.Size() < 1 || info.Size() > workerStatusMaxBytes {
		return workerStatusSnapshot{}, errors.New("worker status snapshot is unavailable")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return workerStatusSnapshot{}, errors.New("worker status snapshot is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || !ownedByCurrentUser(opened) || logLinkCount(opened) != 1 {
		return workerStatusSnapshot{}, errors.New("worker status snapshot is unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, workerStatusMaxBytes+1))
	if err != nil || len(raw) > workerStatusMaxBytes {
		return workerStatusSnapshot{}, errors.New("worker status snapshot is unavailable")
	}
	var snapshot workerStatusSnapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return workerStatusSnapshot{}, errors.New("worker status snapshot is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return workerStatusSnapshot{}, errors.New("worker status snapshot is invalid")
	}
	if err := validateWorkerStatusSnapshot(snapshot); err != nil {
		return workerStatusSnapshot{}, err
	}
	return snapshot, nil
}

func validateWorkerStatusSnapshot(snapshot workerStatusSnapshot) error {
	if snapshot.SchemaVersion != workerStatusSchemaVersion || strings.TrimSpace(snapshot.WorkerInstanceID) == "" || snapshot.ProcessID < 1 || !validProcessStartIdentity(snapshot.ProcessStartID) || snapshot.Cycles < 0 || snapshot.ObservedAt.IsZero() {
		return errors.New("worker status snapshot is invalid")
	}
	if !validWorkerStatus(snapshot.Status) || snapshot.PreviousStatus != "" && !validWorkerStatus(snapshot.PreviousStatus) {
		return errors.New("worker status snapshot is invalid")
	}
	return nil
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
