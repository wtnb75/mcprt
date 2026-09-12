# Dynamic skill_dir Prompt Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a `prompts:` entry point `skill_dir` at a directory of `*.md` files, each auto-registered as its own static prompt, with additions/edits/removals picked up live (no SIGHUP/restart needed) and announced to downstream clients via `notifications/prompts/list_changed`.

**Architecture:** `config.StaticPromptConfig` gains a `SkillDir` field, mutually exclusive with `name`/`description`/`text`/`skill_file`/`arguments`. `config.ScanSkillDir`/`ScanSkillDirLenient` list a directory's `*.md` files (non-recursive, sorted by filename) and parse each via the existing `parseSkillFile`, falling back to the filename (sans extension) for `name` when front matter omits it. At config-load time (`mcprt validate`/startup/SIGHUP reload) every prompt source -- fixed entries and every `skill_dir`'s scanned files -- is checked together for name/template errors, exactly as strict as today. At runtime, `internal/cli`'s `watchSkillDir` uses `fsnotify` to watch each `skill_dir`, debounces bursts of events, rescans leniently (a bad file is skipped and logged, not fatal), diffs against its last-applied scan, and calls the new `gateway.Server.UpdateDirPrompts` to apply exactly the changed names -- which calls `AddPrompt`/`RemovePrompts` on the live `*mcp.Server`, so `go-sdk` sends `notifications/prompts/list_changed` on its own. This never swaps the `*gateway.Server` generation, so it works identically for stdio and HTTP sessions, unlike the existing SIGHUP hot-reload path. A `prompts:` entry's position (its index) is its priority: a name a higher-priority entry already owns is never taken by a lower-priority `skill_dir`'s file; this can only actually happen once a directory changes at runtime, since config-load's global duplicate check already forbids any such collision existing at startup.

**Tech Stack:** Go, `github.com/modelcontextprotocol/go-sdk/mcp`, `github.com/fsnotify/fsnotify` (new dependency), Go standard `text/template`, `gopkg.in/yaml.v3`, `github.com/invopop/jsonschema` (existing, for `config.schema.json` regeneration).

**Spec:** `docs/superpowers/specs/2026-09-10-dynamic-skill-dir-design.md`

## Global Constraints

- `skill_dir` is mutually exclusive with `name`/`description`/`text`/`skill_file`/`arguments` on the same `prompts:` entry -- setting both is a config validation error.
- `skill_dir` only lists files directly inside it (non-recursive); only files ending in `.md` are considered.
- A file's prompt `name` comes from its front matter `name` field; if that's empty, the filename minus its `.md` extension is used instead. `arguments` is always empty for a `skill_dir`-sourced prompt (SKILL.md's front matter format has no field for it).
- At config-load time (`mcprt validate`, `mcprt server` startup, SIGHUP reload), every static prompt name -- across fixed entries and every `skill_dir`'s current files -- must be unique, and every prompt's `text` must parse as a `text/template`; any violation fails the load, exactly as strict as today's `text`/`skill_file` validation.
- At runtime (a `skill_dir` file changing while the server is up), a single bad file (parse error, template syntax error) is logged and skipped -- it must never take down the whole directory's other prompts, let alone the process.
- A `prompts:` entry's index in the YAML list is its priority. A name change at runtime is applied unless a *different, already-registered* entry currently owns that name, in which case the change is skipped and logged (this only matters for a name collision introduced purely at runtime, since config-load's global check already forbids one existing at load time).
- File-change detection uses `fsnotify` (event-driven), debounced by a fixed 300ms before rescanning -- not configurable.
- A `skill_dir`'s file-watcher goroutine is scoped to the same per-generation context (`genCtx`) backend supervisors use, so a SIGHUP-triggered reload's new generation gets its own fresh watcher and the superseded generation's watcher stops via `genCancel`, matching the existing hot-reload lifecycle.
- Runtime updates apply directly to the live `*gateway.Server` (`AddPrompt`/`RemovePrompts`) -- no generation swap, so stdio and HTTP sessions both see the change immediately, unlike SIGHUP-triggered reload.
- Every task ends in a state where `go build ./...`, `gofmt -l .`, `go vet ./...`, and `go test ./...` all pass clean, and ends with a commit.

---

## File Structure

- **`internal/config/config.go`** (modify): `StaticPromptConfig.SkillDir` field, `ScanSkillDir`, `ScanSkillDirLenient`, `SkillFileError`, `validateOnePrompt` (extracted from `validateStaticPrompts`), `validateStaticPrompts` wired to expand+validate `skill_dir` entries.
- **`internal/config/config_test.go`** (modify): table-driven tests for `ScanSkillDir`/`ScanSkillDirLenient` and `Parse`'s `skill_dir` handling.
- **`internal/gateway/static_prompt.go`** (modify): `StaticPrompt.EntryIndex` field.
- **`internal/gateway/gateway.go`** (modify): rename `Server.staticPromptNames map[string]bool` to `Server.promptOwner map[string]int`, populate it from `StaticPrompt.EntryIndex` in `New`, update the 2 existing lookups (`New`'s shadow-warning loop, `completionHandler`).
- **`internal/gateway/reconcile.go`** (modify): update `updatePromptsLocked`'s 2 `staticPromptNames` lookups to `promptOwner`; add `UpdateDirPrompts`.
- **`internal/gateway/reconcile_test.go`** (modify): tests for `UpdateDirPrompts`.
- **`internal/cli/server.go`** (modify): `buildStaticPrompts` returns each `skill_dir` entry's scan result too; `buildOneStaticPrompt` helper; `buildGateway` spawns `watchSkillDir` per `skill_dir` entry.
- **`internal/cli/server_internal_test.go`** (modify): update the 2 existing `buildStaticPrompts` tests for its new return signature; add a `buildGateway` runtime-update integration test.
- **`internal/cli/skill_dir_watcher.go`** (create): `watchSkillDir`, `skillDirDebounce`, `skillDirPromptsByName` helper.
- **`internal/cli/skill_dir_watcher_test.go`** (create): standalone tests for `watchSkillDir` against a directly-constructed `*gateway.Server`.
- **`go.mod`/`go.sum`** (modify): add `github.com/fsnotify/fsnotify`.
- **`config.schema.json`** (regenerate): pick up `skill_dir`.
- **`README.md`** (modify): document `skill_dir`.

---

### Task 1: `internal/config` -- `skill_dir` field, `ScanSkillDir`, strict validation

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.StaticPromptConfig.SkillDir string`, `config.ScanSkillDir(dir string) ([]StaticPromptConfig, error)`. Task 2 and Task 4 consume `ScanSkillDir`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`, near the existing `TestParse_StaticPromptSkillFile*` tests:

```go
func TestScanSkillDir_ParsesFrontMatterAndFallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "with-front-matter.md"), "---\nname: greet\ndescription: greets someone\n---\nhello {{.user}}\n")
	writeFile(t, filepath.Join(dir, "no-front-matter.md"), "just a plain prompt body\n")
	writeFile(t, filepath.Join(dir, "ignored.txt"), "not markdown, must be skipped\n")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "subdir", "nested.md"), "must be ignored: skill_dir is non-recursive\n")

	prompts, err := config.ScanSkillDir(dir)
	if err != nil {
		t.Fatalf("ScanSkillDir: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("ScanSkillDir returned %d prompts, want 2 (subdir and .txt must be excluded): %+v", len(prompts), prompts)
	}
	byName := make(map[string]config.StaticPromptConfig, len(prompts))
	for _, p := range prompts {
		byName[p.Name] = p
	}
	greet, ok := byName["greet"]
	if !ok || greet.Description != "greets someone" || greet.Text != "hello {{.user}}\n" {
		t.Fatalf("prompts[\"greet\"] = %+v, want description=%q text=%q", greet, "greets someone", "hello {{.user}}\n")
	}
	plain, ok := byName["no-front-matter"]
	if !ok || plain.Text != "just a plain prompt body\n" {
		t.Fatalf("prompts[\"no-front-matter\"] (from filename fallback) = %+v, want text=%q", plain, "just a plain prompt body\n")
	}
}

func TestScanSkillDir_MissingDirectoryReturnsError(t *testing.T) {
	if _, err := config.ScanSkillDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("ScanSkillDir: expected error for missing directory, got nil")
	}
}

func TestParse_StaticPromptSkillDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "greet.md"), "hello {{.user}}\n")

	data := []byte(fmt.Sprintf(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - skill_dir: %q
`, dir))
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Prompts) != 1 || cfg.Prompts[0].SkillDir != dir {
		t.Fatalf("cfg.Prompts = %+v, want one entry with SkillDir=%q (Parse does not expand skill_dir into cfg.Prompts itself)", cfg.Prompts, dir)
	}
}

