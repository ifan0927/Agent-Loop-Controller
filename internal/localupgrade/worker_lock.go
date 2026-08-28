package localupgrade

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type workerLock struct{ file *os.File }

func acquireWorkerLock(databasePath string, uid int) (*workerLock, error) {
	directory := filepath.Dir(databasePath)
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !ownedByUID(info, uid) {
		return nil, errors.New("worker lock directory is unsafe")
	}
	file, err := os.OpenFile(filepath.Join(directory, "worker.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("worker lock is unavailable")
	}
	if !safePrivateFileHandle(file, uid, 0) {
		_ = file.Close()
		return nil, errors.New("worker lock is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("worker remains active")
	}
	return &workerLock{file: file}, nil
}

func (lock *workerLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
