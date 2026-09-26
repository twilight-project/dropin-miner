// Command trajectory-poc is the driver for the trajectory proof of concept.
// It is not part of the product: goreleaser builds ./cmd/dropin-miner only,
// the installers build the same one path, and nothing imports this. It reads
// the directories it is given and writes to its own standard output.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/twilight-project/dropin-miner/internal/trajectory"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "scan":
		return scan(args[1:], stdout, stderr)
	case "emit":
		return emit(args[1:], stderr)
	case "measure":
		return measure(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "trajectory-poc: unknown subcommand %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: trajectory-poc scan [-lineage dir] <transcripts-dir>")
	fmt.Fprintln(w, "  scan prints counts only. It prints no content at any setting, reads only the")
	fmt.Fprintln(w, "  directories named, and writes nothing but this output.")
	fmt.Fprintln(w, "usage: trajectory-poc emit [-config file] <transcripts-dir> <output-file>")
	fmt.Fprintln(w, "  emit writes one JSON line per turn that contains a search. With no config and no")
	fmt.Fprintln(w, "  consent record it writes level 1: hashed ids and request ids, no text. Every gate")
	fmt.Fprintln(w, "  is closed until the compiled ceiling, the config (default ~/.trajectory-poc/config.json)")
	fmt.Fprintln(w, "  and a consent record (~/.trajectory-poc/consent/<workspace-hash>.json) all agree to it;")
	fmt.Fprintln(w, "  both files are written by hand, never by this tool. The output file must not exist.")
	fmt.Fprintln(w, "usage: trajectory-poc measure [-lineage dir] <transcripts-dir>")
	fmt.Fprintln(w, "  measure prints the numbers the findings are written from — the anchoring gap by")
	fmt.Fprintln(w, "  reason, what a record would cost at each level, what the scrubber would catch, the")
	fmt.Fprintln(w, "  reader's refusals, and what a turn holds that nobody consented to share, by category.")
	fmt.Fprintln(w, "  It creates no file and writes no record: the report is the whole of its output.")
}

// scan prints counts. Content is not retained by the reader it drives
// (Options.KeepContent stays false), so there is none here to print.
func scan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	lineage := fs.String("lineage", "", "the client's sessions directory, for anchoring path 2 (ids only)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		usage(stderr)
		return exitUsage
	}
	var idx *trajectory.LineageIndex
	if *lineage != "" {
		var err error
		if idx, err = trajectory.LoadLineage(*lineage); err != nil {
			fmt.Fprintf(stderr, "trajectory-poc: %v\n", err)
			return exitError
		}
	}
	counts := trajectory.NewCounts(idx)
	err := trajectory.Walk(fs.Arg(0), trajectory.Options{}, func(s *trajectory.Session) { counts.Add(s, idx) })
	if err != nil {
		fmt.Fprintf(stderr, "trajectory-poc: %v\n", err)
		return exitError
	}
	if _, err := counts.WriteTo(stdout); err != nil {
		return exitError
	}
	return exitOK
}
