package main

// #99: our block is written where it is found, and no byte outside our
// markers moves.
//
// The block used to be stripped and appended on every write, so a file's
// order changed for a change that was only ever to our own table: the
// participant's [projects] and [windows] ended up above a block that had
// been below them, and "nothing else changed" stopped being checkable by
// comparing bytes.

import (
	"strings"
	"testing"
)

const codexConfigPath = "/home/u/.codex/config.toml"

// outsideOurMarkers is everything in a Codex config that is not ours: the
// bytes before the begin marker and the bytes after the end marker. That
// pair is the guarantee this file exists for -- a participant auditing a
// machine is making a claim about these bytes, not about ours.
func outsideOurMarkers(t *testing.T, file string) (pre, post string) {
	t.Helper()
	pre, _, post, ok := markedRegion([]byte(file))
	if !ok {
		t.Fatalf("no dropin-miner block in:\n%s", file)
	}
	return pre, post
}

// participantLines is every line of the file that is not inside our markers,
// blank lines dropped. Its ORDER is what an uninstall-and-install round trip
// must preserve even where the block itself cannot go back to where it was.
func participantLines(t *testing.T, file string) []string {
	t.Helper()
	var out []string
	inside := false
	for _, l := range strings.Split(file, "\n") {
		switch {
		case strings.Contains(l, agentsMarkerBegin):
			inside = true
		case strings.Contains(l, agentsMarkerEnd):
			inside = false
		case inside:
		case strings.TrimSpace(l) == "":
		default:
			out = append(out, strings.TrimRight(l, "\r"))
		}
	}
	return out
}

// staleOurTable makes our own table differ from what the renderer would
// write now, so an install has something to do. It changes a value INSIDE
// our markers and nothing else.
func staleOurTable(t *testing.T, file string) string {
	t.Helper()
	stale := strings.Replace(file, "network_access = true", "network_access = false", 1)
	if stale == file {
		t.Fatal("this fixture is meant to make our own table stale and did not change a byte, so the install below would have nothing to write")
	}
	return stale
}

// A reinstall rewrites our block where it stands. Both placements matter:
// our block last, which is where install puts it, and our block in the
// middle, which is what Codex or a participant makes of it afterwards.
func TestReinstallingWritesTheCodexBlockWhereItWas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where codexPlacement
	}{
		{"our block is the last thing in the file", beforeOurBlock},
		{"the participant's tables sit below our block", afterOurBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ops, cfgPath, before := installedCodexConfig(t, tc.where)
			stale := staleOurTable(t, before)
			m.files[codexConfigPath] = []byte(stale)
			wantPre, wantPost := outsideOurMarkers(t, stale)

			if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
				t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
			}
			got := string(m.files[codexConfigPath])
			if !strings.Contains(got, "network_access = true") {
				t.Fatalf("the install did not refresh our own table, so nothing about its position is being tested:\n%s", got)
			}
			gotPre, gotPost := outsideOurMarkers(t, got)
			if gotPre != wantPre {
				t.Errorf("bytes BEFORE our block changed\n got %q\nwant %q", gotPre, wantPre)
			}
			if gotPost != wantPost {
				t.Errorf("bytes AFTER our block changed\n got %q\nwant %q", gotPost, wantPost)
			}
			if n := strings.Count(got, agentsMarkerBegin); n != 1 {
				t.Errorf("begin markers after the install: %d, want 1:\n%s", n, got)
			}
		})
	}
}

