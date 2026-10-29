package main

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	dreamplan "github.com/ksi512/BrainX-prototype/internal/dream"
	"github.com/ksi512/BrainX-prototype/internal/hook"
	"github.com/ksi512/BrainX-prototype/internal/index"
	installpkg "github.com/ksi512/BrainX-prototype/internal/install"
	"github.com/ksi512/BrainX-prototype/internal/maintain"
	"github.com/ksi512/BrainX-prototype/internal/recall"
	"github.com/ksi512/BrainX-prototype/internal/report"
	scopepkg "github.com/ksi512/BrainX-prototype/internal/scope"
	"github.com/ksi512/BrainX-prototype/internal/store"
	"github.com/ksi512/BrainX-prototype/internal/wiki"
)

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	// 退出码 4 的 stderr 已经是一份完整 JSON（含 options）：
	// 再补一行人话会让宿主 parse 失败，弹框就弹不出来。
	// 代价是终端里只剩 JSON —— 这是给机器看的，人看 options 也够。
	if !errors.Is(err, errNotReachable) && !errors.Is(err, store.ErrDownload) {
		fmt.Fprintln(os.Stderr, "brainx:", err)
	}
	os.Exit(code(err))
}
func code(err error) int {
	if errors.Is(err, errArgs) {
		return 2
	}
	if errors.Is(err, os.ErrNotExist) {
		return 3
	}
	// 4 = 原件不可达。**只影响取原件，不影响检索**——这是设计里明确的分工。
	if errors.Is(err, errNotReachable) {
		return 4
	}
	// 5 = 判断文件非法（Agent 的错，不是用户的错）
	if errors.Is(err, dreamplan.ErrBadDecision) || errors.Is(err, dreamplan.ErrBadOp) ||
		errors.Is(err, dreamplan.ErrBadPlanID) || errors.Is(err, dreamplan.ErrImmutable) {
		return 5
	}
	// 6 = 写锁竞争失败，内容已落 queue/，不会丢
	if errors.Is(err, store.ErrBusyExhausted) {
		return 6
	}
	// 7 = 库损坏 / schema 不符：停止写入
	if errors.Is(err, store.ErrCorrupt) {
		return 7
	}
	// 8 = 外部下载失败
	if errors.Is(err, store.ErrDownload) {
		return 8
	}
	return 1
}

var (
	errArgs         = errors.New("参数错误")
	errNotReachable = errors.New("原件不可达")
	errUnhealthy    = errors.New("体检未通过")
)

// 发布时用 -ldflags "-X main.version=v1.0.0 -X main.commit=$(git rev-parse --short HEAD) -X main.buildDate=..." 注入。
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

var knownCommands = map[string]bool{
	"init": true, "install": true, "hook": true, "context": true, "status": true, "scope": true,
	"remember": true, "import": true, "update": true, "forget": true, "confirm": true,
	"get": true, "open": true, "relink": true, "list": true, "search": true, "recall": true,
	"neighbors": true, "link": true, "dream": true, "review": true, "replay": true, "audit": true,
	"export": true, "wiki": true, "reindex": true, "rechunk": true, "config": true,
	"suppress": true, "doctor": true, "report": true,
}

