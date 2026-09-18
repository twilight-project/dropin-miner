// Package trajectory is a proof of concept, not a feature. It reads a coding
// host's own on-disk session record into typed turns so that the findings
// document can say, with numbers, what is there and how much of it can be
// tied to a search this client actually made. Nothing here ships: goreleaser
// builds ./cmd/dropin-miner only, and nothing under cmd/dropin-miner or pkg/
// imports this package.
//
// Three rules hold for every file in it.
//
// It only reads. There is no write path into a host's directory, a config or
// the network, and no import of net/http. Every file is opened through an
// os.Root on the directory the caller named, so a symbolic link inside that
// directory cannot lead a read outside it, and a path a transcript merely
// mentions is never opened at all.
//
// It declares the shapes it understands and refuses the rest. Claude Code
// calls its transcript format internal and changes it between versions, so an
// entry type, a content block or a layout this package has not declared is a
// Refusal — counted, naming the entry type and the file — and never a guess.
// A refusal inside a turn marks that turn, so a consumer can leave it out.
//
// It counts from parsed structure. A search is a tool call whose command
// parses, as a shell command, to this client's binary run with its search
// subcommand; a request id is a field of the JSON document the tool returned.
// Neither is a substring match: the skill's own text contains the search
// command, so a transcript that only loaded the skill would otherwise look
// like a transcript full of searches. The one substring count this package
// keeps exists to measure that error, and is named for what it is.
package trajectory
