package wire

// enrollment_authorization_template (§37.2, TWILIGHT_MINING_PROXY_AS_V1_12).
//
// The corpus now carries the field. Until it did, this suite's subject was
// the safety of the gap between the struct field and the fixture — a
// document WITHOUT the key had to validate and round-trip byte-identically,
// or the two mirrors would have drifted during the window. That window is
// closed, and the tests that guarded it have been rewritten rather than
// deleted: absence is still a legal state, because an AS that does not offer
// the door omits the key, so the property is still worth holding. What has
// changed is that the fixture can no longer be the thing that demonstrates
// it.

import (
	"encoding/json"
	"strings"
	"testing"
)

// withoutTemplate is the fixture as an AS that does not offer the door would
// serve it. Derived from the corpus rather than hand-written, so it stays
// the corpus document in every other respect.
func withoutTemplate(t *testing.T) *DiscoveryDocument {
	t.Helper()
	var doc DiscoveryDocument
	decodeStrict(t, readFixture(t, "discovery_document.json"), &doc)
	doc.EnrollmentAuthorizationTemplate = ""
	return &doc
}

// The corpus carries the template, and this is the assertion that says so —
// so a mirror that silently reverted to the older document fails here rather
// than by making some other suite mysteriously green.
func TestTheCorpusCarriesTheEnrollmentAuthorizationTemplate(t *testing.T) {
	var doc DiscoveryDocument
	decodeStrict(t, readFixture(t, "discovery_document.json"), &doc)

	if doc.EnrollmentAuthorizationTemplate == "" {
		t.Fatal("the shared fixture no longer carries enrollment_authorization_template; " +
			"either the mirror is stale or the corpus lost the field at its source")
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("the corpus document does not validate: %v", err)
	}
	// The one placeholder the client substitutes, and the method the AS
	// publishes. internal/auth refuses a template missing either; asserting
	// it here means a corpus change that broke the door is caught in the
	// package that owns the document, not only in the package that consumes
	// it.
	for _, want := range []string{"{code_challenge}", "code_challenge_method=S256", "https://"} {
		if !strings.Contains(doc.EnrollmentAuthorizationTemplate, want) {
			t.Errorf("the corpus template is missing %q: %s", want, doc.EnrollmentAuthorizationTemplate)
		}
	}
}

// Absence remains legal: an AS that predates the door, or does not offer it,
// omits the key, and that is not a malformed document. The field is not in
// Validate and must not become so.
func TestEnrollmentAuthorizationTemplateIsOptional(t *testing.T) {
	doc := withoutTemplate(t)

	if err := doc.Validate(); err != nil {
		t.Fatalf("a document without the template must still validate — the door being "+
			"unoffered is not a malformed document: %v", err)
	}
}

// omitempty still matters, for the same reason it mattered before the corpus
// changed: the other mirror may serve a document without the key, and a
// client that re-encoded one with an empty string present would not be
// reproducing what it received.
func TestEnrollmentAuthorizationTemplateIsOmittedWhenAbsent(t *testing.T) {
	out, err := json.Marshal(withoutTemplate(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "enrollment_authorization_template") {
		t.Errorf("an empty template was encoded as a key; a document that lacked it would "+
			"not round-trip byte-identically:\n%s", out)
	}
}

// And the corpus document round-trips whole, which is the L3 obligation the
// rest of the fixtures are held to.
func TestEnrollmentAuthorizationTemplateSurvivesRoundTrip(t *testing.T) {
	data := readFixture(t, "discovery_document.json")
	var doc DiscoveryDocument
	decodeStrict(t, data, &doc)
	roundTrip(t, data, &doc)

	out, err := json.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	var back DiscoveryDocument
	decodeStrict(t, out, &back)

	// The template is the one advertised value carrying a literal
	// placeholder, so any helpful normalization of it would be silent
	// breakage.
	if back.EnrollmentAuthorizationTemplate != doc.EnrollmentAuthorizationTemplate {
		t.Errorf("template did not survive the round trip:\n  got  %q\n  want %q",
			back.EnrollmentAuthorizationTemplate, doc.EnrollmentAuthorizationTemplate)
	}
}
