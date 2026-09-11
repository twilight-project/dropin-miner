package fsx

// Publication uses build-tagged backends. Unix replacement is rename;
// exclusive publication links the fully synced staged inode without replacing
// an existing name. Both require directory fsync. A failed directory fsync
// returns a StageError with Published=true; the complete new file may exist,
// but callers must not perform destructive cleanup on that error.
//
// Windows uses MoveFileEx with MOVEFILE_WRITE_THROUGH, plus
// MOVEFILE_REPLACE_EXISTING for replacement only. Exclusive destination-exists
// errors match fs.ErrExist. There is no delete-final fallback. Windows does
// not support the directory-fsync operation provided here; its backend returns
// ErrDirectorySyncUnsupported explicitly. Write-through publication supplies
// the Windows durability barrier instead. Arbitrary sync errors still fail.
//
// The existing CI test job on windows-latest runs TestPublication (replacement,
// absent exclusive publication, refusal of an existing winner, and failed
// publication preserving the final file), TestFailureStages, and
// TestExclusiveConcurrentCreators through go test -count=1 ./.... Cross-building
// those tests is not evidence of Windows runtime qualification.
