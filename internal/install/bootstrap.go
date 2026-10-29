package install

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// SkillDoc 是 WorkBuddy 技能包的正文（单一真源）。
//
// WorkBuddy 没有 hook 机制，也查不到读 AGENTS.md 的证据——写一个它不读的文件比不写更糟，
// 因为用户会以为装好了。技能是它官方文档列明的扩展点，所以这条路才是通的。
//
// 必须短：技能是按需加载的，被 description 匹配到才进上下文。
const SkillDoc = `---
name: brainx
description: 查询本地记忆库中「我以前说过、定过、记过」的事实与决定。当用户问及历史决定、个人偏好、之前的结论、某个资料里的具体数字时使用。
---

# brainx

本地有一个记忆库，命令行是 ` + "`brainx`" + `。它不属于某个项目，整机一份。

## 什么时候用

- 用户问「我之前怎么说的」「上次定的方案是什么」「我那个什么什么在哪」
- 需要引用某个文档里的具体段落、公式、数字
- 要做判断，而这个判断依赖用户过去的选择

**不要用于**：寒暄、常识、模型自己就知道的事。

## 怎么用

` + "```" + `
brainx recall "<关键词>" --scope global
` + "```" + `

- 零命中就换同义词再查一次：` + "`--expand \"同义词,英文写法\"`" + `
- 查到了就把结果当依据用，并标出处 id
- 一次空结果不等于「库里没有」，两次都空才可以说没有

## 写入

用户说「记住这个」「以后都这样」时：

` + "```" + `
brainx remember "<一条一义，含条件与时间>" --kind fact --scope global
` + "```" + `

存完告诉用户存了什么。拿不准用 ` + "`global`" + ` 还是 ` + "`proj`" + ` 就问一句。
`

// defaultPrefix 是自举安装的默认落点。
// macOS/Windows 用 ~/brainx/bin（不污染系统目录），Linux 用 ~/.local/bin（多数发行版已在 PATH）。
func defaultPrefix(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return filepath.Join(home, "brainx", "bin")
	}
	return filepath.Join(home, ".local", "bin")
}

// Bootstrap 把当前二进制复制到 prefix，并按需把 prefix 加进 PATH。
//
// 为什么必须复制而不是建符号链接：符号链接在「源文件被删/被挪」后静默失效；
// 复制出来的那份可以直接 `cp -r` 走，且 hook 里要写的是这个绝对路径。
func Bootstrap(o Options, exe string) (Result, error) {
	prefix := o.Prefix
	if prefix == "" {
		prefix = defaultPrefix(o.Home)
	}
	bin := "brainx"
	if runtime.GOOS == "windows" {
		bin = "brainx.exe"
	}
	target := filepath.Join(prefix, bin)
	result := Result{Host: "self", Path: target}

	current, err := os.Executable()
	if err != nil {
		return result, err
	}
	same := false
	if info, statErr := os.Stat(target); statErr == nil && info.Mode().IsRegular() {
		if sourceInfo, srcErr := os.Stat(current); srcErr == nil && sourceInfo.Size() == info.Size() {
			// 大小相同就当作已是最新：完整哈希比较成本高于收益（几十 MB）
			same = true
		}
	}
	if o.DryRun {
		result.Action = "would-write"
		if same {
			result.Action = "unchanged"
		}
	} else if !same {
		if err := os.MkdirAll(prefix, 0o755); err != nil {
			return result, err
		}
		if err := copyFileMode(current, target, 0o755); err != nil {
			return result, err
		}
		result.Action = "created"
	} else {
		result.Action = "unchanged"
	}

	if o.NoPath {
		result.Note = "已跳过 PATH 配置（--no-path）"
		return result, nil
	}
	changed, pathFile, err := ensurePath(o.Home, prefix, o.DryRun)
	if err != nil {
		return result, err
	}
	if changed {
		result.Note = "已把 " + prefix + " 加进 " + pathFile + "（改前已备份 .bak）"
	} else if pathFile != "" {
		result.Note = pathFile + " 里已有该路径，未改动"
	} else if runtime.GOOS == "windows" {
		result.Note = "Windows 不自动改 PATH；请把 " + prefix + " 加入 PATH，或直接用绝对路径"
	}
	return result, nil
}

// shellPathFile 返回需要追加 PATH 的 shell 配置文件。空字符串表示该平台不走文件。
func shellPathFile(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	switch runtime.GOOS {
	case "darwin":
		// 用 .zshenv 而不是 .zshrc：前者对所有 zsh 生效，含非交互场景
		return filepath.Join(home, ".zshenv")
	case "windows":
		// Windows 走用户级环境变量，绝不能 setx PATH "%PATH%;…"（超 1024 字符会截断系统 PATH）
		return ""
	default:
		if strings.Contains(os.Getenv("SHELL"), "zsh") {
			return filepath.Join(home, ".zshenv")
		}
		return filepath.Join(home, ".profile")
	}
}

