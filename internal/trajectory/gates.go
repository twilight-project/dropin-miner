package trajectory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Gate names one thing a record may carry beyond ids. The five content
// classes are named after Claude Code's own telemetry switches, so an
// operator who knows one knows the other — but unlike that host's, these do
// not lean on each other: there, the assistant class falls back to the prompt
// class and the raw-bodies class implies the rest. Here a gate is decided
// from its own three inputs and from nothing else, so opening one can never
// open another.
type Gate string

const (
	// GateLevel2 permits outcome labels about our own results.
	GateLevel2 Gate = "level_2"
	// GateLevel3 permits the turn's other events. It is necessary for every
	// content class and sufficient for none: with it open and the classes
	// closed, a record gains event kinds and sizes and no text.
	GateLevel3 Gate = "level_3"

	GatePrompts     Gate = "prompts"      // the participant's prompt text
	GateResponses   Gate = "responses"    // the model's prose
	GateToolDetails Gate = "tool_details" // tool parameters, a search's query
	GateToolContent Gate = "tool_content" // what tools returned, search answers included
	// GateRawBodies exists for the vocabulary's sake. A transcript holds no
	// API bodies, the compiled ceiling keeps it shut, and no code reads it.
	GateRawBodies Gate = "raw_bodies"

	GateHostAndModel Gate = "host_and_model" // host name, host version, model
	GateWorkspace    Gate = "workspace"      // the workspace, as a hash
	// GateUpload is not built in phase 1: it is closed by the ceiling and
	// there is nothing behind it. It is listed so that a config or a consent
	// record that tries to open it is answered, by name, with a no.
	GateUpload Gate = "upload"
)

// AllGates is every gate, in the order they are reported.
var AllGates = []Gate{
	GateLevel2, GateLevel3, GatePrompts, GateResponses, GateToolDetails, GateToolContent,
	GateRawBodies, GateHostAndModel, GateWorkspace, GateUpload,
}

// PolicyVersion names the terms a config and a consent record were written
// against. It is the ninth name in the brief's list and is not a switch: it
// is the condition every switch shares. A config or a consent record that
// states another version opens nothing, because agreement to one set of terms
// is not agreement to the next.
const PolicyVersion = "poc-phase1"

// compiledCeiling is the first of the three agreements: what this binary is
// able to emit at all, whatever a file says.
var compiledCeiling = map[Gate]bool{
	GateLevel2: true, GateLevel3: true, GatePrompts: true, GateResponses: true,
	GateToolDetails: true, GateToolContent: true, GateHostAndModel: true, GateWorkspace: true,
	GateRawBodies: false, GateUpload: false,
}

// Why a gate is closed. More than one may hold.
const (
	BlockedByCeiling         = "ceiling"
	BlockedByConfig          = "config"
	BlockedByConsent         = "consent"
	BlockedByPolicyVersion   = "policy_version"
	BlockedByConfigLocation  = "config_inside_workspace"
	BlockedByConsentLocation = "consent_inside_workspace"
)

// Settings is the shape of both files an operator writes by hand: the config
// for this tool, and one consent record per workspace. This code never writes
// either.
type Settings struct {
	V             int           `json:"v"`
	PolicyVersion string        `json:"policy_version"`
	WorkspaceHash string        `json:"workspace_hash,omitempty"` // consent records only
	Gates         map[Gate]bool `json:"gates"`
}

const settingsVersion = 1

// GateSet is the decision for one workspace: for each gate, the reasons it is
// closed. A gate is open exactly when it has none.
type GateSet map[Gate][]string

// Open reports whether gate, and only gate, is open.
func (g GateSet) Open(gate Gate) bool {
	blockers, decided := g[gate]
	return decided && len(blockers) == 0
}

// OpenGates lists the open gates in reporting order.
func (g GateSet) OpenGates() []Gate {
	var open []Gate
	for _, gate := range AllGates {
		if g.Open(gate) {
			open = append(open, gate)
		}
	}
	return open
}

