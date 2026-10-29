# BrainX 集成

自动注入由宿主 hook 调用 CLI 完成，不依赖 MCP Resource 推送。

```sh
brainx hook --event session-start --format claude-code
brainx hook --event user-prompt --format claude-code
brainx hook --event stop --format claude-code
```

`session-start` 与 `user-prompt` 从 stdin 接收宿主 JSON，并只在 stdout 输出注入 JSON。`stop` 成功时不输出 stdout；所有诊断只写 stderr。

| `--format` | 输出字段 |
|---|---|
| `claude-code`（默认） | `hookSpecificOutput.hookEventName` + `additionalContext` |
| `codex` | 同 Claude Code 结构 |
| `cursor` | `additional_context`（小驼峰扁平结构） |

DSH 不直接读 Claude Code 的 hook 配置，需要官方桥接包 `@deepseek-ai/dsh-hooks-claude-code` 或 `@deepseek-ai/dsh-hooks-codex`（二选一，同挂会让同一事件执行两遍），由桥接调用 `brainx hook`。

建议将以下职责写入宿主的 `AGENTS.md` 标记块：

1. 需要持久化的事实使用 `brainx remember … --scope …` 写入。
2. 回答涉及历史或私有资料时先用 `brainx recall` 查询。
3. 看到待整理提示时运行 `brainx dream --plan`，由 Agent 判断后再应用。
4. `pending` 不会参与召回，使用 `brainx confirm` 处理待确认资产。

`brainx install` 把 hook 合并进 Claude Code、Codex、Cursor 的配置文件：内容未变化时不写盘，保证字节级幂等；支持 `--dry-run`、`--uninstall`、`--only` 与 `--project`。`--uninstall` 会一并收回自装二进制、PATH 行与空目录，`$BRAINX_HOME` 数据保留。DSH 只输出桥接包说明，不写文件。
