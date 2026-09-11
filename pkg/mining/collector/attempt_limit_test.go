package collector

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExplicitZeroUnlimitedAndDefaultFinite(t *testing.T) {
	for _, limit := range []int{0, DefaultMaxAttempts} {
		sp, sub, _ := testEnv(t)
		c := New(sp, sub, Options{MaxAttempts: limit})
		id := enqueue(t, sp, c, 1)
		sub.answers[id] = func(int) (bool, bool, time.Duration, error) { return false, false, 0, errors.New("unavailable") }
		for i := 1; i <= 51; i++ {
			records, err := sp.Pending()
			if err != nil {
				t.Fatal(err)
			}
			if len(records) == 0 {
				if limit != 50 || i != 51 {
					t.Fatalf("limit=%d prematurely exhausted at attempt %d", limit, i)
				}
				break
			}
			c.deliver(context.Background(), records[0])
		}
		n, err := sp.Count()
		if err != nil {
			t.Fatal(err)
		}
		if (limit == 0 && n != 1) || (limit == 50 && n != 0) {
			t.Fatalf("limit=%d remaining=%d", limit, n)
		}
		want := 51
		if limit == 50 {
			want = 50
		}
		if got := sub.count(id); got != want {
			t.Fatalf("limit=%d submitted=%d want=%d", limit, got, want)
		}
	}
}
