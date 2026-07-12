package linear

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const issueQuery = `query ControllerIssue($identifier: String!, $after: String, $first: Int!) {
  issue(id: $identifier) {
    id identifier url title description createdAt updatedAt branchName
    team { id key name }
    state { id name type }
    cycle { id number startsAt endsAt isActive }
    labels(first: $first, after: $after) {
      nodes { id name }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

type CredentialSource interface {
	Resolve(context.Context, string) (string, error)
}

type Clock interface{ Now() time.Time }
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

type Client struct {
	cfg         Config
	credentials CredentialSource
	http        *http.Client
	clock       Clock
}

func New(cfg Config, credentials CredentialSource, clock Clock) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, errors.New("Linear credential source is required")
	}
	if clock == nil {
		clock = RealClock{}
	}
	return &Client{cfg: cfg, credentials: credentials, http: &http.Client{Timeout: cfg.HTTPTimeout}, clock: clock}, nil
}

func (c *Client) ReadIssue(ctx context.Context, identifier string) (application.LinearTaskSource, []application.LinearRequestObservation, error) {
	if !validIdentifier(identifier, c.cfg.TeamKey) {
		return application.LinearTaskSource{}, nil, errors.New("Linear issue identifier does not match the configured team")
	}
	token, err := c.credentials.Resolve(ctx, c.cfg.CredentialSourceRef)
	if err != nil || strings.TrimSpace(token) == "" {
		return application.LinearTaskSource{}, nil, errors.New("Linear credentials are unavailable")
	}
	defer clearString(&token)

	var observations []application.LinearRequestObservation
	var after *string
	var source *rawIssue
	labels := make(map[string]application.LinearLabel)
	for page := 0; page < c.cfg.MaxLabelPages; page++ {
		response, observation, readErr := c.fetch(ctx, token, identifier, after)
		if readErr != nil {
			observations = append(observations, observation)
			return application.LinearTaskSource{}, observations, readErr
		}
		if len(response.Errors) != 0 {
			observation.ErrorClass = "graphql"
			observations = append(observations, observation)
			return application.LinearTaskSource{}, observations, errors.New("Linear GraphQL response contains errors")
		}
		observations = append(observations, observation)
		if response.Data.Issue == nil {
			return application.LinearTaskSource{}, observations, errors.New("Linear issue was not found")
		}
		if source == nil {
			source = response.Data.Issue
		} else if source.fingerprint() != response.Data.Issue.fingerprint() {
			return application.LinearTaskSource{}, observations, errors.New("Linear issue changed while labels were fetched")
		}
		if response.Data.Issue.Labels == nil || response.Data.Issue.Labels.Nodes == nil || response.Data.Issue.Labels.PageInfo == nil || response.Data.Issue.Labels.PageInfo.HasNextPage == nil {
			return application.LinearTaskSource{}, observations, errors.New("Linear labels response is incomplete")
		}
		for _, label := range *response.Data.Issue.Labels.Nodes {
			if label.ID == "" || strings.TrimSpace(label.Name) == "" {
				return application.LinearTaskSource{}, observations, errors.New("Linear label is incomplete")
			}
			if _, exists := labels[label.ID]; exists {
				return application.LinearTaskSource{}, observations, errors.New("Linear label appeared more than once")
			}
			labels[label.ID] = application.LinearLabel{ID: label.ID, Name: label.Name}
		}
		pageInfo := *response.Data.Issue.Labels.PageInfo
		if !*pageInfo.HasNextPage {
			break
		}
		if page+1 == c.cfg.MaxLabelPages || pageInfo.EndCursor == "" {
			return application.LinearTaskSource{}, observations, errors.New("Linear label pagination limit exceeded")
		}
		cursor := pageInfo.EndCursor
		after = &cursor
	}
	if source == nil {
		return application.LinearTaskSource{}, observations, errors.New("Linear issue response is incomplete")
	}
	result, err := c.normalize(identifier, *source, labels)
	if err != nil {
		return application.LinearTaskSource{}, observations, err
	}
	return result, observations, nil
}

func (c *Client) fetch(ctx context.Context, token, identifier string, after *string) (graphqlResponse, application.LinearRequestObservation, error) {
	payload, err := json.Marshal(struct {
		Query     string `json:"query"`
		Variables struct {
			Identifier string  `json:"identifier"`
			After      *string `json:"after"`
			First      int     `json:"first"`
		} `json:"variables"`
	}{Query: issueQuery, Variables: struct {
		Identifier string  `json:"identifier"`
		After      *string `json:"after"`
		First      int     `json:"first"`
	}{Identifier: identifier, After: after, First: c.cfg.LabelPageSize}})
	if err != nil {
		return graphqlResponse{}, application.LinearRequestObservation{}, errors.New("encode Linear request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL, bytes.NewReader(payload))
	if err != nil {
		return graphqlResponse{}, application.LinearRequestObservation{}, errors.New("create Linear request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.authorizationValue(token))
	response, err := c.http.Do(req)
	if err != nil {
		return graphqlResponse{}, application.LinearRequestObservation{Operation: "read_issue", ErrorClass: "transport", ObservedAt: c.clock.Now().UTC()}, errors.New("Linear request failed")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, c.cfg.MaxResponseBytes+1))
	observation := newObservation(response, body, c.clock.Now())
	if readErr != nil {
		observation.ErrorClass = "read_body"
		return graphqlResponse{}, observation, errors.New("read Linear response")
	}
	if int64(len(body)) > c.cfg.MaxResponseBytes {
		observation.ErrorClass = "response_too_large"
		return graphqlResponse{}, observation, errors.New("Linear response exceeds configured limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		observation.ErrorClass = "http_status"
		return graphqlResponse{}, observation, fmt.Errorf("Linear request returned HTTP status %d", response.StatusCode)
	}
	var decoded graphqlResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		observation.ErrorClass = "malformed_json"
		return graphqlResponse{}, observation, errors.New("invalid Linear response JSON")
	}
	return decoded, observation, nil
}

func (c *Client) authorizationValue(token string) string {
	if c.cfg.AuthorizationScheme == "bearer" {
		return "Bearer " + token
	}
	return token
}

func (c *Client) normalize(identifier string, raw rawIssue, labels map[string]application.LinearLabel) (application.LinearTaskSource, error) {
	if raw.ID == "" || raw.Identifier != identifier || raw.URL == "" || strings.TrimSpace(raw.Title) == "" || raw.Description == "" ||
		raw.Team.ID == "" || raw.Team.Key != c.cfg.TeamKey || raw.Team.Name == "" || raw.State.ID == "" || raw.State.Name == "" || raw.State.Type == "" ||
		raw.Cycle.ID == "" || raw.Cycle.Number < 1 || raw.Cycle.StartsAt.IsZero() || raw.Cycle.EndsAt.IsZero() || raw.Cycle.IsActive == nil ||
		strings.TrimSpace(raw.BranchName) == "" || raw.CreatedAt.IsZero() || raw.UpdatedAt.IsZero() || raw.UpdatedAt.Before(raw.CreatedAt) {
		return application.LinearTaskSource{}, errors.New("Linear issue response is incomplete or mismatched")
	}
	if raw.Cycle.EndsAt.Before(raw.Cycle.StartsAt) {
		return application.LinearTaskSource{}, errors.New("Linear cycle dates are contradictory")
	}
	result := application.LinearTaskSource{
		Provider: "linear", IssueID: raw.ID, Identifier: raw.Identifier, URL: raw.URL, Title: raw.Title, Description: raw.Description,
		Team:       application.LinearTeam{ID: raw.Team.ID, Key: raw.Team.Key, Name: raw.Team.Name},
		State:      application.LinearState{ID: raw.State.ID, Name: raw.State.Name, Type: raw.State.Type},
		Cycle:      application.LinearCycle{ID: raw.Cycle.ID, Number: raw.Cycle.Number, StartsAt: raw.Cycle.StartsAt.UTC(), EndsAt: raw.Cycle.EndsAt.UTC(), IsActive: *raw.Cycle.IsActive},
		BranchName: raw.BranchName, SourceRevision: raw.UpdatedAt.UTC().Format(time.RFC3339Nano), CreatedAt: raw.CreatedAt.UTC(), UpdatedAt: raw.UpdatedAt.UTC(), ObservedAt: c.clock.Now().UTC(),
	}
	for _, label := range labels {
		result.Labels = append(result.Labels, label)
	}
	sort.Slice(result.Labels, func(i, j int) bool { return result.Labels[i].ID < result.Labels[j].ID })
	return result, nil
}

type graphqlResponse struct {
	Data struct {
		Issue *rawIssue `json:"issue"`
	} `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

