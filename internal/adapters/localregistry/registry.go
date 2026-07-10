package localregistry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/adapters/verifier"
)

type Repository struct {
	Label       string   `json:"label"`
	OriginPath  string   `json:"origin_path"`
	SourcePath  string   `json:"source_path"`
	BaseBranch  string   `json:"base_branch"`
	VerifierIDs []string `json:"verifier_ids"`
}

type File struct {
	Repositories []Repository `json:"repositories"`
}

type Registry struct{ repositories map[string]Repository }

func Load(path string) (Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Registry{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return Registry{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Registry{}, errors.New("registry must contain exactly one JSON value")
	}
	registry := Registry{repositories: make(map[string]Repository, len(file.Repositories))}
	for _, repo := range file.Repositories {
		if strings.TrimSpace(repo.Label) == "" || !filepath.IsAbs(repo.OriginPath) || !filepath.IsAbs(repo.SourcePath) || strings.TrimSpace(repo.BaseBranch) == "" {
			return Registry{}, fmt.Errorf("invalid repository registry entry %q", repo.Label)
		}
		if _, exists := registry.repositories[repo.Label]; exists {
			return Registry{}, fmt.Errorf("duplicate repository label: %s", repo.Label)
		}
		for _, id := range repo.VerifierIDs {
			if _, ok := BuiltinVerifierCommands()[id]; !ok {
				return Registry{}, fmt.Errorf("registry references unsupported controller verifier: %s", id)
			}
		}
		registry.repositories[repo.Label] = repo
	}
	return registry, nil
}

func (r Registry) HasRepository(label string) bool { _, ok := r.repositories[label]; return ok }

func (r Registry) HasVerifier(label, verifierID string) bool {
	repo, ok := r.repositories[label]
	if !ok {
		return false
	}
	for _, id := range repo.VerifierIDs {
		if id == verifierID {
			return true
		}
	}
	return false
}

func (r Registry) Resolve(label string) (Repository, error) {
	repo, ok := r.repositories[label]
	if !ok {
		return Repository{}, fmt.Errorf("unknown repository label: %s", label)
	}
	return repo, nil
}

func BuiltinVerifierCommands() map[string]verifier.Command {
	return map[string]verifier.Command{
		"fixture-go-test": {Program: "go", Args: []string{"test", "./..."}},
	}
}
