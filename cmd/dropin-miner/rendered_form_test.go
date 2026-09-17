package main

// Cursor auto-allows exactly what the skill renders, and nothing looser.
//
// Every input here is built FROM the rendered skill — the same text the
// participant's Cursor is taught — rather than written out by hand, so the
// recognizer and the skill cannot drift apart again. That drift is #66: the
// skill taught a heredoc, the recognizer parsed only single-line commands,
// and every Cursor search waited for a human and lost its lineage.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recognizerFixture is an installation whose binary is this test's own
// executable, so the identity check has a real file to agree with.
type recognizerFixture struct {
	entry  binEntry
	shells []shellKind
	exe    func() (string, error)
}

func newRecognizerFixture(t *testing.T, shells ...shellKind) recognizerFixture {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if len(shells) == 0 {
		shells = []shellKind{shellPOSIX}
	}
	return recognizerFixture{
		// The config lives under the installation directory a participant
		// has, which is also what the skill-block helpers look for.
		entry:  binEntry{command: self, cfg: filepath.Join(t.TempDir(), installMarker, "tokendrop.toml")},
		shells: shells,
		exe:    func() (string, error) { return self, nil },
	}
}

// renderedSearch is the search command this installation's skill teaches for
// sh, carrying body.
func (f recognizerFixture) renderedSearch(t *testing.T, sh shellKind, body string) string {
	t.Helper()
	_, script, err := searchBlockForShell(sh, f.entry, body)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func (f recognizerFixture) recognize(command string) *recognizedForm {
	return recognizeRenderedForm(command, f.exe, f.entry.cfg, f.shells)
}

// The rendered search is allowed, in every shell the host may run it in, and
// the body it carries is the one the command actually contained.
func TestRecognizerAllowsTheRenderedSearchInEveryDeclaredShell(t *testing.T) {
	for _, sh := range []shellKind{shellPOSIX, shellPowerShell} {
		t.Run(string(sh), func(t *testing.T) {
			f := newRecognizerFixture(t, sh)
			body := `{"version":1,"query":"it's \"quoted\" a\\b café 東京 😀"}`
			got := f.recognize(f.renderedSearch(t, sh, body))
			if got == nil || len(got.path) != 1 || got.path[0] != "search" {
				t.Fatalf("the rendered search was not recognized: %+v", got)
			}
			if got.body != body {
				t.Fatalf("recognized body\n got %q\nwant %q", got.body, body)
			}
		})
	}
}

// The preference command the same skill teaches is allowed, with each of the
// three arguments it documents and no others.
func TestRecognizerAllowsTheRenderedPreferenceCommand(t *testing.T) {
	for _, sh := range []shellKind{shellPOSIX, shellPowerShell} {
		f := newRecognizerFixture(t, sh)
		prefer, err := f.entry.preferCommandForShell(sh)
		if err != nil {
			t.Fatal(err)
		}
		for _, arg := range []string{"on", "off", "status"} {
			got := f.recognize(prefer + " " + arg)
			if got == nil || len(got.path) != 2 || got.path[0] != "agents" || got.path[1] != "prefer" {
				t.Fatalf("%s: the rendered preference command %q was not recognized: %+v", sh, arg, got)
			}
		}
		if got := f.recognize(prefer + " sideways"); got != nil {
			t.Fatalf("%s: an undocumented preference argument was allowed", sh)
		}
	}
}

// Everything looser, each as its own case, and each built from the rendered
// text so it differs from the allowed command only in the way it names.
func TestRecognizerRefusesAnythingButTheRenderedForm(t *testing.T) {
	const body = `{"version":1,"query":"ordinary"}`
	for _, sh := range []shellKind{shellPOSIX, shellPowerShell} {
		f := newRecognizerFixture(t, sh)
		rendered := f.renderedSearch(t, sh, body)
		second := f.renderedSearch(t, sh, `{"version":1,"query":"second"}`)
		separator, pipe := "; ", " | "
		cases := map[string]string{
			"plus a second statement":       rendered + separator + "echo x",
			"plus a pipeline":               rendered + pipe + "tee /tmp/x",
			"plus another whole search":     rendered + "\n" + second,
			"something before it":           "echo x" + separator + rendered,
			"trailing text after the body":  rendered + " x",
			"a second JSON object":          f.renderedSearch(t, sh, body+body),
			"a body that is not an object":  f.renderedSearch(t, sh, `[{"version":1,"query":"q"}]`),
			"a body of the wrong version":   f.renderedSearch(t, sh, `{"version":2,"query":"q"}`),
			"a body with trailing text":     f.renderedSearch(t, sh, body+" trailing"),
			"a different binary":            strings.Replace(rendered, f.entry.command, filepath.Join(t.TempDir(), "other"), 1),
			"a different config":            strings.Replace(rendered, f.entry.cfg, filepath.Join(t.TempDir(), "other.toml"), 1),
			"a different subcommand":        strings.Replace(rendered, " search ", " enroll ", 1),
			"an extra flag":                 strings.Replace(rendered, " --stdin", " --stdin -format json", 1),
			"the body's terminator inlined": f.renderedSearch(t, sh, body+"\nJSON\n"+body),
		}
		if sh == shellPowerShell {
			// Without the encoding line the query does not survive Windows
			// PowerShell 5.1, so a command missing it is not the command the
			// skill teaches and is not allowed to pass as it.
			cases["without the encoding line"] = strings.TrimPrefix(rendered, psOutputEncodingLine+"\n")
		}
		for name, command := range cases {
			t.Run(string(sh)+"/"+name, func(t *testing.T) {
				if got := f.recognize(command); got != nil {
					t.Fatalf("allowed %q\ncommand:\n%s", name, command)
				}
			})
		}
	}
}

// A command rendered for a shell this host does not run is not ours to
// allow, however well formed it is.
func TestRecognizerRefusesAFormRenderedForAnotherShell(t *testing.T) {
	f := newRecognizerFixture(t, shellPOSIX)
	if got := f.recognize(f.renderedSearch(t, shellPowerShell, `{"version":1,"query":"q"}`)); got != nil {
		t.Fatal("a PowerShell command was allowed for a host declared POSIX")
	}
}

// The identity check is the file, not the spelling: a path that reaches this
// same binary another way is still recognized.
func TestRecognizerFollowsTheBinaryNotItsSpelling(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), exeName("linked"))
	if err := os.Symlink(self, link); err != nil {
		t.Skip("this environment does not allow symlinks: " + err.Error())
	}
	f := recognizerFixture{
		entry:  binEntry{command: link, cfg: filepath.Join(t.TempDir(), "tokendrop.toml")},
		shells: []shellKind{shellPOSIX},
		exe:    func() (string, error) { return self, nil },
	}
	if got := f.recognize(f.renderedSearch(t, shellPOSIX, `{"version":1,"query":"q"}`)); got == nil {
		t.Fatal("a link to this binary was not recognized as this binary")
	}
}

