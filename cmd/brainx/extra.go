package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	dreamplan "github.com/ksi512/BrainX-prototype/internal/dream"
	"github.com/ksi512/BrainX-prototype/internal/recall"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

// openCmd 是 `brainx open <id>`：取原件。
// 与 `get --open` 是同一实现的两个入口——原版把 open 列成独立命令，
// 但两者语义完全一致，没有必要写两份。
func openCmd(s *store.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w：用法 open <id>", errArgs)
	}
	asset, err := s.Get(args[0])
	if err != nil {
		return err
	}
	return openOriginal(s, asset)
}

// relinkCmd 单条重指外部位置。整根换路径用 config set archive.roots.<name>，不要逐条重指。
func relinkCmd(s *store.Store, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("%w：用法 relink <id> <root>:<relpath>", errArgs)
	}
	if err := s.Relink(args[0], args[1]); err != nil {
		return err
	}
	fmt.Println("已重指：", args[0], "→", args[1])
	return nil
}

// reviewCmd 处理待标注冲突。annotate 是默认处置——绝大多数冲突是「两条都对、前提不同」。
func reviewCmd(s *store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w：用法 review list | review resolve <id> <resolution>", errArgs)
	}
	switch args[0] {
	case "list":
		reviews, err := s.ReviewList(50)
		if err != nil {
			return err
		}
		if len(reviews) == 0 {
			fmt.Println("（没有未裁决冲突）")
			return nil
		}
		for _, r := range reviews {
			fmt.Printf("## %s ｜ %s\n", r.ID, r.Conflict)
			if r.Detail != "" {
				fmt.Printf("理由：%s\n", r.Detail)
			}
			fmt.Printf("- [%s] %s\n  %s\n", r.AssetA.ID, r.AssetA.Title, trimOneLine(r.AssetA.Body))
			fmt.Printf("- [%s] %s\n  %s\n", r.AssetB.ID, r.AssetB.Title, trimOneLine(r.AssetB.Body))
			fmt.Printf("处置：brainx review resolve %s annotate|supersede|pick_a|pick_b|merge\n\n", r.ID)
		}
		return nil
	case "resolve":
		f := flags("review", args)
		noteA := f.String("note-a", "", "")
		noteB := f.String("note-b", "", "")
		if err := f.Parse(reorderFlags(args[1:])); err != nil {
			return errArgs
		}
		if f.NArg() != 2 {
			return fmt.Errorf("%w：用法 review resolve <id> <resolution> [--note-a …] [--note-b …]", errArgs)
		}
		if err := s.ReviewResolve(f.Arg(0), f.Arg(1), *noteA, *noteB); err != nil {
			return err
		}
		fmt.Println("已标注：", f.Arg(0), f.Arg(1))
		return nil
	default:
		return fmt.Errorf("%w：review 只支持 list / resolve", errArgs)
	}
}

// replayCmd 幂等重放某次巩固。
//
// 幂等靠三件事：① plan 校验指向被重放的那次 run 的清单快照；
// ② 已完成的动作（已 merged / 已 archived）跳过；③ 库里若与当时快照完全一致，直接报「无需重放」。
func replayCmd(s *store.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%w：用法 replay <run_id>", errArgs)
	}
	runID := args[0]
	var decisionsJSON, snapshot, status string
	if err := s.DB.QueryRow(`SELECT COALESCE(decisions_json,''),input_snapshot_hash,status FROM runs WHERE id=?`, runID).
		Scan(&decisionsJSON, &snapshot, &status); err != nil {
		return fmt.Errorf("%w：找不到 run %s", os.ErrNotExist, runID)
	}
	if decisionsJSON == "" {
		return fmt.Errorf("%w：run %s 没有存判断内容，无法重放", errArgs, runID)
	}
	current, err := s.Snapshot("")
	if err != nil {
		return err
	}
	if current == snapshot {
		fmt.Println("库与该次 run 的输入快照一致，无需重放（幂等）")
		return nil
	}
	in, err := dreamplan.ParseDecisions([]byte(decisionsJSON))
	if err != nil {
		return err
	}
	report, err := dreamplan.Replay(s, in)
	if err != nil {
		return err
	}
	for _, line := range report.Lines {
		fmt.Println("-", line)
	}
	fmt.Printf("\nrun_id: %s 已执行 %d 条 · 跳过 %d 条（已完成）\n", report.RunID, report.Applied, report.Skipped)
	return nil
}

// suppressCmd 抑制某类提示。目前只有 root-warning：根不可达时别每次都弹。
func suppressCmd(s *store.Store, args []string) error {
	f := flags("suppress", args)
	session := f.Bool("session", false, "")
	until := f.String("until", "", "")
	list := f.Bool("list", false, "")
	clear := f.Bool("clear", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if *list {
		items, err := s.SuppressList()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("（没有抑制项）")
		}
		for _, item := range items {
			fmt.Printf("%s\t%s\n", item[0], item[1])
		}
		return nil
	}
	if f.NArg() != 1 {
		return fmt.Errorf("%w：用法 suppress <subject> [--session | --until 1d] | --list | --clear <subject>", errArgs)
	}
	subject := f.Arg(0)
	if *clear {
		return s.ClearSuppress(subject)
	}
	mode := "session"
	if *session {
		mode = "session"
	}
	if *until != "" {
		mode = "until:" + *until
	}
	if err := s.SetSuppress(subject, mode); err != nil {
		return err
	}
	fmt.Println("已抑制：", subject, mode)
	return nil
}

// maintenanceCmd 是 reindex / rechunk 的入口。两者都支持中断续跑。
func maintenanceCmd(s *store.Store, verb string, args []string) error {
	f := flags(verb, args)
	batch := f.Int("batch", 500, "")
	resume := f.Bool("resume", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	switch verb {
	case "reindex":
		n, err := s.Reindex(*batch, *resume)
		if err != nil {
			return err
		}
		fmt.Printf("reindex：重建 %d 片索引\n", n)
		return nil
	case "rechunk":
		n, err := s.Rechunk(*batch, *resume)
		if err != nil {
			return err
		}
		fmt.Printf("rechunk：重切 %d 条资产（旧向量已作废，需重算）\n", n)
		return nil
	}
	return errArgs
}

func trimOneLine(body string) string {
	one := strings.ReplaceAll(strings.TrimSpace(body), "\n", " ")
	runes := []rune(one)
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return one
}

// recallExpand 把 --expand 的补充词接在查询后面。
// 它不是同义词重写，只是「把 Agent 想到的相关词一起丢给 FTS」。
func recallExpand(query, expand string) string {
	if strings.TrimSpace(expand) == "" {
		return query
	}
	parts := []string{query}
	for _, part := range strings.Split(expand, ",") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " ")
}

var _ = strconv.Itoa
var _ = recall.Search
