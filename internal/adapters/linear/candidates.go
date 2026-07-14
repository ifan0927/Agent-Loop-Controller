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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const repositoryLabelPrefix = "repo:"

// candidateQuery intentionally requests no issue title, description, or
// comments. Those external fields are neither scheduling input nor scan
// evidence, and are read only by a later authoritative admission operation.
const candidateQuery = `query ControllerTodoCandidates($teamID: String!, $stateIDs: [ID!]!, $todoStateID: ID!, $after: String, $first: Int!) {
  team(id: $teamID) {
    id key
    states(filter: { id: { in: $stateIDs } }, first: 2) { nodes { id name type } }
    issues(filter: { and: [
      { state: { id: { eq: $todoStateID } } }
      { cycle: { isActive: { eq: true } } }
      { labels: { some: { name: { eq: "agent:codex" } } } }
      { labels: { every: { name: { neq: "agent:hermes" } } } }
      { labels: { some: { name: { startsWith: "repo:" } } } }
    ] }, first: $first, after: $after) {
      nodes {
        id identifier priority branchName createdAt updatedAt
        team { id key }
        state { id name type }
        cycle { id number startsAt endsAt isActive }
        labels(first: 100) { nodes { id name } pageInfo { hasNextPage endCursor } }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// ListTodoCandidates reads a complete bounded candidate snapshot. A result is
// useful only when every requested page and its immutable authority agree; any
// ambiguity returns no prefix to the caller.
func (c *Client) ListTodoCandidates(ctx context.Context, authority application.LinearTodoCandidateAuthority) (application.LinearTodoCandidateScan, []application.LinearRequestObservation, error) {
	if err := c.validateCandidateAuthority(authority); err != nil {
		return application.LinearTodoCandidateScan{}, nil, err
	}
	token, err := c.credentials.Resolve(ctx, c.cfg.CredentialSourceRef)
	if err != nil || strings.TrimSpace(token) == "" {
		return application.LinearTodoCandidateScan{}, nil, errors.New("Linear credentials are unavailable")
	}
	defer clearString(&token)

	var observations []application.LinearRequestObservation
	var after *string
	seenCursors := make(map[string]struct{})
	seenIDs := make(map[string]struct{})
	seenIdentifiers := make(map[string]struct{})
	candidates := make([]application.LinearTodoCandidate, 0, authority.MaxCandidates)
	for page := 0; page < authority.MaxPages; page++ {
		response, observation, readErr := c.fetchCandidates(ctx, token, authority, after, page+1)
		if readErr != nil {
			observations = append(observations, observation)
			return application.LinearTodoCandidateScan{}, observations, readErr
		}
		if len(response.Errors) != 0 {
			observation.ErrorClass = "graphql"
			observations = append(observations, observation)
			return application.LinearTodoCandidateScan{}, observations, errors.New("Linear GraphQL response contains errors")
		}
		if response.Data.Team == nil || response.Data.Team.Issues == nil || response.Data.Team.Issues.Nodes == nil || response.Data.Team.Issues.PageInfo == nil || response.Data.Team.Issues.PageInfo.HasNextPage == nil {
			observation.ErrorClass = "incomplete_response"
			observations = append(observations, observation)
			return application.LinearTodoCandidateScan{}, observations, errors.New("Linear candidate response is incomplete")
		}
		if err := validateCandidateScanTeam(*response.Data.Team, authority); err != nil {
			observation.ErrorClass = "authority_mismatch"
			observations = append(observations, observation)
			return application.LinearTodoCandidateScan{}, observations, err
		}
		observation.Count = len(*response.Data.Team.Issues.Nodes)
		observations = append(observations, observation)
		for _, raw := range *response.Data.Team.Issues.Nodes {
			candidate, normalizeErr := normalizeCandidate(raw, authority)
			if normalizeErr != nil {
				return application.LinearTodoCandidateScan{}, observations, normalizeErr
			}
			if _, found := seenIDs[candidate.IssueID]; found {
				return application.LinearTodoCandidateScan{}, observations, errors.New("Linear candidate issue appeared more than once")
			}
			if _, found := seenIdentifiers[candidate.Identifier]; found {
				return application.LinearTodoCandidateScan{}, observations, errors.New("Linear candidate identifier appeared more than once")
			}
			seenIDs[candidate.IssueID] = struct{}{}
			seenIdentifiers[candidate.Identifier] = struct{}{}
			candidates = append(candidates, candidate)
			if len(candidates) > authority.MaxCandidates {
				return application.LinearTodoCandidateScan{}, observations, errors.New("Linear candidate limit exceeded")
			}
		}
		pageInfo := *response.Data.Team.Issues.PageInfo
		if !*pageInfo.HasNextPage {
			return finalizedCandidateScan(candidates, c.clock.Now()), observations, nil
		}
		if page+1 == authority.MaxPages || len(candidates) == authority.MaxCandidates || pageInfo.EndCursor == "" {
			return application.LinearTodoCandidateScan{}, observations, errors.New("Linear candidate pagination limit exceeded")
		}
		if _, found := seenCursors[pageInfo.EndCursor]; found {
			return application.LinearTodoCandidateScan{}, observations, errors.New("Linear candidate cursor appeared more than once")
		}
		seenCursors[pageInfo.EndCursor] = struct{}{}
		cursor := pageInfo.EndCursor
		after = &cursor
	}
	return application.LinearTodoCandidateScan{}, observations, errors.New("Linear candidate pagination limit exceeded")
}

func (c *Client) validateCandidateAuthority(authority application.LinearTodoCandidateAuthority) error {
	if !validCandidateUUID(authority.TeamID) || !validCandidateUUID(authority.TodoState.ID) || !validCandidateUUID(authority.InProgressState.ID) || authority.TeamKey != c.cfg.TeamKey || authority.TeamKey != "IFAN" ||
		!sameWorkflowState(authority.TodoState, "", "Todo", "unstarted") ||
		!sameWorkflowState(authority.InProgressState, "", "In Progress", "started") ||
		authority.TodoState.ID == authority.InProgressState.ID ||
		authority.MaxCandidates < 1 || authority.MaxCandidates > 100 || authority.MaxPages < 1 || authority.MaxPages > 20 {
		return errors.New("Linear candidate scan authority is invalid")
	}
	return nil
}

func validCandidateUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value && parsed.Variant() == uuid.RFC4122
}

func (c *Client) fetchCandidates(ctx context.Context, token string, authority application.LinearTodoCandidateAuthority, after *string, page int) (candidateGraphQLResponse, application.LinearRequestObservation, error) {
	payload, err := json.Marshal(struct {
		Query     string `json:"query"`
		Variables struct {
			TeamID      string   `json:"teamID"`
			StateIDs    []string `json:"stateIDs"`
			TodoStateID string   `json:"todoStateID"`
			After       *string  `json:"after"`
			First       int      `json:"first"`
		} `json:"variables"`
	}{Query: candidateQuery, Variables: struct {
		TeamID      string   `json:"teamID"`
		StateIDs    []string `json:"stateIDs"`
		TodoStateID string   `json:"todoStateID"`
		After       *string  `json:"after"`
		First       int      `json:"first"`
	}{TeamID: authority.TeamID, StateIDs: []string{authority.TodoState.ID, authority.InProgressState.ID}, TodoStateID: authority.TodoState.ID, After: after, First: authority.MaxCandidates}})
	if err != nil {
		return candidateGraphQLResponse{}, application.LinearRequestObservation{}, errors.New("encode Linear candidate request")
	}
	req, err := httpRequest(ctx, c.cfg.APIURL, c.authorizationValue(token), payload)
	if err != nil {
		return candidateGraphQLResponse{}, application.LinearRequestObservation{}, errors.New("create Linear candidate request")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return candidateGraphQLResponse{}, application.LinearRequestObservation{Operation: "list_todo_candidates", Page: page, ErrorClass: "transport", ObservedAt: c.clock.Now().UTC()}, errors.New("Linear candidate request failed")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, c.cfg.MaxResponseBytes+1))
	observation := newObservation(response, body, c.clock.Now())
	observation.Operation, observation.Page = "list_todo_candidates", page
	if readErr != nil {
		observation.ErrorClass = "read_body"
		return candidateGraphQLResponse{}, observation, errors.New("read Linear candidate response")
	}
	if int64(len(body)) > c.cfg.MaxResponseBytes {
		observation.ErrorClass = "response_too_large"
		return candidateGraphQLResponse{}, observation, errors.New("Linear candidate response exceeds configured limit")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if response.StatusCode == http.StatusTooManyRequests {
			observation.ErrorClass = "rate_limited"
			return candidateGraphQLResponse{}, observation, errors.New("Linear candidate request is rate limited")
		}
		observation.ErrorClass = "http_status"
		return candidateGraphQLResponse{}, observation, fmt.Errorf("Linear candidate request returned HTTP status %d", response.StatusCode)
	}
	var decoded candidateGraphQLResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		observation.ErrorClass = "malformed_json"
		return candidateGraphQLResponse{}, observation, errors.New("invalid Linear candidate response JSON")
	}
	return decoded, observation, nil
}

func httpRequest(ctx context.Context, url, authorization string, payload []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	return req, nil
}

type candidateGraphQLResponse struct {
	Data struct {
		Team *rawCandidateTeam `json:"team"`
	} `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

