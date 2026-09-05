// Package draw holds the proxy's Selection identity: custody of the local
// participation_secret and the frozen DrawIDV1 derivation (Participant
// Selection V1 r5 §36–§38), plus the CandidateSetHashV1 construction and
// CandidateList canonicality checks a self-verifying proxy needs (r5 §19,
// §39).
//
// This package is stdlib-only and deliberately tiny: DrawIDV1 is the one
// place Twilight-specific cryptographic composition is unavoidable
// (ADR-0009), and it is proven byte-for-byte against the checksum-pinned
// revision-2 golden vectors on every PR (VEC-001/VEC-002; ADR-X-0003). The
// vector file is frozen — any change to it is a canonical protocol change,
// never a local fix.
//
// The participation_secret is exactly 32 CSPRNG bytes. It MUST remain local,
// MUST NOT be transmitted to the AS or appear in any log, diagnostic, or
// redaction output, and MAY be retained across epochs — the derivation
// already binds (chain_id, slot_id, target_epoch).
package draw
