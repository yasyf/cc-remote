package images

import (
	"testing"
	"time"
)

func TestParsePayloadURLReadsTheSignatureWindow(t *testing.T) {
	signedAt := time.Date(2026, 11, 12, 18, 4, 5, 0, time.UTC)
	tests := []struct {
		name    string
		query   string
		ok      bool
		expires time.Time
	}{
		{"signed", "X-Amz-Date=20261112T180405Z&X-Amz-Expires=900&X-Amz-Signature=ab", true, signedAt.Add(15 * time.Minute)},
		{"no expiry", "X-Amz-Date=20261112T180405Z&X-Amz-Signature=ab", false, time.Time{}},
		{"no date", "X-Amz-Expires=900&X-Amz-Signature=ab", false, time.Time{}},
		{"a zero expiry", "X-Amz-Date=20261112T180405Z&X-Amz-Expires=0", false, time.Time{}},
		{"unsigned", "v=1", false, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := "https://bucket.s3.us-west-2.amazonaws.com/tools.sqfs?" + tt.query
			u, err := ParsePayloadURL(raw)
			if err != nil {
				t.Fatal(err)
			}
			signed, expires, ok := u.Signed()
			if ok != tt.ok || !expires.Equal(tt.expires) || (ok && !signed.Equal(signedAt)) {
				t.Errorf("Signed() = %v, %v, %v; want %v, %v", signed, expires, ok, tt.expires, tt.ok)
			}
		})
	}
}