func TestParse_StaticPromptSkillDirMutualExclusionRejected(t *testing.T) {
	dir := t.TempDir()
	data := []byte(fmt.Sprintf(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - skill_dir: %q
    text: "also set, must be rejected"
`, dir))
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for skill_dir combined with text, got nil")
	}
}

func TestParse_StaticPromptSkillDirMissingDirectoryRejected(t *testing.T) {
	data := []byte(fmt.Sprintf(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - skill_dir: %q
`, filepath.Join(t.TempDir(), "does-not-exist")))
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for missing skill_dir, got nil")
	}
}

func TestParse_StaticPromptSkillDirDuplicateNameAcrossSourcesRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "greet.md"), "hello from the directory\n")

	data := []byte(fmt.Sprintf(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - name: greet
    text: "hello from a fixed entry"
  - skill_dir: %q
`, dir))
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for a skill_dir file colliding with a fixed prompt's name, got nil")
	}
}

func TestParse_StaticPromptSkillDirDuplicateNameWithinDirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "---\nname: greet\n---\nfirst\n")
	writeFile(t, filepath.Join(dir, "b.md"), "---\nname: greet\n---\nsecond\n")

	data := []byte(fmt.Sprintf(`
backends:
  - name: a
    transport: stdio
    command: ["x"]

prompts:
  - skill_dir: %q
`, dir))
	if _, err := config.Parse(data); err == nil {
		t.Fatal("Parse: expected error for two files in the same skill_dir producing the same name, got nil")
	}
}

// writeFile is a small t.Fatal-on-error wrapper shared by the skill_dir
// tests above, matching this file's existing terse test-helper style.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
```

Check `internal/config/config_test.go`'s existing imports (top of file) for `"os"`, `"path/filepath"`, and `"fmt"` -- add whichever of these three aren't already imported.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/... -run 'TestScanSkillDir|TestParse_StaticPromptSkillDir' -v`
Expected: FAIL with `undefined: config.ScanSkillDir` and `unknown field SkillDir in struct literal` (from the YAML-driven tests, once `skill_dir:` is unmarshaled by `yaml.v3` into a struct with no matching field it's silently dropped, not a compile error -- so those specific tests will instead fail at the `err == nil`/content assertions, not at compile time; the `ScanSkillDir` tests do fail to compile).

- [ ] **Step 3: Implement `SkillDir`, `ScanSkillDir`, and strict validation wiring**

In `internal/config/config.go`, add `SkillDir` to `StaticPromptConfig` (after `SkillFile`, updating the struct's doc comment):

```go
// StaticPromptConfig defines a prompt mcprt serves directly from config,
// without forwarding prompts/get to any backend. See
// internal/gateway.StaticPrompt for the runtime representation built from
// this at gateway-construction time (internal/cli's buildStaticPrompts
// parses Text as a template there; validateStaticPrompts below only checks
// it parses, it doesn't keep the *template.Template around).
//
// SkillFile, when set, is resolved by expandSkillFiles (called from Parse,
// before validation) into Name/Description/Text -- whichever of those three
// fields is still empty after that. A field set here in config.yaml always
// wins over the skill_file's front matter/body, so a single entry can
// override any of them, or add Arguments (which a SKILL.md's front matter
// doesn't carry), without touching the file.
//
// SkillDir, when set, must be the only non-empty field on this entry (see
// validateStaticPrompts): it names a directory whose *.md files (direct
// children only, no recursion) each become their own prompt via
// ScanSkillDir, using front matter the same way SkillFile does, with the
// filename (minus ".md") as the name fallback when front matter omits one.
// Unlike SkillFile, SkillDir's contents are re-read live while mcprt is
// running (see internal/cli's watchSkillDir) -- SkillFile is not.
type StaticPromptConfig struct {
	Name        string                 `yaml:"name,omitempty" json:"name,omitempty"`
	Description string                 `yaml:"description,omitempty" json:"description,omitempty"`
	Arguments   []StaticPromptArgument `yaml:"arguments,omitempty" json:"arguments,omitempty"`
	Text        string                 `yaml:"text,omitempty" json:"text,omitempty"`
	SkillFile   string                 `yaml:"skill_file,omitempty" json:"skill_file,omitempty"`
	SkillDir    string                 `yaml:"skill_dir,omitempty" json:"skill_dir,omitempty"`
}
```

Add `ScanSkillDir` and its private helpers right after `parseSkillFile` (which ends around line 275), before `expandHome`:

```go
// ScanSkillDir lists dir's *.md files (direct children only, no recursion --
// os.ReadDir already returns entries sorted by filename, so callers get a
// stable, reproducible ordering across repeated scans of an unchanged
// directory) and parses each via parseSkillFile into a StaticPromptConfig.
// A file whose front matter omits name falls back to the filename minus its
// ".md" extension. Arguments is always empty -- SKILL.md's front matter
// format has no field for it. Aborts on the first file that fails to read
// or parse, matching config.Parse's existing all-or-nothing strictness at
// startup/SIGHUP reload; ScanSkillDirLenient (added in a later task) is the
// runtime rescan's tolerant counterpart.
func ScanSkillDir(dir string) ([]StaticPromptConfig, error) {
	files, err := listSkillDirFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("skill_dir %q: %w", dir, err)
	}
	out := make([]StaticPromptConfig, 0, len(files))
	for _, name := range files {
		p, err := parseSkillDirFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("skill_dir %q: file %q: %w", dir, name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// listSkillDirFiles returns dir's direct-child *.md filenames (not full
// paths), in the sorted order os.ReadDir already guarantees.
func listSkillDirFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		files = append(files, e.Name())
	}
	return files, nil
}

// parseSkillDirFile reads dir/name and parses it exactly like a skill_file
// entry (parseSkillFile), falling back to name (minus ".md") when front
// matter omits Name. Arguments is always left empty.
func parseSkillDirFile(dir, name string) (StaticPromptConfig, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return StaticPromptConfig{}, err
	}
	pname, description, body, err := parseSkillFile(data)
	if err != nil {
		return StaticPromptConfig{}, err
	}
	if pname == "" {
		pname = strings.TrimSuffix(name, ".md")
	}
	return StaticPromptConfig{Name: pname, Description: description, Text: body}, nil
}
```

Refactor `validateStaticPrompts` (currently around line 456) to extract its per-prompt body into `validateOnePrompt`, and add the `skill_dir` branch:

```go
// validateStaticPrompts rejects an empty/duplicate prompt or argument name,
// an empty text body, and a text body that doesn't parse as a Go
// text/template -- so a broken prompts: entry fails fast at config-load
// time (mcprt validate/server startup/SIGHUP reload), the same as every
// other misconfiguration this file checks. A skill_dir entry is expanded via
// ScanSkillDir first, and every file it produces is checked exactly like a
// fixed entry, against the SAME seen map -- so a name collision between two
// skill_dir files, or between a skill_dir file and a fixed entry, fails
// config-load just as strictly as two fixed entries sharing a name always
// have.
func validateStaticPrompts(prompts []StaticPromptConfig) error {
	seen := make(map[string]bool, len(prompts))
	for _, p := range prompts {
		if p.SkillDir == "" {
			if err := validateOnePrompt(p, seen); err != nil {
				return err
			}
			continue
		}
		if p.Name != "" || p.Description != "" || p.Text != "" || p.SkillFile != "" || len(p.Arguments) > 0 {
			return fmt.Errorf("prompts: skill_dir %q: name/description/text/skill_file/arguments must be empty when skill_dir is set", p.SkillDir)
		}
		dirPrompts, err := ScanSkillDir(p.SkillDir)
		if err != nil {
			return err
		}
		for _, dp := range dirPrompts {
			if err := validateOnePrompt(dp, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateOnePrompt is validateStaticPrompts' per-prompt check, shared by a
// fixed prompts: entry and every file a skill_dir entry expands to (both
// checked against the same seen map, so names collide across sources the
// same way they always have across fixed entries).
func validateOnePrompt(p StaticPromptConfig, seen map[string]bool) error {
	if p.Name == "" {
		return fmt.Errorf("prompts: name is required")
	}
	if seen[p.Name] {
		return fmt.Errorf("prompts %q: duplicate name", p.Name)
	}
	seen[p.Name] = true
	if p.Text == "" {
		return fmt.Errorf("prompts %q: text is required", p.Name)
	}
	if _, err := template.New(p.Name).Parse(p.Text); err != nil {
		return fmt.Errorf("prompts %q: parse text template: %w", p.Name, err)
	}
	argSeen := make(map[string]bool, len(p.Arguments))
	for _, a := range p.Arguments {
		if a.Name == "" {
			return fmt.Errorf("prompts %q: argument name is required", p.Name)
		}
		if argSeen[a.Name] {
			return fmt.Errorf("prompts %q: duplicate argument %q", p.Name, a.Name)
		}
		argSeen[a.Name] = true
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS (including every pre-existing test in this package -- `validateOnePrompt`'s body is a verbatim extraction, so no existing `TestParse_StaticPrompt*` behavior changes).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
feat(config): add skill_dir for auto-discovering prompts from a directory

skill_dir lists a directory's *.md files (non-recursive) and parses each
like an existing skill_file entry, so many prompts can be added without
growing config.yaml's prompts: list one entry at a time. Strict at
config-load time: every prompt name across every source must still be
unique, and every text must still parse as a template.
EOF
)"
```

---

### Task 2: `internal/config` -- `ScanSkillDirLenient` for the runtime rescan path

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `listSkillDirFiles`, `parseSkillDirFile` (Task 1, same package, unexported).
- Produces: `config.SkillFileError{File string; Err error}` (with an `Error() string` method), `config.ScanSkillDirLenient(dir string) (prompts []StaticPromptConfig, skipped []SkillFileError, err error)`. Task 5 consumes `ScanSkillDirLenient`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestScanSkillDirLenient_SkipsUnparseableFileAndKeepsOthers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "good.md"), "---\nname: good\n---\nfine\n")
	writeFile(t, filepath.Join(dir, "bad.md"), "---\nname: [this is not valid yaml\n---\nbroken front matter\n")

	prompts, skipped, err := config.ScanSkillDirLenient(dir)
	if err != nil {
		t.Fatalf("ScanSkillDirLenient: %v", err)
	}
	if len(prompts) != 1 || prompts[0].Name != "good" {
		t.Fatalf("prompts = %+v, want exactly the \"good\" entry", prompts)
	}
	if len(skipped) != 1 || skipped[0].File != "bad.md" {
		t.Fatalf("skipped = %+v, want exactly one entry for bad.md", skipped)
	}
}

func TestScanSkillDirLenient_DuplicateNameKeepsAlphabeticallyFirstFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a-first.md"), "---\nname: dup\n---\nfrom a-first\n")
	writeFile(t, filepath.Join(dir, "b-second.md"), "---\nname: dup\n---\nfrom b-second\n")

	prompts, skipped, err := config.ScanSkillDirLenient(dir)
	if err != nil {
		t.Fatalf("ScanSkillDirLenient: %v", err)
	}
	if len(prompts) != 1 || prompts[0].Text != "from a-first\n" {
		t.Fatalf("prompts = %+v, want exactly one entry with text from a-first.md (alphabetically first)", prompts)
	}
	if len(skipped) != 1 || skipped[0].File != "b-second.md" {
		t.Fatalf("skipped = %+v, want b-second.md reported as skipped", skipped)
	}
}

func TestScanSkillDirLenient_MissingDirectoryReturnsError(t *testing.T) {
	if _, _, err := config.ScanSkillDirLenient(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("ScanSkillDirLenient: expected error for missing directory, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/... -run TestScanSkillDirLenient -v`
Expected: FAIL with `undefined: config.ScanSkillDirLenient`.

- [ ] **Step 3: Implement `ScanSkillDirLenient`**

Add to `internal/config/config.go`, after `ScanSkillDir`:

```go
// SkillFileError records one file ScanSkillDirLenient could not use --
// either it failed to read/parse, or its name duplicated an
// alphabetically-earlier file's in the same directory.
type SkillFileError struct {
	File string
	Err  error
}

func (e SkillFileError) Error() string { return fmt.Sprintf("%s: %v", e.File, e.Err) }

// ScanSkillDirLenient is ScanSkillDir's tolerant counterpart, for a live
// rescan while mcprt is already running (see internal/cli's
// watchSkillDir): a file that fails to read/parse, or whose name duplicates
// an earlier file's (files are processed in listSkillDirFiles' sorted
// order, so "earlier" means alphabetically first), is excluded from prompts
// and reported via skipped instead of aborting the whole scan -- one bad
// edit must not take down every other prompt the directory serves. err is
// non-nil only for a directory-level failure (the directory itself can't be
// listed), which the runtime path treats as "keep whatever was registered
// before, try again next event" rather than clearing anything.
func ScanSkillDirLenient(dir string) (prompts []StaticPromptConfig, skipped []SkillFileError, err error) {
	files, err := listSkillDirFiles(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("skill_dir %q: %w", dir, err)
	}
	seenBy := make(map[string]string, len(files)) // prompt name -> file that already claimed it
	for _, name := range files {
		p, perr := parseSkillDirFile(dir, name)
		if perr != nil {
			skipped = append(skipped, SkillFileError{File: name, Err: perr})
			continue
		}
		if first, dup := seenBy[p.Name]; dup {
			skipped = append(skipped, SkillFileError{File: name, Err: fmt.Errorf("duplicate name %q (already used by %q)", p.Name, first)})
			continue
		}
		seenBy[p.Name] = name
		prompts = append(prompts, p)
	}
	return prompts, skipped, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "$(cat <<'EOF'
feat(config): add ScanSkillDirLenient for live skill_dir rescans

Unlike ScanSkillDir (used at strict config-load time), a bad file here is
reported and excluded instead of aborting -- needed for the upcoming
runtime file-watcher, where one broken edit must not take down every
other prompt the directory serves.
EOF
)"
```

---

### Task 3: `internal/gateway` -- `EntryIndex`, `promptOwner`, `UpdateDirPrompts`

**Files:**
- Modify: `internal/gateway/static_prompt.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/reconcile.go`
- Test: `internal/gateway/reconcile_test.go`

**Interfaces:**
- Produces: `gateway.StaticPrompt.EntryIndex int` (exported field, zero-value default), `(*gateway.Server).UpdateDirPrompts(entryIndex int, added []*StaticPrompt, removed []string)`. Task 4 sets `EntryIndex`; Task 5's `watchSkillDir` calls `UpdateDirPrompts`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/gateway/reconcile_test.go`:

```go
func TestUpdateDirPrompts_AddsAndRemovesByDiff(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: map[string]*backend.Backend{}})

	first, err := gateway.NewStaticPrompt("alpha", "", nil, "alpha text")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	first.EntryIndex = 0
	srv.UpdateDirPrompts(0, []*gateway.StaticPrompt{first}, nil)

	second, err := gateway.NewStaticPrompt("beta", "", nil, "beta text")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	second.EntryIndex = 0
	srv.UpdateDirPrompts(0, []*gateway.StaticPrompt{second}, []string{"alpha"})

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "beta"}); err != nil {
		t.Fatalf("GetPrompt(beta): %v, want it registered", err)
	}
	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "alpha"}); err == nil {
		t.Fatal("GetPrompt(alpha) succeeded, want an error: it should have been removed")
	}
}

func TestUpdateDirPrompts_SkipsNameOwnedByAnotherEntry(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fixed, err := gateway.NewStaticPrompt("shared", "", nil, "fixed entry's text")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	fixed.EntryIndex = 0

	srv := gateway.New(gateway.NewConfig{
		Logger:        logger,
		Backends:      map[string]*backend.Backend{},
		StaticPrompts: []*gateway.StaticPrompt{fixed},
	})

	dirVersion, err := gateway.NewStaticPrompt("shared", "", nil, "skill_dir's text, must be rejected")
	if err != nil {
		t.Fatalf("NewStaticPrompt: %v", err)
	}
	dirVersion.EntryIndex = 1
	srv.UpdateDirPrompts(1, []*gateway.StaticPrompt{dirVersion}, nil)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "shared"})
	if err != nil {
		t.Fatalf("GetPrompt(shared): %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "fixed entry's text" {
		t.Fatalf("GetPrompt(shared) content = %+v, want the fixed entry's text (entry 0 must keep winning over entry 1)", res.Messages[0].Content)
	}
}
```

Check `internal/gateway/reconcile_test.go`'s existing imports for `"context"`, `"io"`, `"log/slog"`, `"net/http"`, `"net/http/httptest"`, `"github.com/modelcontextprotocol/go-sdk/mcp"` -- these are already used by the file's other tests (per `TestUpdatePrompts_StaticPromptStillWinsAfterBackendReportsCollidingName`), so no new imports should be needed.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/gateway/... -run TestUpdateDirPrompts -v`
Expected: FAIL to compile with `sp.EntryIndex undefined` and `srv.UpdateDirPrompts undefined`.

- [ ] **Step 3: Implement `EntryIndex`, `promptOwner`, and `UpdateDirPrompts`**

In `internal/gateway/static_prompt.go`, add the field to `StaticPrompt`:

```go
// StaticPrompt is a prompt mcprt serves directly from config, without
// forwarding prompts/get to any backend. Unlike a backend-sourced prompt, it
// always wins a name collision (see New in gateway.go and
// updatePromptsLocked in reconcile.go).
type StaticPrompt struct {
	Prompt   *mcp.Prompt
	Template *template.Template
	// EntryIndex is this prompt's originating prompts: entry's index in
	// config.Config.Prompts -- the same index for every StaticPrompt one
	// skill_dir entry expands to. It is this prompt's priority for
	// Server.promptOwner (see gateway.go/reconcile.go): a smaller index
	// wins. Left at its zero value by NewStaticPrompt itself; internal/cli's
	// buildStaticPrompts sets it explicitly after construction.
	EntryIndex int
}
```

In `internal/gateway/gateway.go`, rename the field and its 2 usages. Replace the `Server` struct's field:

```go
	// promptOwner maps every currently-registered static prompt name (from
	// a fixed prompts: entry, or currently produced by a skill_dir entry) to
	// the index of the prompts: entry that owns it -- the same index
	// StaticPrompt.EntryIndex carries. Populated once in New from
	// NewConfig.StaticPrompts, and mutated afterward only by
	// UpdateDirPrompts (reconcile.go), always under mu like every other
	// Server field below.
	promptOwner map[string]int
```

Replace `New`'s construction of `staticNames`:

```go
func New(cfg NewConfig) *Server {
	promptOwner := make(map[string]int, len(cfg.StaticPrompts))
	for _, sp := range cfg.StaticPrompts {
		promptOwner[sp.Prompt.Name] = sp.EntryIndex
	}

	s := &Server{
		logger:   cfg.Logger,
		backends: cfg.Backends,
		maskKeys: cfg.MaskKeys,
		relays:   cfg.Relays,

		watchedSessions: make(map[*mcp.ServerSession]bool),

		toolEntries:   cfg.Entries.Tools,
		toolTable:     emptyTable(cfg.Tables.Tools),
		toolOverrides: cfg.Overrides.Tools,

		resourceEntries:           cfg.Entries.Resources,
		resourceTable:             emptyTable(cfg.Tables.Resources),
		resourceOverrides:         cfg.Overrides.Resources,
		resourceTemplateEntries:   cfg.Entries.ResourceTemplates,
		resourceTemplateTable:     emptyTable(cfg.Tables.ResourceTemplates),
		resourceTemplateOverrides: cfg.Overrides.ResourceTemplates,

		promptEntries:   cfg.Entries.Prompts,
		promptTable:     emptyTable(cfg.Tables.Prompts),
		promptOverrides: cfg.Overrides.Prompts,

		promptOwner: promptOwner,
	}
```

And its shadow-warning loop:

```go
	if cfg.Tables.Prompts != nil {
		for _, resolved := range cfg.Tables.Prompts.Items {
			if _, ok := promptOwner[resolved.Item.Name]; ok {
				LogEvent(context.Background(), cfg.Logger, slog.LevelWarn, EventPromptShadowedByStatic,
					"prompt", resolved.Item.Name, "backend", resolved.BackendName)
				continue
			}
			registerPrompt(mcpSrv, cfg.Logger, cfg.Backends, resolved, cfg.MaskKeys)
		}
	}
```

And `completionHandler`'s check:

```go
		if _, ok := s.promptOwner[ref.Name]; ok {
```

(replacing the earlier `if s.staticPromptNames[ref.Name] {`, same surrounding code otherwise).

In `internal/gateway/reconcile.go`, update `updatePromptsLocked`'s two lookups:

```go
func (s *Server) updatePromptsLocked(backendName string, items []*mcp.Prompt, rebind bool) {
	s.promptEntries = replaceEntry(s.promptEntries, backendName, items)
	newTable := router.Resolve(s.promptEntries, PromptNameOf, PromptRename, s.promptOverrides)

	for name := range s.promptTable.Items {
		if _, ok := s.promptOwner[name]; ok {
			continue // never registered from the table in the first place; RemovePrompts would incorrectly drop the static prompt
		}
		if _, ok := newTable.Items[name]; !ok {
			s.mcp.RemovePrompts(name)
		}
	}
	for name, resolved := range newTable.Items {
		if _, ok := s.promptOwner[name]; ok {
			continue // a static config prompt always wins; never register a backend's version under this name
		}
		old, ok := s.promptTable.Items[name]
		if !touchedBy(resolved, old, ok, backendName) {
			continue
		}
		unchanged := ok && reflect.DeepEqual(old, resolved)
		if unchanged && (!rebind || !boundTo(resolved, backendName)) {
			continue
		}
		registerPrompt(s.mcp, s.logger, s.backends, resolved, s.maskKeys)
	}
	logNewConflicts(s.logger, "prompt", s.promptTable.Conflicts, newTable.Conflicts)

	s.promptTable = newTable
}
```

Then add `UpdateDirPrompts` after `updatePromptsLocked`:

```go
// UpdateDirPrompts applies a skill_dir rescan's diff to the live server:
// added holds the *StaticPrompt values to (re)register (already
// EntryIndex-tagged), removed holds the prompt names entryIndex's directory
// no longer produces. Both are computed by the caller (internal/cli's
// watchSkillDir) by diffing its own record of what that directory scanned
// last time -- this method's only added responsibility is the cross-entry
// ownership check: a name already owned by a DIFFERENT entryIndex (a
// higher-priority skill_dir, or a fixed prompts: entry -- unreachable at
// config-load time thanks to validateStaticPrompts' global duplicate check,
// but only ever unreachable AT LOAD time; two directories can still
// introduce the same new name purely at runtime, which this guards against)
// is skipped and logged rather than stolen. AddPrompt/RemovePrompts on
// s.mcp trigger go-sdk's own notifications/prompts/list_changed to every
// downstream client -- there is no separate notification step here.
func (s *Server) UpdateDirPrompts(entryIndex int, added []*StaticPrompt, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, name := range removed {
		if s.promptOwner[name] == entryIndex {
			delete(s.promptOwner, name)
			s.mcp.RemovePrompts(name)
		}
	}
	for _, sp := range added {
		name := sp.Prompt.Name
		if owner, ok := s.promptOwner[name]; ok && owner != entryIndex {
			s.logger.Warn("skill_dir: prompt name claimed by another source, skipping",
				"name", name, "entry_index", entryIndex, "owner_entry_index", owner)
			continue
		}
		s.mcp.AddPrompt(sp.Prompt, staticPromptHandler(s.logger, s.maskKeys, sp))
		s.promptOwner[name] = entryIndex
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gateway/... -v`
Expected: PASS (including every pre-existing test in this package).

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/static_prompt.go internal/gateway/gateway.go internal/gateway/reconcile.go internal/gateway/reconcile_test.go
git commit -m "$(cat <<'EOF'
feat(gateway): add UpdateDirPrompts for live skill_dir registration changes

Replaces the fixed staticPromptNames set with promptOwner, tracking which
prompts: entry (by index, i.e. priority) currently owns each registered
static prompt name. UpdateDirPrompts applies an already-diffed add/remove
list to the live *mcp.Server, so go-sdk's own AddPrompt/RemovePrompts
send notifications/prompts/list_changed without any extra plumbing.
EOF
)"
```

---

### Task 4: `internal/cli` -- `buildStaticPrompts` returns each `skill_dir`'s scan

**Files:**
- Modify: `internal/cli/server.go`
- Test: `internal/cli/server_internal_test.go`

**Interfaces:**
- Consumes: `config.ScanSkillDir` (Task 1), `gateway.NewStaticPrompt`, `gateway.StaticPrompt.EntryIndex` (Task 3).
- Produces: `buildStaticPrompts(prompts []config.StaticPromptConfig) (out []*gateway.StaticPrompt, dirScans map[int][]config.StaticPromptConfig, err error)`, `buildOneStaticPrompt(p config.StaticPromptConfig, entryIndex int) (*gateway.StaticPrompt, error)`. Task 5's `skill_dir_watcher_test.go` and Task 6 consume `buildOneStaticPrompt`/`buildStaticPrompts`.

- [ ] **Step 1: Update the existing tests for the new return signature**

In `internal/cli/server_internal_test.go`, `TestBuildStaticPrompts_ConvertsConfig` and `TestBuildStaticPrompts_InvalidTemplatePropagatesError` currently call `buildStaticPrompts` with 2 return values. Update both call sites:

```go
func TestBuildStaticPrompts_ConvertsConfig(t *testing.T) {
	prompts := []config.StaticPromptConfig{
		{
			Name:        "greet",
			Description: "greets someone",
			Arguments:   []config.StaticPromptArgument{{Name: "name", Description: "who to greet", Required: true}},
			Text:        "hello {{.name}}",
		},
	}
	out, dirScans, err := buildStaticPrompts(prompts)
	if err != nil {
		t.Fatalf("buildStaticPrompts: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("buildStaticPrompts returned %d entries, want 1", len(out))
	}
	sp := out[0]
	if sp.Prompt.Name != "greet" || sp.Prompt.Description != "greets someone" {
		t.Fatalf("Prompt = %+v, want name=greet description=%q", sp.Prompt, "greets someone")
	}
	if len(sp.Prompt.Arguments) != 1 || sp.Prompt.Arguments[0].Name != "name" || !sp.Prompt.Arguments[0].Required {
		t.Fatalf("Prompt.Arguments = %+v, want one required argument named \"name\"", sp.Prompt.Arguments)
	}
	if sp.EntryIndex != 0 {
		t.Fatalf("sp.EntryIndex = %d, want 0", sp.EntryIndex)
	}
	if len(dirScans) != 0 {
		t.Fatalf("dirScans = %+v, want empty (no skill_dir entries)", dirScans)
	}
}

func TestBuildStaticPrompts_InvalidTemplatePropagatesError(t *testing.T) {
	prompts := []config.StaticPromptConfig{{Name: "bad", Text: "{{.unterminated"}}
	if _, _, err := buildStaticPrompts(prompts); err == nil {
		t.Fatal("buildStaticPrompts: expected error for invalid template, got nil")
	}
}

func TestBuildStaticPrompts_SkillDirExpandsAndRecordsScan(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "greet.md"), []byte("hello {{.user}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompts := []config.StaticPromptConfig{
		{Name: "fixed", Text: "fixed text"},
		{SkillDir: dir},
	}
	out, dirScans, err := buildStaticPrompts(prompts)
	if err != nil {
		t.Fatalf("buildStaticPrompts: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("buildStaticPrompts returned %d entries, want 2 (1 fixed + 1 from skill_dir)", len(out))
	}
	var fixed, fromDir *gateway.StaticPrompt
	for _, sp := range out {
		switch sp.Prompt.Name {
		case "fixed":
			fixed = sp
		case "greet":
			fromDir = sp
		}
	}
	if fixed == nil || fixed.EntryIndex != 0 {
		t.Fatalf("fixed entry = %+v, want EntryIndex 0", fixed)
	}
	if fromDir == nil || fromDir.EntryIndex != 1 {
		t.Fatalf("skill_dir entry = %+v, want name=greet EntryIndex=1", fromDir)
	}
	scan, ok := dirScans[1]
	if !ok || len(scan) != 1 || scan[0].Name != "greet" {
		t.Fatalf("dirScans[1] = %+v, want the one-entry scan of dir", scan)
	}
}
```

Check `internal/cli/server_internal_test.go`'s existing imports for `"os"` and `"path/filepath"` -- add whichever isn't already imported.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go build ./... `
Expected: FAIL with `assignment mismatch: 2 variables but buildStaticPrompts returns 1 value` at every call site (including inside `buildGateway` itself, which this task does not yet update -- that's expected and fixed in this same task's implementation step).

- [ ] **Step 3: Implement the new `buildStaticPrompts` signature**

In `internal/cli/server.go`, replace `buildStaticPrompts` and add `buildOneStaticPrompt`:

```go
// buildOneStaticPrompt converts one config.StaticPromptConfig (a fixed
// entry, or one file a skill_dir entry expanded to) into the
// *gateway.StaticPrompt New actually registers, tagging it with entryIndex
// (its originating prompts: entry's position, i.e. its priority -- see
// gateway.Server.promptOwner).
func buildOneStaticPrompt(p config.StaticPromptConfig, entryIndex int) (*gateway.StaticPrompt, error) {
	args := make([]*mcp.PromptArgument, 0, len(p.Arguments))
	for _, a := range p.Arguments {
		args = append(args, &mcp.PromptArgument{Name: a.Name, Description: a.Description, Required: a.Required})
	}
	sp, err := gateway.NewStaticPrompt(p.Name, p.Description, args, p.Text)
	if err != nil {
		return nil, err
	}
	sp.EntryIndex = entryIndex
	return sp, nil
}

// buildStaticPrompts converts every prompts: entry into the
// *gateway.StaticPrompt list New actually registers, expanding each
// skill_dir entry via config.ScanSkillDir along the way (internal/config's
// validateStaticPrompts already rejects an unparseable template or a
// skill_dir that can't be scanned at config-load time, so an error here
// should only happen if that validation was skipped somehow -- still
// handled, not assumed impossible). dirScans records, per skill_dir entry's
// index, exactly the []config.StaticPromptConfig its scan produced -- the
// caller (buildGateway) passes this to watchSkillDir as the starting point
// for that directory's live diffing, so the watcher's first rescan compares
// against precisely what got registered here, not a second, differently-
// timed scan of the same directory.
func buildStaticPrompts(prompts []config.StaticPromptConfig) (out []*gateway.StaticPrompt, dirScans map[int][]config.StaticPromptConfig, err error) {
	dirScans = make(map[int][]config.StaticPromptConfig)
	for i, p := range prompts {
		if p.SkillDir == "" {
			sp, err := buildOneStaticPrompt(p, i)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, sp)
			continue
		}
		dirPrompts, err := config.ScanSkillDir(p.SkillDir)
		if err != nil {
			return nil, nil, err
		}
		dirScans[i] = dirPrompts
		for _, dp := range dirPrompts {
			sp, err := buildOneStaticPrompt(dp, i)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, sp)
		}
	}
	return out, dirScans, nil
}
```

Update `buildGateway`'s call site (it currently reads `staticPrompts, err := buildStaticPrompts(cfg.Prompts)`):

```go
	staticPrompts, dirScans, err := buildStaticPrompts(cfg.Prompts)
	if err != nil {
		return nil, err
	}
```

(`dirScans` is unused until Task 6 wires it into a `watchSkillDir` call -- to keep this task's `go build ./...` clean without an unused-variable error, assign it to `_` for now: `staticPrompts, _, err := buildStaticPrompts(cfg.Prompts)`. Task 6 changes this back to `dirScans` when it adds the code that uses it.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/cli/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "$(cat <<'EOF'
feat(cli): expand skill_dir entries in buildStaticPrompts

buildStaticPrompts now also returns, per skill_dir entry, the exact scan
it used to build that entry's prompts -- the upcoming file-watcher will
diff future rescans against this same baseline instead of a second,
differently-timed scan of the same directory.
EOF
)"
```

---

### Task 5: `internal/cli` -- `watchSkillDir` (fsnotify, debounce, diff)

**Files:**
- Create: `internal/cli/skill_dir_watcher.go`
- Create: `internal/cli/skill_dir_watcher_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `config.ScanSkillDirLenient` (Task 2), `buildOneStaticPrompt` (Task 4), `(*gateway.Server).UpdateDirPrompts` (Task 3).
- Produces: `watchSkillDir(ctx context.Context, logger *slog.Logger, dir string, entryIndex int, initial []config.StaticPromptConfig, gw *gateway.Server)`, `skillDirDebounce time.Duration` (var). Task 6 consumes `watchSkillDir`.

- [ ] **Step 1: Add the fsnotify dependency**

Run: `go get github.com/fsnotify/fsnotify@latest && go mod tidy`
Expected: `go.mod`/`go.sum` gain `github.com/fsnotify/fsnotify`.

- [ ] **Step 2: Write the failing test**

Create `internal/cli/skill_dir_watcher_test.go`:

```go
package cli

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wtnb75/mcprt/internal/backend"
	"github.com/wtnb75/mcprt/internal/config"
	"github.com/wtnb75/mcprt/internal/gateway"
)

// waitForPrompt polls session.GetPrompt(name) until it succeeds/fails
// matching want, or deadline passes -- watchSkillDir's debounce plus
// fsnotify's own OS-level delivery latency mean a file-system change is
// never visible to the gateway synchronously.
func waitForPromptPresence(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, wantPresent bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: name})
		if (err == nil) == wantPresent {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("prompt %q presence never became %v within the deadline", name, wantPresent)
}

func TestWatchSkillDir_PicksUpAddedChangedAndRemovedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.md"), []byte("keep body v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initial, err := config.ScanSkillDir(dir)
	if err != nil {
		t.Fatalf("ScanSkillDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var staticPrompts []*gateway.StaticPrompt
	for _, p := range initial {
		sp, err := buildOneStaticPrompt(p, 0)
		if err != nil {
			t.Fatalf("buildOneStaticPrompt: %v", err)
		}
		staticPrompts = append(staticPrompts, sp)
	}
	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: map[string]*backend.Backend{}, StaticPrompts: staticPrompts})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSkillDir(ctx, logger, dir, 0, initial, srv)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// Add a new file.
	if err := os.WriteFile(filepath.Join(dir, "added.md"), []byte("added body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForPromptPresence(t, ctx, session, "added", true)

	// Change an existing file's content.
	if err := os.WriteFile(filepath.Join(dir, "keep.md"), []byte("keep body v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "keep"})
		if err == nil {
			if text, ok := res.Messages[0].Content.(*mcp.TextContent); ok && text.Text == "keep body v2\n" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("prompt \"keep\" never picked up its updated body within the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Remove a file.
	if err := os.Remove(filepath.Join(dir, "added.md")); err != nil {
		t.Fatal(err)
	}
	waitForPromptPresence(t, ctx, session, "added", false)
}

func TestWatchSkillDir_SkipsUnparseableEditWithoutRemovingOthers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "good.md"), []byte("good body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	initial, err := config.ScanSkillDir(dir)
	if err != nil {
		t.Fatalf("ScanSkillDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp, err := buildOneStaticPrompt(initial[0], 0)
	if err != nil {
		t.Fatalf("buildOneStaticPrompt: %v", err)
	}
	srv := gateway.New(gateway.NewConfig{Logger: logger, Backends: map[string]*backend.Backend{}, StaticPrompts: []*gateway.StaticPrompt{sp}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSkillDir(ctx, logger, dir, 0, initial, srv)

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// Add a file with an unterminated template action -- buildOneStaticPrompt
	// must reject it, and that rejection must not remove "good".
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte("{{.unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// There is no positive event to wait for here (the bad file is never
	// meant to become visible), so wait out one debounce window instead.
	time.Sleep(skillDirDebounce * 3)

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "good"}); err != nil {
		t.Fatalf("GetPrompt(good): %v, want it still registered after an unrelated bad file appeared", err)
	}
	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "bad"}); err == nil {
		t.Fatal("GetPrompt(bad) succeeded, want it rejected (unterminated template action)")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/cli/... -run TestWatchSkillDir -v`
Expected: FAIL to compile with `undefined: watchSkillDir` and `undefined: skillDirDebounce`.

- [ ] **Step 4: Implement `watchSkillDir`**

Create `internal/cli/skill_dir_watcher.go`:

```go
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

		var added []*gateway.StaticPrompt
		for name, p := range freshByName {
			if old, ok := last[name]; ok && reflect.DeepEqual(old, p) {
				continue
			}
			sp, err := buildOneStaticPrompt(p, entryIndex)
			if err != nil {
				logger.Warn("skill_dir: skipping file with invalid template", "dir", dir, "name", name, "error", err)
				continue
			}
			added = append(added, sp)
		}

		if len(added) == 0 && len(removedNames) == 0 {
			return
		}
		gw.UpdateDirPrompts(entryIndex, added, removedNames)
		last = freshByName
	}

	var debounce *time.Timer
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
				debounce = time.AfterFunc(skillDirDebounce, rescan)
			} else {
				debounce.Reset(skillDirDebounce)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Warn("skill_dir: watcher error", "dir", dir, "error", err)
		}
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/cli/... -run TestWatchSkillDir -v`
Expected: PASS. (These tests exercise real filesystem events and short sleeps, so they're not instant, but should complete well within Go's default test timeout.)

- [ ] **Step 6: Run the full test suite**

Run: `go build ./... && gofmt -l . && go vet ./... && go test ./...`
Expected: PASS, `gofmt -l .` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/skill_dir_watcher.go internal/cli/skill_dir_watcher_test.go go.mod go.sum
git commit -m "$(cat <<'EOF'
feat(cli): add watchSkillDir for live skill_dir file-change detection

Watches a skill_dir with fsnotify, debounces bursts of events by 300ms,
rescans leniently (a bad file is logged and skipped, not fatal), diffs
against the last applied scan, and applies exactly the changed names via
gateway.Server.UpdateDirPrompts. Not yet wired into buildGateway.
EOF
)"
```

---

### Task 6: `internal/cli` -- wire `watchSkillDir` into `buildGateway`

**Files:**
- Modify: `internal/cli/server.go`
- Test: `internal/cli/server_internal_test.go`

**Interfaces:**
- Consumes: `watchSkillDir` (Task 5), `buildStaticPrompts`'s `dirScans` return (Task 4).

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/server_internal_test.go`:

```go
func TestBuildGateway_SkillDirPicksUpRuntimeFileChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "greet.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Listen:  config.ListenConfig{Stdio: true},
		Prompts: []config.StaticPromptConfig{{SkillDir: dir}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := buildGateway(ctx, logger, cfg)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}

	gw := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv.MCP() }, nil))
	defer gw.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gw.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet"}); err != nil {
		t.Fatalf("GetPrompt(greet): %v, want it registered from the initial scan", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "farewell.md"), []byte("bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForPromptPresence(t, ctx, session, "farewell", true)
}
```

Check `internal/cli/server_internal_test.go`'s existing imports for `"os"`, `"path/filepath"`, `"net/http"`, `"net/http/httptest"`, `"github.com/modelcontextprotocol/go-sdk/mcp"` -- add whichever aren't already imported (several of `buildGateway`'s other existing tests in this file already exercise HTTP round trips, so most are likely already present).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/... -run TestBuildGateway_SkillDirPicksUpRuntimeFileChanges -v`
Expected: FAIL -- `GetPrompt(farewell)` never succeeds within `waitForPromptPresence`'s deadline, because `buildGateway` doesn't spawn `watchSkillDir` yet.

- [ ] **Step 3: Wire `watchSkillDir` into `buildGateway`**

In `internal/cli/server.go`, change `buildGateway`'s `buildStaticPrompts` call (from Task 4's placeholder) back to capturing `dirScans`, and spawn a watcher per `skill_dir` entry right after `gwH.ptr.Store(srv)`:

```go
	staticPrompts, dirScans, err := buildStaticPrompts(cfg.Prompts)
	if err != nil {
		return nil, err
	}

	srv := gateway.New(gateway.NewConfig{
		Logger:   logger,
		Backends: conn.backends,
		Tables: gateway.Tables{
			Tools:             toolTable,
			Resources:         resourceTable,
			ResourceTemplates: resourceTemplateTable,
			Prompts:           promptTable,
		},
		Entries: gateway.Entries{
			Tools:             conn.toolEntries,
			Resources:         conn.resourceEntries,
			ResourceTemplates: conn.resourceTemplateEntries,
			Prompts:           conn.promptEntries,
		},
		Overrides: gateway.Overrides{
			Tools:             cfg.Overrides,
			Resources:         cfg.ResourceOverrides,
			ResourceTemplates: cfg.ResourceTemplateOverrides,
			Prompts:           cfg.PromptOverrides,
		},
		StaticPrompts:             staticPrompts,
		MaskKeys:                  cfg.Logging.MaskKeys,
		Relays:                    gwH.relays,
		KeepAlive:                 time.Duration(cfg.Timeouts.DownstreamKeepAlive),
		KeepAliveFailureThreshold: cfg.Timeouts.DownstreamKeepAliveFailureThreshold,
	})
	gwH.ptr.Store(srv)

	// Each skill_dir entry gets its own live-update watcher, scoped to this
	// generation's ctx: a SIGHUP-triggered reload's new generation builds
	// its own fresh watcher (seeded from ITS OWN initial scan, above), and
	// this one stops when ctx is cancelled on this generation's supersession
	// (see watchSIGHUP/scheduleDrain) -- the same lifecycle backend
	// supervisors already follow.
	for i, p := range cfg.Prompts {
		if p.SkillDir == "" {
			continue
		}
		go watchSkillDir(ctx, logger, p.SkillDir, i, dirScans[i], srv)
	}

	return srv, nil
}
```

(This replaces both the old `staticPrompts, _, err := buildStaticPrompts(cfg.Prompts)` line from Task 4 and the old bare `return srv, nil` that followed `gwH.ptr.Store(srv)`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/cli/... -v`
Expected: PASS.

- [ ] **Step 5: Run the full test suite**

Run: `go build ./... && gofmt -l . && go vet ./... && go test ./...`
Expected: PASS, `gofmt -l .` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/server.go internal/cli/server_internal_test.go
git commit -m "$(cat <<'EOF'
feat(cli): watch every skill_dir entry for live prompt updates

buildGateway now spawns watchSkillDir per skill_dir entry, scoped to that
generation's context -- adding, editing, or removing a file under a
watched directory is reflected in prompts/list (and notified via
notifications/prompts/list_changed) without a SIGHUP or restart.
EOF
)"
```

---

### Task 7: schema regeneration and README documentation

**Files:**
- Regenerate: `config.schema.json`
- Modify: `README.md`

**Interfaces:** none (docs/schema only).

- [ ] **Step 1: Regenerate `config.schema.json`**

Run: `task schema` (or, equivalently, `go run ./cmd/mcprt schema --force config.schema.json`)
Expected: `config.schema.json` gains a `skill_dir` property under the `prompts` array items, and its `$defs`/property ordering otherwise stays alphabetical (matching `invopop/jsonschema`'s existing output style) -- diff it to confirm `skill_dir` is the only substantive addition.

- [ ] **Step 2: Verify the staleness test passes**

Run: `go test ./internal/cli/... -run TestSchemaCommand_MatchesCommittedFile -v`
Expected: PASS.

- [ ] **Step 3: Document `skill_dir` in README.md**

In `README.md`, right after the existing `skill_file` paragraph (the one starting "Writing a long `text` inline in YAML gets unwieldy fast..."), add:

```markdown
`skill_file` still means one entry per file. To serve many prompts from a
directory of Markdown files instead, point `skill_dir` at it -- every direct
child `*.md` file (subdirectories are not scanned) becomes its own prompt,
parsed the same `---`-delimited front matter way `skill_file` is, falling
back to the filename (minus `.md`) for `name` when front matter omits one.
Unlike every other `prompts` field, `skill_dir` must be the ONLY field set
on its entry -- `name`/`description`/`text`/`skill_file`/`arguments` are all
rejected alongside it, since one entry now expands into many prompts rather
than describing a single one; give each file its own front matter `name`/
`description` instead. `arguments` is not available for a `skill_dir`
prompt at all (SKILL.md's front matter format has no field for it) -- use
`skill_file` if a prompt needs `required: true` arguments.

Unlike `skill_file`, `skill_dir`'s contents are watched live while `mcprt
server` is running: adding, editing, or removing a `*.md` file under it
updates `prompts/list` (and sends `notifications/prompts/list_changed` to
every connected client) within about 300ms, without a SIGHUP or restart --
and, unlike SIGHUP-triggered config reload, this applies equally to stdio
and HTTP sessions. A name any other `prompts` entry already serves (a fixed
entry, or another `skill_dir`) is never taken over; the earlier entry
(higher up in `prompts:`) keeps winning, and the conflicting file is logged
and ignored until the name-holder changes. A file that fails to parse (bad
front matter, or a body that isn't a valid template) is logged and skipped
without disturbing the directory's other prompts -- but this leniency is
runtime-only: a `skill_dir` with a bad file already in place at `mcprt
validate`/startup/SIGHUP-reload time fails to load, exactly as strict as
every other prompts: misconfiguration.
```

Add `skill_dir` to the example `prompts:` block earlier in the file (right after the existing `skill_file` entry, around line 89-92):

```yaml
    prompts:
      - name: release-notes
        description: "Summarize a diff into release notes"
        arguments:
          - name: diff
            description: "The diff text to summarize"
            required: true
        text: |
          Summarize the following diff as release notes:

          {{.diff}}
      - skill_file: ~/.claude/skills/code-review/SKILL.md
        arguments:
          - name: diff
            required: true
      - skill_dir: ~/.claude/skills/team-prompts
```

- [ ] **Step 4: Run the full test suite one more time**

Run: `go build ./... && gofmt -l . && go vet ./... && go test ./...`
Expected: PASS, `gofmt -l .` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add config.schema.json README.md
git commit -m "$(cat <<'EOF'
docs: document skill_dir and regenerate config.schema.json

EOF
)"
```

---

## After All Tasks: open a pull request

Once every task above is committed on a feature branch:

1. Push the branch: `git push -u origin <branch-name>`
2. Open a PR with `gh pr create`, summarizing: what `skill_dir` adds (auto-discovering prompts from a directory, live-reloaded via fsnotify + `notifications/prompts/list_changed`, no restart/SIGHUP needed), linking both spec (`docs/superpowers/specs/2026-09-10-dynamic-skill-dir-design.md`) and this plan (`docs/superpowers/plans/2026-09-10-dynamic-skill-dir.md`), and a test-plan checklist covering: `go test ./...` green, `mcprt validate` rejects a bad `skill_dir` config, manual smoke test (`mcprt server --config ...` with a `skill_dir` entry, then adding/editing/removing a file under it and observing `prompts/list` change without restarting).