type rawCandidateTeam struct {
	ID     string                  `json:"id"`
	Key    string                  `json:"key"`
	States *rawCandidateStateSet   `json:"states"`
	Issues *rawCandidateConnection `json:"issues"`
}

type rawCandidateStateSet struct {
	Nodes *[]rawCandidateState `json:"nodes"`
}

type rawCandidateState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type rawCandidateConnection struct {
	Nodes    *[]rawCandidate `json:"nodes"`
	PageInfo *rawPageInfo    `json:"pageInfo"`
}

type rawCandidate struct {
	ID         string            `json:"id"`
	Identifier string            `json:"identifier"`
	Priority   *int              `json:"priority"`
	BranchName string            `json:"branchName"`
	CreatedAt  time.Time         `json:"createdAt"`
	UpdatedAt  time.Time         `json:"updatedAt"`
	Team       rawCandidateTeam  `json:"team"`
	State      rawCandidateState `json:"state"`
	Cycle      struct {
		ID       string    `json:"id"`
		Number   int       `json:"number"`
		StartsAt time.Time `json:"startsAt"`
		EndsAt   time.Time `json:"endsAt"`
		IsActive *bool     `json:"isActive"`
	} `json:"cycle"`
	Labels *rawLabelConnection `json:"labels"`
}

