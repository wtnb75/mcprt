package cli

import (
	"context"
	"log/slog"
	"reflect"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
)

// skillDirDebounce bounds how long watchSkillDir waits after the last
// fsnotify event on a directory before rescanning it -- coalescing a burst
// of events (an editor's atomic save is often a temp-file write plus a
// rename, for example) into a single rescan. Not exposed in config,
// matching the existing convention for this class of hardcoded interval
// (see backendConnectTimeout). A var so tests can shrink it.
var skillDirDebounce = 300 * time.Millisecond

// skillDirPromptsByName indexes prompts by name, for watchSkillDir's rescan
// to diff against its previous scan's equivalent index. config.
// ScanSkillDir/ScanSkillDirLenient already reject/skip a duplicate name
// within one directory, so this never silently drops an entry by
// overwriting a map key.
func skillDirPromptsByName(prompts []config.StaticPromptConfig) map[string]config.StaticPromptConfig {
	m := make(map[string]config.StaticPromptConfig, len(prompts))
	for _, p := range prompts {
		m[p.Name] = p
	}
	return m
}

// watchSkillDir runs until ctx is cancelled, debouncing fsnotify events on
// dir by skillDirDebounce before rescanning it (via
// config.ScanSkillDirLenient) and applying the diff against last -- the most
// recently applied scan, starting from initial, the exact scan buildGateway
// already used to build entryIndex's startup registrations -- to gw via
// UpdateDirPrompts. A file that fails to parse, or whose template fails to
// compile, is logged and excluded from this rescan's result, leaving
// whatever was registered for it before untouched.
//
// ctx is the same per-generation context backend supervisors use (see
// buildGateway), so a SIGHUP-triggered generation swap stops this goroutine
// the same way it stops a backend's reconnect loop -- the new generation's
// buildGateway starts a fresh watcher, seeded from its own initial scan,
// after building its own *gateway.Server.
func watchSkillDir(ctx context.Context, logger *slog.Logger, dir string, entryIndex int, initial []config.StaticPromptConfig, gw *gateway.Server) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Error("skill_dir: watcher setup failed, changes to this directory won't be picked up until the next reload", "dir", dir, "error", err)
		return
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(dir); err != nil {
		logger.Error("skill_dir: watch failed, changes to this directory won't be picked up until the next reload", "dir", dir, "error", err)
		return
	}

	// last means "what this entry currently has successfully applied to the
	// live server" -- NOT "what this directory's last scan looked like".
	// The two differ exactly when UpdateDirPrompts rejects a name from
	// added because a different entryIndex already owns it (see
	// UpdateDirPrompts' ownership-conflict check): that name must not enter
	// last, so the next rescan treats it as still-new and retries claiming
	// it (in case the conflicting owner has since freed the name), and so a
	// later deletion of that same file never asks UpdateDirPrompts to
	// remove a name this entry never actually registered.
	last := skillDirPromptsByName(initial)

	rescan := func() {
		fresh, skipped, err := config.ScanSkillDirLenient(dir)
		for _, s := range skipped {
			logger.Warn("skill_dir: skipping file", "dir", dir, "file", s.File, "error", s.Err)
		}
		if err != nil {
			logger.Warn("skill_dir: rescan failed, keeping previous state", "dir", dir, "error", err)
			return
		}
		freshByName := skillDirPromptsByName(fresh)

		var removedNames []string
		for name := range last {
			if _, ok := freshByName[name]; !ok {
				removedNames = append(removedNames, name)
			}
		}

		// buildFailed tracks a name whose fresh content failed to compile as
		// a template: unlike an ownership-conflict rejection (this entry
		// genuinely has nothing new to apply), this entry's PREVIOUS version
		// of that name is still registered and being served, so it must
		// carry forward into newLast unchanged rather than being dropped --
		// otherwise a later deletion of the broken file would never be
		// recognized as a removal (see
		// TestWatchSkillDir_BuildFailureDoesNotOrphanTheOldRegistration).
		buildFailed := make(map[string]bool)
		var added []*gateway.StaticPrompt
		for name, p := range freshByName {
			if old, ok := last[name]; ok && reflect.DeepEqual(old, p) {
				continue
			}
			sp, err := buildOneStaticPrompt(p, entryIndex)
			if err != nil {
				logger.Warn("skill_dir: skipping file with invalid template", "dir", dir, "name", name, "error", err)
				buildFailed[name] = true
				continue
			}
			added = append(added, sp)
		}

		if len(added) == 0 && len(removedNames) == 0 {
			return
		}
		applied := gw.UpdateDirPrompts(entryIndex, added, removedNames)

		// Rebuild last from the fresh scan: a name unchanged from the old
		// last needed no trip through UpdateDirPrompts at all, so it's kept
		// as-is; a name whose fresh content failed to build keeps its OLD
		// entry (still what's actually registered); a new/changed name that
		// did reach UpdateDirPrompts is kept only if it confirms the name
		// was actually applied -- a name it rejected (lost an ownership
		// race) is left out entirely, per the invariant above.
		newLast := make(map[string]config.StaticPromptConfig, len(freshByName))
		for name, p := range freshByName {
			if old, ok := last[name]; ok && reflect.DeepEqual(old, p) {
				newLast[name] = p
				continue
			}
			if buildFailed[name] {
				if old, ok := last[name]; ok {
					newLast[name] = old
				}
				continue
			}
			if applied[name] {
				newLast[name] = p
			}
		}
		last = newLast
	}

	// debounce/debounceC are read and written from this goroutine alone --
	// rescan() runs synchronously in the <-debounceC case below, never in a
	// separately spawned goroutine (unlike time.AfterFunc, whose documented
	// Reset semantics allow a second firing to run concurrently with a
	// still-running prior one). That makes overlapping rescan() invocations
	// -- which would race on the shared last map and on gw.UpdateDirPrompts
	// -- impossible by construction, and makes ctx cancellation's cleanup
	// complete: rescan can never be "in flight" while this goroutine is
	// blocked in select, since it only ever runs from inside a select-case
	// body on this same goroutine.
	var debounce *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-watcher.Events:
			if !ok {
				return
			}
			if debounce == nil {
				debounce = time.NewTimer(skillDirDebounce)
				debounceC = debounce.C
			} else {
				if !debounce.Stop() {
					<-debounce.C // drain: timer already fired but its value wasn't read yet
				}
				debounce.Reset(skillDirDebounce)
			}
		case <-debounceC:
			rescan()
			debounce = nil
			debounceC = nil
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Warn("skill_dir: watcher error", "dir", dir, "error", err)
		}
	}
}