// decide applies the three-way agreement, gate by gate. Nothing in the loop
// body reads any gate but the one it is deciding.
func decide(ceiling map[Gate]bool, config, consent *Settings, configBlock, consentBlock string) GateSet {
	set := GateSet{}
	for _, gate := range AllGates {
		blockers := []string{}
		if !ceiling[gate] {
			blockers = append(blockers, BlockedByCeiling)
		}
		blockers = append(blockers, fileBlockers(config, gate, BlockedByConfig, configBlock)...)
		blockers = append(blockers, fileBlockers(consent, gate, BlockedByConsent, consentBlock)...)
		set[gate] = dedupe(blockers)
	}
	return set
}

// fileBlockers says why one file does not agree to one gate.
func fileBlockers(s *Settings, gate Gate, absent, location string) []string {
	switch {
	case location != "":
		return []string{location}
	case s == nil:
		return []string{absent}
	case s.PolicyVersion != PolicyVersion:
		return []string{BlockedByPolicyVersion}
	case !s.Gates[gate]:
		return []string{absent}
	}
	return nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Policy holds the operator's config and knows where consent records live.
type Policy struct {
	Config     *Settings
	ConfigPath string
	// ConsentDir is a directory in the participant's home, never in a
	// workspace. Four independent vendors refuse to let a project file open a
	// content class, because a cloned repository would otherwise carry its
	// own consent; a record found inside the workspace it speaks for is
	// refused for the same reason.
	ConsentDir string

	cache map[string]GateSet
}

// LoadPolicy reads the config. A missing file is no config, which is not an
// error: it is the default, and it opens nothing.
func LoadPolicy(configPath, consentDir string) (*Policy, error) {
	p := &Policy{ConfigPath: configPath, ConsentDir: consentDir, cache: map[string]GateSet{}}
	if configPath == "" {
		return p, nil
	}
	s, err := loadSettings(configPath)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", configPath, err)
	}
	p.Config = s
	return p, nil
}

// ConsentPath is where the consent record for a workspace is looked for.
func (p *Policy) ConsentPath(cwd string) string {
	return filepath.Join(p.ConsentDir, WorkspaceHash(cwd)+".json")
}

// WorkspaceHash keys a workspace the way the client keys its lineage files,
// so a listing of the consent directory reveals no project paths.
func WorkspaceHash(cwd string) string {
	return TraceHash("workspace|" + filepath.Clean(cwd))
}

// For decides the gates for one workspace. An unreadable or malformed consent
// record is no consent.
func (p *Policy) For(cwd string) GateSet {
	if set, ok := p.cache[cwd]; ok {
		return set
	}
	var consent *Settings
	configBlock, consentBlock := "", ""
	if cwd != "" {
		if p.ConfigPath != "" && inside(p.ConfigPath, cwd) {
			configBlock = BlockedByConfigLocation
		}
		if p.ConsentDir != "" {
			if inside(p.ConsentDir, cwd) {
				consentBlock = BlockedByConsentLocation
			} else if s, err := loadSettings(p.ConsentPath(cwd)); err == nil && s != nil && s.WorkspaceHash == WorkspaceHash(cwd) {
				// A record copied from another workspace names that one's
				// hash, and agrees to nothing here.
				consent = s
			}
		}
	}
	set := decide(compiledCeiling, p.Config, consent, configBlock, consentBlock)
	p.cache[cwd] = set
	return set
}

// loadSettings reads one settings file. nil, nil means it does not exist. A
// gate name nobody declared is an error, not a no-op: a typo that silently
// opened nothing would be found late, and one that some later version gave a
// meaning to would be found later still.
func loadSettings(path string) (*Settings, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- a path the operator named, or one derived under their home
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.V != settingsVersion {
		return nil, fmt.Errorf("v is %d, this reads %d", s.V, settingsVersion)
	}
	known := map[Gate]bool{}
	for _, gate := range AllGates {
		known[gate] = true
	}
	var unknown []string
	for gate := range s.Gates {
		if !known[gate] {
			unknown = append(unknown, string(gate))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown gate(s): %s", strings.Join(unknown, ", "))
	}
	return &s, nil
}

// inside reports whether path is dir or lies under it.
func inside(path, dir string) bool {
	p, err1 := filepath.Abs(path)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return true // cannot tell: refuse
	}
	rel, err := filepath.Rel(d, p)
	if err != nil {
		return false // different volumes: not inside
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
