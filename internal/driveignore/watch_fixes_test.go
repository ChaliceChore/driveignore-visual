package driveignore

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func countConflictCopies(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, name := range dirNames(t, dir) {
		if strings.Contains(name, ".sync-conflict-") {
			n++
		}
	}
	return n
}

// trashOpts is applyOpts with an existing Trash directory, so deletions land.
func trashOpts(t *testing.T) WatchOptions {
	t.Helper()
	opts := applyOpts(t)
	require.NoError(t, os.MkdirAll(opts.TrashDir, 0o755))
	return opts
}

func requireCaseInsensitive(t *testing.T) {
	t.Helper()
	if !caseInsensitiveFS {
		t.Skip("case-only renames only collide on case-insensitive filesystems")
	}
}

// A conflict whose final install keeps failing (the file is locked by the
// application editing it) must reuse its conflict copy on every retry instead
// of adding another one each pass.
func TestReconcileConflictRetryReusesCopy(t *testing.T) {
	src, out, state := syncedPair(t)
	replaceFile(t, filepath.Join(src, "keep.txt"), "local edit")
	replaceFile(t, filepath.Join(out, "keep.txt"), "drive edit!")
	setMtime(t, filepath.Join(src, "keep.txt"), time.Now().Add(-time.Hour))
	setMtime(t, filepath.Join(out, "keep.txt"), time.Now().Add(-2*time.Hour))

	locked := true
	cfg := baseConfig(t, src, out)
	cfg.renameFn = func(oldname, newname string) error {
		if locked && filepath.Base(newname) == "keep.txt" {
			return errors.New("sharing violation")
		}
		return os.Rename(oldname, newname)
	}
	opts := applyOpts(t)
	for i := 0; i < 4; i++ {
		_, err := Reconcile(cfg, state, opts)
		require.Error(t, err)
	}
	require.Equal(t, 1, countConflictCopies(t, src), "a retried conflict must reuse its copy: %v", dirNames(t, src))
	require.Equal(t, 1, countConflictCopies(t, out))

	locked = false
	_, err := Reconcile(cfg, state, opts)
	require.NoError(t, err)
	assertLinked(t, filepath.Join(src, "keep.txt"), filepath.Join(out, "keep.txt"))
	require.Equal(t, "local edit", read(t, filepath.Join(out, "keep.txt")))
	require.Equal(t, 1, countConflictCopies(t, src))

	second, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Empty(t, second.Actions)
}

// Conflict copies are ordinary synced files: deleting one side's copy removes
// the other side's.
func TestReconcileConflictCopyDeletionPropagates(t *testing.T) {
	src, out, state := syncedPair(t)
	replaceFile(t, filepath.Join(src, "keep.txt"), "local edit")
	replaceFile(t, filepath.Join(out, "keep.txt"), "drive edit!")
	setMtime(t, filepath.Join(src, "keep.txt"), time.Now().Add(-time.Hour))
	setMtime(t, filepath.Join(out, "keep.txt"), time.Now().Add(-2*time.Hour))
	opts := trashOpts(t)
	_, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	// The copies were created this pass; the next one journals them as synced.
	_, err = Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)

	var copyName string
	for _, name := range dirNames(t, src) {
		if strings.Contains(name, ".sync-conflict-") {
			copyName = name
		}
	}
	require.NotEmpty(t, copyName)
	require.NoError(t, os.Remove(filepath.Join(src, copyName)))

	report, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Equal(t, []Action{{ActionRemovedDrive, copyName, ""}}, report.Actions)
	require.NoFileExists(t, filepath.Join(out, copyName))
}

