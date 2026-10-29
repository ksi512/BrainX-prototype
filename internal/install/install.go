// Package install writes (and removes) host hook configurations. Every write is
// byte-idempotent: if the rendered file equals the existing one, nothing is
// touched, so mtimes stay stable for sync tools.
package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ksi512/BrainX-prototype/internal/scope"
)

type Options struct {
	DryRun    bool
	Uninstall bool
	Only      []string
	// Scope 决定作用域：auto（默认，在 git 仓库里→项目级）/ global / project / both
	Scope string
	// AgentsTarget 决定约定文件写哪一份：auto（override 非空就写它）/ override / main
	AgentsTarget string
	Prefix       string
	NoPath       bool
	Exe          string
	Home         string
	WorkDir      string
}

// scopeChoice 实现「作用域自动判定」：在项目里→项目级，否则→全局。
// 判据与 scope 判定同一套（向上找 .git），避免两处口径漂移。
func (o Options) scopeChoice() string {
	if o.Scope != "" && o.Scope != "auto" {
		return o.Scope
	}
	if scope.IsRepo(o.WorkDir) {
		return "project"
	}
	return "global"
}

// ScopeChoice 供 CLI 打印实际生效的作用域。
func (o Options) ScopeChoice() string { return o.scopeChoice() }

// WantProject 供 CLI 决定要不要同时写项目级的约定文件与技能包。
func (o Options) WantProject() bool { return o.wantProject() }

func (o Options) wantGlobal() bool {
	choice := o.scopeChoice()
	return choice == "global" || choice == "both"
}

func (o Options) wantProject() bool {
	choice := o.scopeChoice()
	return choice == "project" || choice == "both"
}

type Result struct {
	Host   string
	Path   string
	Action string
	Note   string
}

type host struct {
	id      string
	format  string
	project bool
	events  [][2]string
}

var hosts = []host{
	{
		id:      "claude-code",
		format:  "claude-code",
		project: true,
		events:  [][2]string{{"SessionStart", "session-start"}, {"UserPromptSubmit", "user-prompt"}, {"Stop", "stop"}},
	},
	{
		id:     "codex",
		format: "codex",
		events: [][2]string{{"SessionStart", "session-start"}, {"UserPromptSubmit", "user-prompt"}, {"Stop", "stop"}},
	},
	{
		id:     "cursor",
		format: "cursor",
		events: [][2]string{{"sessionStart", "session-start"}, {"beforeSubmitPrompt", "user-prompt"}, {"stop", "stop"}},
	},
}

const dshNote = "DSH 需要官方桥接包：@deepseek-ai/dsh-hooks-claude-code（或 @deepseek-ai/dsh-hooks-codex，二选一，同挂会让事件执行两遍）；装好桥接后把 brainx hook 写进它的 hooks 配置。"