func validateCandidateScanTeam(team rawCandidateTeam, authority application.LinearTodoCandidateAuthority) error {
	if team.ID != authority.TeamID || team.Key != authority.TeamKey || team.States == nil || team.States.Nodes == nil || len(*team.States.Nodes) != 2 {
		return errors.New("Linear candidate scan authority does not match configured team or states")
	}
	states := make(map[string]rawCandidateState, 2)
	for _, state := range *team.States.Nodes {
		if _, found := states[state.ID]; found {
			return errors.New("Linear candidate scan returned duplicate workflow state")
		}
		states[state.ID] = state
	}
	if state, found := states[authority.TodoState.ID]; !found || !sameWorkflowState(application.LinearState{ID: state.ID, Name: state.Name, Type: state.Type}, authority.TodoState.ID, authority.TodoState.Name, authority.TodoState.Type) {
		return errors.New("Linear candidate Todo workflow authority does not match")
	}
	if state, found := states[authority.InProgressState.ID]; !found || !sameWorkflowState(application.LinearState{ID: state.ID, Name: state.Name, Type: state.Type}, authority.InProgressState.ID, authority.InProgressState.Name, authority.InProgressState.Type) {
		return errors.New("Linear candidate In Progress workflow authority does not match")
	}
	return nil
}

func sameWorkflowState(state application.LinearState, id, name, stateType string) bool {
	if id != "" && state.ID != id {
		return false
	}
	return state.Name == name && state.Type == stateType && strings.TrimSpace(state.ID) != ""
}

