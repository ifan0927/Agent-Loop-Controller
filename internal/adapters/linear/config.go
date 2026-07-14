package linear

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
)

const (
	EnvironmentCredentialSourceRef = "secret://env/IFAN_LOOP_LINEAR_TOKEN"
	FileCredentialSourceRef        = "secret://file/linear-token"
)

type Config struct {
	APIURL              string        `json:"api_url"`
	CredentialSourceRef string        `json:"credential_source_ref"`
	AuthorizationScheme string        `json:"authorization_scheme"`
	TeamKey             string        `json:"team_key"`
	HTTPTimeout         time.Duration `json:"http_timeout"`
	MaxResponseBytes    int64         `json:"max_response_bytes"`
	LabelPageSize       int           `json:"label_page_size"`
	MaxLabelPages       int           `json:"max_label_pages"`
}

type configFile struct {
	APIURL              string `json:"api_url"`
	CredentialSourceRef string `json:"credential_source_ref"`
	AuthorizationScheme string `json:"authorization_scheme"`
	TeamKey             string `json:"team_key"`
	HTTPTimeout         string `json:"http_timeout"`
	MaxResponseBytes    int64  `json:"max_response_bytes"`
	LabelPageSize       int    `json:"label_page_size"`
	MaxLabelPages       int    `json:"max_label_pages"`
}

func DecodeConfig(r io.Reader) (Config, error) {
	var raw configFile
	decoder := json.NewDecoder(io.LimitReader(r, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, errors.New("invalid Linear configuration")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("Linear configuration must contain one JSON value")
	}
	timeout, err := time.ParseDuration(raw.HTTPTimeout)
	if err != nil {
		return Config{}, errors.New("invalid Linear HTTP timeout")
	}
	config := Config{
		APIURL:              raw.APIURL,
		CredentialSourceRef: raw.CredentialSourceRef,
		AuthorizationScheme: raw.AuthorizationScheme,
		TeamKey:             raw.TeamKey,
		HTTPTimeout:         timeout,
		MaxResponseBytes:    raw.MaxResponseBytes,
		LabelPageSize:       raw.LabelPageSize,
		MaxLabelPages:       raw.MaxLabelPages,
	}
	return config, config.Validate()
}

func (c Config) Validate() error {
	if c.APIURL == "" || c.CredentialSourceRef == "" || c.AuthorizationScheme == "" || c.TeamKey == "" {
		return errors.New("incomplete Linear configuration")
	}
	if !ValidCredentialSourceRef(c.CredentialSourceRef) {
		return errors.New("invalid Linear credential source reference")
	}
	if c.AuthorizationScheme != "bearer" && c.AuthorizationScheme != "api_key" {
		return errors.New("invalid Linear authorization scheme")
	}
	if !validTeamKey(c.TeamKey) {
		return errors.New("invalid Linear team key")
	}
	if c.HTTPTimeout <= 0 || c.HTTPTimeout > 2*time.Minute || c.MaxResponseBytes < 1024 || c.MaxResponseBytes > 4<<20 ||
		c.LabelPageSize < 1 || c.LabelPageSize > 100 || c.MaxLabelPages < 1 || c.MaxLabelPages > 20 {
		return errors.New("invalid Linear request limits")
	}
	u, err := url.Parse(c.APIURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/graphql" {
		return errors.New("invalid Linear API URL")
	}
	official := u.Scheme == "https" && u.Host == "api.linear.app"
	fixture := u.Scheme == "http" && strings.HasPrefix(u.Host, "127.0.0.1:")
	if !official && !fixture {
		return errors.New("Linear API endpoint is not allowed")
	}
	return nil
}

// ValidCredentialSourceRef validates a secret reference without resolving it.
// It is shared by offline configuration authority validation.
func ValidCredentialSourceRef(value string) bool {
	return value == EnvironmentCredentialSourceRef || value == FileCredentialSourceRef
}

// CredentialSourceType is the only credential metadata safe for offline
// readiness output. It never returns a reference, path, or credential value.
func CredentialSourceType(value string) string {
	switch value {
	case FileCredentialSourceRef:
		return "file"
	case EnvironmentCredentialSourceRef:
		return "environment"
	default:
		return ""
	}
}

func validReferenceComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validTeamKey(value string) bool {
	if len(value) < 2 || len(value) > 32 {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' && index > 0 || character == '_' && index > 0 {
			continue
		}
		return false
	}
	return true
}
