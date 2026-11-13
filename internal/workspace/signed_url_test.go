package workspace

import (
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/images"
)

func TestReusableTakesASignedURLOnlyEarlyInItsWindow(t *testing.T) {
	signed := time.Date(2026, 11, 12, 18, 4, 5, 0, time.UTC)
	parse := func(query string) images.PayloadURL {
		u, err := images.ParsePayloadURL("https://bucket.s3.us-west-2.amazonaws.com/tools.sqfs?" + query)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	fifteen := parse("X-Amz-Date=20261112T180405Z&X-Amz-Expires=900")
	eleven := parse("X-Amz-Date=20261112T180405Z&X-Amz-Expires=660")
	tests := []struct {
		name string
		url  images.PayloadURL
		at   time.Time
		want bool
	}{
		{"just signed", fifteen, signed.Add(time.Second), true},
		{"before the reuse window ends", fifteen, signed.Add(signedURLReuse - time.Second), true},
		{"after the reuse window", fifteen, signed.Add(signedURLReuse), false},
		{"too close to its expiry", eleven, signed.Add(2 * time.Minute), false},
		{"unsigned", parse("v=1"), signed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reusable(tt.url, tt.at); got != tt.want {
				t.Errorf("reusable at %v = %v, want %v", tt.at, got, tt.want)
			}
		})
	}
}