func versionText() string {
	return fmt.Sprintf("brainx %s (commit %s, built %s)", version, commit, buildDate)
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	command := args[0]
	switch command {
	case "help", "--help", "-h", "-help":
		return usage()
	case "version", "--version", "-v", "-version":
		fmt.Println(versionText())
		return nil
	}
	if !knownCommands[command] {
		return fmt.Errorf("%w：未知子命令 %q（运行 brainx --help 查看全部命令）", errArgs, command)
	}
	home := store.Home()
	if command == "init" {
		if label := store.MountWarning(home); label != "" {
			return fmt.Errorf("拒绝在 %s 上初始化（%s）：SQLite WAL 依赖可靠的字节范围锁与共享内存映射，网络文件系统上会间歇性损坏数据库。\n换成本地路径，或设 BRAINX_ALLOW_NETWORK_HOME=1 强行放行", home, label)
		}
		s, err := store.Init(home)
		if err != nil {
			return err
		}
		defer s.Close()
		if err := store.ProbeFTS(s); err != nil {
			return err
		}
		if err := store.EnsureScaffold(home); err != nil {
			return err
		}
		fmt.Println("已初始化", home)
		return nil
	}
	s, err := store.Open(home)
	if err != nil {
		return err
	}
	defer s.Close()
	// queue/ 的消费入口：每次非 hook 命令启动时补写一次。
	// 放在这里而不是各处：preflight 只在该进程本来就要写的时候才有意义。
	if command != "hook" && command != "doctor" {
		if done, failed, qerr := s.DrainQueue(200); qerr == nil && (done > 0 || failed > 0) {
			fmt.Fprintf(os.Stderr, "brainx: 补偿队列 补写 %d 条 / 失败 %d 条\n", done, failed)
		}
		// 看板按钮排入的命令也在这一层执行：静态页起不了进程，只能等下一次 CLI 启动。
		maintain.RunQueuedCommands(s)
	}
	switch command {
	case "remember":
		return remember(s, args[1:])
	case "import":
		return importFile(s, args[1:])
	case "get":
		return get(s, args[1:])
	case "list":
		return list(s, args[1:])
	case "search":
		return search(s, args[1:], false)
	case "recall":
		return search(s, args[1:], true)
	case "update":
		return update(s, args[1:])
	case "wiki":
		return wikiCmd(s, args[1:])
	case "open":
		return openCmd(s, args[1:])
	case "relink":
		return relinkCmd(s, args[1:])
	case "review":
		return reviewCmd(s, args[1:])
	case "replay":
		return replayCmd(s, args[1:])
	case "suppress":
		return suppressCmd(s, args[1:])
	case "reindex", "rechunk":
		return maintenanceCmd(s, command, args[1:])
	case "context":
		return contextCmd(s, args[1:])
	case "status":
		return status(s)
	case "scope":
		return scopeCmd()
	case "hook":
		return hookCmd(s, args[1:])
	case "link":
		return link(s, args[1:])
	case "neighbors":
		return neighbors(s, args[1:])
	case "forget":
		return forget(s, args[1:])
	case "config":
		return config(s, args[1:])
	case "report":
		return reportCmd(s, args[1:])
	case "doctor":
		return doctor(s, args[1:])
	case "export":
		return exportDB(s, args[1:])
	case "dream":
		return dream(s, args[1:])
	case "confirm":
		return confirm(s, args[1:])
	case "audit":
		return audit(s, args[1:])
	case "install":
		return installCmd(s, args[1:])
	default:
		return fmt.Errorf("%w：未知子命令 %q", errArgs, command)
	}
}
func usage() error {
	fmt.Print(`brainx — 本地记忆与知识库

命令：init install hook context status scope
      remember import update forget confirm get open relink list search recall neighbors link
      dream review replay audit export wiki reindex rechunk config suppress doctor report

选项：--help / -h     显示本帮助
      --version / -v  显示版本（不需要先 init）
`)
	return nil
}
func hasDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func flags(name string, args []string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}
func scopeRequired(v string) error {
	if v == "" {
		return fmt.Errorf("%w：写入必须指定 --scope global 或 --scope <project>", errArgs)
	}
	return nil
}
func remember(s *store.Store, args []string) error {
	f := flags("remember", args)
	kind := f.String("kind", "memory", "")
	scope := f.String("scope", "", "")
	title := f.String("title", "", "")
	force := f.Bool("force", false, "")
	tags := multi{}
	f.Var(&tags, "tag", "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if err := scopeRequired(*scope); err != nil {
		return err
	}
	if f.NArg() != 1 {
		return fmt.Errorf("%w：用法 remember <text> --scope …", errArgs)
	}
	resolvedScope, err := resolveScope(*scope)
	if err != nil {
		return err
	}
	body := f.Arg(0)
	status := "active"
	if !*force {
		verdict, err := s.Judge(body, *kind)
		if err != nil {
			return err
		}
		switch verdict.Status {
		case "reject":
			return fmt.Errorf("拒绝写入（%d 分）：%s。确认要写请加 --force", verdict.Score, strings.Join(verdict.Reason, "；"))
		case "pending":
			// pending.auto_confirm_above：import-low 类里分数达标的直接放行。
			// 注意 insight 不在适用范围内——它必须人工确认（硬约束，不能被配置绕过）。
			if s.Config.PendingAutoConfirmAbove > 0 && *kind != "insight" && verdict.Score >= s.Config.PendingAutoConfirmAbove {
				status = "active"
				fmt.Fprintf(os.Stderr, "brainx: %d 分 ≥ auto_confirm_above(%d)，直接落 active\n", verdict.Score, s.Config.PendingAutoConfirmAbove)
			} else {
				status = "pending"
				fmt.Fprintf(os.Stderr, "brainx: 进待确认（%d 分：%s），用 brainx confirm <id> 激活\n", verdict.Score, strings.Join(verdict.Reason, "；"))
			}
		}
	}
	asset, err := s.Create(store.CreateAsset{Kind: *kind, Scope: resolvedScope, Title: *title, Body: body, Tags: tags, Origin: "manual", Archive: "text-only", Status: status, PendingReason: pendingReasonFor(status)})
	if err != nil {
		return err
	}
	fmt.Println(asset.ID)
	return nil
}

// firstHeading 取正文里第一个 Markdown H1（`# xxx`），最多 80 字。
// 只看前 40 行：标题在文件末尾的文档，H1 通常不是标题。
func firstHeading(text string) string {
	for i, line := range strings.Split(text, "\n") {
		if i > 40 {
			break
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "# ") {
			continue
		}
		title := strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		if title == "" {
			continue
		}
		if runes := []rune(title); len(runes) > 80 {
			title = string(runes[:80])
		}
		return title
	}
	return ""
}

func pendingReasonFor(status string) string {
	if status == "pending" {
		return "import-low"
	}
	return ""
}

