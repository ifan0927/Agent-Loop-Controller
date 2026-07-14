package domain

import (
	"strings"
	"testing"
	"time"
)

func trustedFeedbackFixture() TrustedReviewFeedback {
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	line := 7
	return TrustedReviewFeedback{PRNumber: 1, PRDatabaseID: 2, PRNodeID: "PR_2", ReviewDatabaseID: 3, ReviewNodeID: "REVIEW_3", ThreadNodeID: "THREAD_4", RootCommentDatabaseID: 5, RootCommentNodeID: "COMMENT_5", Author: ActorIdentity{DatabaseID: 6, NodeID: "USER_6", Login: "ifan0927", Type: "User"}, OriginalReviewHeadSHA: strings.Repeat("a", 40), Path: "internal/example.go", Line: &line, Body: "please fix this", SourceAt: now, ObservedAt: now}
}

func TestTrustedReviewFeedbackObservationBounds(t *testing.T) {
	valid := trustedFeedbackFixture()
	if err := valid.ValidateObservation(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*TrustedReviewFeedback){
		func(v *TrustedReviewFeedback) { v.Author.Type = "Bot" }, func(v *TrustedReviewFeedback) { v.OriginalReviewHeadSHA = "short" }, func(v *TrustedReviewFeedback) { v.Path = "../escape" }, func(v *TrustedReviewFeedback) { v.Body = "x\x00y" }, func(v *TrustedReviewFeedback) { v.Body = string(make([]byte, MaxTrustedReviewFeedbackBodyBytes+1)) }, func(v *TrustedReviewFeedback) { v.SourceAt = time.Time{} },
	} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.ValidateObservation(); err == nil {
			t.Fatalf("invalid feedback accepted: %+v", candidate)
		}
	}
}

func TestTrustedReviewFeedbackObservationIdentityAndLocationBoundaryMatrix(t *testing.T) {
	valid := trustedFeedbackFixture()
	assertValid := func(name string, feedback TrustedReviewFeedback) {
		t.Helper()
		if err := feedback.ValidateObservation(); err != nil {
			t.Fatalf("%s rejected: %v", name, err)
		}
	}
	assertInvalid := func(name string, mutate func(*TrustedReviewFeedback)) {
		t.Helper()
		candidate := valid
		mutate(&candidate)
		if err := candidate.ValidateObservation(); err == nil {
			t.Fatalf("%s was accepted: %+v", name, candidate)
		}
	}

	for _, field := range []struct {
		name   string
		mutate func(*TrustedReviewFeedback, int64)
	}{
		{"pr_number", func(v *TrustedReviewFeedback, id int64) { v.PRNumber = id }},
		{"pr_database_id", func(v *TrustedReviewFeedback, id int64) { v.PRDatabaseID = id }},
		{"review_database_id", func(v *TrustedReviewFeedback, id int64) { v.ReviewDatabaseID = id }},
		{"root_comment_database_id", func(v *TrustedReviewFeedback, id int64) { v.RootCommentDatabaseID = id }},
		{"author_database_id", func(v *TrustedReviewFeedback, id int64) { v.Author.DatabaseID = id }},
	} {
		for _, invalidID := range []int64{0, -1} {
			field, invalidID := field, invalidID
			assertInvalid(field.name, func(v *TrustedReviewFeedback) { field.mutate(v, invalidID) })
		}
	}
	for _, field := range []struct {
		name string
		set  func(*TrustedReviewFeedback, string)
	}{
		{"pr_node_id", func(v *TrustedReviewFeedback, value string) { v.PRNodeID = value }},
		{"review_node_id", func(v *TrustedReviewFeedback, value string) { v.ReviewNodeID = value }},
		{"thread_node_id", func(v *TrustedReviewFeedback, value string) { v.ThreadNodeID = value }},
		{"root_comment_node_id", func(v *TrustedReviewFeedback, value string) { v.RootCommentNodeID = value }},
		{"author_node_id", func(v *TrustedReviewFeedback, value string) { v.Author.NodeID = value }},
	} {
		for _, invalidNode := range []string{"", "\x00"} {
			field, invalidNode := field, invalidNode
			assertInvalid(field.name, func(v *TrustedReviewFeedback) { field.set(v, invalidNode) })
		}
	}
	assertInvalid("observed_at", func(v *TrustedReviewFeedback) { v.ObservedAt = time.Time{} })

	withoutLocation := valid
	withoutLocation.Path, withoutLocation.Line = "", nil
	assertValid("nullable path and line", withoutLocation)
	withoutLine := valid
	withoutLine.Line = nil
	assertValid("nullable line", withoutLine)
	for _, line := range []int{-1, 0} {
		line := line
		assertInvalid("non-positive line", func(v *TrustedReviewFeedback) { v.Line = &line })
	}
	line := 1
	assertInvalid("line without path", func(v *TrustedReviewFeedback) { v.Path, v.Line = "", &line })
	for _, unsafe := range []string{".", "..", "../escape", "/absolute", "~/.secret", "dir\\file", "a/../b", "contains\x00nul", " padded "} {
		unsafe := unsafe
		assertInvalid("unsafe path", func(v *TrustedReviewFeedback) { v.Path, v.Line = unsafe, nil })
	}
}

