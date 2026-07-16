package process

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var errManagedLeaderAbsentGroupLive = errors.New("managed process leader is absent while its process group remains")

var managedAttemptControlFiles = []string{
	"codex-version.process-control.json",
	"codex-exec-help.process-control.json",
	"codex-exec-resume-help.process-control.json",
	"implementation.process-control.json",
	"review.process-control.json",
}

// AttemptStopper proves that every controller-managed process recorded for an
// attempt has exited. An authenticated lock inode plus kernel process-start
// identity binds any signal to the exact process group created for the attempt.
type AttemptStopper struct {
	InterruptGrace time.Duration
}

func (s AttemptStopper) StopAttempt(ctx context.Context, artifactDir, controlKey string) error {
	if !filepath.IsAbs(artifactDir) || len(controlKey) < 32 {
		return errors.New("attempt artifact directory must be absolute")
	}
	roster, err := readProcessControlRoster(filepath.Join(artifactDir, processControlRosterName), []byte(controlKey))
	if err != nil {
		return err
	}
	listed := make(map[string]bool, len(roster.Entries))
	for _, name := range roster.Entries {
		listed[name] = true
		path := filepath.Join(artifactDir, name)
		_, identityErr := os.Lstat(path)
		_, lockErr := os.Lstat(processControlLockPath(path))
		identityMissing := errors.Is(identityErr, os.ErrNotExist)
		lockMissing := errors.Is(lockErr, os.ErrNotExist)
		if identityMissing && lockMissing {
			return errors.New("rostered managed process control evidence is missing")
		}
		if identityErr != nil && !identityMissing {
			return fmt.Errorf("inspect managed process identity: %w", identityErr)
		}
		if lockErr != nil && !lockMissing {
			return fmt.Errorf("inspect managed process lock: %w", lockErr)
		}
		if identityMissing || lockMissing {
			return errors.New("managed process control evidence is incomplete")
		}
		if err := s.stop(ctx, path, []byte(controlKey)); err != nil {
			return err
		}
	}
	for _, name := range managedAttemptControlFiles {
		if listed[name] {
			continue
		}
		path := filepath.Join(artifactDir, name)
		if _, identityErr := os.Lstat(path); identityErr == nil || !errors.Is(identityErr, os.ErrNotExist) {
			return errors.New("managed process control evidence is not rostered")
		}
		if _, lockErr := os.Lstat(processControlLockPath(path)); lockErr == nil || !errors.Is(lockErr, os.ErrNotExist) {
			return errors.New("managed process lock evidence is not rostered")
		}
	}
	return nil
}

func (s AttemptStopper) stop(ctx context.Context, path string, key []byte) error {
	identity, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer identity.Close()
	lock, err := os.OpenFile(processControlLockPath(path), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer lock.Close()
	control, err := readProcessControl(identity, lock, key)
	if err != nil {
		return err
	}
	active, err := managedProcessControlAuthorized(control, lock)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}
	if err := signalManagedProcessGroup(control, lock, syscall.SIGINT); err != nil {
		return fmt.Errorf("interrupt managed process group: %w", err)
	}
	grace := s.InterruptGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	if stopped, err := waitProcessControl(ctx, control, lock, grace); err != nil {
		return err
	} else if stopped {
		return nil
	}
	if err := signalManagedProcessGroup(control, lock, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill managed process group: %w", err)
	}
	if stopped, err := waitProcessControl(ctx, control, lock, grace); err != nil {
		return err
	} else if !stopped {
		return errors.New("managed process group stop could not be proven")
	}
	return nil
}

func signalManagedProcessGroup(control processControl, lock *os.File, signal syscall.Signal) error {
	active, err := managedProcessControlAuthorized(control, lock)
	if err != nil || !active {
		return err
	}
	return signalObservedProcessGroup(control, signal)
}

