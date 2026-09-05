// Package scope holds the Participation Context the forwarding path
// snapshots at request start (contract §4.2, §32). It is deliberately
// tiny and dependency-free: `internal/forward` may import it — the only
// mining package it may ever reach — so this package must never import
// anything from the auth or mining planes, and its read path must be
// O(1), lock-free, and allocation-free.
package scope

import (
	"sync/atomic"
	"time"
)

// Context is one immutable participation snapshot. A nil *Context means
// "not participating right now", which is always a valid state: the
// inference path never waits for, or fails on, mining context.
type Context struct {
	SlotID      uint64
	TargetEpoch uint64
	// Capability is the opaque Participation Capability (§28: treated
	// as opaque by the proxy, never parsed).
	Capability string
	// ExpiresAt is the capability's own expiry; Deadline is the
	// target's observation submission deadline (§33). Activity observed
	// while the capability was valid stays attributable even if the
	// capability expires mid-inference.
	ExpiresAt time.Time
	Deadline  time.Time
}

// Valid reports whether the snapshot may still be used for submission
// at time now.
func (c *Context) Valid(now time.Time) bool {
	return c != nil && c.Capability != "" && now.Before(c.ExpiresAt)
}

// Holder publishes the current context to the request path.
type Holder struct {
	current atomic.Pointer[Context]
}

// Snapshot is the request-start read: a single atomic load, no lock, no
// allocation. Returns nil when mining is not currently participating.
func (h *Holder) Snapshot() *Context { return h.current.Load() }

// Set publishes a new context (control plane only).
func (h *Holder) Set(c *Context) { h.current.Store(c) }

// Clear drops participation (revocation, epoch rollover, shutdown).
func (h *Holder) Clear() { h.current.Store(nil) }
