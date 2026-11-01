package main

// Each task in a git repository works in a worktree of its own, on a branch
// of its own, so agents never touch your checkout or each other's files.
// Every turn ends in a commit; the task's diff is its branch against the
// commit it forked from; merging into the base branch is a choice you make.
// A folder outside git runs in place, as before, with no diff.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Workspace is where a task's agent works.
type Workspace struct {
	Repo     string // the top level of your repository
	Worktree string // the task's checkout
	Sub      string // the task's folder relative to Repo ("" for the top)
	Branch   string // kanban/<id>-<slug>
	Base     string // the branch it forked from; "" if HEAD was detached
	BaseSHA  string // the commit it forked from: the diff's left side
}

// Dir is where the agent runs: the task's folder inside the worktree.
func (w *Workspace) Dir() string { return filepath.Join(w.Worktree, w.Sub) }

// Changes counts what a branch changed against its base.
type Changes struct{ Files, Added, Deleted int }

type fileChange struct {
	Path           string
	Added, Deleted int // -1 for binary files
}

// repoMu serializes the operations that edit a repository's worktree list
// and its checked-out branch.
// ponytail: one lock for every repository; per-repository locks if many
// repos run at once.
var repoMu sync.Mutex

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, fmt.Errorf("git %s: %s", args[0], firstLine(s))
	}
	return s, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	return s
}

// openWorkspace makes a task's worktree: a new branch from your HEAD, under
// root. A folder outside git, or a repository without commits, gives nil and
// a note: the task runs in place.
func openWorkspace(dir, root string, id int, title string) (*Workspace, string, error) {
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, "not a git repository: the agent works in the folder itself, with no diff", nil
	}
	sha, err := git(top, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, "the repository has no commits yet: the agent works in the folder itself, with no diff", nil
	}
	ws := &Workspace{Repo: top, BaseSHA: sha}
	if b, _ := git(top, "rev-parse", "--abbrev-ref", "HEAD"); b != "HEAD" {
		ws.Base = b
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil { // /var and /private/var are one place
		if rel, err := filepath.Rel(top, real); err == nil && rel != "." {
			ws.Sub = rel
		}
	}
	name := fmt.Sprintf("kanban/%d", id)
	if s := slug(title); s != "" {
		name += "-" + s
	}
	repoMu.Lock()
	defer repoMu.Unlock()
	ws.Branch = name
	for n := 2; branchExists(top, ws.Branch); n++ { // a start over: the old branch stays
		ws.Branch = name + "-" + strconv.Itoa(n)
	}
	ws.Worktree = filepath.Join(root, filepath.Base(top)+"-"+strings.TrimPrefix(strings.ReplaceAll(ws.Branch, "/", "-"), "kanban-"))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, "", err
	}
	if _, err := git(top, "worktree", "add", "-b", ws.Branch, ws.Worktree, sha); err != nil {
		return nil, "", err
	}
	note := ""
	if dirty, _ := git(top, "status", "--porcelain"); dirty != "" {
		note = "uncommitted changes in " + top + " are not in the task's worktree"
	}
	return ws, note, nil
}

