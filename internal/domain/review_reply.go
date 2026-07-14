package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const reviewReplyMarkerPrefix = "ifan-loop-review-reply:v1:"

// ReviewReply is the minimum remote evidence needed to reconcile one reply.
// Its body is not retained: callers compare only a controller marker digest.
type ReviewReply struct {
	DatabaseID   int64
	NodeID       string
	ReplyToID    int64
	MarkerDigest string
	Actor        ActorIdentity
	CreatedAt    time.Time
}

func ReviewReplyMarker(runID string, pr int64, thread string, root int64, rootNode, bodyDigest, head string) (string, string, error) {
	if strings.TrimSpace(runID) == "" || pr < 1 || strings.TrimSpace(thread) == "" || root < 1 || strings.TrimSpace(rootNode) == "" || len(bodyDigest) != 64 || !fullSHA(head) {
		return "", "", errors.New("review reply marker authority is incomplete")
	}
	material := fmt.Sprintf("v1\x00%s\x00%d\x00%s\x00%d\x00%s\x00%s\x00%s", runID, pr, thread, root, rootNode, bodyDigest, head)
	sum := sha256.Sum256([]byte(material))
	digest := hex.EncodeToString(sum[:])
	return "<!-- " + reviewReplyMarkerPrefix + digest + " -->", digest, nil
}

func ReviewReplyBody(head, marker string) (string, error) {
	if !fullSHA(head) || !strings.HasPrefix(marker, "<!-- "+reviewReplyMarkerPrefix) || !strings.HasSuffix(marker, " -->") {
		return "", errors.New("review reply body authority is invalid")
	}
	return "Addressed in " + head + ". Controller verification, required checks, and a fresh independent review passed for this exact head. Please review and resolve this conversation when satisfied.\n\n" + marker, nil
}

func ReviewReplyMarkerDigest(body string) string {
	start := strings.LastIndex(body, "<!-- "+reviewReplyMarkerPrefix)
	if start < 0 {
		return ""
	}
	value := strings.TrimSuffix(strings.TrimPrefix(body[start:], "<!-- "+reviewReplyMarkerPrefix), " -->")
	if len(value) != 64 {
		return ""
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return ""
		}
	}
	return value
}
