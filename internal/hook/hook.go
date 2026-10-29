package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/dream"
	"github.com/ksi512/BrainX-prototype/internal/maintain"
	"github.com/ksi512/BrainX-prototype/internal/recall"
	"github.com/ksi512/BrainX-prototype/internal/scope"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

// watermarkPrefix keys the "last injected" timestamp per scope in the meta table.
const watermarkPrefix = "brief:last_updated_at:"

type Input struct {
	SessionID string
	CWD       string
	Prompt    string
	Event     string
}

// Run renders the host-specific hook output. Only the protocol JSON is written
// to stdout; every diagnostic belongs to stderr.
func Run(s *store.Store, event, format string, reader io.Reader) (string, error) {
	format = normalizeFormat(format)
	if format == "" {
		return "", fmt.Errorf("未知 hook 格式（支持 claude-code / codex / cursor）")
	}
	input := parseInput(reader)
	rawEvent := strings.TrimSpace(event)
	event = normalizeEvent(event)
	if event == "" && rawEvent != "" {
		return "", fmt.Errorf("未知 hook 事件 %q", rawEvent)
	}
	if event == "" {
		rawEvent = strings.TrimSpace(input.Event)
		event = normalizeEvent(input.Event)
	}
	if event == "" {
		if rawEvent != "" {
			return "", fmt.Errorf("未知 hook 事件 %q", rawEvent)
		}
		return "", fmt.Errorf("hook 需要 --event，或 stdin 中带 hook_event_name")
	}
	if event == "stop" {
		// Stop 是收尾：补写队列 + 机械层 + WAL 回收，全部挤在 hook.stop_budget_ms 里。
		//
		// 为什么这里必须补写队列：检查单要求「只跑 Stop 也能把队列排空」——
		// 用户不该为了清掉一次写锁竞争而手动敲 doctor。
		// 输出一律丢弃：stdout 必须严格为空，否则宿主的严格校验会把整次 hook 判失败。
		budget := time.Duration(s.Config.HookStopBudgetMs) * time.Millisecond
		if budget <= 0 {
			budget = 400 * time.Millisecond
		}
		deadline := time.Now().Add(budget)

		if done, failed, err := s.DrainQueue(50); err != nil {
			fmt.Fprintln(os.Stderr, "brainx: 补写队列失败:", err)
		} else if done > 0 || failed > 0 {
			fmt.Fprintf(os.Stderr, "brainx: 补写 %d 条 / 失败 %d 条\n", done, failed)
		}

		if s.Config.TidyAuto && time.Now().Before(deadline) {
			if _, err := dream.Mechanical(s, false); err != nil {
				fmt.Fprintln(os.Stderr, "brainx: 机械层失败:", err)
			}
		}

		// 看板按钮排的命令也在收尾执行：Stop 是每次会话必跑的那个点，
		// 放在这里之后，「点了按钮」到「真的做了」最多隔一轮对话。
		if time.Now().Before(deadline) {
			maintain.RunQueuedCommands(s)
		}

		// 剩余预算 > 30ms 才回收：别让收尾把预算吃穿（checkpoint 在长 WAL 上会慢）
		if time.Until(deadline) > 30*time.Millisecond {
			_ = s.CheckpointWAL()
		}
		return "", nil
	}

	// scope 判定走统一实现：向上找 .git，取 remote 哈希或目录名。
	// 不能用 cwd 当标识——子目录里查不到仓库根目录记的东西，是静默失忆。
	scopeName := scope.Global
	if cwd := strings.TrimSpace(input.CWD); cwd != "" {
		scopeName = scope.Detect(cwd).Scope
	}
	// 队列非空时只报数、不补写：这两个事件的预算是 100ms，
	// 补写要开事务、可能要退避重试，正好会把预算吃穿。补写留给 Stop 与任意命令。
	if pending, dead := s.QueueDepth(); pending > 0 || dead > 0 {
		fmt.Fprintf(os.Stderr, "brainx: queue 待补写 %d / 死信 %d（Stop 或任意命令会处理）\n", pending, dead)
	}

	key := watermarkPrefix + scopeName
	watermark, _ := strconv.ParseInt(mustMeta(s, key), 10, 64)

	var brief string
	var err error
	switch event {
	case "session-start":
		brief, err = recall.ContextBrief(s, scopeName, 0, true)
	case "user-prompt":
		// 增量注入不带「待整理」提示：同一会话里重复提醒同一件事是纯噪音
		brief, err = recall.ContextBrief(s, scopeName, watermark, false)
	default:
		return "", fmt.Errorf("未知 hook 事件 %q", event)
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(brief) == "" {
		return "", nil
	}
	if newest, err := s.MaxUpdatedAt(scopeName); err == nil && newest > watermark {
		_ = s.SetMeta(key, strconv.FormatInt(newest, 10))
	}
	return render(format, event, brief)
}

func mustMeta(s *store.Store, key string) string {
	value, err := s.Meta(key)
	if err != nil {
		return ""
	}
	return value
}

func render(format, event, brief string) (string, error) {
	if format == "cursor" {
		data, err := json.Marshal(map[string]string{"additional_context": brief})
		return string(data), err
	}
	name := "SessionStart"
	if event == "user-prompt" {
		name = "UserPromptSubmit"
	}
	output := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     name,
			"additionalContext": brief,
		},
	}
	data, err := json.Marshal(output)
	return string(data), err
}

func normalizeFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "claude", "claude-code", "claude_code":
		return "claude-code"
	case "codex":
		return "codex"
	case "cursor":
		return "cursor"
	}
	return ""
}

func normalizeEvent(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "session-start", "sessionstart":
		return "session-start"
	case "user-prompt", "user-prompt-submit", "userpromptsubmit", "before-submit-prompt", "beforesubmitprompt":
		return "user-prompt"
	case "stop":
		return "stop"
	}
	return ""
}

// parseInput tolerates both snake_case (Claude Code / Codex) and camelCase
// (Cursor) payloads, and never fails on missing fields or empty stdin.
func parseInput(reader io.Reader) Input {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(reader).Decode(&raw); err != nil {
		return Input{}
	}
	get := func(keys ...string) string {
		for _, key := range keys {
			value, ok := raw[key]
			if !ok {
				continue
			}
			var text string
			if json.Unmarshal(value, &text) == nil {
				return text
			}
		}
		return ""
	}
	return Input{
		SessionID: get("session_id", "sessionId", "sessionID"),
		CWD:       get("cwd", "working_directory", "workingDirectory"),
		Prompt:    get("prompt", "user_prompt", "userPrompt"),
		Event:     get("hook_event_name", "hookEventName", "event", "eventName"),
	}
}

func ProjectScope(cwd string) string { return scope.Detect(cwd).Scope }
