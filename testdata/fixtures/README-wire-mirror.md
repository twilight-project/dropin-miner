# Wire fixtures — MIRROR, do not edit

Authored in `tokendrop-auth-server-design/docs/wire-fixtures/` and copied here
verbatim, checksums included. `ADR-X-0001` (ACCEPTED 2026-08-26) records why the
corpus lives there and not here.

**A change to a fixture is a specification change under D1 §15, not a fixture edit.**
Amend `docs/spec/` first, then the corpus, then refresh this mirror in the commit that
consumes the new behaviour — copying both the `.json` files and `SHA256SUMS` rather than
regenerating them. A mirror that regenerates its own checksums is verifying a copy against
itself, which always passes and proves nothing.

Editing a fixture here to make a test pass is how `ESC-026` happened: the two copies drifted
by one field, and nothing noticed for two phases because nothing was watching. `ESC-030` is
what that drift turned out to be hiding — a proxy-visible field that no specification
defined, invisible precisely because a tolerant consumer ignores what it does not recognize.

`TestFixtureChecksums` (in `internal/wire`) fails if these files and `SHA256SUMS` disagree.
It cannot tell you that both were changed together to something upstream does not have; the
rule above is what covers that, and the rule is the point.
