package linear

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const onboardingLabelLookup = `query ControllerOnboardingLabel($teamKey: String!, $label: String!, $after: String, $first: Int!) {
  teams(filter: { key: { eq: $teamKey } }, first: 2) { nodes { id key } }
  issueLabels(filter: { team: { key: { eq: $teamKey } }, name: { eq: $label } }, first: $first, after: $after) {
    nodes { id name team { id key } }
    pageInfo { hasNextPage endCursor }
  }
}`

const onboardingLabelCreate = `mutation ControllerCreateRepositoryLabel($teamID: String!, $label: String!) {
  issueLabelCreate(input: { teamId: $teamID, name: $label }) {
    success
    issueLabel { id name team { id key } }
  }
}`

type RepositoryLabelObservation struct {
	Found          bool
	LabelID        string
	TeamID         string
	TeamKey        string
	Name           string
	EvidenceDigest string
	ObservedAt     time.Time
}

func (c *Client) LookupRepositoryLabel(ctx context.Context, label string) (RepositoryLabelObservation, error) {
	if !validRepositoryLabelName(label) {
		return RepositoryLabelObservation{}, errors.New("Linear repository label is invalid")
	}
	token, err := c.credentials.Resolve(ctx, c.cfg.CredentialSourceRef)
	if err != nil || strings.TrimSpace(token) == "" {
		return RepositoryLabelObservation{}, errors.New("Linear credentials are unavailable")
	}
	defer clearString(&token)
	var after *string
	var teamID string
	labels := map[string]RepositoryLabelObservation{}
	for page := 0; page < c.cfg.MaxLabelPages; page++ {
		variables := map[string]any{"teamKey": c.cfg.TeamKey, "label": label, "after": after, "first": c.cfg.LabelPageSize}
		var decoded struct {
			Data struct {
				Teams struct {
					Nodes []struct{ ID, Key string } `json:"nodes"`
				} `json:"teams"`
				IssueLabels struct {
					Nodes []struct {
						ID, Name string
						Team     struct{ ID, Key string } `json:"team"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"issueLabels"`
			} `json:"data"`
			Errors []json.RawMessage `json:"errors"`
		}
		if err := c.onboardingGraphQL(ctx, token, onboardingLabelLookup, variables, &decoded); err != nil || len(decoded.Errors) != 0 || len(decoded.Data.Teams.Nodes) != 1 || decoded.Data.Teams.Nodes[0].ID == "" || decoded.Data.Teams.Nodes[0].Key != c.cfg.TeamKey {
			return RepositoryLabelObservation{}, errors.New("Linear repository label observation failed")
		}
		team := decoded.Data.Teams.Nodes[0]
		if teamID != "" && teamID != team.ID {
			return RepositoryLabelObservation{}, errors.New("Linear team identity changed")
		}
		teamID = team.ID
		for _, candidate := range decoded.Data.IssueLabels.Nodes {
			if candidate.ID == "" || candidate.Name != label || candidate.Team.ID != teamID || candidate.Team.Key != c.cfg.TeamKey {
				return RepositoryLabelObservation{}, errors.New("Linear repository label identity conflicts")
			}
			labels[candidate.ID] = RepositoryLabelObservation{Found: true, LabelID: candidate.ID, TeamID: teamID, TeamKey: c.cfg.TeamKey, Name: label}
		}
		if !decoded.Data.IssueLabels.PageInfo.HasNextPage {
			break
		}
		if page+1 == c.cfg.MaxLabelPages || decoded.Data.IssueLabels.PageInfo.EndCursor == "" {
			return RepositoryLabelObservation{}, errors.New("Linear repository label observation is incomplete")
		}
		cursor := decoded.Data.IssueLabels.PageInfo.EndCursor
		after = &cursor
	}
	if len(labels) > 1 {
		return RepositoryLabelObservation{}, errors.New("Linear repository label is ambiguous")
	}
	observedAt := c.clock.Now().UTC()
	result := RepositoryLabelObservation{TeamID: teamID, TeamKey: c.cfg.TeamKey, Name: label, ObservedAt: observedAt}
	for _, candidate := range labels {
		result.Found, result.LabelID = true, candidate.LabelID
	}
	result.EvidenceDigest = linearOnboardingDigest("lookup", result.TeamID, result.TeamKey, result.Name, result.LabelID)
	return result, nil
}

func (c *Client) CreateRepositoryLabel(ctx context.Context, teamID, label string) (RepositoryLabelObservation, error) {
	if teamID == "" || !validRepositoryLabelName(label) {
		return RepositoryLabelObservation{}, errors.New("Linear repository label create authority is invalid")
	}
	token, err := c.credentials.Resolve(ctx, c.cfg.CredentialSourceRef)
	if err != nil || strings.TrimSpace(token) == "" {
		return RepositoryLabelObservation{}, errors.New("Linear credentials are unavailable")
	}
	defer clearString(&token)
	var decoded struct {
		Data struct {
			IssueLabelCreate struct {
				Success    bool `json:"success"`
				IssueLabel *struct {
					ID, Name string
					Team     struct{ ID, Key string } `json:"team"`
				} `json:"issueLabel"`
			} `json:"issueLabelCreate"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := c.onboardingGraphQL(ctx, token, onboardingLabelCreate, map[string]any{"teamID": teamID, "label": label}, &decoded); err != nil || len(decoded.Errors) != 0 {
		return RepositoryLabelObservation{}, errors.New("Linear repository label creation outcome is unknown")
	}
	created := decoded.Data.IssueLabelCreate.IssueLabel
	if !decoded.Data.IssueLabelCreate.Success || created == nil || created.ID == "" || created.Name != label || created.Team.ID != teamID || created.Team.Key != c.cfg.TeamKey {
		return RepositoryLabelObservation{}, errors.New("Linear repository label creation conflicts")
	}
	result := RepositoryLabelObservation{Found: true, LabelID: created.ID, TeamID: teamID, TeamKey: c.cfg.TeamKey, Name: label, ObservedAt: c.clock.Now().UTC()}
	result.EvidenceDigest = linearOnboardingDigest("create", result.TeamID, result.TeamKey, result.Name, result.LabelID)
	return result, nil
}

func (c *Client) onboardingGraphQL(ctx context.Context, token, query string, variables map[string]any, target any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", c.authorizationValue(token))
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, c.cfg.MaxResponseBytes+1))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || int64(len(body)) > c.cfg.MaxResponseBytes {
		return errors.New("bounded Linear response failed")
	}
	return json.Unmarshal(body, target)
}

func validRepositoryLabelName(value string) bool {
	if !strings.HasPrefix(value, "repo:") || len(value) <= len("repo:") || len(value) > 69 {
		return false
	}
	for index, character := range strings.TrimPrefix(value, "repo:") {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || index > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}

func linearOnboardingDigest(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
