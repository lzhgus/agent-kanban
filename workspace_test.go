package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo makes a repository on main with one commit: app/main.go.
func testRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	os.MkdirAll(filepath.Join(repo, "app"), 0o755)
	os.WriteFile(filepath.Join(repo, "app", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return repo
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return out
}

func TestWorkspace(t *testing.T) {
	repo, root := testRepo(t), t.TempDir()

	// A folder outside git runs in place.
	if ws, note, err := openWorkspace(t.TempDir(), root, 1, "x"); ws != nil || err != nil || !strings.Contains(note, "not a git repository") {
		t.Fatalf("outside git: %v %q %v", ws, note, err)
	}

	// A task in a subfolder works in the same subfolder of its worktree.
	os.WriteFile(filepath.Join(repo, "app", "main.go"), []byte("package main // dirty\n"), 0o644)
	ws, note, err := openWorkspace(filepath.Join(repo, "app"), root, 7, "Add a README!")
	if err != nil || ws == nil {
		t.Fatal(err)
	}
	if ws.Branch != "kanban/7-add-a-readme" || ws.Base != "main" || ws.Sub != "app" || !strings.Contains(note, "uncommitted changes") {
		t.Errorf("workspace %+v, note %q", ws, note)
	}
	if got, _ := os.ReadFile(filepath.Join(ws.Dir(), "main.go")); string(got) != "package main\n\nfunc main() {}\n" {
		t.Errorf("the worktree has the commit, not your edits: %q", got)
	}
	gitT(t, repo, "checkout", "--", ".") // your checkout clean again

	// A turn's work is committed as the agent; the diff is against the fork.
	os.WriteFile(filepath.Join(ws.Dir(), "README.md"), []byte("# App\nhello\n"), 0o644)
	os.WriteFile(filepath.Join(ws.Dir(), "main.go"), []byte("package main\n\nfunc main() { println() }\n"), 0o644)
	ch, err := ws.checkpoint("claude", "claude: add a README")
	if err != nil || ch != (Changes{Files: 2, Added: 3, Deleted: 1}) {
		t.Fatalf("checkpoint %+v, %v", ch, err)
	}
	if author := gitT(t, ws.Worktree, "log", "-1", "--format=%an|%s"); author != "claude (Agent Kanban)|claude: add a README" {
		t.Errorf("commit %q", author)
	}
	if ch2, _ := ws.checkpoint("claude", "nothing"); ch2 != ch || gitT(t, ws.Worktree, "rev-list", "--count", "HEAD") != "2" {
		t.Error("a turn with no changes made a commit")
	}
	files, _ := ws.files()
	if len(files) != 2 || files[0].Path != "app/README.md" || files[0].Added != 2 {
		t.Errorf("files %+v", files)
	}
	before, after, err := ws.texts("app/README.md")
	if err != nil || before != "" || after != "# App\nhello\n" {
		t.Errorf("texts %q %q %v", before, after, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "app", "README.md")); err == nil {
		t.Error("the agent's work reached your checkout before a merge")
	}

	// A deleted worktree comes back from its branch.
	os.RemoveAll(ws.Worktree)
	if err := ws.ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir(), "README.md")); err != nil {
		t.Error("the restored worktree lost the work")
	}

	// A start over forks a new branch; the old one stays.
	ws2, _, err := openWorkspace(repo, root, 7, "Add a README!")
	if err != nil || ws2.Branch != "kanban/7-add-a-readme-2" || ws2.Worktree == ws.Worktree {
		t.Fatalf("start over %+v, %v", ws2, err)
	}
	ws2.remove()
	if _, err := os.Stat(ws2.Worktree); err == nil || !branchExists(repo, ws2.Branch) {
		t.Error("remove left the worktree, or took the branch")
	}

	// Merging refuses a checkout elsewhere or with changes, then merges.
	gitT(t, repo, "checkout", "-q", "-b", "other")
	if err := ws.merge("Add a README"); err == nil || !strings.Contains(err.Error(), "switch it to main") {
		t.Errorf("merge on another branch: %v", err)
	}
	gitT(t, repo, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(repo, "app", "main.go"), []byte("dirty"), 0o644)
	if err := ws.merge("Add a README"); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("merge into a dirty checkout: %v", err)
	}
	gitT(t, repo, "checkout", "--", ".")
	if err := ws.merge("Add a README"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "app", "README.md")); string(got) != "# App\nhello\n" {
		t.Errorf("after the merge: %q", got)
	}
}

func TestMergeConflict(t *testing.T) {
	repo := testRepo(t)
	ws, _, err := openWorkspace(repo, t.TempDir(), 1, "edit")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws.Worktree, "app", "main.go"), []byte("package main // agent\n"), 0o644)
	ws.checkpoint("claude", "agent edit")
	os.WriteFile(filepath.Join(repo, "app", "main.go"), []byte("package main // you\n"), 0o644)
	gitT(t, repo, "commit", "-q", "-am", "your edit")
	err = ws.merge("edit")
	if err == nil || !strings.Contains(err.Error(), "conflicts with main") {
		t.Fatalf("merge: %v", err)
	}
	if status := gitT(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("a failed merge left %q", status)
	}
}

// The board runs a task in a worktree, commits its turn, shows the change,
// merges it, and starts over on a new branch.
func TestBoardWorktree(t *testing.T) {
	saved := drivers
	drivers = map[string]driver{"claude": func(ctx context.Context, task Task, log emit) (result, error) {
		os.WriteFile(filepath.Join(task.Dir, "NOTES.md"), []byte("turn "+task.Prompt+"\n"), 0o644)
		return result{Text: "wrote notes", SessionRef: task.SessionRef}, nil
	}}
	defer func() { drivers = saved }()
	repo := testRepo(t)
	queue := make(chan func(), 32)
	b := newBoard("")
	b.worktrees = t.TempDir()
	b.update = func(f func()) { queue <- f }
	b.dir, b.prompt, b.title = repo, "one", "Notes"
	b.add(true)
	task := b.Tasks[0]
	for task.Status == Running {
		(<-queue)()
	}
	if task.WS == nil || task.Status != Review || task.Changes != (Changes{Files: 1, Added: 1}) {
		t.Fatalf("after the turn: %+v %+v", task, task.WS)
	}
	if _, err := os.Stat(filepath.Join(repo, "NOTES.md")); err == nil {
		t.Fatal("the agent wrote in your checkout")
	}
	if !strings.Contains(strings.Join(task.Log, "\n"), "── branch kanban/1-notes") {
		t.Errorf("log %q", task.Log)
	}

	// A reply works on the same branch.
	b.reply(task, "two")
	for task.Status == Running {
		(<-queue)()
	}
	if gitT(t, task.WS.Worktree, "rev-list", "--count", "HEAD") != "3" || task.WS.Branch != "kanban/1-notes" {
		t.Errorf("the reply's commit: %+v", task.WS)
	}

	b.merge(task)
	for task.merging {
		(<-queue)()
	}
	if !task.Merged || task.Status != Done || task.Err != "" {
		t.Fatalf("after merging: %+v", task)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "NOTES.md")); string(got) != "turn two\n" {
		t.Errorf("merged %q", got)
	}

	old := task.WS.Worktree
	b.start(task) // start over
	for task.Status == Running {
		(<-queue)()
	}
	if task.WS.Branch != "kanban/1-notes-2" || task.Merged || !branchExists(repo, "kanban/1-notes") {
		t.Errorf("start over: %+v", task.WS)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("start over left the old worktree")
	}
}
