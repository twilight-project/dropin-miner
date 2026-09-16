package main

// Who can read the wallet directory.
//
// On POSIX the wallet is a 0700 directory of 0600 files, and that is all file
// modes can say: a coding agent's sandbox runs as the participant, so a mode
// cannot tell the participant's own sandboxed commands apart from the
// participant. Nothing here changes POSIX.
//
// On Windows access is a DACL, and a DACL is inherited. Setup gives the
// installation directory a protected owner-only DACL, but another program may
// later add an inheritable entry to that directory — a coding agent's sandbox
// setup granting its sandbox group read access, for example — and the entry then
// reaches every object beneath it that has no protected DACL of its own. Two of
// those objects are meant to be reachable: a search inside the sandbox reads
// credentials.json, and a flush reads and writes state\. Neither needs the
// wallet. So on Windows the wallet directory, and every file in it, carries its
// own protected owner-only DACL:
//
//   - openWalletDir sets it on a directory it creates;
//   - writeWalletFile sets it on the directory before a file is written (so the
//     temporary file inherits nothing else) and on the published file;
//   - setup and set-aside adoption repair an existing wallet recursively, and a
//     wallet object they cannot secure fails the run;
//   - doctor reports anyone other than the owner who can read the wallet.
//
// The walk, the repair decision and the judgment of an access list are here,
// the same on every platform, over walletACL; the platform half — reading a
// DACL, setting one, recognizing a reparse point — is wallet_acl_windows.go
// and wallet_acl_other.go.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// walletACLBackend is the platform half. managed is false where file modes are
// the whole story, and then nothing else in it is called.
type walletACLBackend struct {
	managed bool
	// inspect reads one object's access list. It is only called on an object
	// isLink has already cleared, because reading a DACL by name follows a link.
	inspect func(path string) (walletObjectAccess, error)
	// protect gives one object a protected owner-only access list.
	protect func(path string, dir bool) error
	// isLink reports a symlink or any other reparse point.
	isLink func(info fs.FileInfo) bool
}

// walletACL is the running platform's backend; tests substitute a fake.
var walletACL = systemWalletACL()

// walletObjectAccess is what one object's access list says.
type walletObjectAccess struct {
	// protected: the list does not inherit from the parent.
	protected bool
	// ownerOnly: every entry is an allow entry for the owner.
	ownerOnly bool
	// readers names every principal other than the owner that the list lets
	// read the object (or, on a directory, list it or read what is created in it).
	readers []string
}

// walletACE is one access-list entry, reduced to what the judgment needs.
type walletACE struct {
	allow bool
	flags uint8
	mask  uint32
	sid   string
}

// The access-mask bits that let a principal read: FILE_READ_DATA (on a
// directory, FILE_LIST_DIRECTORY), GENERIC_READ and GENERIC_ALL.
const (
	walletReadData    uint32 = 0x00000001
	walletGenericRead uint32 = 0x80000000
	walletGenericAll  uint32 = 0x10000000
)

// judgeWalletACEs decides ownerOnly and readers from the entries. An inherited
// entry grants exactly what an explicit one does, so the inheritance flags play
// no part: the entry a program adds to a parent reaches a child as an inherited
// one, and that is the case this exists for. A deny entry never makes a reader;
// counting only allow entries can over-report a principal a deny entry also
// names, which errs toward saying so.
func judgeWalletACEs(aces []walletACE, owner string) (ownerOnly bool, readers []string) {
	ownerOnly = len(aces) > 0
	seen := map[string]bool{}
	for _, ace := range aces {
		if !ace.allow || ace.sid != owner {
			ownerOnly = false
		}
		if !ace.allow || ace.sid == owner {
			continue
		}
		if ace.mask&(walletReadData|walletGenericRead|walletGenericAll) == 0 {
			continue
		}
		if !seen[ace.sid] {
			seen[ace.sid] = true
			readers = append(readers, ace.sid)
		}
	}
	sort.Strings(readers)
	return ownerOnly, readers
}

type walletEntryKind int

const (
	walletEntryDir walletEntryKind = iota
	walletEntryFile
	walletEntryLink  // a symlink or other reparse point: reported, never followed
	walletEntryOther // neither a regular file nor a directory
)

// walkWalletTree visits root and everything beneath it, a directory before its
// contents, in name order. A link is visited as a link and never entered. A
// failure to read an object is passed to visit as err, and the walk goes on.
func walkWalletTree(root string, visit func(path string, kind walletEntryKind, err error)) {
	info, err := os.Lstat(root)
	if err != nil {
		visit(root, walletEntryOther, err)
		return
	}
	kind := walletEntryKindOf(info)
	visit(root, kind, nil)
	if kind != walletEntryDir {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		visit(root, walletEntryDir, fmt.Errorf("list %s: %w", root, err))
		return
	}
	for _, e := range entries {
		walkWalletTree(filepath.Join(root, e.Name()), visit)
	}
}

func walletEntryKindOf(info fs.FileInfo) walletEntryKind {
	switch {
	case walletACL.isLink(info):
		return walletEntryLink
	case info.IsDir():
		return walletEntryDir
	case info.Mode().IsRegular():
		return walletEntryFile
	}
	return walletEntryOther
}