func importFile(s *store.Store, args []string) error {
	f := flags("import", args)
	scope := f.String("scope", "", "")
	kind := f.String("kind", "doc", "")
	title := f.String("title", "", "")
	archive := f.String("archive", "", "")
	move := f.Bool("move", false, "")
	replace := f.Bool("replace", false, "")
	tags := multi{}
	f.Var(&tags, "tag", "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if f.NArg() < 1 {
		return fmt.Errorf("%w：用法 import <path...> --scope … | import <in.mbz> [--replace]", errArgs)
	}
	sources := f.Args()

	// .mbz 是「恢复整库」，自带全部数据，不需要 scope，也不该走文档解析
	if len(sources) == 1 && strings.HasSuffix(strings.ToLower(sources[0]), ".mbz") {
		backup, err := s.ImportBundle(sources[0], *replace)
		if err != nil {
			return err
		}
		if backup != "" {
			fmt.Fprintln(os.Stderr, "旧库已备份到", backup)
		}
		fmt.Println("已导入", sources[0])
		return nil
	}
	if err := scopeRequired(*scope); err != nil {
		return err
	}
	resolvedScope, err := resolveScope(*scope)
	if err != nil {
		return err
	}

	// 解析并行、落库串行。
	// 并行是因为解析是唯一的重活（PDF 尤其），落库受单连接约束本来就没法并行。
	type parsed struct {
		source string
		text   string
		err    error
	}
	results := make([]parsed, len(sources))
	workers := s.Config.ImportWorkers
	if workers <= 0 {
		workers = 4
	}
	if workers > len(sources) {
		workers = len(sources)
	}
	maxBytes := int64(s.Config.ImportMaxFileMB) << 20
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				source := sources[i]
				results[i].source = source
				if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
					// URL 直链：先下载成临时文件，再走同一条解析路径。
					// 这是「读取一次」的场景，不进入归档缓存逻辑。
					local, fetchErr := s.FetchURL(source, "")
					if fetchErr != nil {
						results[i].err = fetchErr
						continue
					}
					source = local
				}
				info, statErr := os.Stat(source)
				if statErr != nil {
					results[i].err = statErr
					continue
				}
				if maxBytes > 0 && info.Size() > maxBytes {
					results[i].err = fmt.Errorf("%s 超过 import.max_file_mb（%dMB）", source, s.Config.ImportMaxFileMB)
					continue
				}
				data, readErr := os.ReadFile(source)
				if readErr != nil {
					results[i].err = readErr
					continue
				}
				text, parseErr := index.Parse(source, data)
				if parseErr != nil {
					results[i].err = parseErr
					continue
				}
				if len(strings.TrimSpace(text)) < 20 {
					results[i].err = fmt.Errorf("抽出的正文过短（%d 字）", len([]rune(strings.TrimSpace(text))))
					continue
				}
				results[i].text = text
			}
		}()
	}
	for i := range sources {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	// 单条失败不中断整批：跳过并报错（提示词 M3：超出体积「跳过并报错，不中断整批」）
	failed := 0
	imported := 0
	for _, result := range results {
		if result.err != nil {
			fmt.Fprintf(os.Stderr, "brainx: 跳过 %s：%v\n", result.source, result.err)
			failed++
			continue
		}
		// 标题回退链（M3）：--title → 正文首个 H1 → 文件名。
		// H1 排在文件名前面是有理由的：文件名常是 `2026-10-26-notes-final-v2.md`，
		// 而 H1 是作者自己写的「这份文档叫什么」。
		assetTitle := *title
		titleSource := "flag"
		if assetTitle == "" {
			if heading := firstHeading(result.text); heading != "" {
				assetTitle = heading
				titleSource = "h1"
			} else {
				assetTitle = filepath.Base(result.source)
				titleSource = "filename"
			}
		}
		asset, err := s.ImportFile(store.CreateAsset{
			Kind: *kind, Scope: resolvedScope, Title: assetTitle, Body: result.text, Tags: tags,
			TitleSource: titleSource,
		}, result.source, *archive)
		if errors.Is(err, store.ErrDuplicate) {
			// 重复导入不是错误：同 content_hash 跳过（已归档的重导在 store 里已转成恢复）
			fmt.Fprintf(os.Stderr, "brainx: %s 与既有资产正文相同，跳过\n", result.source)
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "brainx: 写入 %s 失败：%v\n", result.source, err)
			failed++
			continue
		}
		if *move {
			if err := os.Remove(result.source); err != nil {
				fmt.Fprintf(os.Stderr, "brainx: 已入库但移走原件失败：%v\n", err)
			}
		}
		fmt.Println(asset.ID)
		imported++
	}
	if failed > 0 {
		// 全部失败 = 用户给的文件不对（格式不支持、空正文、超体积），退出码 2。
		// 部分成功 = 库已进了一部分，按内部错误退出（1），让人先看落库情况。
		if imported == 0 {
			return fmt.Errorf("%w：%d 个文件全部导入失败（原因见上）", errArgs, failed)
		}
		return fmt.Errorf("%d 个文件导入失败（已入库 %d 个）", failed, imported)
	}
	return nil
}

