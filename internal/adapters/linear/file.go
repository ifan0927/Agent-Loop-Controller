package linear

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxFileCredentialBytes = 8 << 10

// FileCredentialSource reads only the fixed linear-token leaf below Root.
// Root is controller-owned configuration, never a credential-source URI path.
type FileCredentialSource struct {
	Root string
}

func (s FileCredentialSource) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ref != FileCredentialSourceRef {
		return "", unavailableCredentialError()
	}
	file, err := s.openChecked()
	if err != nil {
		return "", unavailableCredentialError()
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFileCredentialBytes+1))
	if err != nil || len(data) > maxFileCredentialBytes {
		return "", unavailableCredentialError()
	}
	value, ok := singleCredentialLine(data)
	if !ok {
		return "", unavailableCredentialError()
	}
	return value, nil
}

// Check verifies a runtime credential source without exposing token bytes,
// paths, ownership, or permission detail to its caller.
func (s FileCredentialSource) Check(ctx context.Context) error {
	_, err := s.Resolve(ctx, FileCredentialSourceRef)
	return err
}

func (s FileCredentialSource) openChecked() (*os.File, error) {
	if !secureCredentialDirectory(s.Root) {
		return nil, unavailableCredentialError()
	}
	path := filepath.Join(s.Root, "linear-token")
	pathInfo, err := os.Lstat(path)
	if err != nil || !secureCredentialFile(pathInfo) {
		return nil, unavailableCredentialError()
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, unavailableCredentialError()
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(pathInfo, info) || !secureCredentialFile(info) {
		file.Close()
		return nil, unavailableCredentialError()
	}
	return file, nil
}

func secureCredentialDirectory(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	parent := filepath.Dir(path)
	if !canonicalDirectory(parent, false) || !canonicalDirectory(path, true) {
		return false
	}
	return true
}

func canonicalDirectory(path string, private bool) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return false
	}
	if private && (!ownedByCurrentUser(info) || info.Mode().Perm() != 0o700) {
		return false
	}
	return true
}

func secureCredentialFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && ownedByCurrentUser(info) && info.Mode().Perm() == 0o600 && linkCount(info) == 1 && info.Size() >= 1 && info.Size() <= maxFileCredentialBytes
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func linkCount(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}

func singleCredentialLine(data []byte) (string, bool) {
	if len(data) == 0 || strings.IndexByte(string(data), 0) >= 0 {
		return "", false
	}
	value := strings.TrimSuffix(string(data), "\n")
	if value == "" || strings.ContainsAny(value, "\r\n") || strings.TrimSpace(value) != value {
		return "", false
	}
	return value, true
}

func unavailableCredentialError() error {
	return errors.New("Linear file credential is unavailable")
}
