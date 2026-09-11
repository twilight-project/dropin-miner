package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryAfterUsesProvidedClock(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"3600", time.Hour}, {now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0}, {"garbage", 0}, {"", 0},
	} {
		if got := retryAfterAt(tc.value, now); got != tc.want {
			t.Fatalf("%q: %v want %v", tc.value, got, tc.want)
		}
	}
}