func get(s *store.Store, args []string) error {
	f := flags("get", args)
	open := f.Bool("open", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if f.NArg() != 1 {
		return fmt.Errorf("%w：用法 get <id> [--open]", errArgs)
	}
	asset, err := s.Get(f.Arg(0))
	if err != nil {
		return err
	}
	if *open {
		return openOriginal(s, asset)
	}
	fmt.Printf("# %s\n\n%s\n\n[id=%s; kind=%s; scope=%s; status=%s; archive=%s]\n",
		asset.Title, asset.Body, asset.ID, asset.Kind, asset.Scope, asset.Status, asset.Archive)
	links, err := s.Neighbors(asset.ID)
	if err != nil {
		return err
	}
	if len(links) > 0 {
		fmt.Printf("\n## 关系\n\n")
		for _, link := range links {
			fmt.Printf("- %s %s\n", link.Direction, link.Other)
		}
	}
	return nil
}

// openOriginal 打印原件的绝对路径（必要时先把 URL 型根下到本地缓存）。
//
// 不调用系统的 open(1)：hook 与无人值守环境里弹窗口比不出声更糟。
// 拿不到时按设计退出码 4，并把「哪个根、哪条资产、怎么改」以 JSON 打到 stderr。
func openOriginal(s *store.Store, asset store.Asset) error {
	path, err := s.ResolveOriginal(asset)
	if err == nil && (strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")) {
		path, err = s.FetchURL(path, "")
	}
	if err == nil {
		// 1 秒探测：网络挂载掉线时 stat 会阻塞数秒，open 是交互命令不能卡住
		err = store.ProbeReachable(path)
	}
	if err != nil {
		hint, _ := json.MarshalIndent(map[string]any{
			"error":   "原件不可达",
			"asset":   asset.ID,
			"archive": asset.Archive,
			"detail":  err.Error(),
			"options": []string{
				"正文与索引仍在库里，检索不受影响",
				"路径型根：检查 archive.roots." + rootName(asset.Archive) + " 是否还挂在原处",
				"确实不需要原件：重新 import 时加 --archive=text-only",
			},
		}, "", "  ")
		fmt.Fprintln(os.Stderr, string(hint))
		// 下载失败与「路径不在」是两种错：前者是外部服务/网络的问题（退出码 8），
		// 后者是根本地没挂上（退出码 4）。混成一个，宿主就没法给不同提示。
		if errors.Is(err, store.ErrDownload) {
			return fmt.Errorf("%w：%v", store.ErrDownload, err)
		}
		return fmt.Errorf("%w：%s", errNotReachable, asset.Archive)
	}
	fmt.Println(path)
	return nil
}

func rootName(archive string) string {
	rest := strings.TrimPrefix(archive, "ext:")
	if name, _, ok := strings.Cut(rest, ":"); ok {
		return name
	}
	return "<root>"
}

func list(s *store.Store, args []string) error {
	f := flags("list", args)
	scope := f.String("scope", "", "")
	kind := f.String("kind", "", "")
	status := f.String("status", "active", "")
	limit := f.Int("limit", 50, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if *scope != "" {
		resolved, err := resolveScope(*scope)
		if err != nil {
			return err
		}
		*scope = resolved
	}
	assets, err := s.List(*scope, *kind, *status, *limit)
	if err != nil {
		return err
	}
	for _, a := range assets {
		fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.Kind, a.Status, a.Title)
	}
	return nil
}
func search(s *store.Store, args []string, enhance bool) error {
	f := flags("search", args)
	scopeFlag := f.String("scope", "", "")
	kind := f.String("kind", "", "")
	limit := f.Int("limit", s.Config.RecallMaxItems, "")
	maxItems := f.Int("max-items", 0, "")
	chars := f.Int("max-chars", s.Config.RecallMaxChars, "")
	expand := f.String("expand", "", "")
	statusFlag := f.String("status", "active", "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if *maxItems > 0 {
		*limit = *maxItems
	}
	scope, err := resolveScope(*scopeFlag)
	if err != nil {
		return err
	}
	// 空查询 = 列举（提示词 §9.1）。把它当成「查不到」是最容易踩的坑：
	// 用户想看库里有什么，不应该被迫编一个关键词。
	if f.NArg() == 0 {
		assets, err := s.List(scope, *kind, *statusFlag, *limit)
		if err != nil {
			return err
		}
		for _, a := range assets {
			fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.Kind, a.Status, a.Title)
		}
		return nil
	}
	if f.NArg() != 1 {
		return fmt.Errorf("%w：search 只接受一个查询词", errArgs)
	}
	query := recallExpand(f.Arg(0), *expand)
	if enhance {
		result, err := recall.Recall(s, query, scope, *kind, *limit, *chars)
		if err == nil {
			fmt.Println(result)
		}
		return err
	}
	// search 是纯检索：不融合、不注入、不出 brief（原版 §4.4）
	results, err := recall.SearchLexical(s, query, scope, *kind, *limit)
	if err != nil {
		return err
	}
	for _, r := range results {
		fmt.Printf("%s\t%s\t%s\n", r.Asset.ID, r.Asset.Kind, strings.ReplaceAll(strings.TrimSpace(r.Snippet), "\n", " "))
	}
	return nil
}

// resolveScope 实现「读猜、写不猜」里的「猜」：不给 scope 时按 cwd 判定，
// 并把依据打到 stderr——判错时用户要能一眼看出为什么是这样的范围。
//
// `--scope proj` 是显式请求：不在仓库里就必须报错（退出码 2），
// **不回落 global**——回落就是静默降级。
func resolveScope(explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" && explicit != "proj" {
		return explicit, nil
	}
	res := scopepkg.Detect("")
	if explicit == "proj" && res.Scope == scopepkg.Global {
		return "", fmt.Errorf("%w：当前目录不在任何 git 仓库里，--scope proj 无法解析。改用 --scope global，或 cd 到仓库根再试", errArgs)
	}
	fmt.Fprintf(os.Stderr, "brainx: scope=%s（依据：%s）\n", res.Scope, res.Reason)
	return res.Scope, nil
}

// pendingCmd 是「待确认」的出口：待确认资产 + 未裁决冲突。
// 没有这个出口，pending 就是只进不出的死信箱。
func pendingCmd(s *store.Store) error {
	assets, err := s.List("", "", "pending", 50)
	if err != nil {
		return err
	}
	fmt.Printf("## 待确认资产（%d）\n", len(assets))
	for _, a := range assets {
		fmt.Printf("- %s [%s] %s\n", a.ID, a.Kind, a.Title)
	}
	conflicts, err := s.OpenReviewList(50)
	if err != nil {
		return err
	}
	fmt.Printf("\n## 未裁决冲突（%d）\n", len(conflicts))
	for _, c := range conflicts {
		fmt.Printf("- %s ⇄ %s（%s）%s\n", c.A, c.B, c.Conflict, c.Detail)
	}
	pendingQueue, deadQueue := s.QueueDepth()
	fmt.Printf("\n## 补偿队列（待补写 %d · 死信 %d）\n", pendingQueue, deadQueue)
	if pendingQueue > 0 || deadQueue > 0 {
		fmt.Println("（下次任意 brainx 命令会自动补写一次；死信用 doctor --retry-dead 移回）")
	}
	if len(assets) == 0 && len(conflicts) == 0 && pendingQueue == 0 && deadQueue == 0 {
		fmt.Println("（没有待办）")
	}
	return nil
}

// scopeCmd 打印当前目录判定出的 scope 与依据。判错时要能一条命令看清原因。
func scopeCmd() error {
	res := scopepkg.Detect("")
	fmt.Println(res.Scope)
	if res.ProjectRoot != "" {
		fmt.Printf("仓库根：%s\n", res.ProjectRoot)
	}
	fmt.Printf("依据：%s\n", res.Reason)
	return nil
}

