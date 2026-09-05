package observe

// The §9.2 retention assertion, merge-gating: scanner allocation must not
// grow with input size. This is the test that would have caught the
// buffered-body design — a regression here is a privacy regression, not a
// performance one. The scanner and its buffers are warmed BEFORE the
// measurement window so the preallocated state does not swamp the signal;
// absolute RSS belongs in benchmarks, not a unit assertion.

import (
	"strings"
	"testing"
)

func syntheticDoc(contentBytes int) []byte {
	// Obviously-fictional content (testdata/README.md): one long run of 'a'.
	var b strings.Builder
	b.Grow(contentBytes + 256)
	b.WriteString(`{"id":"gen-retention","model":"synthetic/model","choices":[{"message":{"role":"assistant","content":"`)
	b.WriteString(strings.Repeat("a", contentBytes))
	b.WriteString(`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	return []byte(b.String())
}

func TestScannerAllocationDoesNotGrowWithInput(t *testing.T) {
	if testing.Short() {
		t.Skip("64 MiB scan skipped in -short mode")
	}
	s := newScanner(32, 256)

	scanAllocs := func(doc []byte) float64 {
		const chunk = 32 << 10
		return testing.AllocsPerRun(3, func() {
			s.reset()
			for i := 0; i < len(doc); i += chunk {
				s.feed(doc[i:min(i+chunk, len(doc))])
			}
			s.finish()
			r := s.result()
			if r.ID != "gen-retention" || r.Usage.TotalTokens != 3 {
				t.Fatalf("scan broke: %+v", r)
			}
		})
	}

	small := syntheticDoc(1 << 20)  // 1 MiB
	large := syntheticDoc(64 << 20) // 64 MiB

	_ = scanAllocs(small) // warm the scanner and every lazy runtime path
	allocsSmall := scanAllocs(small)
	allocsLarge := scanAllocs(large)

	// A 64× input increase must not produce an input-proportional
	// allocation increase. The captured strings cost a constant handful of
	// allocations per run in both cases.
	if allocsLarge > allocsSmall+4 {
		t.Errorf("allocation grows with input: %.0f allocs at 1MiB, %.0f at 64MiB", allocsSmall, allocsLarge)
	}
}
