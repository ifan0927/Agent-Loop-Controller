package domain

import "testing"

func TestGitHubUserIdentityRequiresCompleteImmutableUserTuple(t *testing.T) {
	valid := GitHubUserIdentity{Login: "I-Fan", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, identity := range map[string]GitHubUserIdentity{
		"login":       {DatabaseID: 7, NodeID: "U_7", ActorType: "User"},
		"database ID": {Login: "ifan", NodeID: "U_7", ActorType: "User"},
		"node ID":     {Login: "ifan", DatabaseID: 7, ActorType: "User"},
		"actor type":  {Login: "ifan", DatabaseID: 7, NodeID: "U_7", ActorType: "Bot"},
	} {
		t.Run(name, func(t *testing.T) {
			if identity.Validate() == nil {
				t.Fatal("incomplete identity was accepted")
			}
		})
	}
	if !valid.Equal(GitHubUserIdentity{Login: "i-fAN", DatabaseID: 7, NodeID: "U_7", ActorType: "User"}) {
		t.Fatal("GitHub login comparison should be case-insensitive")
	}
	if valid.Equal(GitHubUserIdentity{Login: "I-Fan", DatabaseID: 8, NodeID: "U_7", ActorType: "User"}) {
		t.Fatal("lookalike immutable identity was accepted")
	}
}
