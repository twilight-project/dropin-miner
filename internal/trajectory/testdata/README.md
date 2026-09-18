# Fixtures

Everything under `projects/` is synthetic. Each transcript was written by hand to the shapes
`shapes.go` declares — entry types, keys and content-block types taken from a structure-only
survey of real files (key names, type names and counts; no values) — and none of it was copied
from a real session. The rule exists because a transcript is somebody's private work, and a
fixture is published with the repository.

Every string a person or a model could have written starts with `ZEBRA-`, including the planted
host account id in the `bridge-session` entry, the attachment body and the subagent sidecar's
description. That marker is how the tests prove that none of it reaches an output: a test that
greps the output for `ZEBRA-` fails on the first leak of any of them.

`emit/` holds the fixtures for the levels, the gates and the scrubber, apart from `projects/`
because the reader's counts are pinned to that directory. Same rules: synthetic, marked, one file
per case. Session `…21` is a turn with three searches, a fetch of a page the first one offered and
an answer that cites a page the second one offered, which between them carry all six origin
classes; its first search's command line carries a trace bridge, which must never be emitted.
Session `…22` plants one of everything the scrubber acts on — an email address, a credential-shaped
string, the value of an environment secret, the machine's name, and home paths of three accounts —
with words on either side of each so that a cut, as opposed to an omission, would show. Session
`…23` is a search interrupted in a second workspace, for which no test writes a consent record.
None of the planted values is a real credential, address or host.

`emitdup/` is one turn read twice, which is what a resumed session looks like on disk: `…32` is a
transcript that copied `…31`'s entries verbatim — the same uuids, still carrying the session id
they were written under — and then held a turn of its own. Two files, three turns read, two turns.
It is apart from `emit/` because that directory's record count is pinned to it.

One file per case in `projects/`, so a failure names its case:

| Session (last characters) | Case |
|---|---|
| `…0a` | a plain turn, among bookkeeping entries the reader skips knowingly |
| `…0b` | a turn with two searches, in a transcript where the search command's text appears seven times and runs twice |
| `…0c` | two main turns; a subagent with its own search, linked to the second; a fork whose sidecar names a call the parent never made |
| `…0d` | a turn interrupted while its search was running, then an attachment that belongs to no turn, then a second turn |
| `…0e` | a compaction, and its model-written summary, inside one turn |
| `…0f` | an entry type, a content-block type and a system subtype nobody declared |
| `…10` | a final line cut mid-document, with no newline — keep it that way when editing |