func TestTrustedReviewFeedbackObservedRejectsFutureLifecycleEvidence(t *testing.T) {
	valid := trustedFeedbackFixture()
	for _, mutate := range []func(*TrustedReviewFeedback){
		func(v *TrustedReviewFeedback) { v.BoundRepairHead = strings.Repeat("b", 40) },
		func(v *TrustedReviewFeedback) { v.ReplyIntentKey = "reply-intent" },
		func(v *TrustedReviewFeedback) { v.ReplyDatabaseID, v.ReplyNodeID = 9, "REPLY_9" },
		func(v *TrustedReviewFeedback) { v.Resolved = true },
		func(v *TrustedReviewFeedback) { v.Outdated = true },
	} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.ValidateObservation(); err == nil {
			t.Fatalf("observed feedback accepted future evidence: %+v", candidate)
		}
	}
}

func TestTrustedReviewFeedbackLifecycleIsClosed(t *testing.T) {
	legal := [][2]TrustedReviewFeedbackLifecycle{
		{TrustedReviewFeedbackObserved, TrustedReviewFeedbackSelectedForRepair},
		{TrustedReviewFeedbackSelectedForRepair, TrustedReviewFeedbackRepairVerified},
		{TrustedReviewFeedbackRepairVerified, TrustedReviewFeedbackReplyPending},
		{TrustedReviewFeedbackReplyPending, TrustedReviewFeedbackReplied},
		{TrustedReviewFeedbackReplied, TrustedReviewFeedbackResolved},
		{TrustedReviewFeedbackObserved, TrustedReviewFeedbackSuperseded},
		{TrustedReviewFeedbackSelectedForRepair, TrustedReviewFeedbackSuperseded},
		{TrustedReviewFeedbackRepairVerified, TrustedReviewFeedbackSuperseded},
		{TrustedReviewFeedbackReplyPending, TrustedReviewFeedbackSuperseded},
		{TrustedReviewFeedbackReplied, TrustedReviewFeedbackSuperseded},
	}
	for _, transition := range legal {
		if err := ValidateTrustedReviewFeedbackTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("legal %v: %v", transition, err)
		}
	}
	for _, transition := range [][2]TrustedReviewFeedbackLifecycle{
		{TrustedReviewFeedbackObserved, TrustedReviewFeedbackObserved},
		{TrustedReviewFeedbackSelectedForRepair, TrustedReviewFeedbackSelectedForRepair},
		{TrustedReviewFeedbackRepairVerified, TrustedReviewFeedbackRepairVerified},
		{TrustedReviewFeedbackReplyPending, TrustedReviewFeedbackReplyPending},
		{TrustedReviewFeedbackReplied, TrustedReviewFeedbackReplied},
		{TrustedReviewFeedbackResolved, TrustedReviewFeedbackResolved},
		{TrustedReviewFeedbackSuperseded, TrustedReviewFeedbackSuperseded},
		{TrustedReviewFeedbackObserved, TrustedReviewFeedbackReplied},
		{TrustedReviewFeedbackResolved, TrustedReviewFeedbackSuperseded},
		{TrustedReviewFeedbackResolved, TrustedReviewFeedbackObserved},
		{TrustedReviewFeedbackSuperseded, TrustedReviewFeedbackResolved},
		{TrustedReviewFeedbackSuperseded, TrustedReviewFeedbackObserved},
	} {
		if err := ValidateTrustedReviewFeedbackTransition(transition[0], transition[1]); err == nil {
			t.Fatalf("illegal %v accepted", transition)
		}
	}
}
