package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
)

const moveReservedIssueToStartedMutation = `mutation ControllerMoveReservedIssueToStarted($issueID: String!, $stateID: String!) {
  issueUpdate(id: $issueID, input: { stateId: $stateID }) {
    success
    issue { id state { id name type } }
  }
}`

// MoveReservedIssueToStarted is intentionally the only Linear write exposed by
// this adapter. The application owns the persisted intent and post-write read.
func (c *Client) MoveReservedIssueToStarted(ctx context.Context, mutation application.LinearIssueStartMutation) (application.LinearIssueStartMutationResult, []application.LinearRequestObservation, error) {
	if !validCandidateUUID(mutation.IssueID) || !validCandidateUUID(mutation.TargetStateID) {
		return application.LinearIssueStartMutationResult{}, nil, &application.LinearIssueStartMutationError{Class: "invalid_authority"}
	}
	token, err := c.credentials.Resolve(ctx, c.cfg.CredentialSourceRef)
	if err != nil || strings.TrimSpace(token) == "" {
		return application.LinearIssueStartMutationResult{}, nil, &application.LinearIssueStartMutationError{Class: "credentials"}
	}
	defer clearString(&token)
	result, observation, err := c.moveReservedIssueToStarted(ctx, token, mutation)
	observations := []application.LinearRequestObservation{observation}
	if !isUnauthorizedStartError(err) {
		return result, observations, err
	}
	clearString(&token)
	token, err = c.credentials.Resolve(ctx, c.cfg.CredentialSourceRef)
	if err != nil || strings.TrimSpace(token) == "" {
		return application.LinearIssueStartMutationResult{}, observations, &application.LinearIssueStartMutationError{Class: "unauthorized"}
	}
	result, observation, err = c.moveReservedIssueToStarted(ctx, token, mutation)
	observations = append(observations, observation)
	return result, observations, err
}

func (c *Client) moveReservedIssueToStarted(ctx context.Context, token string, mutation application.LinearIssueStartMutation) (application.LinearIssueStartMutationResult, application.LinearRequestObservation, error) {
	payload, err := json.Marshal(struct {
		Query     string `json:"query"`
		Variables struct {
			IssueID string `json:"issueID"`
			StateID string `json:"stateID"`
		} `json:"variables"`
	}{Query: moveReservedIssueToStartedMutation, Variables: struct {
		IssueID string `json:"issueID"`
		StateID string `json:"stateID"`
	}{IssueID: mutation.IssueID, StateID: mutation.TargetStateID}})
	if err != nil {
		return application.LinearIssueStartMutationResult{}, application.LinearRequestObservation{}, &application.LinearIssueStartMutationError{Class: "encode"}
	}
	req, err := httpRequest(ctx, c.cfg.APIURL, c.authorizationValue(token), payload)
	if err != nil {
		return application.LinearIssueStartMutationResult{}, application.LinearRequestObservation{}, &application.LinearIssueStartMutationError{Class: "request"}
	}
	response, err := c.http.Do(req)
	if err != nil {
		return application.LinearIssueStartMutationResult{}, application.LinearRequestObservation{Operation: "move_reserved_issue_to_started", ErrorClass: "transport", ObservedAt: c.clock.Now().UTC()}, &application.LinearIssueStartMutationError{Class: "transport", Ambiguous: true}
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, c.cfg.MaxResponseBytes+1))
	observation := newObservation(response, body, c.clock.Now())
	observation.Operation = "move_reserved_issue_to_started"
	if readErr != nil {
		observation.ErrorClass = "read_body"
		return application.LinearIssueStartMutationResult{}, observation, &application.LinearIssueStartMutationError{Class: "read_body", Ambiguous: response.StatusCode >= http.StatusInternalServerError}
	}
	if int64(len(body)) > c.cfg.MaxResponseBytes {
		observation.ErrorClass = "response_too_large"
		return application.LinearIssueStartMutationResult{}, observation, &application.LinearIssueStartMutationError{Class: "response_too_large"}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		class, ambiguous := linearStartHTTPError(response.StatusCode)
		observation.ErrorClass = class
		return application.LinearIssueStartMutationResult{}, observation, &application.LinearIssueStartMutationError{Class: class, Ambiguous: ambiguous}
	}
	var decoded moveIssueStartGraphQLResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		observation.ErrorClass = "malformed_json"
		return application.LinearIssueStartMutationResult{}, observation, &application.LinearIssueStartMutationError{Class: "malformed_json"}
	}
	if len(decoded.Errors) != 0 {
		observation.ErrorClass = "graphql"
		return application.LinearIssueStartMutationResult{}, observation, &application.LinearIssueStartMutationError{Class: "graphql"}
	}
	if decoded.Data.IssueUpdate == nil || decoded.Data.IssueUpdate.Success == nil || !*decoded.Data.IssueUpdate.Success || decoded.Data.IssueUpdate.Issue == nil {
		observation.ErrorClass = "partial_mutation"
		return application.LinearIssueStartMutationResult{}, observation, &application.LinearIssueStartMutationError{Class: "partial_mutation"}
	}
	issue := decoded.Data.IssueUpdate.Issue
	if !validCandidateUUID(issue.ID) || !validCandidateUUID(issue.State.ID) || strings.TrimSpace(issue.State.Name) == "" || strings.TrimSpace(issue.State.Type) == "" {
		observation.ErrorClass = "partial_mutation"
		return application.LinearIssueStartMutationResult{}, observation, &application.LinearIssueStartMutationError{Class: "partial_mutation"}
	}
	return application.LinearIssueStartMutationResult{IssueID: issue.ID, State: application.LinearState{ID: issue.State.ID, Name: issue.State.Name, Type: issue.State.Type}}, observation, nil
}

type moveIssueStartGraphQLResponse struct {
	Data struct {
		IssueUpdate *struct {
			Success *bool `json:"success"`
			Issue   *struct {
				ID    string `json:"id"`
				State struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"issueUpdate"`
	} `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

func linearStartHTTPError(status int) (string, bool) {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized", false
	case http.StatusForbidden:
		return "forbidden", false
	case http.StatusNotFound:
		return "not_found", false
	case http.StatusTooManyRequests:
		return "rate_limited", true
	default:
		if status >= http.StatusInternalServerError {
			return "server_error", true
		}
		return fmt.Sprintf("http_%d", status), false
	}
}

func isUnauthorizedStartError(err error) bool {
	var mutationError *application.LinearIssueStartMutationError
	return errors.As(err, &mutationError) && mutationError.Class == "unauthorized"
}