// The hook learns its config path from its own argv, and the host's hook
// command may spell it differently from the skill's: a `%q`-quoted hook
// command on Windows hands the process doubled separators. Same file, other
// bytes — and an exact-match recognizer that compared the spelling would
// refuse the very command its own skill teaches, which is #66 again by
// another route. This is what the Windows runner caught.
func TestRecognizerComparesTheConfigAsAPathNotASpelling(t *testing.T) {
	f := newRecognizerFixture(t, shellPOSIX)
	command := f.renderedSearch(t, shellPOSIX, `{"version":1,"query":"q"}`)
	doubled := strings.ReplaceAll(f.entry.cfg, string(filepath.Separator), strings.Repeat(string(filepath.Separator), 2))
	if doubled == f.entry.cfg {
		t.Fatal("this path has no separator to double")
	}
	got := recognizeRenderedForm(command, f.exe, doubled, f.shells)
	if got == nil {
		t.Fatalf("a hook holding %q refused the search rendered with %q", doubled, f.entry.cfg)
	}
	// A different file is still a different file.
	if other := recognizeRenderedForm(command, f.exe, filepath.Join(t.TempDir(), "other.toml"), f.shells); other != nil {
		t.Fatal("a command naming another installation's config was allowed")
	}
}

// The skill's own text is the input: whatever the renderer produces, the
// recognizer accepts that and the body inside it.
func TestRecognizerAcceptsTheCommandTakenFromTheRenderedSkill(t *testing.T) {
	f := newRecognizerFixture(t, shellPOSIX)
	skill := renderedSkillFor("cursor", f.entry, "linux")
	block := skillSearchBlock(t, skill)
	got := f.recognize(block.body)
	if got == nil {
		t.Fatalf("the command block the skill teaches was refused:\n%s", block.body)
	}
	if got.body != exampleRequest {
		t.Fatalf("body from the skill's own block\n got %q\nwant %q", got.body, exampleRequest)
	}
}