// A case-only rename arriving on the drive side (Drive applying a rename made
// on another machine) renames the local file in place: nothing is trashed.
func TestReconcileCaseRenameOnDrive(t *testing.T) {
	requireCaseInsensitive(t)
	src, out, state := syncedPair(t)
	require.NoError(t, os.Rename(filepath.Join(out, "keep.txt"), filepath.Join(out, "KEEP.txt")))
	opts := trashOpts(t)

	report, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Equal(t, []Action{{ActionRenamedCase, "KEEP.txt", "local"}}, report.Actions)
	require.Contains(t, dirNames(t, src), "KEEP.txt")
	require.NotContains(t, dirNames(t, src), "keep.txt")
	require.Empty(t, dirNames(t, opts.TrashDir), "a case-only rename must not trash the file")
	assertLinked(t, filepath.Join(src, "KEEP.txt"), filepath.Join(out, "KEEP.txt"))

	second, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Empty(t, second.Actions)
}

// A local case-only rename renames the drive entry: Drive sees a rename, not
// a deletion and a new upload.
func TestReconcileCaseRenameLocal(t *testing.T) {
	requireCaseInsensitive(t)
	src, out, state := syncedPair(t)
	require.NoError(t, os.Rename(filepath.Join(src, "keep.txt"), filepath.Join(src, "KEEP.txt")))
	opts := trashOpts(t)

	report, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Equal(t, []Action{{ActionRenamedCase, "KEEP.txt", "drive"}}, report.Actions)
	require.Contains(t, dirNames(t, out), "KEEP.txt")
	require.NotContains(t, dirNames(t, out), "keep.txt")
	assertLinked(t, filepath.Join(src, "KEEP.txt"), filepath.Join(out, "KEEP.txt"))

	second, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Empty(t, second.Actions)
}

// A directory renamed in case carries its subtree along in one rename.
func TestReconcileCaseRenameDirectory(t *testing.T) {
	requireCaseInsensitive(t)
	src, out, state := syncedPair(t)
	require.NoError(t, os.Rename(filepath.Join(out, "sub"), filepath.Join(out, "Sub")))
	opts := trashOpts(t)

	report, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Equal(t, []Action{{ActionRenamedCase, "Sub", "local"}}, report.Actions)
	require.Contains(t, dirNames(t, src), "Sub")
	require.Empty(t, dirNames(t, opts.TrashDir))
	assertLinked(t, filepath.Join(src, "Sub", "nested.txt"), filepath.Join(out, "Sub", "nested.txt"))

	second, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Empty(t, second.Actions)
}

// A file dated in the future (a skewed clock elsewhere) syncs instead of
// being deferred until the clock catches up.
func TestReconcileFutureMtimeIsNotDeferred(t *testing.T) {
	src, out, state := syncedPair(t)
	write(t, filepath.Join(src, "future.txt"), "x")
	setMtime(t, filepath.Join(src, "future.txt"), time.Now().Add(10*time.Minute))
	opts := applyOpts(t)
	opts.MinAge = 2 * time.Second

	report, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.Equal(t, []Action{{ActionLinked, "future.txt", ""}}, report.Actions)
	assertLinked(t, filepath.Join(src, "future.txt"), filepath.Join(out, "future.txt"))
}

// A Windows junction cannot be hardlinked; it is skipped like a symlink
// instead of failing every pass.
func TestReconcileSkipsJunctions(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are a Windows feature")
	}
	src, out, state := syncedPair(t)
	target := t.TempDir()
	write(t, filepath.Join(target, "inside.txt"), "x")
	output, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(src, "junction"), target).CombinedOutput()
	require.NoError(t, err, string(output))

	report, err := Reconcile(baseConfig(t, src, out), state, applyOpts(t))
	require.NoError(t, err)
	require.Empty(t, report.Actions)
	require.NoFileExists(t, filepath.Join(out, "junction"))
}

// An emptied drive folder (a Drive reset or re-sync) must not trash the whole
// source: every deletion is held until the flag lets it through.
func TestReconcileHoldsDeletionsWhenDriveEmptied(t *testing.T) {
	src, out, state := syncedPair(t)
	for _, name := range dirNames(t, out) {
		require.NoError(t, os.RemoveAll(filepath.Join(out, name)))
	}
	before := treeSnapshot(t, src)
	opts := trashOpts(t)

	report, err := Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	require.NotEmpty(t, report.Actions)
	for _, a := range report.Actions {
		require.Equal(t, ActionHeld, a.Kind, "every deletion must be held: %v", report.Actions)
	}
	require.Equal(t, before, treeSnapshot(t, src), "a held pass must not touch the source")
	require.Empty(t, dirNames(t, opts.TrashDir))

	// The proof survives: an explicit run applies the held deletions.
	opts.AllowMassDelete = true
	report, err = Reconcile(baseConfig(t, src, out), state, opts)
	require.NoError(t, err)
	for _, a := range report.Actions {
		require.Equal(t, ActionTrashedLocal, a.Kind)
	}
	require.NoFileExists(t, filepath.Join(src, "keep.txt"))
}

