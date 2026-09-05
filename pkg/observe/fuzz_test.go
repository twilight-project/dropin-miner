package observe

// FuzzStructuralScanner: the invariant is no panic, no unbounded
// allocation, no hang — not output correctness (§9.5). Generated corpora
// are never committed (**/testdata/fuzz/ is ignored); the seeds below are
// the reviewed corpus.

import "testing"

func FuzzStructuralScanner(f *testing.F) {
	f.Add([]byte(`{"id":"gen-1","model":"m","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	f.Add([]byte(`{"choices":[{"delta":{"content":"x"},"finish_reason":null}]}`))
	f.Add([]byte(`{"error":{"type":"t","code":"c","message":"m"}}`))
	f.Add([]byte(`{"id":"gen","nested":{"a":[1,2,{"b":"c"}]}}`))
	f.Add([]byte("\xEF\xBB\xBF{}"))
	f.Add([]byte(`[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[`))
	f.Add([]byte(`{"a":"😀"}`))
	f.Add([]byte(`{"usage":{"prompt_tokens":184467440737095516150}}`))
	f.Add([]byte(`{"a`))
	f.Add([]byte(`123 456`))

	f.Fuzz(func(t *testing.T, data []byte) {
		s := newScanner(32, 256)
		// Feed in size-varying chunks so incremental state is exercised.
		step := 1 + len(data)%97
		for i := 0; i < len(data); i += step {
			s.feed(data[i:min(i+step, len(data))])
		}
		s.finish()
		_ = s.result()
	})
}

// FuzzSearchProfileScanner runs the same invariant over the search
// whitelist, whose extra machinery — array index tracking, the positional
// candidate table, the one signed integer — is the part the completion
// profile never exercises.
func FuzzSearchProfileScanner(f *testing.F) {
	f.Add([]byte(`{"request_id":"req-1","chosen":1,"candidates":[{"provider":"a"},{"provider":"b"}]}`))
	f.Add([]byte(`{"chosen":-1,"candidates":[]}`))
	f.Add([]byte(`{"chosen":99999999999999999999,"candidates":[{"provider":"a"}]}`))
	f.Add([]byte(`{"candidates":[[{"provider":"a"}],{"provider":"b"}],"chosen":0}`))
	f.Add([]byte(`{"candidates":{"0":{"provider":"a"}},"chosen":0}`))
	f.Add([]byte(`{"chosen":-,"candidates":[`))
	f.Add([]byte(`{"request_id":"req-1","request_id":"req-2","chosen":0,"chosen":3}`))
	f.Add([]byte(`{"candidates":[{"provider":"a","answer":"…"},{"provider":`))

	f.Fuzz(func(t *testing.T, data []byte) {
		s := newScannerFor(searchWhitelist, 32, 256)
		step := 1 + len(data)%97
		for i := 0; i < len(data); i += step {
			s.feed(data[i:min(i+step, len(data))])
		}
		s.finish()
		r := s.result()
		// The positional table is fixed size, so a resolved provider can
		// only ever be one the document actually named at a position the
		// table holds.
		if r.ChosenProvider != "" && (r.Chosen < 0 || r.Chosen >= maxCandidateSlots) {
			t.Fatalf("provider resolved from outside the table: chosen=%d provider=%q", r.Chosen, r.ChosenProvider)
		}
	})
}
