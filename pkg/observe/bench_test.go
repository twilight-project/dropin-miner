package observe

// §9.6 benchmarks — reported and tracked, gating nothing. A regression is a
// discussion, not a red build.

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func BenchmarkScannerNonStreaming(b *testing.B) {
	doc := syntheticDoc(1 << 20)
	s := newScanner(32, 256)
	b.SetBytes(int64(len(doc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.reset()
		s.feed(doc)
		s.finish()
	}
}

func BenchmarkSSEFramer(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "data: {\"id\":\"gen-bench\",\"choices\":[{\"delta\":{\"content\":\"%s\"},\"finish_reason\":null}]}\n\n",
			strings.Repeat("b", 256))
	}
	sb.WriteString("data: {\"id\":\"gen-bench\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
	sb.WriteString("data: [DONE]\n\n")
	stream := []byte(sb.String())

	b.SetBytes(int64(len(stream)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := newSSEParser(32, 256, 1<<20, 100_000)
		obsv := &Observation{}
		p.consume(stream)
		if !p.terminated() {
			b.Fatal("stream did not terminate")
		}
		_ = p.finalize(false, nil, obsv)
	}
}

// The copy-path overhead of the tap itself, with a live consumer draining.
func BenchmarkTapCopyOverhead(b *testing.B) {
	const size = 1 << 20
	payload := strings.Repeat("t", size)
	pool := newRingPool(256<<10, 256<<10)
	m := &Metrics{}

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := newObs(pool.tryGet(), pool)
		tp := newTap(io.NopCloser(strings.NewReader(payload)), o, m)
		done := make(chan struct{})
		go func() { // draining consumer
			defer close(done)
			for {
				o.drainInto(func([]byte) {})
				if o.done.Load() && o.tail.Load() == o.head.Load() {
					break
				}
			}
			o.setConsumerDone()
			o.tryRelease()
		}()
		if _, err := io.Copy(io.Discard, tp); err != nil {
			b.Fatal(err)
		}
		_ = tp.Close()
		<-done
	}
}

func BenchmarkPlainCopyBaseline(b *testing.B) {
	const size = 1 << 20
	payload := strings.Repeat("t", size)
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := io.Copy(io.Discard, strings.NewReader(payload)); err != nil {
			b.Fatal(err)
		}
	}
}