type rawIssue struct {
	ID          string    `json:"id"`
	Identifier  string    `json:"identifier"`
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	BranchName  string    `json:"branchName"`
	Team        struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"team"`
	State struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Cycle struct {
		ID       string    `json:"id"`
		Number   int       `json:"number"`
		StartsAt time.Time `json:"startsAt"`
		EndsAt   time.Time `json:"endsAt"`
		IsActive *bool     `json:"isActive"`
	} `json:"cycle"`
	Labels *rawLabelConnection `json:"labels"`
}

type rawLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type rawPageInfo struct {
	HasNextPage *bool  `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type rawLabelConnection struct {
	Nodes    *[]rawLabel  `json:"nodes"`
	PageInfo *rawPageInfo `json:"pageInfo"`
}

func (r rawIssue) fingerprint() string {
	copy := r
	copy.Labels = nil
	raw, _ := json.Marshal(copy)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func newObservation(response *http.Response, body []byte, observedAt time.Time) application.LinearRequestObservation {
	sum := sha256.Sum256(body)
	return application.LinearRequestObservation{
		Operation:          "read_issue",
		HTTPStatus:         response.StatusCode,
		RequestID:          sanitizeRequestID(response.Header.Get("X-Request-ID")),
		RateLimitLimit:     parseHeaderInt(response.Header.Get("X-RateLimit-Requests-Limit")),
		RateLimitRemaining: parseHeaderInt(response.Header.Get("X-RateLimit-Requests-Remaining")),
		RateLimitReset:     parseEpochMilliseconds(response.Header.Get("X-RateLimit-Requests-Reset")),
		ResponseDigest:     hex.EncodeToString(sum[:]),
		ObservedAt:         observedAt.UTC(),
	}
}

func sanitizeRequestID(value string) string {
	if len(value) != 36 {
		return ""
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return ""
			}
			continue
		}
		if !isHexadecimal(character) {
			return ""
		}
	}
	if value[14] < '1' || value[14] > '8' || !isRFC4122Variant(value[19]) {
		return ""
	}
	return strings.ToLower(value)
}

func isHexadecimal(value rune) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func isRFC4122Variant(value byte) bool {
	return value == '8' || value == '9' || value == 'a' || value == 'b' || value == 'A' || value == 'B'
}

func parseHeaderInt(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func parseEpochMilliseconds(value string) time.Time {
	milliseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || milliseconds < 0 {
		return time.Time{}
	}
	return time.UnixMilli(milliseconds).UTC()
}

func validIdentifier(identifier, teamKey string) bool {
	prefix := teamKey + "-"
	if !strings.HasPrefix(identifier, prefix) || len(identifier) == len(prefix) {
		return false
	}
	for _, character := range identifier[len(prefix):] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func clearString(value *string) {
	if value == nil {
		return
	}
	*value = ""
}
