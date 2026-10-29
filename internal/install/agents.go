package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 约定文件（AGENTS.md 一类）的安装口径见提示词 §10.3 / §10.4：
//   - 正文单一真源是 $BRAINX_HOME/AGENTS.block.md，所有宿主由它渲染，**禁止每家一份文案**
//   - 只替换标记块之间的内容，绝不覆盖用户已有内容
//   - 内容逐字节相同则不写盘（保 mtime，同步工具不会误判全变）
const (
	blockBegin = "<!-- brainx:begin -->"
	blockEnd   = "<!-- brainx:end -->"
)

// agentsTarget 是一个「约定文件落点」。
//
// Override 是 Codex 特有的遮蔽机制：**每一层只取第一个非空文件**，
// AGENTS.override.md 存在就用它，AGENTS.md 完全不读（官方文档语义，全局与项目级都一样）。
// 所以「写到 AGENTS.md 就算装好」是错的——override 存在时那段是死代码。
type agentsTarget struct {
	Host     string
	Label    string
	Main     string
	Override string // 空 = 该宿主没有 override 机制
}

// agentsTargets 是「装哪儿」的唯一真源。
// Cursor 全局在账户设置里、无文件可写；WorkBuddy 无证据表明读 AGENTS.md —— 都不写。
func agentsTargets(o Options) []agentsTarget {
	return []agentsTarget{
		{
			Host: "codex", Label: "Codex 全局",
			Main:     filepath.Join(o.Home, ".codex", "AGENTS.md"),
			Override: filepath.Join(o.Home, ".codex", "AGENTS.override.md"),
		},
		{
			Host: "claude-code", Label: "Claude Code 全局",
			Main: filepath.Join(o.Home, ".claude", "CLAUDE.md"),
		},
	}
}

// pick 决定这次写到哪个文件，并给一句人话说明。
//
// 三种取值：
//   - main      ：强制写 AGENTS.md（用户自己负责清掉 override）
//   - override  ：强制写 override（不存在就创建）
//   - auto（默认）：override 存在且非空 → 写 override；否则写 main
//
// **不替换任何文件**：两种落点都只追加/替换自己的标记块，用户内容原样保留。
func (t agentsTarget) pick(o Options) (string, string) {
	if t.Override == "" || o.AgentsTarget == "main" {
		if t.Override != "" && o.AgentsTarget == "main" {
			return t.Main, t.Label + "：按 --agents-target=main 写入 AGENTS.md"
		}
		return t.Main, ""
	}
	if o.AgentsTarget == "override" {
		return t.Override, t.Label + "：按 --agents-target=override 写入 AGENTS.override.md"
	}
	data, err := os.ReadFile(t.Override)
	if err == nil && strings.TrimSpace(string(data)) != "" {
		return t.Override, t.Label + "：检测到 AGENTS.override.md 非空，写入它（Codex 只读第一个非空文件，写 AGENTS.md 不生效）"
	}
	return t.Main, ""
}

// candidates 返回卸载时要清理的所有落点。
//
// 卸载不能跟着 pick 走：用户可能在我们装完之后删掉了 override，
// 那时 auto 会指向 main，override 里那段就永远清不掉。
func (t agentsTarget) candidates() []string {
	if t.Override == "" {
		return []string{t.Main}
	}
	return []string{t.Main, t.Override}
}

// syncBlock 把标记块写进文件。返回动作：created / updated / unchanged / would-write。
func syncBlock(path, block string, dryRun bool) (string, error) {
	old, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	content := string(old)
	var next string
	if start := strings.Index(content, blockBegin); start >= 0 {
		if end := strings.Index(content[start:], blockEnd); end >= 0 {
			next = content[:start] + block + content[start+end+len(blockEnd):]
		} else {
			next = content + "\n\n" + block
		}
	} else if strings.TrimSpace(content) == "" {
		next = block + "\n"
	} else {
		next = strings.TrimRight(content, "\n") + "\n\n" + block + "\n"
	}
	if exists && next == string(old) {
		return "unchanged", nil
	}
	if dryRun {
		return "would-write", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return "", err
	}
	if exists {
		return "updated", nil
	}
	return "created", nil
}

// removeBlock 只摘掉自己的块，用户写在别处的内容原样保留。
// 摘完文件为空才删文件——留一个空壳文件比删掉更脏。
func removeBlock(path string, dryRun bool) (string, error) {
	old, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "skipped", nil
	}
	if err != nil {
		return "", err
	}
	content := string(old)
	start := strings.Index(content, blockBegin)
	if start < 0 {
		return "unchanged", nil
	}
	end := strings.Index(content[start:], blockEnd)
	if end < 0 {
		return "unchanged", nil
	}
	next := strings.TrimRight(content[:start]+content[start+end+len(blockEnd):], "\n")
	if strings.TrimSpace(next) == "" {
		if dryRun {
			return "would-remove", nil
		}
		if err := os.Remove(path); err != nil {
			return "", err
		}
		return "removed", nil
	}
	next += "\n"
	if next == content {
		return "unchanged", nil
	}
	if dryRun {
		return "would-write", nil
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return "", err
	}
	return "updated", nil
}

