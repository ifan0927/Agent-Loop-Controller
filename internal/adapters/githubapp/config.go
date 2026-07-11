package githubapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	APIBaseURL        string        `json:"api_base_url"`
	GraphQLURL        string        `json:"graphql_url"`
	AppID             int64         `json:"app_id"`
	InstallationID    int64         `json:"installation_id"`
	RepositoryOwner   string        `json:"repository_owner"`
	RepositoryName    string        `json:"repository_name"`
	RepositoryID      int64         `json:"repository_id"`
	PrivateKeyFile    string        `json:"private_key_file"`
	HTTPTimeout       time.Duration `json:"http_timeout"`
	TokenRefreshSkew  time.Duration `json:"token_refresh_skew"`
	APIVersion        string        `json:"api_version"`
	CodeRabbitActorID int64         `json:"coderabbit_actor_id"`
	CodeRabbitNodeID  string        `json:"coderabbit_node_id"`
	CodeRabbitAppID   int64         `json:"coderabbit_app_id"`
}

type configFile struct {
	APIBaseURL        string `json:"api_base_url"`
	GraphQLURL        string `json:"graphql_url"`
	AppID             int64  `json:"app_id"`
	InstallationID    int64  `json:"installation_id"`
	RepositoryOwner   string `json:"repository_owner"`
	RepositoryName    string `json:"repository_name"`
	RepositoryID      int64  `json:"repository_id"`
	PrivateKeyFile    string `json:"private_key_file"`
	HTTPTimeout       string `json:"http_timeout"`
	TokenRefreshSkew  string `json:"token_refresh_skew"`
	APIVersion        string `json:"api_version"`
	CodeRabbitActorID int64  `json:"coderabbit_actor_id"`
	CodeRabbitNodeID  string `json:"coderabbit_node_id"`
	CodeRabbitAppID   int64  `json:"coderabbit_app_id"`
}

func DecodeConfig(r io.Reader) (Config, error) {
	var f configFile
	d := json.NewDecoder(io.LimitReader(r, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return Config{}, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return Config{}, errors.New("GitHub config must contain one JSON value")
	}
	timeout, err := time.ParseDuration(f.HTTPTimeout)
	if err != nil {
		return Config{}, errors.New("invalid http_timeout")
	}
	skew, err := time.ParseDuration(f.TokenRefreshSkew)
	if err != nil {
		return Config{}, errors.New("invalid token_refresh_skew")
	}
	c := Config{APIBaseURL: f.APIBaseURL, GraphQLURL: f.GraphQLURL, AppID: f.AppID, InstallationID: f.InstallationID, RepositoryOwner: f.RepositoryOwner, RepositoryName: f.RepositoryName, RepositoryID: f.RepositoryID, PrivateKeyFile: f.PrivateKeyFile, HTTPTimeout: timeout, TokenRefreshSkew: skew, APIVersion: f.APIVersion, CodeRabbitActorID: f.CodeRabbitActorID, CodeRabbitNodeID: f.CodeRabbitNodeID, CodeRabbitAppID: f.CodeRabbitAppID}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.APIBaseURL == "" || c.GraphQLURL == "" || c.AppID < 1 || c.InstallationID < 1 || c.RepositoryID < 1 || c.RepositoryOwner == "" || c.RepositoryName == "" || c.PrivateKeyFile == "" || c.APIVersion == "" {
		return errors.New("incomplete GitHub App configuration")
	}
	if c.HTTPTimeout <= 0 || c.HTTPTimeout > 2*time.Minute || c.TokenRefreshSkew < 0 || c.TokenRefreshSkew >= time.Hour {
		return errors.New("invalid GitHub timeout or refresh skew")
	}
	configuredCodeRabbit := c.CodeRabbitActorID > 0 || c.CodeRabbitNodeID != "" || c.CodeRabbitAppID > 0
	if configuredCodeRabbit && (c.CodeRabbitActorID < 1 || c.CodeRabbitNodeID == "" || c.CodeRabbitAppID < 1) {
		return errors.New("CodeRabbit identity must include actor, node, and App IDs")
	}
	for _, raw := range []string{c.APIBaseURL, c.GraphQLURL} {
		if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://127.0.0.1:") {
			return errors.New("GitHub endpoint must use HTTPS")
		}
	}
	_, err := ReadPrivateKeyFile(c.PrivateKeyFile)
	return err
}

func ReadPrivateKeyFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("private key file path must be absolute")
	}
	clean := filepath.Clean(path)
	file, err := os.Open(clean)
	if err != nil {
		return nil, fmt.Errorf("open private key source: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	pathInfo, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		return nil, errors.New("private key source must be a non-symlink regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("private key source must not be group or world accessible")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil || resolved != clean {
		return nil, errors.New("private key source path is ambiguous")
	}
	if info.Size() > 64<<10 {
		return nil, errors.New("private key source is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<10 {
		return nil, errors.New("private key source is too large")
	}
	return data, nil
}
