package main

// #106 and #125: our marked block is cut out only when it can be vouched for.
//
// Whose it is, what is in it, and what follows it — hermes_install.go's
// removeOurHermesBlock, asked by uninstall and by install's refresh alike.
// The files Hermes is said to have written are testdata/hermes/resave.py's
// real output; what a participant typed into them is typed here, because that
// is what it is.

import (
	"fmt"
	"strings"
	"testing"
)

func hermesPlanUninstall(ops agentOps, entry binEntry) agentPlan {
	var p agentPlan
	hermesTarget{}.PlanUninstall(ops, agentPaths{hermesConfig: hermesConfigPath, hermesSkill: hermesSkillPath}, entry, noEnv, &p)
	return p
}

// hermesLineOf is the 1-based line of the one line of config that holds text.
func hermesLineOf(t *testing.T, config, text string) int {
	t.Helper()
	found := 0
	for i, l := range strings.Split(config, "\n") {
		if strings.Contains(l, text) {
			if found != 0 {
				t.Fatalf("%q is on more than one line", text)
			}
			found = i + 1
		}
	}
	if found == 0 {
		t.Fatalf("%q is on no line of:\n%s", text, config)
	}
	return found
}

// Two installations sharing a binary share this one block in this one file —
// the second was set up by running the first's copy. #73 fixed that for every
// host but this one: uninstalling the disposable installation took the hook the
// real one relies on, and `agents install` from it did the same and then wrote
// its own.
func TestAnotherInstallationsHermesBlockIsLeftAndNamed(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		owner := tc.entry
		others := map[string]binEntry{
			"sharing the binary":   {command: owner.command, cfg: "/tmp/disposable/tokendrop.toml"},
			"another binary":       {command: "/somewhere/else/bin/dropin-miner", cfg: owner.cfg},
			"running on no config": {command: owner.command},
		}
		for _, fixture := range []string{".input.yaml", ".roundtrip.yaml"} {
			config := readHermesFixture(t, name+fixture)
			for otherName, other := range others {
				t.Run(name+fixture+"/"+otherName, func(t *testing.T) {
					m, ops := newFakeMachine("hermes")
					m.files[hermesConfigPath] = []byte(config)

					if hermesHookInstalledFor(ops, hermesConfigPath, other, tc.windows) {
						t.Error("status counted another installation's hook as this one's")
					}

					uninstall := hermesPlanUninstall(ops, other)
					if len(uninstall.writes) != 0 {
						t.Fatalf("uninstall planned to remove a block that is another installation's:\n%s", uninstall.writes[0].contents)
					}
					for _, want := range []string{"left the dropin-miner pre_tool_call hook block", "another installation", owner.cfg} {
						if notes := strings.Join(uninstall.notes, "\n"); !strings.Contains(notes, want) {
							t.Errorf("uninstall did not say %q:\n%s", want, notes)
						}
					}

					var install agentPlan
					if planHermesHookFor(ops, "Hermes", hermesConfigPath, other, tc.windows, &install) || len(install.writes) != 0 {
						t.Fatalf("install planned to replace a block that is another installation's:\n%s", install.writes[0].contents)
					}
					// A sentence, not a refusal: there is nothing to paste
					// beside a hooks: key that is already taken, and the
					// participant did nothing wrong. S20 drives the same case
					// through the real command and requires exit 0.
					if len(install.refused) != 0 {
						t.Errorf("install refused, and named nothing the participant could do:\n%s", strings.Join(install.refused, "\n"))
					}
					notes := strings.Join(install.notes, "\n")
					for _, want := range []string{"another installation", owner.cfg, "was not added"} {
						if !strings.Contains(notes, want) {
							t.Errorf("install did not say %q:\n%s", want, notes)
						}
					}
				})
			}

			// And the installation it does belong to still takes it out whole.
			t.Run(name+fixture+"/its owner", func(t *testing.T) {
				m, ops := newFakeMachine("hermes")
				m.files[hermesConfigPath] = []byte(config)
				uninstall := hermesPlanUninstall(ops, owner)
				if len(uninstall.writes) != 1 {
					t.Fatalf("the owner's uninstall planned %d writes; notes: %v", len(uninstall.writes), uninstall.notes)
				}
				// For what install wrote, that is the file install was handed,
				// its missing final newline included. After Hermes has saved
				// it, it is every line Hermes left outside our block.
				want := tc.above
				if fixture != ".input.yaml" {
					want = hermesAroundOurBlock(t, config)
				}
				if got := string(uninstall.writes[0].contents); got != want {
					t.Errorf("the surrounding file is not byte for byte what it was\n--- got ---\n%q\n--- want ---\n%q", got, want)
				}
			})
		}
	}
}

