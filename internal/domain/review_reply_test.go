package domain

import (
	"strings"
	"testing"
)

func TestReviewReplyMarkerAndFixedBodyAreDeterministic(t *testing.T) {
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digest := strings.Repeat("b", 64)
	marker, markerDigest, err := ReviewReplyMarker("run-1", 7, "THREAD_7", 9, "COMMENT_9", digest, head)
	if err != nil || markerDigest == "" || ReviewReplyMarkerDigest(marker) != markerDigest {
		t.Fatalf("marker=%q digest=%q err=%v", marker, markerDigest, err)
	}
	body, err := ReviewReplyBody(head, marker)
	if err != nil || !strings.Contains(body, "Addressed in "+head+".") || !strings.HasSuffix(body, marker) || strings.Contains(body, "COMMENT_9") || strings.Contains(body, "THREAD_7") {
		t.Fatalf("body=%q err=%v", body, err)
	}
	_, changed, err := ReviewReplyMarker("run-2", 7, "THREAD_7", 9, "COMMENT_9", digest, head)
	if err != nil || changed == markerDigest {
		t.Fatalf("changed=%q err=%v", changed, err)
	}
}
