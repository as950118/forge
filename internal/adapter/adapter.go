// Package adapter holds the client-independent part of `forge sync`: the
// Adapter interface, the plan/result model and the apply engine (backup,
// verification, atomic writes, rollback, locking, restore). Client adapters
// (internal/adapter/claudecode, internal/adapter/opencode) only decide WHAT to change by
// producing a Plan; every write goes through Apply, so the safety rules hold
// for all of them:
//
//   - A Plan is pure data. Building it reads files but writes nothing.
//   - Apply re-verifies every file against the state the plan was built from,
//     backs up everything it is about to change, writes atomically, and rolls
//     back from the backup when a step fails.
//   - User-owned content is never part of a Plan: adapters mark such items as
//     conflicts and leave their files out of the operations.
//   - Secret/user-level client files (the Claude Code user file, OpenCode secrets) are unreachable (see guard.go).
package adapter

import (
	"context"
	"io/fs"
	"time"

	"github.com/as950118/forge/internal/mcp"
	"github.com/as950118/forge/internal/profile"
)

// Scope selects where a sync writes.
type Scope string

// Scopes.
const (
	ScopeProject Scope = "project"
	ScopeUser    Scope = "user"
)

// Kinds of plan items.
const (
	KindSkill   = "skill"
	KindMCP     = "mcp"
	KindHook    = "hook"
	KindCommand = "command" // a command printed for the user to run; nothing is written
)

// Action is what the plan intends for one item.
type Action string

// Actions. "unchanged" items are counted but not listed in the table.
const (
	ActionAdd       Action = "add"
	ActionUpdate    Action = "update"
	ActionRemove    Action = "remove"
	ActionConflict  Action = "conflict"
	ActionSkip      Action = "skip"
	ActionUnchanged Action = "unchanged"
	ActionInfo      Action = "info"
)

// Item is one entry of the plan table.
type Item struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action Action `json:"action"`
	Reason string `json:"reason,omitempty"`
	Path   string `json:"path,omitempty"`
	// Overridable marks a conflict that --force-managed may overwrite
	// (a Forge-managed item that was edited by hand). User-owned content is
	// never overridable.
	Overridable bool `json:"-"`
	// Tampered marks a skill conflict whose registry snapshot no longer matches
	// its registered content hash (decided by the hash comparison, not by
	// parsing Reason). It is neither a hand-edited managed item nor a name
	// clash, and --force-managed does not help.
	Tampered bool `json:"-"`
}

// OpKind is a low-level file operation.
type OpKind string

// Operation kinds.
const (
	OpWriteFile   OpKind = "write-file"
	OpDeleteFile  OpKind = "delete-file"
	OpReplaceTree OpKind = "replace-tree"
	OpDeleteTree  OpKind = "delete-tree"
)

// TreeSpec describes a directory to materialize from a source directory.
type TreeSpec struct {
	SrcDir string
	// Extra files written next to the copied ones (e.g. the management marker).
	Extra map[string][]byte
	// Modes overrides file modes by slash-separated relative path (restore).
	// When nil, files get 0644 (0755 if the source is executable) and
	// directories 0755: read-only snapshot modes are not inherited.
	Modes map[string]uint32
}

// Op is one file-level change. Before is the state observed when the plan was
// built; Apply refuses to run when the file changed since.
type Op struct {
	Kind OpKind
	Path string
	Data []byte // OpWriteFile
	// BeforeData is the previous content of a file (nil when it did not exist);
	// it only feeds the diff.
	BeforeData []byte
	Mode       fs.FileMode
	Tree       *TreeSpec
	Before     FileState
}

// FileState is a comparable snapshot of a path.
type FileState struct {
	Exists bool   `json:"exists"`
	Kind   string `json:"kind,omitempty"` // "file" | "dir"
	Hash   string `json:"hash,omitempty"` // sha256 of a file, tree hash of a directory
	Mode   uint32 `json:"mode,omitempty"`
	// Files lists the regular files of a directory snapshot (restore needs the modes).
	Files []TreeFile `json:"files,omitempty"`
}

// Equal compares existence, kind and content hash (mode is informational).
func (s FileState) Equal(o FileState) bool {
	return s.Exists == o.Exists && s.Kind == o.Kind && s.Hash == o.Hash
}