// Anything a participant typed between the markers used to go with the block,
// unannounced — from uninstall, and from install's refresh. It is kept, and the
// plan names the line.
func TestAParticipantsLineInsideOurHermesBlockIsNamedAndKept(t *testing.T) {
	entry := hermesResavedEntries["posix"].entry
	matcher := hermesHookLines("")[3] + "\n"
	for _, fixture := range []string{"posix.input.yaml", "posix.roundtrip.yaml"} {
		original := readHermesFixture(t, fixture)
		for name, tc := range map[string]struct {
			typed string // the line, as it is named
			// comment: the line changes nothing YAML reads, so the entry is
			// still today's and install has nothing to do and nothing to say.
			// Only uninstall would delete it, and only uninstall names it.
			comment bool
			edit    func(string) string
		}{
			"a further key in our entry": {"timeout: 5", false, func(s string) string { return strings.Replace(s, matcher, matcher+"      timeout: 5\n", 1) }},
			"a comment":                  {"# mine, keep", true, func(s string) string { return strings.Replace(s, matcher, matcher+"# mine, keep\n", 1) }},
			"a comment above hooks:":     {"# mine, keep", true, func(s string) string { return strings.Replace(s, "hooks:\n", "# mine, keep\nhooks:\n", 1) }},
			"a second entry":             {"- command: /usr/bin/mine", false, func(s string) string { return strings.Replace(s, matcher, matcher+"    - command: /usr/bin/mine\n", 1) }},
			"another event":              {"post_tool_call: []", false, func(s string) string { return strings.Replace(s, matcher, matcher+"  post_tool_call: []\n", 1) }},
		} {
			t.Run(fixture+"/"+name, func(t *testing.T) {
				config := tc.edit(original)
				if config == original {
					t.Fatal("the edit did not apply")
				}
				where := fmt.Sprintf("line %d (%s)", hermesLineOf(t, config, tc.typed), tc.typed)
				m, ops := newFakeMachine("hermes")
				m.files[hermesConfigPath] = []byte(config)

				uninstall := hermesPlanUninstall(ops, entry)
				if len(uninstall.writes) != 0 {
					t.Fatalf("uninstall planned a write that takes the participant's line:\n%s", uninstall.writes[0].contents)
				}
				if notes := strings.Join(uninstall.notes, "\n"); !strings.Contains(notes, where) || !strings.Contains(notes, "is not a line dropin-miner writes") {
					t.Errorf("uninstall did not name %s:\n%s", where, notes)
				}
				if got := strings.Join(uninstall.skipped, "\n"); strings.Contains(got, "not installed") {
					t.Errorf("uninstall says both that our block is there and that Hermes is not installed:\n%s", got)
				}

				// Install: our hook is in that block and runs, so nothing has
				// failed — a sentence, not a refusal — and nothing is written.
				var install agentPlan
				if planHermesHookFor(ops, "Hermes", hermesConfigPath, entry, false, &install) || len(install.writes) != 0 {
					t.Fatalf("install planned a write that takes the participant's line:\n%s", install.writes[0].contents)
				}
				if len(install.refused) != 0 {
					t.Errorf("install refused, though this installation's hook is there and runs:\n%s", strings.Join(install.refused, "\n"))
				}
				notes := strings.Join(install.notes, "\n")
				switch {
				case tc.comment && notes != "":
					t.Errorf("install has something to say about a block whose entry is today's:\n%s", notes)
				case tc.comment:
				case !strings.Contains(notes, where) || !strings.Contains(notes, "left the dropin-miner pre_tool_call hook block"):
					t.Errorf("install did not name %s as what it left:\n%s", where, notes)
				case !strings.Contains(notes, "Hermes goes on running it"):
					// Our hook is in that block and fires; only its spelling is
					// not today's. "was not added" would send someone to fix a
					// hook that works.
					t.Errorf("install said our hook was not added, though it is there and runs:\n%s", notes)
				}
			})
		}
	}
}