func contextCmd(s *store.Store, args []string) error {
	f := flags("context", args)
	scopeFlag := f.String("scope", "", "")
	statusFlag := f.Bool("status", false, "")
	pendingFlag := f.Bool("pending", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if *statusFlag {
		return status(s)
	}
	if *pendingFlag {
		return pendingCmd(s)
	}
	scope, err := resolveScope(*scopeFlag)
	if err != nil {
		return err
	}
	out, err := recall.Context(s, scope)
	if err == nil {
		fmt.Println(out)
	}
	return err
}

// update 改写一条记忆型资产的正文/标题/标签并重建索引。
// kind='doc' 一律拒绝：原文不可变（硬约束 10）。
func update(s *store.Store, args []string) error {
	f := flags("update", args)
	body := f.String("body", "", "")
	title := f.String("title", "", "")
	tags := multi{}
	f.Var(&tags, "tag", "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if f.NArg() != 1 {
		return fmt.Errorf("%w：用法 update <id> [--body 正文] [--title 标题] [--tag 标签]", errArgs)
	}
	asset, err := s.Update(f.Arg(0), store.UpdateInput{Body: *body, Title: *title, AddTags: tags})
	if err != nil {
		return err
	}
	fmt.Printf("%s\t%s\t%s\n", asset.ID, asset.Kind, asset.Title)
	return nil
}

func status(s *store.Store) error {
	counts, err := s.Status()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, key := range keys {
		fmt.Printf("%s: %d\n", key, counts[key])
	}
	return nil
}
func hookCmd(s *store.Store, args []string) error {
	f := flags("hook", args)
	event := f.String("event", "", "")
	format := f.String("format", "claude-code", "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	out, err := hook.Run(s, *event, *format, os.Stdin)
	if err == nil && out != "" {
		fmt.Print(out)
	}
	return err
}
func link(s *store.Store, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("%w：用法 link <from> <to> <rel>", errArgs)
	}
	return s.Link(args[0], args[1], args[2])
}
func neighbors(s *store.Store, args []string) error {
	f := flags("neighbors", args)
	rel := f.String("rel", "", "")
	depth := f.Int("depth", 1, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if f.NArg() != 1 {
		return fmt.Errorf("%w：用法 neighbors <id> [--rel R] [--depth 1]", errArgs)
	}
	links, err := s.NeighborsDepth(f.Arg(0), *rel, *depth)
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.Direction == "in" {
			fmt.Printf("%s <--%s-- %s\n", f.Arg(0), link.Rel, link.Other)
		} else {
			fmt.Printf("%s --%s--> %s\n", f.Arg(0), link.Rel, link.Other)
		}
	}
	return nil
}

func forget(s *store.Store, args []string) error {
	f := flags("forget", args)
	hard := f.Bool("hard", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if f.NArg() != 1 {
		return fmt.Errorf("%w：用法 forget <id> [--hard]", errArgs)
	}
	id := f.Arg(0)
	if *hard {
		if err := s.HardForget(id); err != nil {
			return err
		}
		fmt.Println("已物理删除：", id)
		return nil
	}
	if err := s.SoftForget(id); err != nil {
		return err
	}
	fmt.Println("已归档（可回溯）：", id)
	return nil
}

func config(s *store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w：用法 config schema|get <key>|set <key> <value>|list", errArgs)
	}
	switch args[0] {
	case "schema":
		// 机器可读的唯一真源：config set 的校验、看板渲染、页面即时校验三处都读它
		data, err := json.MarshalIndent(store.ConfigSchema(), "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	case "list":
		rows, err := s.ConfigList()
		if err != nil {
			return err
		}
		for _, row := range rows {
			fmt.Printf("%-32s %-14s %s\n", row[0], row[1], row[2])
		}
		return nil
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("%w：用法 config get <key>", errArgs)
		}
		value, err := s.ConfigGet(args[1])
		if err != nil {
			return fmt.Errorf("%w：%s", errArgs, err.Error())
		}
		fmt.Printf("%v\n", value)
		return nil
	case "set":
		if len(args) != 3 {
			return fmt.Errorf("%w：用法 config set <key> <value>", errArgs)
		}
		if err := s.ConfigSet(args[1], args[2]); err != nil {
			return fmt.Errorf("%w：%s", errArgs, err.Error())
		}
		fmt.Println("已设置", args[1], "=", args[2])
		return nil
	default:
		return fmt.Errorf("%w：未知 config 操作 %q", errArgs, args[0])
	}
}
func reportCmd(s *store.Store, args []string) error {
	f := flags("report", args)
	out := f.String("out", "", "")
	open := f.Bool("open", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	path, stats, err := report.Generate(s, *out)
	if err != nil {
		return err
	}
	// 摘要打在 stdout，便于「生成了但数字不对」时立刻看见，不必先开浏览器
	fmt.Printf("%s\n", path)
	fmt.Printf("总 %d（active %d / archived %d）· 今日 %d · 近 7 日 %d · 待处理 %d（pending %d / 冲突 %d / queue %d）\n",
		stats.Total, stats.Active, stats.Archived, stats.Today, stats.Last7,
		stats.Pending+stats.OpenReviews+stats.QueuePending, stats.Pending, stats.OpenReviews, stats.QueuePending)
	if *open {
		// 只调系统默认程序打开一个 file:// 路径，不起服务、不留进程（硬约束 3）
		cmd := exec.Command("open", path)
		if runtime.GOOS == "linux" {
			cmd = exec.Command("xdg-open", path)
		} else if runtime.GOOS == "windows" {
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("看板已生成，但打开失败：%w", err)
		}
	}
	return nil
}
func doctor(s *store.Store, args []string) error {
	f := flags("doctor", args)
	repair := f.Bool("repair", false, "")
	retryDead := f.Bool("retry-dead", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	// doctor 是「收尾用的那条命令」：进来先把补偿队列排空（全量，不按预算）。
	// 检查单要求 doctor 能全量补写——否则失败写入只能等下一次巧合。
	if done, failed, rounds := s.DrainQueueFull(); done > 0 || failed > 0 {
		fmt.Printf("%-16s %-5s 补写 %d 条 / 失败 %d 条（%d 轮）\n", "queue", "ok", done, failed, rounds)
	}
	if *retryDead {
		moved, err := s.RetryDead()
		if err != nil {
			return err
		}
		fmt.Printf("queue/dead → queue：移回 %d 个文件\n", moved)
	}
	if *repair {
		out, err := s.Repair()
		if err != nil {
			return err
		}
		for key, value := range out {
			fmt.Printf("repair %-14s %s\n", key, value)
		}
	}
	checks, err := store.Doctor(s)
	if err != nil {
		return err
	}
	for _, check := range checks {
		fmt.Printf("%-16s %-5s %s\n", check.Name, check.Status, check.Detail)
	}
	if pending, dead := s.QueueDepth(); pending > 0 || dead > 0 {
		fmt.Printf("%-16s %-5s 待补写 %d · 死信 %d（doctor --retry-dead 可移回重试）\n", "queue", "warn", pending, dead)
	}
	if store.HasFailure(checks) {
		return errUnhealthy
	}
	return nil
}

// wikiCmd 构建投影层。幂等：内容未变的页面不重写（保 mtime），
// 否则任何同步工具都会误判「全变了」。
func wikiCmd(s *store.Store, args []string) error {
	f := flags("wiki", args)
	out := f.String("out", "", "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	dir, err := s.ResolveOut(*out)
	if err != nil {
		return err
	}
	summary, err := wiki.Build(s, dir)
	if err != nil {
		return err
	}
	fmt.Printf("wiki: %d 页 · 新写 %d · 未变 %d · 清理 %d\n%s\n",
		summary.Pages, summary.Written, summary.Skipped, summary.Removed, summary.Dir)
	return nil
}

func exportDB(s *store.Store, args []string) error {
	f := flags("export", args)
	derived := f.Bool("derived", false, "")
	to := f.String("to", "", "")
	kinds := f.String("kind", "", "")
	prune := f.Bool("prune", false, "")
	yes := f.Bool("yes", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}

	if *derived {
		// 生成物导出：Markdown 树，快照性质，不可恢复（提示词 §3.7）
		dir, err := s.ResolveOut(*to)
		if err != nil {
			return err
		}
		var kindList []string
		if *kinds != "" {
			for _, part := range strings.Split(*kinds, ",") {
				if part = strings.TrimSpace(part); part != "" {
					kindList = append(kindList, part)
				}
			}
		}
		stats, err := s.ExportDerived(dir, kindList, *prune, *yes)
		if err != nil {
			return err
		}
		fmt.Printf("export --derived：%d 页 · 新写 %d · 未变 %d · 清理 %d · 孤儿 %d\n%s\n",
			stats.Pages, stats.Written, stats.Skipped, stats.Pruned, len(stats.Orphans), stats.Root)
		return nil
	}

	if f.NArg() != 1 {
		return fmt.Errorf("%w：用法 export <out.mbz> | export --derived --to <dir> [--kind K,...] [--prune --yes]", errArgs)
	}
	outPath := f.Arg(0)
	// 打包前必须 checkpoint：直接拷 store.db 会漏掉还在 -wal 里的事务
	if _, err := s.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zipper := zip.NewWriter(out)
	defer zipper.Close()
	return filepath.Walk(s.Home, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || path == outPath {
			return nil
		}
		// cache/ 是下载缓存，不进备份（它可由 archive_hash 重建）
		if strings.Contains(filepath.ToSlash(path), "/cache/") {
			return nil
		}
		// replace 留下的旧库/旧原件备份不是当前库的一部分，不打包。
		if base := filepath.Base(path); strings.HasPrefix(base, "files.bak-") || strings.HasPrefix(base, "store.db.bak-") {
			return nil
		}
		// WAL / SHM 是连接期临时文件：checkpoint(TRUNCATE) 后 wal 已空，
		// shm 只是共享内存索引。打包它们既浪费空间，也可能带走过期锁状态；
		// 恢复时 SQLite 会按 store.db 自动重建。
		switch filepath.Base(path) {
		case "store.db-wal", "store.db-shm", "store.db-journal":
			return nil
		}
		rel, _ := filepath.Rel(s.Home, path)
		writer, err := zipper.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		_, err = io.Copy(writer, source)
		return err
	})
}

func dream(s *store.Store, args []string) error {
	f := flags("dream", args)
	plan := f.Bool("plan", false, "")
	apply := f.String("apply", "", "")
	autoLevel := f.String("auto-level", "", "")
	mode := f.String("mode", "mutate", "")
	scope := f.String("scope", "", "")
	limit := f.Int("limit", 20, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if *scope != "" {
		resolved, err := resolveScope(*scope)
		if err != nil {
			return err
		}
		*scope = resolved
	}

	// 机械层：纯规则、零模型，供 Stop hook 调用
	if *autoLevel != "" {
		if *autoLevel != "mechanical" {
			return fmt.Errorf("%w：--auto-level 目前只支持 mechanical", errArgs)
		}
		out, err := dreamplan.Mechanical(s, false)
		if err != nil {
			return err
		}
		if out != "" {
			fmt.Println(out)
		}
		return nil
	}

	// 第二段：执行 Agent 的判断。整批原子，错一条全不落。
	if *apply != "" {
		in, err := dreamplan.ReadDecisions([]string{*apply}, os.Stdin)
		if err != nil {
			return fmt.Errorf("%w：%s", errArgs, err.Error())
		}
		report, err := dreamplan.Apply(s, in)
		if err != nil {
			return err
		}
		for _, line := range report.Lines {
			fmt.Println("-", line)
		}
		fmt.Printf("\nrun_id: %s   已执行 %d 条\n", report.RunID, report.Applied)
		return nil
	}

	if !*plan {
		return fmt.Errorf("%w：用法 dream --plan [--mode mutate|derive|both] | --apply <file|-> | --auto-level mechanical", errArgs)
	}
	switch *mode {
	case "mutate", "derive", "both":
	default:
		return fmt.Errorf("%w：--mode 只能是 mutate / derive / both", errArgs)
	}

	// 阶段一：纯规则出候选。零模型调用、零资产写入。
	candidates, truncated, err := dreamplan.Plan(s, *scope, *mode, *limit)
	if err != nil {
		return err
	}
	if truncated {
		fmt.Fprintf(os.Stderr, "brainx: 触到 dream.plan_timeout_ms（%dms），输出的是部分结果，不是完整清单\n", s.Config.DreamPlanTimeoutMs)
	}
	snapshot, err := s.Snapshot(*scope)
	if err != nil {
		return err
	}
	planID := store.ID()
	scopeLabel := *scope
	if scopeLabel == "" {
		scopeLabel = "all"
	}

	fmt.Printf("# plan_id: %s   scope: %s   mode: %s   生成于 %s\n", planID, scopeLabel, *mode, time.Now().Format("2006-01-02 15:04"))

	if len(candidates) == 0 {
		fmt.Printf("\n无候选：当前 scope 下没有任何一组满足条件的资产。\n")
		fmt.Printf("（判据：正文完全相同 / bigram 相似度 ≥ %d%% / 共享 ≥2 个标签，mode=%s）\n", dreamplan.ThresholdPercent(), *mode)
	} else {
		total := 0
		for n, c := range candidates {
			fmt.Printf("\n## 候选 %d ｜ %s（%d 条）\n理由：%s\n\n", n+1, c.Type, len(c.Assets), c.Reason)
			for _, a := range c.Assets {
				title := a.Title
				if len([]rune(title)) > 48 {
					title = string([]rune(title)[:48]) + "…"
				}
				fmt.Printf("- %s [%s] %s\n", a.ID, a.Kind, title)
			}
			fmt.Printf("\n可选：%s\n", c.Options)
			total += len(c.Assets)
		}
		fmt.Printf("\n---\n共 %d 组候选 · 涉及 %d 条资产 · 全程零模型调用、零资产写入\n", len(candidates), total)
	}

	// 令牌：--plan 的唯一写入。--apply 要靠它校验「用的是最近一次的清单」。
	token, _ := json.Marshal(map[string]any{
		"plan_id":    planID,
		"snapshot":   snapshot,
		"created_at": time.Now().UnixMilli(),
		"mode":       *mode,
		"scope":      scopeLabel,
		"candidates": len(candidates),
	})
	if err := s.SetMeta("plan:"+*mode+":"+scopeLabel, string(token)); err != nil {
		return err
	}
	// 第二水位线：出过清单就算「已处理一次」，待整理提示随之归零
	if err := s.SetMeta("dream:last_plan_at", strconv.FormatInt(time.Now().UnixMilli(), 10)); err != nil {
		return err
	}
	_ = s.SetMeta("tidy_hint:first_shown_at", "")
	return nil
}

func confirm(s *store.Store, args []string) error {
	f := flags("confirm", args)
	pending := f.Bool("pending", false, "")
	reject := f.Bool("reject", false, "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	if *pending || f.NArg() == 0 {
		return pendingCmd(s)
	}
	for _, id := range f.Args() {
		if *reject {
			if err := s.Reject(id, "rejected"); err != nil {
				return err
			}
			fmt.Println("已拒绝并归档：", id)
			continue
		}
		if err := s.Confirm(id); err != nil {
			return err
		}
		fmt.Println("已确认：", id)
	}
	return nil
}

func audit(s *store.Store, args []string) error {
	rows, err := s.DB.Query(`SELECT op,COALESCE(asset_id,''),at FROM events ORDER BY at DESC LIMIT 100`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var op, id string
		var at int64
		rows.Scan(&op, &id, &at)
		fmt.Printf("%d\t%s\t%s\n", at, op, id)
	}
	return rows.Err()
}
func installCmd(s *store.Store, args []string) error {
	f := flags("install", args)
	dryRun := f.Bool("dry-run", false, "")
	uninstall := f.Bool("uninstall", false, "")
	purge := f.Bool("purge", false, "")
	yes := f.Bool("yes", false, "")
	only := f.String("only", "", "")
	prefix := f.String("prefix", "", "")
	noPath := f.Bool("no-path", false, "")
	global := f.Bool("global", false, "")
	project := f.Bool("project", false, "")
	both := f.Bool("both", false, "")
	agentsTarget := f.String("agents-target", "auto", "")
	if err := f.Parse(reorderFlags(args)); err != nil {
		return errArgs
	}
	switch *agentsTarget {
	case "auto", "override", "main":
	default:
		return fmt.Errorf("%w：--agents-target 只能是 auto / override / main", errArgs)
	}
	if *purge && !*uninstall {
		return fmt.Errorf("%w：--purge 只能与 --uninstall 一起用（会删除整个 $BRAINX_HOME）", errArgs)
	}
	if *purge && !*dryRun && !*yes {
		return fmt.Errorf("%w：--purge 会删除整个 $BRAINX_HOME；确认后加 --yes 再执行（可先用 --dry-run 查看）", errArgs)
	}
	// 作用域：显式开关优先，否则 auto（在 git 仓库里→项目级）
	scopeMode := "auto"
	switch {
	case *both:
		scopeMode = "both"
	case *global:
		scopeMode = "global"
	case *project:
		scopeMode = "project"
	}
	var selected []string
	for _, part := range strings.Split(*only, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		normalized := installpkg.NormalizeHost(trimmed)
		if normalized == "" && strings.EqualFold(trimmed, "workbuddy") {
			normalized = "workbuddy"
		}
		if normalized == "" {
			return fmt.Errorf("%w：未知宿主 %q（可选：claude-code / codex / cursor / dsh / workbuddy）", errArgs, trimmed)
		}
		selected = append(selected, normalized)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	options := installpkg.Options{
		DryRun: *dryRun, Uninstall: *uninstall, Only: selected,
		Scope: scopeMode, AgentsTarget: *agentsTarget,
		Prefix: *prefix, NoPath: *noPath, Exe: exe,
	}
	results, err := installpkg.Run(options)
	if err != nil {
		return err
	}

	// WorkBuddy 走技能包：不写 hook、不写 AGENTS.md（写它不读的文件比不写更糟）。
	// 默认只给已存在 ~/.workbuddy（或项目 .workbuddy）的机器写，避免凭空建目录。
	wantWorkBuddy := containsString(selected, "workbuddy")
	if len(selected) == 0 {
		wantWorkBuddy = hasDir(filepath.Join(options.Home, ".workbuddy")) ||
			(options.WantProject() && hasDir(filepath.Join(options.WorkDir, ".workbuddy")))
	}
	if *uninstall {
		wantWorkBuddy = true // 卸载时只清理已存在的技能，不存在会 skipped
	}
	if wantWorkBuddy {
		if *uninstall {
			for _, project := range []bool{false, true} {
				if result, err := installpkg.RemoveSkill(options.Home, project); err == nil && result.Action != "skipped" {
					results = append(results, result)
				}
			}
		} else {
			if result, err := installpkg.WriteSkill(options, false); err == nil {
				results = append(results, result)
			} else {
				return err
			}
			if options.WantProject() {
				if result, err := installpkg.WriteSkill(options, true); err == nil {
					results = append(results, result)
				}
			}
		}
	}

	if *purge {
		result, err := purgeData(s, *dryRun)
		if err != nil {
			return err
		}
		results = append(results, result)
	}

	for _, result := range results {
		line := result.Host + "\t" + result.Action
		if result.Path != "" {
			line += "\t" + result.Path
		}
		if result.Note != "" {
			line += "\t" + result.Note
		}
		fmt.Println(line)
	}
	if !*uninstall {
		fmt.Printf("\n作用域：%s（在 git 仓库里默认项目级，否则全局；--global/--project/--both 可强制）\n", options.ScopeChoice())
		// 这里是说明，不是警告：override 已经处理了，用户不需要动手。
		// 真正需要知道的是「谁在干活」——注入靠 hook，约定文件只是兜底。
		fmt.Println("注入主力：hook（SessionStart / UserPromptSubmit 自动把 brief 注入上下文，host 侧无需配合）")
		fmt.Println("约定文件：兜底。hook 没装或被跳过时，Agent 靠「存 / 取 / 整理 / 待确认」四段自己维持记忆。")
		fmt.Printf("落点策略：--agents-target=%s（auto = 该目录下 AGENTS.override.md 非空就写它，否则写 AGENTS.md）\n", *agentsTarget)
		fmt.Println("注意：Codex 每层只读第一个非空文件，且 hook 首次需在宿主里确认信任（信任绑定命令 hash，换命令要重新信任一次）。")
	}
	return nil
}

// purgeData 删除整个 $BRAINX_HOME。只允许显式 --purge 触发，
// 并拒绝根目录与用户家目录，避免手滑把别的目录删掉。
func purgeData(s *store.Store, dryRun bool) (installpkg.Result, error) {
	home, err := filepath.Abs(filepath.Clean(s.Home))
	if err != nil {
		return installpkg.Result{}, err
	}
	userHome, _ := os.UserHomeDir()
	if home == filepath.Clean(userHome) || home == string(filepath.Separator) || home == filepath.VolumeName(home)+string(filepath.Separator) {
		return installpkg.Result{}, fmt.Errorf("%w：拒绝删除 %q（不是 brainx 数据目录）", errArgs, s.Home)
	}
	protected := []string{"Desktop", "Documents", "Downloads", "Library", "Applications", "Movies", "Music", "Pictures", "Public", "Volumes", "tmp", "private", "etc", "usr", "var", "opt", "System", "Users"}
	for _, name := range protected {
		for _, base := range []string{userHome, string(filepath.Separator)} {
			if home == filepath.Join(base, name) {
				return installpkg.Result{}, fmt.Errorf("%w：拒绝删除 %q（这是受保护目录，不是 brainx 数据目录）", errArgs, s.Home)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(home, "manifest.json")); err != nil {
		return installpkg.Result{}, fmt.Errorf("%w：拒绝删除 %q（缺少 manifest.json，不像 brainx 数据目录）", errArgs, s.Home)
	}
	result := installpkg.Result{Host: "data", Path: home}
	if _, err := os.Stat(home); os.IsNotExist(err) {
		result.Action = "skipped"
		return result, nil
	}
	if dryRun {
		result.Action = "would-remove"
		return result, nil
	}
	// 先关库再删：否则 SQLite 可能在 RemoveAll 之后又建出 -wal/-shm。
	if err := s.Close(); err != nil {
		return result, err
	}
	if err := os.RemoveAll(home); err != nil {
		return result, err
	}
	result.Action = "removed"
	return result, nil
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }
func sortStrings(values []string) {
	for i := range values {
		for j := i + 1; j < len(values); j++ {
			if values[j] < values[i] {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
}

// reorderFlags accepts the documented `command <value> --flag value` form as
// well as Go flag's native `command --flag value <value>` form.
func reorderFlags(args []string) []string {
	var flagArgs, positional []string
	for index := 0; index < len(args); index++ {
		value := args[index]
		if !strings.HasPrefix(value, "-") || value == "-" {
			positional = append(positional, value)
			continue
		}
		flagArgs = append(flagArgs, value)
		if !strings.Contains(value, "=") && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
			flagArgs = append(flagArgs, args[index+1])
			index++
		}
	}
	return append(flagArgs, positional...)
}
