package process

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const processControlRosterName = "process-control-roster.json"

type processControlRoster struct {
	SchemaVersion int      `json:"schema_version"`
	Entries       []string `json:"entries"`
	MAC           string   `json:"mac"`
}

func appendProcessControlRoster(directory, name string, key []byte) error {
	if !filepath.IsAbs(directory) || filepath.Base(name) != name || name == "" || strings.ContainsRune(name, '\x00') || !managedProcessControlName(name) {
		return errors.New("managed process roster entry is invalid")
	}
	path := filepath.Join(directory, processControlRosterName)
	roster := processControlRoster{SchemaVersion: 1}
	if _, err := os.Lstat(path); err == nil {
		var readErr error
		roster, readErr = readProcessControlRoster(path, key)
		if readErr != nil {
			return readErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range roster.Entries {
		if entry == name {
			return errors.New("managed process roster entry already exists")
		}
	}
	roster.Entries = append(roster.Entries, name)
	roster.MAC = processControlRosterMAC(roster, key)
	return persistProcessControlRoster(path, roster)
}

func readProcessControlRoster(path string, key []byte) (processControlRoster, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return processControlRoster{}, err
	}
	defer file.Close()
	var roster processControlRoster
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&roster); err != nil || roster.SchemaVersion != 1 || len(roster.Entries) == 0 || len(roster.Entries) > len(managedAttemptControlFiles) || roster.MAC == "" {
		return processControlRoster{}, errors.New("managed process roster is invalid")
	}
	seen := make(map[string]bool, len(roster.Entries))
	for _, entry := range roster.Entries {
		if filepath.Base(entry) != entry || entry == "" || seen[entry] || !managedProcessControlName(entry) {
			return processControlRoster{}, errors.New("managed process roster is invalid")
		}
		seen[entry] = true
	}
	if !hmac.Equal([]byte(roster.MAC), []byte(processControlRosterMAC(roster, key))) {
		return processControlRoster{}, errors.New("managed process roster authentication failed")
	}
	return roster, nil
}

func managedProcessControlName(name string) bool {
	for _, candidate := range managedAttemptControlFiles {
		if candidate == name {
			return true
		}
	}
	return false
}

func processControlRosterMAC(roster processControlRoster, key []byte) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%d", roster.SchemaVersion)
	for _, entry := range roster.Entries {
		fmt.Fprintf(mac, "\n%s", entry)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func persistProcessControlRoster(path string, roster processControlRoster) error {
	data, err := json.Marshal(roster)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".process-roster-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o400); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