// #125. Hermes' ruamel writer keeps our end marker attached to our matcher
// line, so what it adds to the hooks: mapping our block opened lands after the
// end marker and is still inside our mapping. Status and install are right
// about these files and stay right; uninstall used to cut marker to marker and
// leave a config.yaml that does not parse.
func TestWhatContinuesOurMappingAfterTheEndMarkerKeepsTheBlock(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		for suffix, added := range map[string]string{
			".roundtrip-sibling.yaml": "post_tool_call:",
			".roundtrip-item.yaml":    "- command: /usr/bin/other-hook",
		} {
			lf := readHermesFixture(t, name+suffix)
			for eol, config := range map[string]string{"lf": lf, "crlf": hermesAsWindowsSavesIt(lf)} {
				t.Run(name+suffix+"/"+eol, func(t *testing.T) {
					m, ops := newFakeMachine("hermes")
					m.files[hermesConfigPath] = []byte(config)

					if !hermesHookInstalledFor(ops, hermesConfigPath, tc.entry, tc.windows) {
						t.Error("status: Hermes reads our entry and runs it, and it is not counted")
					}
					var install agentPlan
					if planHermesHookFor(ops, "Hermes", hermesConfigPath, tc.entry, tc.windows, &install) || len(install.writes)+len(install.refused)+len(install.notes) != 0 {
						t.Errorf("install has something to do or to say about a block that holds today's entry: %+v", install)
					}

					uninstall := hermesPlanUninstall(ops, tc.entry)
					if len(uninstall.writes) != 0 {
						t.Fatalf("uninstall cut our block out from above what Hermes added to it:\n%s", uninstall.writes[0].contents)
					}
					where := fmt.Sprintf("line %d (%s)", hermesLineOf(t, config, added), added)
					if notes := strings.Join(uninstall.notes, "\n"); !strings.Contains(notes, where) || !strings.Contains(notes, "continues the hooks: mapping") {
						t.Errorf("uninstall did not name %s as what continues our mapping:\n%s", where, notes)
					}
					if got := strings.Join(uninstall.skipped, "\n"); strings.Contains(got, "not installed") {
						t.Errorf("uninstall says both that our block is there and that Hermes is not installed:\n%s", got)
					}
				})
			}
		}
	}
}

// The same guard runs before install's refresh cut. The block here is this
// installation's in v0.2.9's spelling, so install wants to rewrite it; the
// lines after it are the real writer's.
func TestInstallDoesNotRefreshABlockWhoseMappingHermesContinued(t *testing.T) {
	entry := hermesResavedEntries["posix"].entry
	stale := `"` + entry.command + `" hook -config "` + entry.cfg + `" hermes pre_tool_call`
	if !hermesCommandIsOurHook(stale, refFor(entry)) {
		t.Fatal("the stale spelling is not this installation's under H5, so this case tests nothing")
	}
	for _, suffix := range []string{".roundtrip-sibling.yaml", ".roundtrip-item.yaml"} {
		t.Run(suffix, func(t *testing.T) {
			fixture := readHermesFixture(t, "posix"+suffix)
			tail := fixture[strings.Index(fixture, agentsMarkerEnd+"\n")+len(agentsMarkerEnd)+1:]
			config := string(hermesAppendBlock([]byte("model: gpt\n"), strings.Join(hermesHookLines(stale), "\n")+"\n")) + tail

			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(config)
			var install agentPlan
			if planHermesHookFor(ops, "Hermes", hermesConfigPath, entry, false, &install) || len(install.writes) != 0 {
				t.Fatalf("install cut the block out from above what Hermes added to it:\n%s", install.writes[0].contents)
			}
			if got := strings.Join(append(install.notes, install.refused...), "\n"); !strings.Contains(got, "continues the hooks: mapping") {
				t.Errorf("install did not say why it left the block:\n%s", got)
			}
		})
	}
}

// Install's note says the participant's file ended without a newline, and
// uninstall used to take two newlines off the text before the block on its
// say-so — wherever the block was. Once Hermes has put a key of its own after
// the block, that joins the participant's last line to it: `model: gptdisplay:`.
func TestUninstallDoesNotJoinTheParticipantsLastLineToWhatHermesAddedBelow(t *testing.T) {
	entry := hermesResavedEntries["posix-noeol"].entry
	lf := readHermesFixture(t, "posix-noeol.roundtrip.yaml")
	if !strings.Contains(lf, hermesNoEOLNote+"\n") || !strings.HasSuffix(lf, "personality: kawaii\n") {
		t.Fatalf("the fixture is not our note inside the block and Hermes' key after it:\n%s", lf)
	}
	for eol, config := range map[string]string{"lf": lf, "crlf": hermesAsWindowsSavesIt(lf)} {
		t.Run(eol, func(t *testing.T) {
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(config)
			uninstall := hermesPlanUninstall(ops, entry)
			if len(uninstall.writes) != 1 {
				t.Fatalf("uninstall planned %d writes; notes: %v", len(uninstall.writes), uninstall.notes)
			}
			got := string(uninstall.writes[0].contents)
			if strings.Contains(got, "gptdisplay") {
				t.Fatalf("the participant's last line was joined to Hermes' key:\n%q", got)
			}
			if want := hermesAroundOurBlock(t, config); got != want {
				t.Errorf("the surrounding file is not byte for byte what it was\n--- got ---\n%q\n--- want ---\n%q", got, want)
			}
		})
	}

	// With nothing after the block the note still means what it says, in
	// either line ending: the file goes back to having no final newline.
	input := readHermesFixture(t, "posix-noeol.input.yaml")
	for eol, config := range map[string]string{"lf": input, "crlf": hermesAsWindowsSavesIt(input)} {
		t.Run("still the end of the file/"+eol, func(t *testing.T) {
			cut := removeOurHermesBlock([]byte(config), refFor(entry))
			if !cut.had || cut.why != "" || string(cut.next) != "model: gpt" {
				t.Errorf("had %v, left because %q, and what remains is %q, want the participant's one line with no newline", cut.had, cut.why, cut.next)
			}
		})
	}
}

