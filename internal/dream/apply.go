package dream

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/index"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

// Decision 是两段式的第二段输入：宿主 Agent 对每一组候选给出的判断。
//
// 字段刻意宽松（ids / id+by 都能表达同一条 supersede），因为写这份 JSON 的是模型，
// 不是编译器——多一种自然写法比多一条硬性语法便宜。
type Decision struct {
	Op      string   `json:"op"`
	IDs     []string `json:"ids,omitempty"`
	Keep    string   `json:"keep,omitempty"`
	ID      string   `json:"id,omitempty"`
	By      string   `json:"by,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Title   string   `json:"title,omitempty"`
	Body    string   `json:"body,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// Decisions 是 --apply 的完整输入。
type Decisions struct {
	PlanID    string     `json:"plan_id"`
	Decisions []Decision `json:"decisions"`
	Note      string     `json:"note,omitempty"`
}

// Report 是 apply 的结果摘要。
type Report struct {
	RunID   string
	Applied int
	Skipped int
	Lines   []string
}

var (
	ErrBadPlanID   = errors.New("plan_id 不是最近一次 --plan 的令牌")
	ErrBadOp       = errors.New("判断文件非法")
	ErrImmutable   = errors.New("目标资产不可改写（kind='doc'）：文档只能加边、加派生、加冲突，正文一律禁止改写")
	ErrBadDecision = errors.New("判断文件非法")
)

// validOps 是允许的动作集合。keep / normalize 是「不动手」的判断——
// 它们仍要过一遍校验并留痕，否则「Agent 看过但决定不动」这件事就消失了。
var validOps = map[string]bool{
	"keep": true, "normalize": true, "cluster": true,
	"merge": true, "archive": true, "supersede": true,
	"insight": true, "derive": true,
}

// Apply 执行第二段：校验 → 落盘 → 留痕。整批要么全成，要么全不成。
func Apply(s *store.Store, in Decisions) (Report, error) {
	return run(s, in, false)
}

// Replay 幂等重放：不校验 plan（判断内容本来就来自那次 run），
// 已完成的动作跳过，所以重复重放不会二次损坏数据。
func Replay(s *store.Store, in Decisions) (Report, error) {
	return run(s, in, true)
}

func run(s *store.Store, in Decisions, replay bool) (Report, error) {
	if len(in.Decisions) == 0 {
		return Report{}, fmt.Errorf("%w：decisions 为空：没有要执行的动作", ErrBadDecision)
	}
	planMode := ""
	if !replay {
		if strings.TrimSpace(in.PlanID) == "" {
			return Report{}, fmt.Errorf("%w：缺少 plan_id", ErrBadPlanID)
		}
		mode, err := checkPlanID(s, in.PlanID)
		if err != nil {
			return Report{}, err
		}
		planMode = mode
	}
	for n, d := range in.Decisions {
		if !validOps[d.Op] {
			return Report{}, fmt.Errorf("%w：第 %d 条 op=%q（允许 keep/normalize/cluster/merge/archive/supersede/insight/derive）", ErrBadOp, n+1, d.Op)
		}
	}

	// 快照必须在拿连接**之前**算：连接池只有一条连接，
	// 事务里再走 s.DB 会等自己释放锁，形成自锁（症状是测试直接 SIGTERM）。
	snapshot, err := s.Snapshot("")
	if err != nil {
		return Report{}, err
	}

	runID := store.ID()
	report := Report{RunID: runID}
	if err := applyTx(s, runID, snapshot, planMode, in, &report, replay); err != nil {
		return Report{RunID: runID}, err
	}
	return report, nil
}

// checkPlanID 保证「用的是最近一次的清单」。
//
// 为什么必须校验：清单是快照，执行前库里可能已经变了。拿旧清单执行 merge，
// 会合掉一条后来才被改写成别的意思的记忆。
func checkPlanID(s *store.Store, planID string) (string, error) {
	rows, err := s.DB.Query(`SELECT k,v FROM meta WHERE k LIKE 'plan:%'`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	newest := int64(-1)
	newestID, newestMode := "", ""
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return "", err
		}
		var token struct {
			PlanID    string `json:"plan_id"`
			CreatedAt int64  `json:"created_at"`
			Mode      string `json:"mode"`
		}
		if json.Unmarshal([]byte(value), &token) != nil {
			continue
		}
		if token.CreatedAt > newest {
			newest = token.CreatedAt
			newestID = token.PlanID
			newestMode = token.Mode
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if newestID == "" {
		return "", fmt.Errorf("%w：库里没有 plan 令牌，先跑 brainx dream --plan", ErrBadPlanID)
	}
	if newestID != planID {
		return "", fmt.Errorf("%w：给的是 %s，最近一次是 %s", ErrBadPlanID, planID, newestID)
	}
	return newestMode, nil
}

func applyTx(s *store.Store, runID, snapshot, planMode string, in Decisions, report *Report, replay bool) error {
	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	nowMillis := time.Now().UnixMilli()
	decisionsJSON, _ := json.Marshal(in.Decisions)
	trigger := "manual"
	if replay {
		trigger = "replay"
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO runs(id,trigger,scope,started_at,input_snapshot_hash,decisions_json,status) VALUES(?,?,?,?,?,?,?)`,
		runID, trigger, "", nowMillis, snapshot, string(decisionsJSON), "running"); err != nil {
		return err
	}

	for seq, d := range in.Decisions {
		line, skipped, err := applyOne(ctx, conn, s, runID, seq+1, d, nowMillis, planMode)
		if err != nil {
			// 整批中止：不留半条。runs 行也随回滚消失，库里没有「做了一半」的痕迹。
			return fmt.Errorf("第 %d 条（%s）失败，整批中止：%w", seq+1, d.Op, err)
		}
		if skipped {
			report.Skipped++
		} else {
			report.Applied++
		}
		report.Lines = append(report.Lines, line)
	}

	if _, err := conn.ExecContext(ctx, `UPDATE runs SET status='done',finished_at=? WHERE id=?`, time.Now().UnixMilli(), runID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// applyOne 执行一条判断。返回 (摘要, 是否跳过, 错误)。
// 跳过 = 这个动作早已完成（重放时才会发生），不是失败。
func applyOne(ctx context.Context, conn *sql.Conn, s *store.Store, runID string, seq int, d Decision, nowMillis int64, planMode string) (string, bool, error) {
	switch d.Op {
	case "keep", "normalize":
		ids := d.IDs
		if len(ids) == 0 && d.ID != "" {
			ids = []string{d.ID}
		}
		if err := mustExist(ctx, conn, ids); err != nil {
			return "", false, err
		}
		if err := writeEvent(ctx, conn, runID, seq, d.Op, strings.Join(ids, ","), "", "", d.Op, "Agent 判断不动"); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("%s：保留 %d 条（记录判断，不改库）", d.Op, len(ids)), false, nil

	case "cluster":
		if len(d.IDs) < 2 {
			return "", false, fmt.Errorf("%w：cluster 需要至少 2 个 id", ErrBadDecision)
		}
		if err := mustExist(ctx, conn, d.IDs); err != nil {
			return "", false, err
		}
		created := 0
		for i := 0; i < len(d.IDs); i++ {
			for j := i + 1; j < len(d.IDs); j++ {
				res, err := conn.ExecContext(ctx,
					`INSERT OR IGNORE INTO edges(id,from_id,to_id,rel,weight,created_at) VALUES(?,?,?,?,?,?)`,
					store.ID(), d.IDs[i], d.IDs[j], "semantic", 1, nowMillis)
				if err != nil {
					return "", false, err
				}
				if n, _ := res.RowsAffected(); n > 0 {
					created++
				}
			}
		}
		if err := writeEvent(ctx, conn, runID, seq, "cluster", strings.Join(d.IDs, ","), "", "", "cluster", "建 semantic 边"); err != nil {
			return "", false, err
		}
		if created == 0 {
			return fmt.Sprintf("cluster：%d 条的 semantic 边已存在（跳过）", len(d.IDs)), true, nil
		}
		return fmt.Sprintf("cluster：%d 条两两建 semantic 边（新增 %d 条）", len(d.IDs), created), false, nil

	case "merge":
		// 原版 M6：derive 模式的清单里不允许 merge——合并是 mutate 的动作
		if planMode == "derive" {
			return "", false, fmt.Errorf("%w：derive 模式的清单里不允许 merge（合并是 mutate 的动作）", ErrBadDecision)
		}
		if len(d.IDs) < 2 {
			return "", false, fmt.Errorf("%w：merge 需要至少 2 个 id", ErrBadDecision)
		}
		keep := d.Keep
		if keep == "" {
			keep = d.IDs[0]
		}
		if !containsID(d.IDs, keep) {
			return "", false, fmt.Errorf("%w：merge 的 keep=%s 不在 ids 里", ErrBadDecision, keep)
		}
		merged, already := 0, 0
		for _, id := range d.IDs {
			if id == keep {
				continue
			}
			asset, err := getAsset(ctx, conn, id)
			if err != nil {
				return "", false, err
			}
			if asset.Immutable {
				return "", false, fmt.Errorf("%w：%s", ErrImmutable, id)
			}
			if asset.Status == "merged" && asset.MergedInto == keep {
				already++
				continue
			}
			if _, err := conn.ExecContext(ctx,
				`UPDATE assets SET status='merged',merged_into=?,archive_reason='merged' WHERE id=?`, keep, id); err != nil {
				return "", false, err
			}
			if _, err := conn.ExecContext(ctx,
				`INSERT OR IGNORE INTO edges(id,from_id,to_id,rel,created_at) VALUES(?,?,?,?,?)`,
				store.ID(), id, keep, "alias_of", nowMillis); err != nil {
				return "", false, err
			}
			if err := writeEvent(ctx, conn, runID, seq, "merge", id, asset.ContentHash, "", "merge", "并入 "+keep); err != nil {
				return "", false, err
			}
			merged++
		}
		if merged == 0 {
			return fmt.Sprintf("merge：%d 条早已并入 %s（跳过）", already, keep), true, nil
		}
		return fmt.Sprintf("merge：%d 条并入 %s", merged, keep), false, nil

	case "archive":
		if len(d.IDs) == 0 {
			return "", false, fmt.Errorf("%w：archive 需要 ids", ErrBadDecision)
		}
		reason := d.Reason
		if reason == "" {
			reason = "duplicate"
		}
		archived, already := 0, 0
		for _, id := range d.IDs {
			asset, err := getAsset(ctx, conn, id)
			if err != nil {
				return "", false, err
			}
			// doc 允许软删（下架≠改写），这是硬约束 10 明确留的口子
			if asset.Status == "archived" {
				already++
				continue
			}
			if _, err := conn.ExecContext(ctx,
				`UPDATE assets SET status='archived',archive_reason=? WHERE id=?`, reason, id); err != nil {
				return "", false, err
			}
			if err := writeEvent(ctx, conn, runID, seq, "archive", id, asset.ContentHash, "", "archive", reason); err != nil {
				return "", false, err
			}
			archived++
		}
		if archived == 0 {
			return fmt.Sprintf("archive：%d 条早已下架（跳过）", already), true, nil
		}
		return fmt.Sprintf("archive：%d 条下架（%s）", archived, reason), false, nil

	case "supersede":
		oldID, newID := d.ID, d.By
		if oldID == "" || newID == "" {
			if len(d.IDs) == 2 {
				oldID, newID = d.IDs[0], d.IDs[1]
			}
		}
		if oldID == "" || newID == "" {
			return "", false, fmt.Errorf("%w：supersede 需要 id 与 by（或被顶替/顶替两条 ids）", ErrBadDecision)
		}
		oldAsset, err := getAsset(ctx, conn, oldID)
		if err != nil {
			return "", false, err
		}
		if _, err := getAsset(ctx, conn, newID); err != nil {
			return "", false, err
		}
		res, err := conn.ExecContext(ctx,
			`INSERT OR IGNORE INTO edges(id,from_id,to_id,rel,created_at) VALUES(?,?,?,?,?)`,
			store.ID(), newID, oldID, "supersedes", nowMillis)
		if err != nil {
			return "", false, err
		}
		created, _ := res.RowsAffected()
		if !oldAsset.Immutable && oldAsset.Status != "archived" {
			if _, err := conn.ExecContext(ctx,
				`UPDATE assets SET status='archived',archive_reason='superseded' WHERE id=?`, oldID); err != nil {
				return "", false, err
			}
		}
		if err := writeEvent(ctx, conn, runID, seq, "supersede", oldID, oldAsset.ContentHash, "", "supersede", "被 "+newID+" 顶替"); err != nil {
			return "", false, err
		}
		if created == 0 {
			return fmt.Sprintf("supersede：%s ← %s（边已存在，跳过）", oldID, newID), true, nil
		}
		return fmt.Sprintf("supersede：%s ← %s", oldID, newID), false, nil

	case "insight", "derive":
		kind := "insight"
		status := "pending"
		if d.Op == "derive" {
			kind, status = "synthesis", "active"
		}
		if strings.TrimSpace(d.Body) == "" {
			return "", false, fmt.Errorf("%w：%s 需要 body（引擎不生成正文）", ErrBadDecision, d.Op)
		}
		distinct := make([]string, 0, len(d.Sources))
		seen := map[string]bool{}
		for _, src := range d.Sources {
			if src = strings.TrimSpace(src); src == "" || seen[src] {
				continue
			}
			seen[src] = true
			distinct = append(distinct, src)
		}
		need := 2
		if d.Op == "derive" {
			need = s.Config.DeriveSynthesisMinDocs
			if need < 2 {
				need = 2
			}
		}
		if len(distinct) < need {
			return "", false, fmt.Errorf("%w：%s 至少需要 %d 个**不同**来源（当前 %d 个）", ErrBadDecision, d.Op, need, len(distinct))
		}
		for _, src := range distinct {
			asset, err := getAsset(ctx, conn, src)
			if err != nil {
				return "", false, err
			}
			if kind == "insight" && asset.Kind != "doc" {
				return "", false, fmt.Errorf("%w：insight 的来源必须是 doc，%s 是 %s", ErrBadDecision, src, asset.Kind)
			}
		}
		// 重放时同样的正文已存在 → 跳过，避免产出重复资产
		var existing string
		if err := conn.QueryRowContext(ctx, `SELECT id FROM assets WHERE content_hash=? LIMIT 1`, store.Hash(d.Body)).Scan(&existing); err == nil {
			return fmt.Sprintf("%s：正文已存在（%s），跳过", d.Op, existing), true, nil
		}
		id := store.ID()
		title := d.Title
		if title == "" {
			title = firstLine(d.Body)
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO assets(id,kind,scope,title,body,importance,confidence,status,pending_reason,origin,archive,content_hash,immutable,layer,created_at,updated_at,use_count,title_source)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,0,?)`,
			id, kind, "global", title, d.Body, 60, 60, status, pendingReason(kind), "derived:"+strings.Join(distinct, ","), "text-only",
			store.Hash(d.Body), 2, nowMillis, nowMillis, "agent"); err != nil {
			return "", false, err
		}
		chunks := index.ChunkText(d.Body, s.Config.ChunkTarget, s.Config.ChunkMax, s.Config.ChunkMin, s.Config.ChunkOverlap)
		for seqNo, chunk := range chunks {
			res, err := conn.ExecContext(ctx,
				`INSERT INTO chunks(id,asset_id,seq,text,start_off,end_off) VALUES(?,?,?,?,?,?)`,
				store.ID(), id, seqNo, chunk.Text, chunk.StartOff, chunk.EndOff)
			if err != nil {
				return "", false, err
			}
			rowID, _ := res.LastInsertId()
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO chunks_fts(rowid,title_ngram,text_ngram,asset_id,seq) VALUES(?,?,?,?,?)`,
				rowID, index.Bigrams(title), index.Bigrams(chunk.Text), id, seqNo); err != nil {
				return "", false, err
			}
		}
		for _, src := range distinct {
			if _, err := conn.ExecContext(ctx,
				`INSERT OR IGNORE INTO edges(id,from_id,to_id,rel,created_at) VALUES(?,?,?,?,?)`,
				store.ID(), id, src, "derived_from", nowMillis); err != nil {
				return "", false, err
			}
		}
		if err := writeEvent(ctx, conn, runID, seq, d.Op, id, "", store.Hash(d.Body), d.Op, "来源 "+strings.Join(distinct, ",")); err != nil {
			return "", false, err
		}
		if kind == "insight" {
			return fmt.Sprintf("insight：新增 %s（pending，等人工 confirm）", id), false, nil
		}
		return fmt.Sprintf("derive：新增 %s（synthesis，active）", id), false, nil
	}
	return "", false, fmt.Errorf("%w：%s", ErrBadOp, d.Op)
}

func pendingReason(kind string) string {
	if kind == "insight" {
		return "insight"
	}
	return ""
}

func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			runes := []rune(line)
			if len(runes) > 40 {
				return string(runes[:40]) + "…"
			}
			return line
		}
	}
	return ""
}

func getAsset(ctx context.Context, conn *sql.Conn, id string) (store.Asset, error) {
	var a store.Asset
	var immutable int
	row := conn.QueryRowContext(ctx,
		`SELECT id,kind,scope,layer,COALESCE(title,''),body,importance,confidence,status,COALESCE(origin,''),archive,content_hash,COALESCE(merged_into,''),immutable,created_at,updated_at,use_count FROM assets WHERE id=?`, id)
	err := row.Scan(&a.ID, &a.Kind, &a.Scope, &a.Layer, &a.Title, &a.Body, &a.Importance, &a.Confidence, &a.Status, &a.Origin, &a.Archive, &a.ContentHash, &a.MergedInto, &immutable, &a.CreatedAt, &a.UpdatedAt, &a.UseCount)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Asset{}, fmt.Errorf("%w：资产不存在 %s", ErrBadDecision, id)
	}
	if err != nil {
		return store.Asset{}, err
	}
	a.Immutable = immutable == 1
	return a, nil
}

func mustExist(ctx context.Context, conn *sql.Conn, ids []string) error {
	for _, id := range ids {
		if _, err := getAsset(ctx, conn, id); err != nil {
			return err
		}
	}
	return nil
}

func writeEvent(ctx context.Context, conn *sql.Conn, runID string, seq int, op, assetID, before, after, decision, reason string) error {
	_, err := conn.ExecContext(ctx,
		`INSERT INTO events(id,run_id,seq,op,asset_id,before_hash,after_hash,decision,reason,at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		store.ID(), runID, seq, op, assetID, nilIfEmpty(before), nilIfEmpty(after), decision, reason, time.Now().UnixMilli())
	return err
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func containsID(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ParseDecisions 解析判断文件。文件与 stdin 共用同一个解析器——
// 否则「文件能跑、管道报错」会浪费一整轮排查。
func ParseDecisions(raw []byte) (Decisions, error) {
	var in Decisions
	if err := json.Unmarshal(raw, &in); err != nil {
		return Decisions{}, fmt.Errorf("%w：JSON 无法解析：%v", ErrBadDecision, err)
	}
	return in, nil
}

// ReadDecisions 支持文件与 stdin（`--apply -`）。
func ReadDecisions(args []string, stdin io.Reader) (Decisions, error) {
	var raw []byte
	var err error
	if len(args) == 1 && args[0] == "-" {
		raw, err = io.ReadAll(io.LimitReader(stdin, 8<<20))
	} else if len(args) == 1 {
		raw, err = os.ReadFile(args[0])
	} else {
		return Decisions{}, errors.New("用法：dream --apply <file|->")
	}
	if err != nil {
		return Decisions{}, err
	}
	return ParseDecisions(raw)
}