// A file the participant edits on Windows has CRLF endings, and our block is
// written with LF. What must hold is the same thing: their bytes, on either
// side of our markers, exactly as they were.
//
// Their table is put BELOW our block on purpose. With our block last this
// case would pass against an implementation that stripped and appended --
// the review found exactly that, since the mutation for #99 left this test
// green -- so the CRLF fixture now carries position as well as bytes.
func TestReinstallingKeepsACRLFCodexConfigsOwnBytes(t *testing.T) {
	cfgPath, _ := sandboxTestConfig(t)
	m, ops := newFakeMachine("codex")
	m.files[codexConfigPath] = []byte("model = \"gpt-5\"\r\n\r\n[projects.'/home/u/work']\r\ntrust_level = \"trusted\"\r\n")
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
	}
	installed := strings.TrimRight(string(m.files[codexConfigPath]), "\n") + "\n\r\n[windows]\r\nsandbox = \"unelevated\"\r\n"
	m.files[codexConfigPath] = []byte(installed)
	if !strings.Contains(installed, "\r\n") {
		t.Fatal("this fixture is meant to carry CRLF endings and does not, so it proves nothing")
	}
	if _, _, post, _ := markedRegion([]byte(installed)); !strings.Contains(post, "[windows]") {
		t.Fatalf("this fixture is meant to leave our block mid-file, and nothing follows it: %q", post)
	}
	stale := staleOurTable(t, installed)
	m.files[codexConfigPath] = []byte(stale)
	wantPre, wantPost := outsideOurMarkers(t, stale)

	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("reinstall: exit %d\n%s%s", code, out, errOut)
	}
	gotPre, gotPost := outsideOurMarkers(t, string(m.files[codexConfigPath]))
	if gotPre != wantPre || gotPost != wantPost {
		t.Errorf("a CRLF config's own bytes changed around our block\n gotPre %q\nwantPre %q\n gotPost %q\nwantPost %q", gotPre, wantPre, gotPost, wantPost)
	}
	if !strings.Contains(gotPost, "\r\n") {
		t.Errorf("the participant's CRLF endings below our block did not survive: %q", gotPost)
	}
}

// The round trip that CAN be byte-identical, and is: a block where our own
// install puts it, at the end of the file.
func TestUninstallThenInstallRestoresTheCodexConfigByteForByte(t *testing.T) {
	m, ops, cfgPath, before := installedCodexConfig(t, beforeOurBlock)
	if _, _, post, _ := markedRegion([]byte(before)); post != "" {
		t.Fatalf("this case is meant to have our block last, and something follows it: %q", post)
	}
	if code, out, errOut := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("uninstall: exit %d\n%s%s", code, out, errOut)
	}
	if got := string(m.files[codexConfigPath]); strings.Contains(got, agentsMarkerBegin) {
		t.Fatalf("the uninstall left our block, so the install below is not the round trip this claims:\n%s", got)
	}
	if code, out, errOut := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("install: exit %d\n%s%s", code, out, errOut)
	}
	if got := string(m.files[codexConfigPath]); got != before {
		t.Errorf("uninstall then install did not return the file to its prior bytes\n got %q\nwant %q", got, before)
	}
}

// And the round trip that cannot be, stated as what it does guarantee.
//
// Uninstall removes the block and with it the only record of where it
// stood, so an install afterwards has nothing to read and appends. What
// must still hold -- and what #99 is really about -- is that no line of the
// participant's own moves relative to any other.
func TestUninstallThenInstallMovesNoParticipantLine(t *testing.T) {
	m, ops, cfgPath, before := installedCodexConfig(t, afterOurBlock)
	if _, _, post, _ := markedRegion([]byte(before)); post == "" {
		t.Fatal("this case is meant to have the participant's tables BELOW our block, and nothing follows it")
	}
	wantLines := participantLines(t, before)
	if len(wantLines) < 4 {
		t.Fatalf("this fixture is meant to carry the participant's own tables and carries %d lines: %q", len(wantLines), wantLines)
	}

	for _, sub := range []string{"uninstall", "install"} {
		if code, out, errOut := runAgents(t, ops, nil, sub, "-config", cfgPath, "-yes"); code != exitOK {
			t.Fatalf("%s: exit %d\n%s%s", sub, code, out, errOut)
		}
	}
	got := string(m.files[codexConfigPath])
	gotLines := participantLines(t, got)
	if strings.Join(gotLines, "\n") != strings.Join(wantLines, "\n") {
		t.Errorf("a participant line moved, changed or was lost\n got %q\nwant %q\nfile:\n%s", gotLines, wantLines, got)
	}
	if n := strings.Count(got, agentsMarkerBegin); n != 1 {
		t.Errorf("begin markers after the round trip: %d, want 1:\n%s", n, got)
	}
}