// On Windows every line Hermes saves ends in CRLF, our marker lines among
// them. The block is found, read, left alone by install, and comes out whole
// with no "\r" of the separator left behind.
func TestOurBlockAsHermesSavesItOnWindowsIsReadAndComesOutWhole(t *testing.T) {
	for name, tc := range hermesResavedEntries {
		t.Run(name, func(t *testing.T) {
			config := hermesAsWindowsSavesIt(readHermesFixture(t, name+".roundtrip.yaml"))
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(config)

			if !hermesHookInstalledFor(ops, hermesConfigPath, tc.entry, tc.windows) {
				t.Error("status: the block is not counted once its lines end in CRLF")
			}
			var install agentPlan
			if planHermesHookFor(ops, "Hermes", hermesConfigPath, tc.entry, tc.windows, &install) || len(install.writes)+len(install.refused)+len(install.notes) != 0 {
				t.Errorf("install has something to do or to say: %+v", install)
			}
			uninstall := hermesPlanUninstall(ops, tc.entry)
			if len(uninstall.writes) != 1 {
				t.Fatalf("uninstall planned %d writes; notes: %v", len(uninstall.writes), uninstall.notes)
			}
			if got, want := string(uninstall.writes[0].contents), hermesAroundOurBlock(t, config); got != want {
				t.Errorf("the surrounding file is not byte for byte what it was\n--- got ---\n%q\n--- want ---\n%q", got, want)
			}
		})
	}
}

// Two blocks are two hooks: keys. PyYAML keeps the last, ruamel refuses the
// file, and nothing here can say which one Hermes runs — so neither is read,
// whichever of them holds today's entry, and neither is cut out.
func TestTwoMarkedBlocksAreNeitherReadNorRemoved(t *testing.T) {
	entry := hermesResavedEntries["posix"].entry
	cmd, _ := hermesHookCommand(entry, false)
	current := string(hermesHookBlock(strings.Join(hermesHookLines(cmd), "\n")+"\n", false))
	other := string(hermesHookBlock(strings.Join(hermesHookLines("/somewhere/else/bin/dropin-miner hook hermes pre_tool_call"), "\n")+"\n", false))
	if !hermesMarkedBlockIsCurrent([]byte("model: gpt\n\n"+current), cmd) {
		t.Fatal("one block holding today's entry is not read; the cases below would pass for the wrong reason")
	}
	for name, config := range map[string]string{
		"today's entry first":              "model: gpt\n\n" + current + "\n" + other,
		"today's entry last":               "model: gpt\n\n" + other + "\n" + current,
		"today's entry twice":              "model: gpt\n\n" + current + "\n" + current,
		"a second begin marker":            "model: gpt\n\n" + agentsMarkerBegin + "\n" + current,
		"a second end marker":              "model: gpt\n\n" + current + agentsMarkerEnd + "\n",
		"a marker inside a longer line":    "model: gpt\nnote: '" + agentsMarkerBegin + "'\n\n" + current,
		"the end marker before the begin":  "model: gpt\n\n" + agentsMarkerEnd + "\n" + agentsMarkerBegin + "\n",
		"a begin marker and never its end": "model: gpt\n\n" + strings.TrimSuffix(current, agentsMarkerEnd+"\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if hermesMarkedBlockIsCurrent([]byte(config), cmd) {
				t.Errorf("read as one block holding today's entry:\n%s", config)
			}
			if cut := removeOurHermesBlock([]byte(config), refFor(entry)); cut.had || string(cut.next) != config {
				t.Errorf("found a block to remove (had %v), or changed the file:\n%s", cut.had, cut.next)
			}
			m, ops := newFakeMachine("hermes")
			m.files[hermesConfigPath] = []byte(config)
			if hermesHookInstalledFor(ops, hermesConfigPath, entry, false) {
				t.Error("status counted it")
			}
			var install agentPlan
			if planHermesHookFor(ops, "Hermes", hermesConfigPath, entry, false, &install) || len(install.writes) != 0 || len(install.refused) == 0 {
				t.Errorf("install wrote, or did not refuse: %+v", install)
			}
			if uninstall := hermesPlanUninstall(ops, entry); len(uninstall.writes) != 0 {
				t.Errorf("uninstall planned a write:\n%s", uninstall.writes[0].contents)
			}
		})
	}
}
