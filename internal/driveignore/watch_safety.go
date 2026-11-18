package driveignore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// removeStaleTemp deletes a leftover temporary file this tool created in an
// earlier, interrupted run. Only regular files older than staleTempAge go, and
// never in a dry run; a fresh one may belong to an install still in flight.
func (r *watchRun) removeStaleTemp(path string, entry fs.DirEntry, rel string) {
	if r.opts.DryRun || !entry.Type().IsRegular() {
		return
	}
	info, err := entry.Info()
	if err != nil || time.Since(info.ModTime()) < staleTempAge {
		return
	}
	if err := os.Remove(path); err == nil {
		r.cfg.logf("removed stale temporary file: %s", filepath.ToSlash(rel))
	}
}

// guardDeletions holds every deletion of the pass when it looks like one side
// vanished rather than was edited: a side with no files left while the journal
// records some, or more deletions than massDeleteMin and massDeleteRatio of
// the journal allow. Held deletions keep their journal proof and are reported
// every pass until the side recovers or AllowMassDelete lets them through.
func (r *watchRun) guardDeletions(actions []Action) []Action {
	if r.opts.AllowMassDelete {
		return actions
	}
	deletions := 0
	for _, a := range actions {
		if isDeletion(a.Kind) {
			deletions++
		}
	}
	if deletions == 0 {
		return actions
	}
	journaled := 0
	for _, e := range r.journal.Entries {
		if e.Type == "file" {
			journaled++
		}
	}
	reason := ""
	switch {
	case journaled > 0 && countFiles(r.out) == 0:
		reason = "the drive folder has no files left"
	case journaled > 0 && countFiles(r.local) == 0:
		reason = "the source folder has no files left"
	case deletions > massDeleteMin && float64(deletions) > massDeleteRatio*float64(journaled):
		reason = fmt.Sprintf("%d deletions in one pass exceed the limit", deletions)
	}
	if reason == "" {
		return actions
	}
	r.cfg.logf("holding %d deletion(s): %s; run once with --allow-mass-delete to apply them", deletions, reason)
	held := make([]Action, len(actions))
	for i, a := range actions {
		if isDeletion(a.Kind) {
			a = Action{ActionHeld, a.Path, string(a.Kind)}
		}
		held[i] = a
	}
	return held
}

func countFiles(side map[string]*sideEntry) int {
	n := 0
	for _, e := range side {
		if !e.isDir {
			n++
		}
	}
	return n
}

// foldPath is the key two spellings of one entry share on a case-insensitive
// filesystem.
func foldPath(rel string) string {
	if !caseInsensitiveFS {
		return rel
	}
	return strings.ToLower(rel)
}

// reconcileCase finds entries present on both sides under spellings that
// differ only in case and, on a case-insensitive filesystem, renames the stale
// side to the other spelling instead of letting the pass see a deletion plus
// a creation. The journal names the old spelling, so the side still using it
// is the stale one; without a journal entry the drive side follows local.
// Directory renames run shallowest first and carry their subtree along. A
// failed rename leaves the pair (and its subtree) untouched until next pass.
func (r *watchRun) reconcileCase() ([]Action, error) {
	r.caseSkip = map[string]bool{}
	if !caseInsensitiveFS {
		return nil, nil
	}
	var actions []Action
	var errs []error
	for {
		pairs := r.casePairs()
		if len(pairs) == 0 {
			break
		}
		for _, p := range pairs {
			localRel, outRel := p[0], p[1]
			if r.local[localRel].isDir != r.out[outRel].isDir {
				r.caseSkip[foldPath(localRel)] = true
				continue
			}
			// The side still spelled the way the journal recorded did not
			// take part in the rename, so it is the one to rename.
			_, localJournaled := r.journal.Entries[localRel]
			_, outJournaled := r.journal.Entries[outRel]
			renameLocal := localJournaled && !outJournaled
			root, from, to, side, seen := r.cfg.Output, outRel, localRel, "drive", r.out
			if renameLocal {
				root, from, to, side, seen = r.cfg.Input, localRel, outRel, "local", r.local
			}
			if !r.opts.DryRun {
				if err := r.cfg.rename(filepath.Join(root, from), filepath.Join(root, to)); err != nil {
					errs = append(errs, fmt.Errorf("rename %s to %s: %w", filepath.ToSlash(from), filepath.ToSlash(to), err))
					r.caseSkip[foldPath(localRel)] = true
					continue
				}
				r.cfg.logf("renamed %s side: %s -> %s", side, filepath.ToSlash(from), filepath.ToSlash(to))
			}
			rekey(seen, from, to)
			actions = append(actions, Action{ActionRenamedCase, filepath.ToSlash(to), side})
		}
	}
	r.foldJournal()
	return actions, errors.Join(errs...)
}

// casePairs returns the shallowest pairs (local spelling, drive spelling) of
// one entry spelled differently on the two sides, skipping failed ones.
func (r *watchRun) casePairs() [][2]string {
	outFold := make(map[string]string, len(r.out))
	for rel := range r.out {
		outFold[foldPath(rel)] = rel
	}
	var pairs [][2]string
	depth := -1
	for localRel := range r.local {
		if _, exact := r.out[localRel]; exact {
			continue
		}
		outRel, ok := outFold[foldPath(localRel)]
		if !ok || outRel == localRel || r.underCaseSkip(localRel) {
			continue
		}
		if _, clash := r.local[outRel]; clash {
			continue
		}
		d := strings.Count(localRel, string(filepath.Separator))
		switch {
		case depth == -1 || d < depth:
			depth = d
			pairs = [][2]string{{localRel, outRel}}
		case d == depth:
			pairs = append(pairs, [2]string{localRel, outRel})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	return pairs
}

// rekey moves the entry at from, and every entry below it, to the spelling to.
func rekey(seen map[string]*sideEntry, from, to string) {
	prefix := from + string(filepath.Separator)
	moved := map[string]*sideEntry{}
	for rel, e := range seen {
		switch {
		case rel == from:
			moved[to] = e
			delete(seen, rel)
		case strings.HasPrefix(rel, prefix):
			moved[to+rel[len(from):]] = e
			delete(seen, rel)
		}
	}
	for rel, e := range moved {
		seen[rel] = e
	}
}

// foldJournal respells journal entries to the spelling the walks observed, so
// a path renamed in case keeps its deletion proof and directory stamps.
func (r *watchRun) foldJournal() {
	if !caseInsensitiveFS {
		return
	}
	observed := make(map[string]string, len(r.local)+len(r.out))
	for rel := range r.out {
		observed[foldPath(rel)] = rel
	}
	for rel := range r.local {
		observed[foldPath(rel)] = rel
	}
	for rel, entry := range r.journal.Entries {
		if r.observed(rel) {
			continue
		}
		spelled, ok := observed[foldPath(rel)]
		if !ok || spelled == rel {
			continue
		}
		if _, taken := r.journal.Entries[spelled]; taken {
			continue
		}
		delete(r.journal.Entries, rel)
		r.journal.Entries[spelled] = entry
	}
}

// underCaseSkip reports whether rel or an ancestor had its case-only rename
// fail this pass.
func (r *watchRun) underCaseSkip(rel string) bool {
	if len(r.caseSkip) == 0 {
		return false
	}
	for {
		if r.caseSkip[foldPath(rel)] {
			return true
		}
		parent := filepath.Dir(rel)
		if parent == rel || parent == "." {
			return false
		}
		rel = parent
	}
}
