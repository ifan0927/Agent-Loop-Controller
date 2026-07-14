package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ifan0927/Agent-Loop-Controller/internal/application"
	"github.com/ifan0927/Agent-Loop-Controller/internal/domain"
)

// FindReviewCommentReplies reads a bounded PR-comment listing and retains only
// direct replies to the controller-authorized root. It is not a conversation
// reader: bodies are reduced to the non-secret controller marker digest.
func (c *Client) FindReviewCommentReplies(ctx context.Context, pr, root int64) ([]domain.ReviewReply, error) {
	if pr < 1 || root < 1 {
		return nil, errors.New("pull request and root comment are required")
	}
	var result []domain.ReviewReply
	for page := 1; page <= 20; page++ {
		var raw json.RawMessage
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100&page=%d", c.cfg.RepositoryOwner, c.cfg.RepositoryName, pr, page)
		if err := c.rest(ctx, "review_comment_replies", "GET", path, nil, &raw, true); err != nil {
			return nil, classifyReviewReplyError(err)
		}
		if len(raw) == 0 || raw[0] != '[' {
			return nil, &application.ReviewReplyInconclusiveError{}
		}
		var comments []restReviewComment
		if err := json.Unmarshal(raw, &comments); err != nil {
			return nil, &application.ReviewReplyInconclusiveError{}
		}
		for _, comment := range comments {
			if comment.InReplyToID != root {
				continue
			}
			marker := domain.ReviewReplyMarkerDigest(comment.Body)
			if marker == "" {
				continue
			}
			result = append(result, comment.reviewReply(marker))
		}
		if len(comments) < 100 {
			return result, nil
		}
	}
	return nil, &application.ReviewReplyInconclusiveError{}
}

// ReplyToReviewComment is the sole GitHub comment write capability. The fixed
// body is validated by the application before this adapter is invoked.
func (c *Client) ReplyToReviewComment(ctx context.Context, request application.ReplyToReviewCommentRequest) (domain.ReviewReply, error) {
	if !c.cfg.ReviewCommentsWrite || request.PullRequestNumber < 1 || request.RootCommentID < 1 || domain.ReviewReplyMarkerDigest(request.Body) != request.MarkerDigest {
		return domain.ReviewReply{}, errors.New("review reply capability or request is invalid")
	}
	var comment restReviewComment
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments/%d/replies", c.cfg.RepositoryOwner, c.cfg.RepositoryName, request.PullRequestNumber, request.RootCommentID)
	payload, err := json.Marshal(map[string]string{"body": request.Body})
	if err != nil {
		return domain.ReviewReply{}, errors.New("encode review reply")
	}
	if err := c.rest(ctx, "reply_to_review_comment", "POST", path, bytes.NewReader(payload), &comment, true); err != nil {
		return domain.ReviewReply{}, classifyReviewReplyError(err)
	}
	marker := domain.ReviewReplyMarkerDigest(comment.Body)
	if marker != request.MarkerDigest {
		return domain.ReviewReply{}, errors.New("review reply response marker mismatch")
	}
	return comment.reviewReply(marker), nil
}

func classifyReviewReplyError(err error) error {
	var status *statusError
	if errors.As(err, &status) && (status.status == 403 || status.status == 404) {
		return &application.ReviewReplyRejectedError{}
	}
	return err
}

type restReviewComment struct {
	ID          int64     `json:"id"`
	NodeID      string    `json:"node_id"`
	InReplyToID int64     `json:"in_reply_to_id"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	User        struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Login  string `json:"login"`
		Type   string `json:"type"`
	} `json:"user"`
	PerformedViaGitHubApp *struct {
		ID int64 `json:"id"`
	} `json:"performed_via_github_app"`
}

func (r restReviewComment) reviewReply(marker string) domain.ReviewReply {
	actor := domain.ActorIdentity{DatabaseID: r.User.ID, NodeID: r.User.NodeID, Login: r.User.Login, Type: r.User.Type}
	if r.PerformedViaGitHubApp != nil {
		actor.AppID = r.PerformedViaGitHubApp.ID
	}
	return domain.ReviewReply{DatabaseID: r.ID, NodeID: r.NodeID, ReplyToID: r.InReplyToID, MarkerDigest: marker, Actor: actor, CreatedAt: r.CreatedAt.UTC()}
}