// Too many deletions at once are held too, while a normal deletion passes.
func TestReconcileHoldsTooManyDeletions(t *testing.T) {
	src, out, state := syncedPair(t)
	for i := 0; i < massDeleteMin+10; i++ {
		write(t, filepath.Join(src, "bulk", fmt.Sprintf("f%03d.txt", i)), "x")
	}
	saveSyncedState(t, src, out, state)
	require.NoError(t, os.RemoveAll(filepath.Join(src, "bulk")))

	report, err := Reconcile(baseConfig(t, src, out), state, trashOpts(t))
	require.NoError(t, err)
	held := 0
	for _, a := range report.Actions {
		require.NotEqual(t, ActionRemovedDrive, a.Kind)
		if a.Kind == ActionHeld {
			held++
		}
	}
	require.Equal(t, massDeleteMin+10, held)
	require.FileExists(t, filepath.Join(out, "bulk", "f000.txt"))
}

// Leftover temporary files from an interrupted install are never synced, and
// stale ones are removed; a fresh one may belong to a live install.
func TestReconcileTemporaryLeftovers(t *testing.T) {
	src, out, state := syncedPair(t)
	stale := []string{tempPrefix + "111", ".driveignore-222", ".driveignore-watch-probe-333"}
	for _, name := range stale {
		write(t, filepath.Join(src, name), "junk")
		backdate(t, filepath.Join(src, name))
	}
	write(t, filepath.Join(out, tempPrefix+"fresh"), "live")

	report, err := Reconcile(baseConfig(t, src, out), state, applyOpts(t))
	require.NoError(t, err)
	require.Empty(t, report.Actions)
	for _, name := range stale {
		require.NoFileExists(t, filepath.Join(src, name), "stale temporary file %s must be removed", name)
		require.NoFileExists(t, filepath.Join(out, name))
	}
	require.FileExists(t, filepath.Join(out, tempPrefix+"fresh"), "a fresh temporary file must be left alone")
	require.NoFileExists(t, filepath.Join(src, tempPrefix+"fresh"))
	// A real .driveignore is never mistaken for a temporary file.
	require.False(t, isTempName(".driveignore"))
	require.False(t, isTempName(".driveignore-notes.txt"))
}

// Rule loading never descends into a directory the rules already exclude, so
// a .driveignore inside it is not read; a negation in a shallower file still
// re-includes a directory the root rules exclude.
func TestLoadIgnoreSkipsExcludedDirectories(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".driveignore"), "node_modules/\nbuild/\n")
	write(t, filepath.Join(root, "node_modules", "pkg", ".driveignore"), "!keep-me\n")
	write(t, filepath.Join(root, "app", ".driveignore"), "!build/\n")
	write(t, filepath.Join(root, "app", "build", ".driveignore"), "*.log\n")
	global := func() (string, error) { return filepath.Join(t.TempDir(), "global"), nil }

	m, _, err := LoadIgnore(root, false, global)
	require.NoError(t, err)
	pm := m.(pathMatcher)
	require.Len(t, pm.patterns, 2+1+1, "the file inside node_modules must not be read")
	require.True(t, m.Match(filepath.Join(root, "node_modules"), true))
	require.True(t, m.Match(filepath.Join(root, "build"), true))
	require.False(t, m.Match(filepath.Join(root, "app", "build"), true), "a shallower negation re-includes the directory")
	require.True(t, m.Match(filepath.Join(root, "app", "build", "x.log"), false), "rules inside a re-included directory apply")
}