func branchExists(repo, branch string) bool {
	_, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// ensure brings back a worktree deleted from disk, from its branch.
func (w *Workspace) ensure() error {
	if _, err := os.Stat(filepath.Join(w.Worktree, ".git")); err == nil {
		return nil
	}
	repoMu.Lock()
	defer repoMu.Unlock()
	git(w.Repo, "worktree", "prune")
	_, err := git(w.Repo, "worktree", "add", w.Worktree, w.Branch)
	return err
}

// remove deletes the worktree; its branch, and the work on it, stay.
func (w *Workspace) remove() error {
	repoMu.Lock()
	defer repoMu.Unlock()
	if _, err := git(w.Repo, "worktree", "remove", "--force", w.Worktree); err != nil {
		os.RemoveAll(w.Worktree)
		git(w.Repo, "worktree", "prune")
	}
	return nil
}

// checkpoint commits everything the turn left, as the agent, and counts
// the branch's changes. A turn that changed nothing makes no commit.
func (w *Workspace) checkpoint(agent, message string) (Changes, error) {
	if _, err := git(w.Worktree, "add", "-A"); err != nil {
		return Changes{}, err
	}
	if _, err := git(w.Worktree, "diff", "--cached", "--quiet"); err != nil { // staged changes
		// The agent's commit: its own name, unsigned, past hooks (no one is there to answer them).
		_, err := git(w.Worktree, "-c", "user.name="+agent+" (Agent Kanban)", "-c", "user.email=agent-kanban@localhost",
			"-c", "commit.gpgsign=false", "commit", "--no-verify", "--quiet", "-m", message)
		if err != nil {
			return Changes{}, err
		}
	}
	return w.changes()
}

// discard puts the worktree back as it was at head, before a read-only
// turn, commits the agent made included, and reports how many files it had
// changed.
func (w *Workspace) discard(head string) (int, error) {
	n := 0
	if out, _ := git(w.Worktree, "status", "--porcelain"); out != "" {
		n = len(strings.Split(out, "\n"))
	}
	if cur, _ := git(w.Worktree, "rev-parse", "HEAD"); n == 0 && cur == head {
		return 0, nil
	}
	if _, err := git(w.Worktree, "reset", "--hard", "--quiet", head); err != nil {
		return n, err
	}
	_, err := git(w.Worktree, "clean", "-fdq")
	return max(n, 1), err
}

func (w *Workspace) files() ([]fileChange, error) {
	out, err := git(w.Worktree, "diff", "--numstat", "--no-renames", w.BaseSHA, "HEAD")
	if err != nil || out == "" {
		return nil, err
	}
	var files []fileChange
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		f := fileChange{Path: parts[2], Added: -1, Deleted: -1}
		if parts[0] != "-" {
			f.Added, _ = strconv.Atoi(parts[0])
			f.Deleted, _ = strconv.Atoi(parts[1])
		}
		files = append(files, f)
	}
	return files, nil
}

func (w *Workspace) changes() (Changes, error) {
	files, err := w.files()
	var c Changes
	for _, f := range files {
		c.Files++
		c.Added += max(f.Added, 0)
		c.Deleted += max(f.Deleted, 0)
	}
	return c, err
}

const diffFileMax = 1 << 20 // a side larger than this shows a note instead

// texts returns a file's text at the base and at the branch's head; ""
// where it does not exist.
func (w *Workspace) texts(path string) (before, after string, err error) {
	show := func(rev string) (string, error) {
		out, err := exec.Command("git", "-C", w.Worktree, "show", rev+":"+path).Output()
		if err != nil {
			return "", nil // not there on that side
		}
		if len(out) > diffFileMax {
			return "", errors.New("the file is too large to show")
		}
		return string(out), nil
	}
	if before, err = show(w.BaseSHA); err != nil {
		return
	}
	after, err = show("HEAD")
	return
}

// merge merges the branch into its base in your checkout, which must be on
// the base branch with no uncommitted changes: the board does not move your
// checkout or touch your work. A conflict leaves everything as it was.
func (w *Workspace) merge(title string) error {
	if w.Base == "" {
		return errors.New("the task forked from a detached HEAD: there is no branch to merge into")
	}
	repoMu.Lock()
	defer repoMu.Unlock()
	if cur, _ := git(w.Repo, "rev-parse", "--abbrev-ref", "HEAD"); cur != w.Base {
		return fmt.Errorf("%s is on %s: switch it to %s to merge", w.Repo, cur, w.Base)
	}
	if dirty, _ := git(w.Repo, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
		return fmt.Errorf("%s has uncommitted changes: commit or stash them to merge", w.Repo)
	}
	out, err := git(w.Repo, "merge", "--no-ff", "--no-verify", "-m", "Merge "+w.Branch+": "+title, w.Branch)
	if err == nil {
		return nil
	}
	git(w.Repo, "merge", "--abort")
	if strings.Contains(out, "CONFLICT") {
		return fmt.Errorf("%s conflicts with %s: ask the agent to merge %s into its branch and resolve them", w.Branch, w.Base, w.Base)
	}
	return err
}