func normalizeCandidate(raw rawCandidate, authority application.LinearTodoCandidateAuthority) (application.LinearTodoCandidate, error) {
	if !validCandidateUUID(raw.ID) || raw.Identifier == "" || raw.Priority == nil || *raw.Priority < 0 || *raw.Priority > 4 || strings.TrimSpace(raw.BranchName) == "" || raw.CreatedAt.IsZero() || raw.UpdatedAt.IsZero() || raw.UpdatedAt.Before(raw.CreatedAt) ||
		raw.Team.ID != authority.TeamID || raw.Team.Key != authority.TeamKey ||
		!sameWorkflowState(application.LinearState{ID: raw.State.ID, Name: raw.State.Name, Type: raw.State.Type}, authority.TodoState.ID, authority.TodoState.Name, authority.TodoState.Type) ||
		!validCandidateUUID(raw.Cycle.ID) || raw.Cycle.Number < 1 || raw.Cycle.StartsAt.IsZero() || raw.Cycle.EndsAt.IsZero() || raw.Cycle.EndsAt.Before(raw.Cycle.StartsAt) || raw.Cycle.IsActive == nil || !*raw.Cycle.IsActive ||
		raw.Labels == nil || raw.Labels.Nodes == nil || raw.Labels.PageInfo == nil || raw.Labels.PageInfo.HasNextPage == nil || *raw.Labels.PageInfo.HasNextPage {
		return application.LinearTodoCandidate{}, errors.New("Linear candidate response is incomplete or outside configured filters")
	}
	labels := make([]application.LinearLabel, 0, len(*raw.Labels.Nodes))
	seen := make(map[string]struct{}, len(*raw.Labels.Nodes))
	var repositoryLabels []application.LinearLabel
	hasCodex, hasHermes := false, false
	for _, label := range *raw.Labels.Nodes {
		if !validCandidateUUID(label.ID) || strings.TrimSpace(label.Name) == "" {
			return application.LinearTodoCandidate{}, errors.New("Linear candidate contains an incomplete label")
		}
		if _, found := seen[label.ID]; found {
			return application.LinearTodoCandidate{}, errors.New("Linear candidate label appeared more than once")
		}
		seen[label.ID] = struct{}{}
		value := application.LinearLabel{ID: label.ID, Name: label.Name}
		labels = append(labels, value)
		if label.Name == "agent:codex" {
			hasCodex = true
		}
		if label.Name == "agent:hermes" {
			hasHermes = true
		}
		if strings.HasPrefix(label.Name, repositoryLabelPrefix) {
			repositoryLabels = append(repositoryLabels, value)
		}
	}
	if !hasCodex || hasHermes || len(repositoryLabels) == 0 {
		return application.LinearTodoCandidate{}, errors.New("Linear candidate response is outside configured filters")
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].ID < labels[j].ID })
	sort.Slice(repositoryLabels, func(i, j int) bool { return repositoryLabels[i].ID < repositoryLabels[j].ID })
	candidate := application.LinearTodoCandidate{IssueID: raw.ID, Identifier: raw.Identifier, Priority: *raw.Priority,
		State:  application.LinearState{ID: raw.State.ID, Name: raw.State.Name, Type: raw.State.Type},
		Cycle:  application.LinearCycle{ID: raw.Cycle.ID, Number: raw.Cycle.Number, StartsAt: raw.Cycle.StartsAt.UTC(), EndsAt: raw.Cycle.EndsAt.UTC(), IsActive: *raw.Cycle.IsActive},
		Labels: labels, RepositoryLabels: repositoryLabels, BranchName: raw.BranchName,
		SourceRevision: raw.UpdatedAt.UTC().Format(time.RFC3339Nano), CreatedAt: raw.CreatedAt.UTC(), UpdatedAt: raw.UpdatedAt.UTC()}
	candidate.SourceDigest = digestCandidate(candidate)
	return candidate, nil
}

func finalizedCandidateScan(candidates []application.LinearTodoCandidate, observedAt time.Time) application.LinearTodoCandidateScan {
	result := application.LinearTodoCandidateScan{Candidates: append([]application.LinearTodoCandidate(nil), candidates...), ObservedAt: observedAt.UTC()}
	sort.Slice(result.Candidates, func(i, j int) bool { return result.Candidates[i].IssueID < result.Candidates[j].IssueID })
	result.Digest = digestCandidate(result.Candidates)
	return result
}

func digestCandidate(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
