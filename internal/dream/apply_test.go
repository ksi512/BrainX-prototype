package dream_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/dream"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func create(t *testing.T, s *store.Store, body string, kind string) store.Asset {
	t.Helper()
	if kind == "" {
		kind = "fact"
	}
	a, err := s.Create(store.CreateAsset{Kind: kind, Scope: "global", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// issuePlan 模拟 --plan 写下的令牌：apply 必须校验「用的是最近一次清单」。
func issuePlan(t *testing.T, s *store.Store, id string, at int64) {
	t.Helper()
	token, _ := json.Marshal(map[string]any{"plan_id": id, "created_at": at, "mode": "mutate", "scope": "all"})
	if err := s.SetMeta("plan:mutate:all", string(token)); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, s *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询失败 %q: %v", query, err)
	}
	return n
}

func TestApplyMergeArchiveSupersede(t *testing.T) {
	s := newStore(t)
	a := create(t, s, "部署一律用 docker compose，数据目录在 /vol2/1000/docker", "")
	b := create(t, s, "docker 部署请用 compose，数据都放 /vol2/1000/docker", "")
	c := create(t, s, "日志轮转每天凌晨执行一次，保留七天", "")
	d := create(t, s, "日志由 logrotate 每日轮转，保留 7 天", "")
	issuePlan(t, s, "plan-1", time.Now().UnixMilli())

	report, err := dream.Apply(s, dream.Decisions{
		PlanID: "plan-1",
		Decisions: []dream.Decision{
			{Op: "merge", IDs: []string{a.ID, b.ID}, Keep: a.ID},
			{Op: "supersede", ID: c.ID, By: d.ID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 2 {
		t.Fatalf("应执行 2 条，得到 %d", report.Applied)
	}
	merged, _ := s.Get(b.ID)
	if merged.Status != "merged" || merged.MergedInto != a.ID {
		t.Fatalf("merge 后应为 merged 且指向 keep，得到 %s/%s", merged.Status, merged.MergedInto)
	}
	old, _ := s.Get(c.ID)
	if old.Status != "archived" {
		t.Fatalf("被顶替的应当下架，得到 %s", old.Status)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM edges WHERE rel='supersedes' AND from_id=? AND to_id=?`, d.ID, c.ID); n != 1 {
		t.Fatal("supersede 必须建 supersedes 边")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM runs WHERE id=? AND status='done'`, report.RunID); n != 1 {
		t.Fatal("runs 必须留下 done 记录")
	}
}

// 清单是快照，执行前库里可能已经变了。拿旧 plan_id 执行等于用过期判断改现在的库。
func TestApplyRejectsStalePlanID(t *testing.T) {
	s := newStore(t)
	a := create(t, s, "一条会被误改的记忆内容，长度足够避免质量扣分", "")
	b := create(t, s, "另一条记忆内容，同样足够长，用于测试合并", "")
	issuePlan(t, s, "plan-new", time.Now().UnixMilli())

	before := countRows(t, s, `SELECT COUNT(*) FROM assets WHERE status='active'`)
	if _, err := dream.Apply(s, dream.Decisions{
		PlanID:    "plan-old",
		Decisions: []dream.Decision{{Op: "merge", IDs: []string{a.ID, b.ID}, Keep: a.ID}},
	}); err == nil {
		t.Fatal("过期 plan_id 必须被拒")
	}
	if after := countRows(t, s, `SELECT COUNT(*) FROM assets WHERE status='active'`); after != before {
		t.Fatal("被拒时不得有任何写入")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM runs`); n != 0 {
		t.Fatal("被拒时不得留下 runs 记录")
	}
}

func TestApplyRejectsUnknownOp(t *testing.T) {
	s := newStore(t)
	a := create(t, s, "内容足够长的记忆，用于测试未知动作被拒绝", "")
	issuePlan(t, s, "plan-1", time.Now().UnixMilli())
	if _, err := dream.Apply(s, dream.Decisions{
		PlanID:    "plan-1",
		Decisions: []dream.Decision{{Op: "rewrite-everything", IDs: []string{a.ID}}},
	}); err == nil {
		t.Fatal("未知 op 必须被拒")
	}
}

// 硬约束 10 + M4 CI 断言：任何 dream 跑完，所有 doc 的 content_hash 不变。
func TestApplyNeverRewritesDoc(t *testing.T) {
	s := newStore(t)
	doc := create(t, s, "原始文档正文，任何巩固都不得改写它", "doc")
	other := create(t, s, "另一条普通记忆，长度足够，用来做合并目标", "")
	issuePlan(t, s, "plan-1", time.Now().UnixMilli())

	if _, err := dream.Apply(s, dream.Decisions{
		PlanID:    "plan-1",
		Decisions: []dream.Decision{{Op: "merge", IDs: []string{doc.ID, other.ID}, Keep: other.ID}},
	}); err == nil {
		t.Fatal("把 doc 当作被合并方必须被拒（immutable=1）")
	}
	after, _ := s.Get(doc.ID)
	if after.ContentHash != doc.ContentHash || after.Body != doc.Body {
		t.Fatal("doc 的正文与 hash 必须原样保留")
	}
	if after.Status != "active" {
		t.Fatalf("被拒之后 doc 状态也不该变，得到 %s", after.Status)
	}
}

// 整批原子：第 2 条非法时，第 1 条也不能落地。
func TestApplyAbortsWholeBatch(t *testing.T) {
	s := newStore(t)
	a := create(t, s, "第一批要合并的记忆，长度足够通过质量判断", "")
	b := create(t, s, "第二批要合并的记忆，长度同样足够", "")
	issuePlan(t, s, "plan-1", time.Now().UnixMilli())

	if _, err := dream.Apply(s, dream.Decisions{
		PlanID: "plan-1",
		Decisions: []dream.Decision{
			{Op: "archive", IDs: []string{a.ID}},
			{Op: "merge", IDs: []string{b.ID}, Keep: "不存在的id"},
		},
	}); err == nil {
		t.Fatal("第二条非法时整批应当失败")
	}
	after, _ := s.Get(a.ID)
	if after.Status != "active" {
		t.Fatalf("整批中止后第一条不能落地，实际状态 %s", after.Status)
	}
}

// insight 必须：先落 pending、必须有 ≥2 个 doc 来源、必须建 derived_from 边。
func TestApplyInsightLandsPendingWithSources(t *testing.T) {
	s := newStore(t)
	d1 := create(t, s, "第一篇原始文档，关于缓存策略的取舍与实测结论", "doc")
	d2 := create(t, s, "第二篇原始文档，关于缓存失效时间的实测数据", "doc")
	issuePlan(t, s, "plan-1", time.Now().UnixMilli())

	report, err := dream.Apply(s, dream.Decisions{
		PlanID: "plan-1",
		Decisions: []dream.Decision{{
			Op:      "insight",
			Title:   "缓存策略的化合结论",
			Body:    "两篇实测数据表明：本地单机场景下缓存失效时间应短于 5 分钟。",
			Sources: []string{d1.ID, d2.ID},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != 1 {
		t.Fatalf("应执行 1 条，得到 %d", report.Applied)
	}
	var id, status, kind string
	if err := s.DB.QueryRow(`SELECT id,status,kind FROM assets WHERE kind='insight' ORDER BY rowid DESC LIMIT 1`).Scan(&id, &status, &kind); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("insight 必须落 pending 等人工确认，得到 %s", status)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM edges WHERE from_id=? AND rel='derived_from'`, id); n != 2 {
		t.Fatalf("必须留 2 条 derived_from 边，得到 %d", n)
	}

	// 来源不足 2 个 doc 时必须拒绝
	one := create(t, s, "只有一篇文档不足以形成化合结论，这条用于测试拒绝", "doc")
	issuePlan(t, s, "plan-2", time.Now().UnixMilli()+1)
	if _, err := dream.Apply(s, dream.Decisions{
		PlanID: "plan-2",
		Decisions: []dream.Decision{{
			Op: "insight", Body: "只有一份原料的叫读后感", Sources: []string{one.ID, one.ID},
		}},
	}); err == nil {
		t.Fatal("insight 的来源必须 ≥2 条 doc")
	}
}

// 机械层：同 content_hash 完全相同的自动合并；interval 闸门让 Stop hook 不会每轮全表扫。
func TestMechanicalDedupesAndGates(t *testing.T) {
	s := newStore(t)
	first := create(t, s, "完全相同的两句话会被机械层合并成一条", "")
	second := create(t, s, "完全相同的两句话会被机械层合并成一条", "")
	// Create 不做去重，所以这里两条都在
	if n := countRows(t, s, `SELECT COUNT(*) FROM assets WHERE status='active'`); n != 2 {
		t.Fatalf("写入阶段不应自动合并，得到 %d", n)
	}

	out, err := dream.Mechanical(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("force 时应当产出摘要")
	}
	// 同毫秒创建时 created_at 相等，谁被保留由 id 决定——所以只断言「一活一合」，
	// 不断言具体是哪一条被保留（那是实现细节，不是契约）。
	stateA, _ := s.Get(first.ID)
	stateB, _ := s.Get(second.ID)
	active, merged := stateA, stateB
	if stateB.Status == "active" {
		active, merged = stateB, stateA
	}
	if active.Status != "active" || merged.Status != "merged" || merged.MergedInto != active.ID {
		t.Fatalf("应一活一合，得到 %s / %s→%s", active.Status, merged.Status, merged.MergedInto)
	}

	// 第二次调用（未到期、未 force）应当直接返回空串
	again, err := dream.Mechanical(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if again != "" {
		t.Fatalf("未到 interval 时应当静默跳过，得到 %q", again)
	}
}
