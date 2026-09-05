package auth

// The participant payout-declaration client (AS `MINIS-VER-014`, ESC-029).
//
// A FIRST declaration takes effect on arrival. A declaration that would
// replace an address already in force does not, and waits for a Slot
// operator. That asymmetry is ESC-031 and it is not arbitrary: an operator
// confirming a participant-declared address has nothing independent to check
// it against, EXCEPT on a change, where they can see the address already
// being paid and be surprised by one that does not match. That is also where
// a stolen refresh token shows up, because theft redirects an address that
// already exists.
//
// So this file has a set and a show and no activate. What the missing
// activate protects is a participant who is already being paid; it never
// protected the first declaration, and pretending it did cost every new
// participant an operator round-trip and a paragraph of documentation
// explaining why "not in force" was normal.
//
// THE ENDPOINT IS CONSTRUCTED, NOT DISCOVERED, and that is a known gap rather
// than a shortcut. The AS's §19 document does not carry these routes yet: the
// shared wire-fixture corpus has no agreed owner and has already drifted, so
// ESC-026 has to settle before either route can be published. Until it does, a
// proxy reaches them only by being configured to know they exist. When the
// fixture gains the fields, this should read them the way providerEndpoint
// reads its template, and the constant below should go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// payoutDeclarationPath is the route the AS serves at (ESC-029). See the file
// comment for why it is a constant and what removes it.
const payoutDeclarationPath = "/v1/payout/declaration"

// PayoutDeclaration is what the AS tells a participant about their own
// proposal. It deliberately carries no operator-facing audit fields.
type PayoutDeclaration struct {
	Status           string `json:"status"`
	Address          string `json:"address"`
	CanonicalAddress string `json:"canonical_address"`
	// Effective is the field to read. A participant who reads "PENDING" as
	// "nearly there" and stops watching is the failure this exists to
	// prevent; the AS answers the question directly so no client has to
	// infer it from a status vocabulary it may not fully know.
	Effective  bool   `json:"effective"`
	DeclaredAt string `json:"declared_at"`
	// HeldFor says why a declaration is NOT in force, and is empty when it
	// is. Absent on an AS that predates ESC-031, which is why nothing here
	// requires it to be set.
	HeldFor string `json:"held_for"`
}

// Hold reasons the AS reports. A client that does not recognize a value
// prints it rather than deciding it means nothing.
const (
	// HeldReplacesActive: an address is already in force for this
	// participant, and an operator compares the two.
	HeldReplacesActive = "REPLACES_ACTIVE"
	// HeldAddressInUse: another participant already holds this address.
	// This one will not be activated by waiting — it needs the participant
	// to act.
	HeldAddressInUse = "ADDRESS_IN_USE"
)

// ErrNoPayoutDeclaration means the participant has not proposed an address.
var ErrNoPayoutDeclaration = errors.New("auth: no payout declaration")

func (m *MiningClient) payoutEndpoint() string {
	return m.discoverer.origin.JoinPath(payoutDeclarationPath).String()
}

// DeclarePayoutAddress proposes a payout destination.
//
// It returns what the AS recorded, which is the thing worth printing: the
// caller learns the canonical rendering the chain gave the address — the one
// an operator will read back — rather than an echo of what was typed.
func (m *MiningClient) DeclarePayoutAddress(ctx context.Context, address string) (*PayoutDeclaration, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, fmt.Errorf("auth: payout address is empty")
	}
	body, err := json.Marshal(map[string]string{"address": address})
	if err != nil {
		return nil, fmt.Errorf("auth: encode declaration: %w", err)
	}
	resp, err := m.authedRequest(ctx, http.MethodPut, m.payoutEndpoint(), body)
	if err != nil {
		return nil, fmt.Errorf("auth: payout declaration request failed: %w", err)
	}
	return decodeDeclaration(resp)
}