// ensurePath 幂等追加 PATH。改之前先备份——用户手写的 shell 配置出事很难查。
func ensurePath(home, prefix string, dryRun bool) (bool, string, error) {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	file := shellPathFile(home)
	if file == "" {
		return false, "", nil
	}
	line := fmt.Sprintf("export PATH=%q:$PATH", prefix)
	existing, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return false, file, err
	}
	if strings.Contains(string(existing), prefix) {
		return false, file, nil
	}
	if dryRun {
		return true, file, nil
	}
	if len(existing) > 0 {
		if err := os.WriteFile(file+".bak", existing, 0o600); err != nil {
			return false, file, err
		}
	}
	merged := strings.TrimRight(string(existing), "\n")
	if merged != "" {
		merged += "\n"
	}
	merged += "# brainx\n" + line + "\n"
	return true, file, os.WriteFile(file, []byte(merged), 0o600)
}

// UninstallSelf 收回 Bootstrap 装出去的东西：二进制、PATH 行、空目录。
// 数据库目录（$BRAINX_HOME）不是安装物，永远不删。
func UninstallSelf(o Options) ([]Result, error) {
	prefix := o.Prefix
	if prefix == "" {
		prefix = defaultPrefix(o.Home)
	}
	bin := "brainx"
	if runtime.GOOS == "windows" {
		bin = "brainx.exe"
	}
	target := filepath.Join(prefix, bin)
	var results []Result

	result := Result{Host: "self", Path: target}
	if _, err := os.Stat(target); os.IsNotExist(err) {
		result.Action = "skipped"
	} else if err != nil {
		return results, err
	} else if o.DryRun {
		result.Action = "would-remove"
	} else if err := os.Remove(target); err != nil {
		return results, err
	} else {
		result.Action = "removed"
	}
	if result.Action != "skipped" {
		results = append(results, result)
	}

	if !o.NoPath {
		removed, file, err := removePath(o.Home, prefix, o.DryRun)
		if err != nil {
			return results, err
		}
		if removed {
			action := "removed"
			if o.DryRun {
				action = "would-remove"
			}
			results = append(results, Result{Host: "self-path", Path: file, Action: action})
		}
	}

	if !o.DryRun {
		// 只删空目录；有内容时 os.Remove 会失败，正好保住用户自己的东西
		_ = os.Remove(prefix)
		parent := filepath.Dir(prefix)
		if parent != filepath.Clean(o.Home) && parent != string(filepath.Separator) {
			_ = os.Remove(parent)
		}
	}
	return results, nil
}

// removePath 摘掉 ensurePath 写的那两行；用户自己的 PATH 行原样保留。
func removePath(home, prefix string, dryRun bool) (bool, string, error) {
	file := shellPathFile(home)
	if file == "" {
		return false, "", nil
	}
	existing, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return false, file, nil
	}
	if err != nil {
		return false, file, err
	}
	pathLine := fmt.Sprintf("export PATH=%q:$PATH", prefix)
	lines := strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	kept := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if line == pathLine {
			removed = true
			if len(kept) > 0 && kept[len(kept)-1] == "# brainx" {
				kept = kept[:len(kept)-1]
			}
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, file, nil
	}
	if dryRun {
		return true, file, nil
	}
	next := strings.Join(kept, "\n")
	if strings.TrimSpace(next) == "" {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return true, file, err
		}
		_ = os.Remove(file + ".bak")
		return true, file, nil
	}
	next = strings.TrimRight(next, "\n") + "\n"
	if err := os.WriteFile(file, []byte(next), 0o600); err != nil {
		return true, file, err
	}
	// 内容回到安装前 = 备份多余，清掉
	if backup, err := os.ReadFile(file + ".bak"); err == nil && string(backup) == next {
		_ = os.Remove(file + ".bak")
	}
	return true, file, nil
}

// WriteSkill 写 WorkBuddy 技能包。全局与项目级各一份，内容相同。
func WriteSkill(o Options, project bool) (Result, error) {
	var path string
	if project {
		path = filepath.Join(o.WorkDir, ".workbuddy", "skills", "brainx", "SKILL.md")
	} else {
		home := o.Home
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		path = filepath.Join(home, ".workbuddy", "skills", "brainx", "SKILL.md")
	}
	result := Result{Host: "workbuddy", Path: path}
	result.Note = "半自动：本宿主不自动注入，靠模型按 description 匹配后主动调 brainx"
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	if string(existing) == SkillDoc {
		result.Action = "unchanged"
		return result, nil
	}
	if o.DryRun {
		result.Action = "would-write"
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return result, err
	}
	if err := os.WriteFile(path, []byte(SkillDoc), 0o644); err != nil {
		return result, err
	}
	result.Action = "created"
	if len(existing) > 0 {
		result.Action = "updated"
	}
	return result, nil
}

// removeSkill 卸载时删掉技能包。只删我们自己写的那份（内容逐字节相同才删）。
func RemoveSkill(home string, project bool) (Result, error) {
	var path string
	if project {
		cwd, _ := os.Getwd()
		path = filepath.Join(cwd, ".workbuddy", "skills", "brainx", "SKILL.md")
	} else {
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		path = filepath.Join(home, ".workbuddy", "skills", "brainx", "SKILL.md")
	}
	result := Result{Host: "workbuddy", Path: path}
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		result.Action = "skipped"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if string(existing) != SkillDoc {
		result.Action = "unchanged"
		result.Note = "内容被改过，未删除"
		return result, nil
	}
	if err := os.Remove(path); err != nil {
		return result, err
	}
	result.Action = "removed"
	return result, nil
}

func copyFileMode(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}