// Run installs or uninstalls hook configs. Explicitly requested hosts are
// written even when their config directory is missing; otherwise hosts are
// detected and untouched ones are reported as skipped.
func Run(o Options) ([]Result, error) {
	if o.Exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		o.Exe = exe
	}
	if o.Home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		o.Home = home
	}
	if o.WorkDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		o.WorkDir = wd
	}

	selected := map[string]bool{}
	for _, id := range o.Only {
		if normalized := NormalizeHost(id); normalized != "" {
			selected[normalized] = true
		}
	}
	explicit := len(selected) > 0

	var results []Result

	// 首次运行 = 自举安装：复制自身到 prefix + 配 PATH。之后运行只是幂等重写。
	if !o.Uninstall {
		if result, err := Bootstrap(o, o.Exe); err != nil {
			return results, err
		} else {
			results = append(results, result)
			// hook 里要写的是复制出来那份的绝对路径：
			// 用户之后删掉/移动源码目录，注入仍然能用。
			if result.Path != "" && result.Action != "skipped" {
				o.Exe = result.Path
			}
		}
	}

	for _, h := range hosts {
		if explicit && !selected[h.id] {
			continue
		}
		if !explicit && !detected(h, o) {
			results = append(results, Result{Host: h.id, Action: "skipped", Note: "未检测到该宿主，跳过（可用 --only 指定）"})
			continue
		}
		for _, project := range h.hookSeats(o) {
			result, err := apply(h, o, project)
			if err != nil {
				return results, err
			}
			if project {
				result.Host = h.id + "-project"
			}
			results = append(results, result)
		}
	}
	if o.Uninstall {
		if result, err := stripCodexFeatureFlag(o); err != nil {
			return results, err
		} else if result.Action != "skipped" {
			results = append(results, result)
		}
	} else if explicit && selected["codex"] || !explicit && dirExists(filepath.Join(o.Home, ".codex")) {
		// Codex 未显式开启 features.hooks 时会静默不跑，这一步不是可选项
		if result, err := ensureCodexFeatureFlag(o); err != nil {
			return results, err
		} else {
			results = append(results, result)
		}
	}

	// 约定文件（AGENTS.md / CLAUDE.md）：② 是主力 hook，① 是兜底，两层都要。
	//
	// 落点不是死的：Codex 每层**只读第一个非空文件**，override 存在时 AGENTS.md 是死代码。
	// 所以装的时候按 override → main 的顺序挑，卸载时两个候选都清。
	block, blockErr := markerBlock()
	if blockErr != nil {
		// 不能静默跳过：读不到正文真源时，用户会以为「已经装好了」，
		// 而实际上 Agent 手里没有任何职责说明。报出来，但不阻断 hook 安装。
		results = append(results, Result{Host: "agents", Action: "skipped", Note: blockErr.Error()})
	}
	if blockErr == nil {
		for _, target := range agentsTargets(o) {
			if explicit && !selected[target.Host] {
				continue
			}
			if o.Uninstall {
				for _, path := range target.candidates() {
					action, err := removeBlock(path, o.DryRun)
					if err != nil {
						return results, err
					}
					if action != "skipped" && action != "unchanged" {
						results = append(results, Result{Host: target.Host + "-agents", Path: path, Action: action})
					}
				}
				continue
			}
			if !explicit && !dirExists(filepath.Dir(target.Main)) {
				continue
			}
			path, note := target.pick(o)
			action, err := syncBlock(path, block, o.DryRun)
			if err != nil {
				return results, err
			}
			if action != "skipped" {
				results = append(results, Result{Host: target.Host + "-agents", Path: path, Action: action, Note: note})
			}
		}
	}

	if (!explicit || selected["dsh"]) && !o.Uninstall {
		results = append(results, Result{Host: "dsh", Action: "manual", Note: dshNote})
	}
	if o.Uninstall {
		self, err := UninstallSelf(o)
		if err != nil {
			return results, err
		}
		results = append(results, self...)
	}
	return results, nil
}

func (h host) path(o Options, project bool) string {
	switch h.id {
	case "claude-code":
		if project {
			return filepath.Join(o.WorkDir, ".claude", "settings.json")
		}
		return filepath.Join(o.Home, ".claude", "settings.json")
	case "codex":
		// Codex 只有全局 hooks.json（项目级靠 AGENTS.md）
		return filepath.Join(o.Home, ".codex", "hooks.json")
	case "cursor":
		return filepath.Join(o.Home, ".cursor", "hooks.json")
	}
	return ""
}

// hookSeats 返回这条宿主该写哪些座位。Codex / Cursor 只有全局，
// Claude Code 有全局与项目两套 settings.json。
func (h host) hookSeats(o Options) []bool {
	if h.id == "claude-code" {
		var seats []bool
		if o.wantGlobal() {
			seats = append(seats, false)
		}
		if o.wantProject() {
			seats = append(seats, true)
		}
		return seats
	}
	return []bool{false}
}

