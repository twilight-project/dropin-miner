package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/twilight-project/dropin-miner/internal/trajectory"
)

// stateDirName is this tool's own directory in the participant's home: the
// operator's config and the consent records. It is read, never written.
const stateDirName = ".trajectory-poc"

// hostDirNames are directories in a home that belong to a coding host or to
// the shipping client. emit refuses to put its output in any of them: the one
// thing this work writes must never land where a host would read it back.
var hostDirNames = []string{".claude", ".codex", ".cursor", ".gemini", ".hermes", ".pi", ".config", ".tokendrop"}

// emit writes records to the one file the operator named, and reports on
// stderr what it wrote and what the gates withheld. Nothing goes to stdout.
func emit(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("emit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configFlag := fs.String("config", "", "the operator's config for this tool (default ~/"+stateDirName+"/config.json)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 2 {
		usage(stderr)
		return exitUsage
	}
	dir, output := fs.Arg(0), fs.Arg(1)

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "trajectory-poc: no home directory, so nowhere a consent record could be: %v\n", err)
		return exitError
	}
	stateDir := filepath.Join(home, stateDirName)
	configPath := *configFlag
	if configPath == "" {
		configPath = filepath.Join(stateDir, "config.json")
	}
	if why := outputRefusal(output, dir, home, stateDir); why != "" {
		fmt.Fprintf(stderr, "trajectory-poc: refusing to write %s: %s\n", output, why)
		return exitUsage
	}
	policy, err := trajectory.LoadPolicy(configPath, filepath.Join(stateDir, "consent"))
	if err != nil {
		fmt.Fprintf(stderr, "trajectory-poc: %v\n", err)
		return exitError
	}
	hostname, _ := os.Hostname()
	scrubber := trajectory.NewScrubber(os.Environ(), hostname, home)

	// The only file this work creates. O_EXCL: it never replaces anything.
	out, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator's own output path, checked above
	if err != nil {
		fmt.Fprintf(stderr, "trajectory-poc: %v\n", err)
		return exitError
	}
	stats, emitErr := trajectory.Emit(dir, policy, scrubber, out)
	closeErr := out.Close()
	if emitErr != nil || closeErr != nil {
		fmt.Fprintf(stderr, "trajectory-poc: the output is incomplete and was left in place: %v %v\n", emitErr, closeErr)
		return exitError
	}
	if policy.Config == nil {
		fmt.Fprintf(stderr, "no config at %s\n", configPath)
	}
	if _, err := stats.WriteTo(stderr); err != nil {
		return exitError
	}
	return exitOK
}

// outputRefusal says why output may not be written, or "" when it may.
func outputRefusal(output, transcripts, home, stateDir string) string {
	abs, err := filepath.Abs(output)
	if err != nil {
		return "its path cannot be resolved"
	}
	if within(abs, transcripts) {
		return "it is inside the directory being read"
	}
	if within(abs, stateDir) {
		return "it is inside this tool's config and consent directory"
	}
	for _, name := range hostDirNames {
		if within(abs, filepath.Join(home, name)) {
			return "it is inside " + filepath.Join("~", name) + ", which belongs to a host or to the client"
		}
	}
	return ""
}

// within reports whether abs is dir or lies under it. A directory that
// cannot be resolved is treated as containing it: when unsure, refuse.
func within(abs, dir string) bool {
	d, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(d, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
