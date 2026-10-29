package maintain

import (
	"fmt"
	"os"

	dreamplan "github.com/ksi512/BrainX-prototype/internal/dream"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

// RunQueuedCommands 是看板按钮的执行端：排队 → 下一次任意 CLI 启动时跑一次 → 删。
//
// 为什么放在独立包里：命令的「执行」既发生在普通命令的 preflight，
// 也发生在 hook Stop 收尾。放 cmd/ 里 hook 包够不着，放 hook 包里
// 又让「hook」这个名字背了不属于它的职责。
func RunQueuedCommands(s *store.Store) {
	commands, broken, err := s.ReadCommands()
	if err != nil {
		fmt.Fprintf(os.Stderr, "brainx: 读排队命令失败：%v\n", err)
		return
	}
	if len(broken) > 0 {
		fmt.Fprintf(os.Stderr, "brainx: 丢弃 %d 条无法解析的排队命令\n", len(broken))
	}
	for _, command := range commands {
		if err := Execute(s, command); err != nil {
			// 失败不重试：会失败的命令重试一百次还是失败，只会拖慢每次启动
			fmt.Fprintf(os.Stderr, "brainx: 排队命令「%s」失败，已丢弃：%v\n", command.Describe(), err)
		} else {
			fmt.Fprintf(os.Stderr, "brainx: 已执行排队命令「%s」\n", command.Describe())
		}
		_ = s.RemoveCommand(command)
	}
}

// Execute 执行一条命令。只支持三个纯规则动作：
// confirm（确认待办）、tidy（机械层）、drain（补写队列）。
//
// dream --plan / --apply 不在此列——它需要 Agent 逐组判断，
// 做成「点一下就自动做」的按钮等于骗人。
func Execute(s *store.Store, command store.Command) error {
	switch command.Verb {
	case "confirm":
		if len(command.IDs) == 0 {
			return fmt.Errorf("confirm 缺少 ids")
		}
		for _, id := range command.IDs {
			if err := s.Confirm(id); err != nil {
				return err
			}
		}
		return nil
	case "tidy":
		_, err := dreamplan.Mechanical(s, false)
		return err
	case "drain":
		_, _, err := s.DrainQueue(200)
		return err
	}
	return fmt.Errorf("未知排队命令 %q", command.Verb)
}