func detected(h host, o Options) bool {
	for _, project := range h.hookSeats(o) {
		dir := filepath.Dir(h.path(o, project))
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func apply(h host, o Options, project bool) (Result, error) {
	path := h.path(o, project)
	result := Result{Host: h.id, Path: path}
	switch h.id {
	case "codex":
		result.Note = "首次安装后需在 /hooks 确认信任"
	case "cursor":
		result.Note = "云代理不支持 sessionStart，仅本地 IDE 生效"
	}
	old, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}

	config := map[string]any{}
	if len(bytes.TrimSpace(old)) > 0 {
		if err := json.Unmarshal(old, &config); err != nil {
			return result, fmt.Errorf("%s 不是合法 JSON，未改动：%w", path, err)
		}
	}
	if h.id == "cursor" {
		if _, ok := config["version"]; !ok {
			config["version"] = 1
		}
	}
	hooks, _ := config["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		config["hooks"] = hooks
	}
	if o.Uninstall {
		for _, event := range h.events {
			hooks[event[0]] = filterOurs(hooks[event[0]])
		}
		for name, value := range hooks {
			if entries, ok := value.([]any); ok && len(entries) == 0 {
				delete(hooks, name)
			}
		}
		if len(hooks) == 0 {
			delete(config, "hooks")
		}
	} else {
		for _, event := range h.events {
			entries := filterOurs(hooks[event[0]])
			entries = append(entries, entry(h, o.Exe, event[1]))
			hooks[event[0]] = entries
		}
	}

	rendered, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return result, err
	}
	rendered = append(rendered, '\n')

	if o.DryRun {
		result.Action = "would-write"
		if o.Uninstall {
			result.Action = "would-remove"
		}
		return result, nil
	}
	if o.Uninstall && onlyVersionLeft(config) {
		if exists {
			if err := os.Remove(path); err != nil {
				return result, err
			}
			result.Action = "removed"
			return result, nil
		}
		result.Action = "unchanged"
		return result, nil
	}
	if exists && bytes.Equal(old, rendered) {
		result.Action = "unchanged"
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return result, err
	}
	if err := os.WriteFile(path, rendered, 0o644); err != nil {
		return result, err
	}
	if o.Uninstall {
		result.Action = "updated"
		return result, nil
	}
	result.Action = "created"
	if exists {
		result.Action = "updated"
	}
	return result, nil
}

// filterOurs drops entries that reference a brainx hook command, keeping any
// hooks the user installed by hand.
func filterOurs(value any) []any {
	entries, _ := value.([]any)
	kept := make([]any, 0, len(entries))
	for _, item := range entries {
		encoded, _ := json.Marshal(item)
		if strings.Contains(string(encoded), "hook --event") {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

func entry(h host, exe, event string) any {
	command := fmt.Sprintf("%s hook --event %s --format %s", shellQuote(exe), event, h.format)
	if h.id == "cursor" {
		return map[string]any{"command": command}
	}
	inner := map[string]any{"type": "command", "command": command}
	if h.id == "codex" {
		inner["additionalContextLimit"] = 5000
	}
	return map[string]any{"hooks": []any{inner}}
}

func shellQuote(value string) string {
	if value == "" {
		return `""`
	}
	if strings.ContainsAny(value, " \t\"'$`\\") {
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	}
	return value
}

func NormalizeHost(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claude-code", "claude_code":
		return "claude-code"
	case "codex":
		return "codex"
	case "cursor":
		return "cursor"
	case "dsh":
		return "dsh"
	}
	return ""
}

// onlyVersionLeft 判断卸载后是否只剩空壳。
// Cursor 的配置必须带 {"version":1}，卸载时如果只剩它，文件应当整个删掉——
// 留一个空壳文件既没作用，又会让人以为装过。
func onlyVersionLeft(config map[string]any) bool {
	if len(config) == 0 {
		return true
	}
	if len(config) == 1 {
		if _, ok := config["version"]; ok {
			return true
		}
	}
	return false
}