// PayoutStanding reads the participant's whole payout state.
func (m *MiningClient) PayoutStanding(ctx context.Context) (*PayoutStanding, error) {
	resp, err := m.authedRequest(ctx, http.MethodGet, m.payoutEndpoint(), nil)
	if err != nil {
		return nil, fmt.Errorf("auth: payout standing request failed: %w", err)
	}
	return decodeStanding(resp)
}

// PayoutStanding is a participant's whole payout state.
//
// It answers "am I set up to be paid", which the old read could not: it
// reported the operator's approval QUEUE, so an actively earning participant
// and one who had never declared got the same empty answer. Both are ordinary
// states and neither is an error.
type PayoutStanding struct {
	// Active is the address in force, or nil.
	Active *PayoutDeclaration
	// Pending is an open proposal, or nil. Both can be set: a paying address
	// with a correction awaiting approval.
	Pending *PayoutDeclaration
}

func decodeStanding(resp *http.Response) (*PayoutStanding, error) {
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read standing response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, joinRefusal(resp.StatusCode, raw)
	}
	var doc struct {
		Active  *PayoutDeclaration `json:"active"`
		Pending *PayoutDeclaration `json:"pending"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("auth: parse standing response: %w", err)
	}
	// An ACTIVE binding is expected to report effective; a PENDING proposal
	// must not. The second is the one worth refusing — telling a participant
	// an unapproved address is live is the failure this whole split exists to
	// prevent — and the first is why the declare path's blanket refusal could
	// not simply be reused here.
	if doc.Pending != nil && doc.Pending.Effective {
		return nil, fmt.Errorf("auth: the AS reported a PENDING proposal as effective; refusing to report an unapproved address as live")
	}
	if doc.Active != nil && !doc.Active.Effective {
		return nil, fmt.Errorf("auth: the AS reported an ACTIVE binding as not effective")
	}
	return &PayoutStanding{Active: doc.Active, Pending: doc.Pending}, nil
}

func decodeDeclaration(resp *http.Response) (*PayoutDeclaration, error) {
	defer drainAndClose(resp.Body)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read declaration response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		// Distinguished from every other refusal, because "you have not
		// declared one" is an answer and the rest are failures.
		return nil, ErrNoPayoutDeclaration
	}
	// 200 means the address is in force; 202 means a human has to act.
	// Both are successes and the difference is the whole answer, so it is
	// read from the body rather than inferred from the code — but a client
	// that saw only the code must not be able to read them alike either,
	// which is why the AS stopped answering 202 for both.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, joinRefusal(resp.StatusCode, raw)
	}
	var doc PayoutDeclaration
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("auth: parse declaration response: %w", err)
	}
	if doc.Address == "" {
		// A document that does not say which address it is about cannot be
		// shown to a participant: the whole value of printing it back is
		// that they check the canonical rendering against what they meant,
		// and a blank line invites them to assume it was fine.
		return nil, fmt.Errorf("auth: the AS returned a payout declaration naming no address")
	}
	// A first declaration IS effective, so effectiveness is no longer
	// refusable on its own — the guard that did refuse it predates ESC-031
	// and would have made every new participant's setup fail. What is still
	// refusable is a document that contradicts itself, because a client
	// deciding which half to believe is how a participant gets told the
	// wrong thing about where their money goes.
	if doc.Effective && doc.HeldFor != "" {
		return nil, fmt.Errorf("auth: the AS reported a payout declaration as in force AND held for %q", doc.HeldFor)
	}
	if doc.Effective != (doc.Status == statusActive) {
		return nil, fmt.Errorf("auth: the AS reported status %q with effective=%v; refusing to guess which is true", doc.Status, doc.Effective)
	}
	return &doc, nil
}

// statusActive is the AS's status vocabulary for a binding in force. The
// client reads `effective` and not this — the AS answers the question
// directly so no client has to know the vocabulary — and cross-checks the two
// only to catch a document that disagrees with itself.
const statusActive = "ACTIVE"