func signalObservedProcessGroup(control processControl, signal syscall.Signal) error {
	active, err := managedProcessControlActive(control)
	if err != nil || !active {
		return err
	}
	if signal == syscall.SIGKILL {
		return killManagedProcessGroupMembers(control)
	}
	if err := syscall.Kill(-control.ProcessGroupID, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func killManagedProcessGroupMembers(control processControl) error {
	members, err := observedProcessGroupMembers(control.ProcessGroupID)
	if err != nil {
		return err
	}
	for _, pid := range members {
		if pid == control.ProcessGroupID {
			continue
		}
		if _, err := managedProcessControlActive(control); err != nil {
			return err
		}
		group, err := syscall.Getpgid(pid)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		if group != control.ProcessGroupID {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func managedProcessControlAuthorized(control processControl, lock *os.File) (bool, error) {
	active, err := managedProcessControlActive(control)
	if err != nil || !active {
		return active, err
	}
	return claimManagedProcessLock(lock)
}

// claimManagedProcessLock either observes the originating runner's shared lock
// or adopts the exact authenticated inode after that runner exited or crashed.
// An uncontended exclusive claim remains held by this descriptor through stop
// proof, preventing another lifecycle owner from replacing the adopted lock.
func claimManagedProcessLock(lock *os.File) (bool, error) {
	err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	return false, err
}

func readProcessControl(file, lock *os.File, key []byte) (processControl, error) {
	identityInfo, identityErr := file.Stat()
	lockInfo, lockErr := lock.Stat()
	if identityErr != nil || lockErr != nil || !identityInfo.Mode().IsRegular() || !lockInfo.Mode().IsRegular() {
		return processControl{}, errors.New("managed process control leaves are invalid")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return processControl{}, err
	}
	limited := io.LimitReader(file, 4097)
	data, err := io.ReadAll(limited)
	if err != nil {
		return processControl{}, err
	}
	if len(data) > 4096 {
		return processControl{}, errors.New("managed process control is oversized")
	}
	var control processControl
	if json.Unmarshal(data, &control) != nil || control.SchemaVersion != 3 || control.ControlName != filepath.Base(file.Name()) || !managedProcessControlName(control.ControlName) || control.ProcessGroupID < 1 || control.ProcessStartToken == "" || control.MAC == "" {
		return processControl{}, errors.New("managed process control is invalid")
	}
	device, inode, err := processControlFileIdentity(lock)
	if err != nil || device != control.LockDevice || inode != control.LockInode || !hmac.Equal([]byte(control.MAC), []byte(processControlMAC(control, key))) {
		return processControl{}, errors.New("managed process control authentication failed")
	}
	return control, nil
}

func managedProcessControlActive(control processControl) (bool, error) {
	token, found, err := observedProcessStartToken(control.ProcessGroupID)
	if err != nil {
		return false, err
	}
	if found {
		if token == control.ProcessStartToken {
			group, groupErr := syscall.Getpgid(control.ProcessGroupID)
			if errors.Is(groupErr, syscall.ESRCH) {
				exists, existsErr := processGroupExists(control.ProcessGroupID)
				if existsErr != nil {
					return false, existsErr
				}
				if exists {
					return false, errManagedLeaderAbsentGroupLive
				}
				return false, nil
			}
			if groupErr != nil {
				return false, groupErr
			}
			if group != control.ProcessGroupID {
				return false, errors.New("managed process leader left its authenticated process group")
			}
			return true, nil
		}
		exists, err := processGroupExists(control.ProcessGroupID)
		if err != nil {
			return false, err
		}
		if exists {
			return false, errManagedLeaderAbsentGroupLive
		}
		return false, nil
	}
	exists, err := processGroupExists(control.ProcessGroupID)
	if err != nil {
		return false, err
	}
	if exists {
		return false, errManagedLeaderAbsentGroupLive
	}
	return false, nil
}

func processGroupExists(processGroupID int) (bool, error) {
	if err := syscall.Kill(-processGroupID, 0); err == nil || errors.Is(err, syscall.EPERM) {
		members, observeErr := observedProcessGroupMembers(processGroupID)
		if observeErr != nil {
			return false, observeErr
		}
		return len(members) > 0, nil
	} else if errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else {
		return false, err
	}
}

func waitProcessControl(ctx context.Context, control processControl, lock *os.File, duration time.Duration) (bool, error) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, nil
		case <-ticker.C:
			active, err := managedProcessControlAuthorized(control, lock)
			if errors.Is(err, errManagedLeaderAbsentGroupLive) {
				continue
			}
			if err != nil || !active {
				return !active, err
			}
		}
	}
}
