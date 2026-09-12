package auth

// The AS's refusals, as values rather than as prose.
//
// These errors cross a package boundary and callers act on them: the
// flush decides whether a participant is told their authorization needs
// attention or that delivery failed. That decision used to be made by
// matching substrings of the text below, which made the wording an API —
// one nobody had agreed to, and one that a rephrasing would have broken
// silently. The type is the contract now, so these tests pin two separate
// things: that the wording operators already read is byte-for-byte what it
// was, and that the behavior callers branch on comes from the value.

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestASRefusedErrorTextIsUnchanged(t *testing.T) {
	for name, tc := range map[string]struct {
		err  *ASRefusedError
		want string
	}{
		"status only": {
			&ASRefusedError{Status: http.StatusUnauthorized},
			"auth: AS refused with status 401",
		},
		"coded": {
			&ASRefusedError{Status: http.StatusForbidden, Code: "SLOT_CLOSED", Message: "slot 3 is not accepting joins"},
			"auth: AS refused (403 SLOT_CLOSED): slot 3 is not accepting joins",
		},
		"enrollment conflict": {
			&ASRefusedError{Status: http.StatusConflict, Code: "ENROLLMENT_CONFLICT", Message: "epoch 1042 is held"},
			"auth: AS refused (409 ENROLLMENT_CONFLICT): epoch 1042 is held: " + ErrEnrollmentConflict.Error(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("text changed:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The conflict is the one code callers branch on, and it stayed an
// errors.Is match through the change from %w to Unwrap.
func TestASRefusedErrorUnwrapsOnlyTheConflict(t *testing.T) {
	conflict := &ASRefusedError{Status: http.StatusConflict, Code: "ENROLLMENT_CONFLICT", Message: "held"}
	if !errors.Is(conflict, ErrEnrollmentConflict) {
		t.Error("ENROLLMENT_CONFLICT no longer matches ErrEnrollmentConflict")
	}
	// Still true through a caller's own wrapping.
	if !errors.Is(fmt.Errorf("join epoch 1042: %w", conflict), ErrEnrollmentConflict) {
		t.Error("the conflict stopped matching once wrapped")
	}
	for name, err := range map[string]*ASRefusedError{
		"other code":  {Status: http.StatusForbidden, Code: "SLOT_CLOSED", Message: "closed"},
		"status only": {Status: http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			if errors.Is(err, ErrEnrollmentConflict) {
				t.Error("a refusal that is not a conflict matched ErrEnrollmentConflict")
			}
		})
	}
}

// joinRefusal is what actually builds these from the wire, so the shapes
// above are the shapes that reach a caller.
func TestJoinRefusalReturnsTypedRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   ASRefusedError
	}{
		"envelope": {
			http.StatusForbidden,
			`{"error":{"code":"SLOT_CLOSED","message":"slot 3 is not accepting joins"}}`,
			ASRefusedError{Status: http.StatusForbidden, Code: "SLOT_CLOSED", Message: "slot 3 is not accepting joins"},
		},
		"conflict": {
			http.StatusConflict,
			`{"error":{"code":"ENROLLMENT_CONFLICT","message":"epoch 1042 is held"}}`,
			ASRefusedError{Status: http.StatusConflict, Code: "ENROLLMENT_CONFLICT", Message: "epoch 1042 is held"},
		},
		// No envelope, or one we cannot read: the status is all the AS
		// actually told us, and inventing a code would be worse than
		// saying only that.
		"no envelope":   {http.StatusUnauthorized, ``, ASRefusedError{Status: http.StatusUnauthorized}},
		"not json":      {http.StatusInternalServerError, `<html>502</html>`, ASRefusedError{Status: http.StatusInternalServerError}},
		"empty code":    {http.StatusBadRequest, `{"error":{"code":"","message":"nope"}}`, ASRefusedError{Status: http.StatusBadRequest}},
		"no error node": {http.StatusNotFound, `{"detail":"missing"}`, ASRefusedError{Status: http.StatusNotFound}},
	} {
		t.Run(name, func(t *testing.T) {
			var refused *ASRefusedError
			err := joinRefusal(tc.status, []byte(tc.body))
			if !errors.As(err, &refused) {
				t.Fatalf("joinRefusal returned an untyped error: %v", err)
			}
			if *refused != tc.want {
				t.Errorf("refusal = %+v, want %+v", *refused, tc.want)
			}
			if got, want := refused.Error(), tc.want.Error(); got != want {
				t.Errorf("text:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// The missing-authorization sentinel is matched by identity, not text.
func TestErrNoRefreshAuthorizationIsASentinel(t *testing.T) {
	const want = "auth: no refresh authorization; interactive authorization required"
	if got := ErrNoRefreshAuthorization.Error(); got != want {
		t.Errorf("text changed:\n got %q\nwant %q", got, want)
	}
	if !errors.Is(fmt.Errorf("refresh: %w", ErrNoRefreshAuthorization), ErrNoRefreshAuthorization) {
		t.Error("the sentinel stopped matching once wrapped")
	}
	if errors.Is(errors.New(want), ErrNoRefreshAuthorization) {
		t.Error("a different error with the same text matched the sentinel; the match is textual")
	}
}
