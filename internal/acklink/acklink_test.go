package acklink

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	u := URL("https://status.example.com/", "s3cret-s3cret-s3cret-s3cret-s3cret", 42)
	tok := strings.TrimPrefix(u, "https://status.example.com/ack/")
	if id, ok := Verify("s3cret-s3cret-s3cret-s3cret-s3cret", tok); !ok || id != 42 {
		t.Fatalf("%s → %d %v", u, id, ok)
	}
	for _, bad := range []string{"43." + strings.SplitN(tok, ".", 2)[1], tok + "x", "42", "x.y", ""} {
		if _, ok := Verify("s3cret-s3cret-s3cret-s3cret-s3cret", bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, ok := Verify("other-secret", tok); ok {
		t.Error("accepted with another secret")
	}
	if URL("", "s", 1) != "" || URL("https://x", "", 1) != "" {
		t.Error("links without config")
	}
}