// secureWalletTree gives dir and every directory and regular file beneath it a
// protected owner-only access list through restrict, skipping an object whose
// list already is one. A link or anything else it cannot secure is a failure,
// and so is an object restrict fails on; the walk still secures everything else
// before returning them all. changed reports whether any list was set. A dir
// that does not exist is nothing to do. Where access lists are not managed this
// does nothing at all.
func secureWalletTree(dir string, restrict func(path string, dir bool) error) (changed bool, err error) {
	if !walletACL.managed {
		return false, nil
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	var problems []error
	walkWalletTree(dir, func(path string, kind walletEntryKind, walkErr error) {
		switch {
		case walkErr != nil:
			problems = append(problems, walkErr)
		case kind == walletEntryLink:
			problems = append(problems, fmt.Errorf("%s is a link or reparse point; it is not followed and was not secured", path))
		case kind == walletEntryOther:
			problems = append(problems, fmt.Errorf("%s is not a regular file or directory and was not secured", path))
		default:
			if acc, ierr := walletACL.inspect(path); ierr == nil && acc.protected && acc.ownerOnly {
				return
			}
			if rerr := restrict(path, kind == walletEntryDir); rerr != nil {
				problems = append(problems, fmt.Errorf("restrict %s to its owner: %w", path, rerr))
				return
			}
			changed = true
		}
	})
	return changed, errors.Join(problems...)
}

// protectWalletDir and protectWalletFile are the creation and write half of the
// rule; they do nothing where access lists are not managed.
func protectWalletDir(dir string) error {
	if !walletACL.managed {
		return nil
	}
	return walletACL.protect(dir, true)
}

func protectWalletFile(path string) error {
	if !walletACL.managed {
		return nil
	}
	return walletACL.protect(path, false)
}

// walletAccessFacts is doctor's view of the installation's wallet.
type walletAccessFacts struct {
	// Checked is false where access lists are not managed; doctor then has no
	// wallet access check at all.
	Checked bool
	Dir     string
	Present bool
	// Readers are the objects someone other than the owner can read.
	Readers []walletReader
	// Unsecurable are links and other objects setup refuses to secure.
	Unsecurable []string
	// Err joins every object whose access list could not be read.
	Err error
}

type walletReader struct {
	Path       string
	Principals []string
}

// inspectWalletAccess reads, and only reads, the access list of dir and of
// everything beneath it.
func inspectWalletAccess(dir string) walletAccessFacts {
	f := walletAccessFacts{Checked: walletACL.managed, Dir: dir}
	if !f.Checked {
		return f
	}
	if dir == "" {
		f.Err = errors.New("no installation directory could be resolved")
		return f
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return f
	} else if err != nil {
		f.Err = err
		return f
	}
	f.Present = true
	var problems []error
	walkWalletTree(dir, func(path string, kind walletEntryKind, walkErr error) {
		switch {
		case walkErr != nil:
			problems = append(problems, walkErr)
		case kind == walletEntryLink || kind == walletEntryOther:
			f.Unsecurable = append(f.Unsecurable, path)
		default:
			acc, err := walletACL.inspect(path)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", path, err))
				return
			}
			if len(acc.readers) > 0 {
				f.Readers = append(f.Readers, walletReader{Path: path, Principals: acc.readers})
			}
		}
	})
	f.Err = errors.Join(problems...)
	return f
}

// doctorWalletCheck is the verdict. Someone else able to read a wallet object
// is a fact whatever else could not be read, so it outranks an error.
//
// A reader says what to do and also that doing it may not be the end of it: the
// entry was added by something — another program, or a machine policy that
// refreshes on its own schedule — and that something can add it again, which is
// a state this check will report again rather than a sign the repair failed.
// Saying only "run setup" would read as a one-off.
func doctorWalletCheck(f walletAccessFacts) doctorCheck {
	c := doctorCheck{Name: "wallet access"}
	if len(f.Readers) > 0 || len(f.Unsecurable) > 0 {
		c.Verdict = verdictNo
		var parts []string
		for _, r := range f.Readers {
			parts = append(parts, fmt.Sprintf("%s can be read by %s", r.Path, strings.Join(r.Principals, ", ")))
		}
		for _, p := range f.Unsecurable {
			parts = append(parts, p+" is a link or not a regular file, and setup will not secure it")
		}
		c.Detail = "someone other than you can read the wallet: " + strings.Join(parts, "; ")
		if len(f.Readers) > 0 {
			c.Detail += "; another program or a machine policy can add such an entry again, and dropin-miner setup re-applies the protection whenever it does"
		}
		if f.Err != nil {
			c.Detail += " (and some of the wallet could not be checked: " + singleLineError(f.Err) + ")"
		}
		c.Fix = "dropin-miner setup"
		if len(f.Unsecurable) > 0 {
			c.Fix = "move " + strings.Join(f.Unsecurable, ", ") + " out of the wallet directory, then run: dropin-miner setup"
		}
		return c
	}
	if f.Err != nil {
		c.Verdict = verdictUnknown
		c.Detail = "could not read who can open the wallet in " + f.Dir + " — " + singleLineError(f.Err)
		return c
	}
	c.Verdict = verdictOK
	if !f.Present {
		c.Detail = "no wallet in " + f.Dir
		return c
	}
	c.Detail = "only you can read " + f.Dir + " and the files in it"
	return c
}

// singleLineError keeps a joined error on the one line a doctor check has.
func singleLineError(err error) string { return strings.ReplaceAll(err.Error(), "\n", "; ") }
