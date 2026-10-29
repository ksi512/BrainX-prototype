package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// queue/cmd 与 queue 故意是两个目录：
//   - queue/     = **丢了的数据**（补写），失败要重试、要死信
//   - queue/cmd/ = **命令**，执行一次即删，失败不重试
//
// 混在一起会让一条失败的 UI 命令被当成丢写反复重放。
//
// 静态页起不了进程，所以看板上的按钮只能「排队」。执行发生在
// 下一次任意 brainx 命令启动时（preflight），一次即删。

// Command 是一条排队命令。看板写入的 JSON 就长这样。
type Command struct {
	File string   `json:"-"`
	Verb string   `json:"verb"`
	IDs  []string `json:"ids"`
	At   string   `json:"at"`
}

// Verb 的取值只允许这三个：都是**纯规则、确定性、可回滚**的动作。
// dream --plan/--apply 不在此列——它需要 Agent 逐组判断，做成按钮等于骗人。
var commandVerbs = map[string]bool{"confirm": true, "tidy": true, "drain": true}

// 单次 preflight 最多消费这么多条：一个被误点上百次的按钮不该卡住下一条命令。
const commandBatchLimit = 20

// ReadCommands 读出待执行命令，按文件名（时间前缀）升序。
// 解析失败的文件直接删掉：它永远不可能被执行，留着只会每次启动都报一遍。
func (s *Store) ReadCommands() ([]Command, []string, error) {
	dir := filepath.Join(s.Home, "queue", "cmd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	var commands []Command
	var broken []string
	for _, name := range names {
		if len(commands) >= commandBatchLimit {
			break
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var command Command
		if err := json.Unmarshal(data, &command); err != nil || !commandVerbs[command.Verb] {
			broken = append(broken, name)
			// 坏命令当场删：它不是数据，不会有人来修
			_ = os.Remove(path)
			continue
		}
		command.File = name
		commands = append(commands, command)
	}
	return commands, broken, nil
}

// RemoveCommand 删掉一条已执行（或已失败）的命令。失败也不重试：
// 一条会失败的命令重试一百次还是失败，只会拖慢每次启动。
func (s *Store) RemoveCommand(command Command) error {
	return os.Remove(filepath.Join(s.Home, "queue", "cmd", command.File))
}

// Describe 给 CLI 输出用的一行说明。
func (c Command) Describe() string {
	switch c.Verb {
	case "confirm":
		return fmt.Sprintf("confirm %s", strings.Join(c.IDs, ","))
	case "tidy":
		return "机械层"
	case "drain":
		return "补写队列重放"
	}
	return c.Verb
}
