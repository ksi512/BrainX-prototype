// Package scope 实现「读猜、写不猜」里的「猜」。
//
// 判定规则（提示词 §4.3）：
//   - 从 cwd 向上逐级找，**遇到 .git 即停**——这样在子目录里也能命中仓库根目录记的记忆；
//   - 标识优先取 git remote origin 的 URL 哈希，无 remote 时取仓库根目录名；
//   - **禁止用 cwd 当标识**：子目录里查不到根目录记的东西，是静默失忆；
//   - **禁止把绝对路径写进标识**：换机器/换用户名后标识会变，等于换了一个库。
//
// 为什么直接读 .git/config 而不调 git 命令：hook 的预算是 100ms，
// 起一个子进程要 15–30ms，且 GUI 宿主启动的进程 PATH 未必含 git。
// 读文件是纯 Go、无依赖、微秒级。
package scope

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// Global 是所有宿主共享的兜底作用域。
const Global = "global"

// Resolution 是判定结果。Reason 走 stderr，不进任何注入内容——
// 判错时要能一眼看出依据，但不能污染宿主的上下文。
type Resolution struct {
	Scope  string
	Reason string
	// ProjectRoot 是命中的仓库根目录（绝对路径），仅用于提示，不入库。
	ProjectRoot string
}

// Detect 判定 cwd 所属的 scope。cwd 为空、或向上找不到 .git 时回落 global。
func Detect(cwd string) Resolution {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		if current, err := os.Getwd(); err == nil {
			cwd = current
		}
	}
	if cwd == "" {
		return Resolution{Scope: Global, Reason: "无法确定当前目录，回落 global"}
	}
	root, ok := findRepoRoot(cwd)
	if !ok {
		return Resolution{Scope: Global, Reason: "向上未找到 .git，回落 global"}
	}
	if remote := originURL(root); remote != "" {
		return Resolution{
			Scope:       "proj:" + shortHash(remote),
			Reason:      "git remote origin = " + remote,
			ProjectRoot: root,
		}
	}
	name := filepath.Base(root)
	if name == "" || name == "/" || name == "." {
		name = "repo"
	}
	return Resolution{
		Scope:       "proj:" + name,
		Reason:      "无 remote，取仓库目录名 " + name,
		ProjectRoot: root,
	}
}

// findRepoRoot 从 dir 向上找第一个含 .git 的目录。.git 可以是目录（普通仓库），
// 也可以是文件（worktree / submodule，内容为 `gitdir: …`）。
func findRepoRoot(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// originURL 读 <root>/.git/config（worktree 走 gitdir 重定向），
// 返回 [remote "origin"] 段的 url。解析失败一律返回空，不报错——
// scope 判定失败不该阻断任何命令，回落目录名即可。
func originURL(root string) string {
	configPath := filepath.Join(root, ".git", "config")
	if info, err := os.Lstat(filepath.Join(root, ".git")); err == nil && !info.IsDir() {
		// worktree / submodule：.git 是指向真实 gitdir 的文件
		data, err := os.ReadFile(filepath.Join(root, ".git"))
		if err != nil {
			return ""
		}
		line := strings.TrimSpace(string(data))
		if !strings.HasPrefix(line, "gitdir:") {
			return ""
		}
		gitdir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(root, gitdir)
		}
		// worktree 的 config 在公共 gitdir 里，common 段优先
		if common := strings.TrimSpace(readValue(filepath.Join(gitdir, "commondir"), "")); common != "" {
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitdir, common)
			}
			if url := originURLIn(filepath.Join(common, "config")); url != "" {
				return url
			}
		}
		configPath = filepath.Join(gitdir, "config")
	}
	return originURLIn(configPath)
}

func readValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(strings.TrimPrefix(line, key))
		}
	}
	return ""
}

func originURLIn(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section := strings.ToLower(strings.Trim(line, "[]"))
			inOrigin = section == `remote "origin"`
			continue
		}
		if !inOrigin {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "url") {
			return normalizeRemote(value)
		}
	}
	return ""
}

// normalizeRemote 去掉尾部的 .git 与两端空白。
// 同一仓库的 https / ssh 写法不同（github.com:a/b 与 github.com/a/b），
// 这里只做最小归一：不猜测协议等价，避免把两个不同远端算成同一个。
func normalizeRemote(value string) string {
	url := strings.TrimSpace(value)
	url = strings.TrimSuffix(url, "/")
	url = strings.TrimSuffix(url, ".git")
	return url
}

// shortHash 取 sha256 前 12 位十六进制。用哈希而不是原文：
// 远端 URL 里可能带用户名或内网域名，不该出现在库里的每一行 scope 上。
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// IsRepo 只回答「这个目录在不在一个 git 仓库里」，不做标识计算。
// install 用它决定约定文件的作用域：在项目里→项目级，否则→全局。
func IsRepo(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		if current, err := os.Getwd(); err == nil {
			dir = current
		}
	}
	if dir == "" {
		return false
	}
	_, ok := findRepoRoot(dir)
	return ok
}
