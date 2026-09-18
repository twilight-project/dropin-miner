package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/twilight-project/dropin-miner/internal/trajectory"
)

// measure prints the numbers the findings are written from and creates
// nothing. It takes no output path because it has no output to place: unlike
// emit, which opens one file, this subcommand's whole result is the report on
// standard output. It reads the directories it is named and no others.
//
// It needs the scrubber, and so the machine's own name, home directory and
// environment, for two of its counts: what the scrubber would catch, and
// which home directories in the corpus belong to somebody else. Those go in;
// no value of them comes out, here or anywhere.
func measure(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("measure", flag.ContinueOnError)
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
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "trajectory-poc: no home directory, so the scrubber cannot be told what this machine is: %v\n", err)
		return exitError
	}
	hostname, _ := os.Hostname()
	m, err := trajectory.Measure(fs.Arg(0), idx, trajectory.NewScrubber(os.Environ(), hostname, home))
	if err != nil {
		fmt.Fprintf(stderr, "trajectory-poc: %v\n", err)
		return exitError
	}
	if _, err := m.WriteTo(stdout); err != nil {
		return exitError
	}
	return exitOK
}
