package scope

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeRepo(t *testing.T, remote string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "proj-root")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[core]\n\tbare = false\n"
	if remote != "" {
		config += "[remote \"origin\"]\n\turl = " + remote + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	writeFile(t, filepath.Join(root, ".git", "config"), config)
	return root
}

// 核心断言：子目录与仓库根目录必须判定成同一个 scope。
// 这是「静默失忆」的守门测试——用 cwd 当标识时它会立刻失败。
func TestSubdirectorySharesRootScope(t *testing.T) {
	root := makeRepo(t, "git@github.com:L/brainx.git")
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	atRoot := Detect(root)
	atDeep := Detect(deep)
	if atRoot.Scope != atDeep.Scope {
		t.Fatalf("子目录与根目录 scope 不一致：root=%s deep=%s", atRoot.Scope, atDeep.Scope)
	}
	if atRoot.Scope == Global {
		t.Fatalf("仓库内不应回落到 global")
	}
	if len(atRoot.Scope) != len("proj:")+12 {
		t.Fatalf("remote 型 scope 应为 proj:<12 位 hash>，得到 %s", atRoot.Scope)
	}
	if atRoot.ProjectRoot != root {
		t.Fatalf("ProjectRoot 应为仓库根 %s，得到 %s", root, atRoot.ProjectRoot)
	}
}

// 同一远端、不同克隆路径必须得到同一 scope（换机器/换目录不影响标识）。
func TestRemoteIdentityIsPathIndependent(t *testing.T) {
	a := makeRepo(t, "https://github.com/L/brainx")
	b := makeRepo(t, "https://github.com/L/brainx.git")
	if Detect(a).Scope != Detect(b).Scope {
		t.Fatalf("同一远端不同路径应同 scope：%s vs %s", Detect(a).Scope, Detect(b).Scope)
	}
}

// 不同远端必须是不同 scope，否则两个仓库的记忆会互相污染。
func TestDifferentRemoteDifferentScope(t *testing.T) {
	a := makeRepo(t, "https://github.com/L/dsh")
	b := makeRepo(t, "https://github.com/L/brainx")
	if Detect(a).Scope == Detect(b).Scope {
		t.Fatal("不同远端不应同 scope")
	}
}

// 无 remote 时回落仓库目录名，而不是绝对路径。
func TestNoRemoteFallsBackToDirName(t *testing.T) {
	root := makeRepo(t, "")
	res := Detect(root)
	if res.Scope != "proj:proj-root" {
		t.Fatalf("无 remote 应取目录名，得到 %s", res.Scope)
	}
	if filepath.IsAbs(res.Scope) {
		t.Fatal("scope 里不得出现绝对路径")
	}
}

// 仓库之外回落 global，并且不报错（读路径不能因判定失败而中断）。
func TestOutsideRepoFallsBackToGlobal(t *testing.T) {
	dir := t.TempDir()
	res := Detect(dir)
	if res.Scope != Global {
		t.Fatalf("仓库外应为 global，得到 %s", res.Scope)
	}
	if res.Reason == "" {
		t.Fatal("回落到 global 时必须给出依据")
	}
}

// worktree / submodule 的 .git 是文件，必须能跟到真实 gitdir。
func TestGitFileWorktree(t *testing.T) {
	base := t.TempDir()
	common := filepath.Join(base, "main", ".git")
	writeFile(t, filepath.Join(common, "config"), "[remote \"origin\"]\n\turl = https://example.com/x/wt.git\n")

	worktree := filepath.Join(base, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(common, "worktrees", "wt")+"\n")
	gitdir := filepath.Join(common, "worktrees", "wt")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(gitdir, "commondir"), "../..\n")

	res := Detect(worktree)
	if res.Scope != "proj:"+shortHash("https://example.com/x/wt") {
		t.Fatalf("worktree 应解析到公共 gitdir 的 remote，得到 %s（%s）", res.Scope, res.Reason)
	}
}
