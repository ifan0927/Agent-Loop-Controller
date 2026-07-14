package githubapp

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
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
	PullRequestsWrite bool          `json:"pull_requests_write"`
	SquashMergeWrite  bool          `json:"squash_merge_write"`
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
	PullRequestsWrite bool   `json:"pull_requests_write"`
	SquashMergeWrite  bool   `json:"squash_merge_write"`
}

func DecodeConfig(r io.Reader) (Config, error) {
	c, err := DecodeConfigWithoutPrivateKey(r)
	if err != nil {
		return Config{}, err
	}
	if _, err := ReadPrivateKeyFile(c.PrivateKeyFile); err != nil {
		return Config{}, err
	}
	return c, nil
}

// DecodeConfigWithoutPrivateKey validates the configuration topology without
// opening the credential source. It is used by offline configuration checks.
func DecodeConfigWithoutPrivateKey(r io.Reader) (Config, error) {
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
	c := Config{APIBaseURL: f.APIBaseURL, GraphQLURL: f.GraphQLURL, AppID: f.AppID, InstallationID: f.InstallationID, RepositoryOwner: f.RepositoryOwner, RepositoryName: f.RepositoryName, RepositoryID: f.RepositoryID, PrivateKeyFile: f.PrivateKeyFile, HTTPTimeout: timeout, TokenRefreshSkew: skew, APIVersion: f.APIVersion, PullRequestsWrite: f.PullRequestsWrite, SquashMergeWrite: f.SquashMergeWrite}
	return c, c.ValidateWithoutPrivateKey()
}

func (c Config) Validate() error {
	if err := c.ValidateWithoutPrivateKey(); err != nil {
		return err
	}
	_, err := ReadPrivateKeyFile(c.PrivateKeyFile)
	return err
}

// ValidateWithoutPrivateKey never opens or reads the configured credential.
func (c Config) ValidateWithoutPrivateKey() error {
	if c.APIBaseURL == "" || c.GraphQLURL == "" || c.AppID < 1 || c.InstallationID < 1 || c.RepositoryID < 1 || c.RepositoryOwner == "" || c.RepositoryName == "" || c.PrivateKeyFile == "" || c.APIVersion == "" {
		return errors.New("incomplete GitHub App configuration")
	}
	if c.HTTPTimeout <= 0 || c.HTTPTimeout > 2*time.Minute || c.TokenRefreshSkew < 0 || c.TokenRefreshSkew >= time.Hour {
		return errors.New("invalid GitHub timeout or refresh skew")
	}
	apiURL, err := url.Parse(c.APIBaseURL)
	if err != nil {
		return errors.New("invalid GitHub API URL")
	}
	graphURL, err := url.Parse(c.GraphQLURL)
	if err != nil {
		return errors.New("invalid GitHub GraphQL URL")
	}
	for _, u := range []*url.URL{apiURL, graphURL} {
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("GitHub endpoint contains forbidden URL components")
		}
	}
	official := apiURL.Scheme == "https" && apiURL.Host == "api.github.com" && apiURL.Path == "" && graphURL.Scheme == "https" && graphURL.Host == "api.github.com" && graphURL.Path == "/graphql"
	fixture := apiURL.Scheme == "http" && strings.HasPrefix(apiURL.Host, "127.0.0.1:") && apiURL.Path == "" && graphURL.Scheme == apiURL.Scheme && graphURL.Host == apiURL.Host && graphURL.Path == "/graphql"
	if !official && !fixture {
		return errors.New("GitHub endpoint topology is not allowed")
	}
	return validatePrivateKeyPath(c.PrivateKeyFile)
}

func validatePrivateKeyPath(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("private key file path must be absolute")
	}
	if filepath.Clean(path) != path {
		return errors.New("private key file path is not canonical")
	}
	return nil
}

func ReadPrivateKeyFile(path string) ([]byte, error) {
	if err := validatePrivateKeyPath(path); err != nil {
		return nil, err
	}
	clean := filepath.Clean(path)
	file, err := os.Open(clean)
	if err != nil {
		return nil, errors.New("private key source is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("private key source is unavailable")
	}
	pathInfo, err := os.Lstat(clean)
	if err != nil {
		return nil, errors.New("private key source is unavailable")
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
		return nil, errors.New("private key source is unavailable")
	}
	if len(data) > 64<<10 {
		return nil, errors.New("private key source is too large")
	}
	return data, nil
}