// TreeFile is one file of a directory snapshot.
type TreeFile struct {
	Rel    string `json:"rel"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// FileChange summarizes an Op for output.
type FileChange struct {
	Path       string `json:"path"`
	Change     string `json:"change"` // create|modify|delete|replace
	BeforeHash string `json:"before_hash,omitempty"`
	AfterHash  string `json:"after_hash,omitempty"`
}

// Plan is the full result of planning one sync.
type Plan struct {
	Target string `json:"target"`
	Scope  Scope  `json:"scope"`
	// Root is the project root (project scope) or the home directory (user scope).
	Root string `json:"root"`
	// LockKey identifies what is serialized (one lock per target+root).
	LockKey string `json:"-"`
	Items   []Item `json:"items"`
	Ops     []Op   `json:"-"`
	// Commands are shell commands printed for the user (nothing is executed).
	Commands []string `json:"commands,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	// Diffs holds the masked unified diff per file (filled by BuildDiffs).
	Diffs []FileDiff `json:"-"`
	// Interrupted lists earlier syncs that were killed before they finished
	// (pending journals of this target and root); the plan treats their items as
	// managed when the content on disk matches the journal.
	Interrupted []InterruptedInfo `json:"interrupted,omitempty"`
	// RecoverIDs are the believable pending journals Apply marks recovered.
	RecoverIDs []string `json:"-"`
	// LostBackups are interrupted syncs whose journal copy next to the state file
	// exists but whose backup directory is gone (see Planner.detectLostBackup).
	LostBackups []string `json:"-"`
	// InvalidJournals are pending journals that were ignored (unreadable or forged).
	InvalidJournals []string `json:"-"`
	// StaleTemps are leftover temporary files of killed syncs; Apply (with the
	// lock held) removes them. TempDirs are the directories searched.
	StaleTemps []string `json:"stale_temp_files,omitempty"`
	TempDirs   []string `json:"-"`
	// NewState/StatePath: the sidecar state this plan ends with (last operation).
	NewState  *State `json:"-"`
	StatePath string `json:"-"`
	// JournalPath is where the journal copy lives next to the state file (checked
	// for symlinks below JournalBase).
	JournalPath, JournalBase string `json:"-"`
	// SkipConflicts is the policy under which the plan was built: conflicting
	// items are left out of Ops either way; Apply refuses unless it is set.
	SkipConflicts bool `json:"skip_conflicts"`
}

// Summary counts items by action.
type Summary map[Action]int

// Summarize counts the plan items.
func (p *Plan) Summarize() Summary {
	s := Summary{}
	for _, it := range p.Items {
		s[it.Action]++
	}
	return s
}

// Conflicts returns the number of conflicting items.
func (p *Plan) Conflicts() int {
	n := 0
	for _, it := range p.Items {
		if it.Action == ActionConflict {
			n++
		}
	}
	return n
}

// HasChanges reports whether Apply would write anything.
func (p *Plan) HasChanges() bool { return len(p.Ops) > 0 }

// Request is the input of Adapter.Plan.
type Request struct {
	Scope Scope
	// ProjectDir is where project-scope resolution starts (repository root search).
	ProjectDir string
	// Home is the user's home directory (HOME): the user-scope root.
	Home string
	// ForgeBin is the forge command written into client configs.
	ForgeBin string
	// NoHooks skips installing/updating hooks and leaves already installed managed
	// hooks alone; RemoveHooks removes the managed hooks. Both together is an error.
	NoHooks     bool
	RemoveHooks bool
	// BackupDir is <forge home>/backups: where interrupted syncs (journals) are looked up.
	BackupDir string
	// SyncInProgress is set by a plan that found the sync lock held: a pending
	// journal then belongs to a live sync and is not reported as interrupted.
	SyncInProgress bool
	// ForceManaged lets Forge overwrite managed items that were edited by hand
	// (still backed up). User-owned items are never touched.
	ForceManaged bool
	// ClaimedSkills are skills another target planned in the same run
	// (`forge sync --target all`: claude-code's) and will manage under
	// .claude/skills, which OpenCode also reads: they are not rendered a second
	// time. It adds to what the claude-code sidecar on disk already claims.
	ClaimedSkills map[string]bool
	Now           func() time.Time
}

// Inputs is a read-only snapshot of the registry for one client.
type Inputs struct {
	// Client is the Forge client id used for exposure lookups ("claude-code").
	Client  string
	Skills  []SkillInput
	Servers []*mcp.Info
	// Profile is the precedence result the inputs were computed with (nil when
	// the inputs were built by hand, e.g. in tests: treated as the legacy rule).
	Profile *profile.Resolved
}

// SkillInput is an enabled skill at its current version.
type SkillInput struct {
	Name        string
	Version     string
	Description string
	// Hash is the registered content hash of the version.
	Hash string
	// StoreDir is the immutable snapshot directory (<home>/skills/<name>@<version>).
	StoreDir string
}

// Adapter renders the registry into one client's configuration.
type Adapter interface {
	// Name is the target id used by --target and stored in backups.
	Name() string
	// Plan reads the current client files and returns what a sync would change.
	// It must not write anything.
	Plan(ctx context.Context, in Inputs, req Request) (*Plan, error)
	// AllowedPath reports whether path is a file or directory this adapter may
	// write in the given scope (root is the project root or the home directory).
	// It returns the base directory below which no path component may be a
	// symlink. Apply and Restore use it to reject anything else, including
	// tampered backup manifests.
	AllowedPath(scope Scope, root, path string) (base string, err error)
}
