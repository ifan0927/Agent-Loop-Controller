package domain

import (
	"errors"
	"strings"
)

// GitHubUserIdentity is the complete immutable human identity used by
// Controller authorization. A login alone is never sufficient authority.
type GitHubUserIdentity struct {
	Login      string `json:"login"`
	DatabaseID int64  `json:"database_id"`
	NodeID     string `json:"node_id"`
	ActorType  string `json:"actor_type"`
}

func (i GitHubUserIdentity) Validate() error {
	if strings.TrimSpace(i.Login) == "" || i.DatabaseID < 1 || strings.TrimSpace(i.NodeID) == "" || i.ActorType != "User" {
		return errors.New("GitHub User identity is incomplete")
	}
	if strings.ContainsRune(i.Login, '\x00') || strings.ContainsRune(i.NodeID, '\x00') {
		return errors.New("GitHub User identity is invalid")
	}
	return nil
}

func (i GitHubUserIdentity) Equal(other GitHubUserIdentity) bool {
	return i.Validate() == nil && other.Validate() == nil &&
		i.DatabaseID == other.DatabaseID && i.NodeID == other.NodeID &&
		strings.EqualFold(i.Login, other.Login) && i.ActorType == other.ActorType
}