// ensureCodexFeatureFlag 处理 Codex 的启用方式：
// 部分版本的文档要求在 config.toml 的 [features] 里设 hooks = true，
// 不设就**静默不跑**（退出码 0、无警告）——属于最难查的一类失败。
//
// 三种情形分开处理，避免出现重复的 [features] 段（TOML 里那是语法错误）：
//  1. 已有 [features] 且其中有 hooks → 不动
//  2. 已有 [features] 但没有 hooks → 在该段头之后插一行
//  3. 没有 [features] → 追加一个带标记的段
func ensureCodexFeatureFlag(o Options) (Result, error) {
	path := filepath.Join(o.Home, ".codex", "config.toml")
	result := Result{Host: "codex-features", Path: path}
	old, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	if !exists && !dirExists(filepath.Dir(path)) {
		result.Action = "skipped"
		result.Note = "未检测到 Codex 配置目录"
		return result, nil
	}
	content := string(old)
	lines := strings.Split(content, "\n")
	section := ""
	featuresAt := -1
	hasHooks := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			section = strings.Trim(trimmed, "[]")
			if section == "features" {
				featuresAt = i
			}
			continue
		}
		if section == "features" && strings.HasPrefix(trimmed, "hooks") {
			hasHooks = true
		}
	}
	var next string
	switch {
	case hasHooks:
		result.Action = "unchanged"
		return result, nil
	case featuresAt >= 0:
		// 行尾带标记：卸载时只摘我们加的这一行，不动用户自己写的同名配置。
		// 没有这行注释，install 与 uninstall 就不对称——装得上、摘不掉。
		lines = append(lines[:featuresAt+1], append([]string{"hooks = true # brainx"}, lines[featuresAt+1:]...)...)
		next = strings.Join(lines, "\n")
	default:
		next = strings.TrimRight(content, "\n")
		if next != "" {
			next += "\n\n"
		}
		next += "# >>> brainx（Codex 需显式开启 hooks，否则静默不跑）\n[features]\nhooks = true\n# <<< brainx\n"
	}
	if o.DryRun {
		result.Action = "would-write"
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return result, err
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return result, err
	}
	if exists {
		result.Action = "updated"
	} else {
		result.Action = "created"
	}
	result.Note = "已写 [features] hooks = true"
	return result, nil
}

func stripCodexFeatureFlag(o Options) (Result, error) {
	path := filepath.Join(o.Home, ".codex", "config.toml")
	result := Result{Host: "codex-features", Path: path}
	old, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		result.Action = "skipped"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	content := string(old)
	changed := false
	// 两种形态都要摘：带标记的整段（无 [features] 时追加的），以及单行注释（插进已有段里的）
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, "# brainx") && strings.HasPrefix(strings.TrimSpace(line), "hooks") {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	content = strings.Join(kept, "\n")
	if strings.Contains(content, "# >>> brainx") {
		start := strings.Index(content, "# >>> brainx")
		if end := strings.Index(content[start:], "# <<< brainx\n"); end >= 0 {
			content = content[:start] + content[start+end+len("# <<< brainx\n"):]
			changed = true
		}
	}
	if !changed {
		// 只摘自己加的；用户手写的 hooks = true 不属于我们
		result.Action = "unchanged"
		return result, nil
	}
	next := strings.TrimRight(content, "\n")
	if strings.TrimSpace(next) == "" {
		if !o.DryRun {
			if err := os.Remove(path); err != nil {
				return result, err
			}
		}
		result.Action = "removed"
		return result, nil
	}
	if o.DryRun {
		result.Action = "would-write"
		return result, nil
	}
	if err := os.WriteFile(path, []byte(next+"\n"), 0o644); err != nil {
		return result, err
	}
	result.Action = "updated"
	return result, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func markerBlock() (string, error) {
	// 正文来自库里的单一真源，不从本包再抄一份
	if block := os.Getenv("BRAINX_AGENTS_BLOCK"); block != "" {
		return block, nil
	}
	home := os.Getenv("BRAINX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".brainx")
	}
	data, err := os.ReadFile(filepath.Join(home, "AGENTS.block.md"))
	if err != nil {
		return "", fmt.Errorf("读不到 $BRAINX_HOME/AGENTS.block.md（先跑 brainx init）：%w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
