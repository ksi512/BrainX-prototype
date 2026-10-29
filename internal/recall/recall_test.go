package recall_test

import (
	"strings"
	"testing"

	"github.com/ksi512/BrainX-prototype/internal/recall"
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

func mustCreate(t *testing.T, s *store.Store, in store.CreateAsset) store.Asset {
	t.Helper()
	a, err := s.Create(in)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func ids(results []recall.Result) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Asset.ID)
	}
	return out
}

func sourcesOf(results []recall.Result, id string) []string {
	for _, r := range results {
		if r.Asset.ID == id {
			return r.Sources
		}
	}
	return nil
}

// 图谱路的核心断言：正文里没有同一个词，但连过边的内容必须被带出来。
// 这是「相关内容」与「关键词匹配」的分界线。
func TestGraphChannelPullsLinkedAsset(t *testing.T) {
	s := newStore(t)
	deploy := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "部署一律用 docker compose，数据目录在 /vol2/1000/docker"})
	nginx := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "反向代理统一走 nginx，证书放在 /etc/certs"})
	unrelated := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "周末读完了两本小说"})
	if err := s.Link(deploy.ID, nginx.ID, "cites"); err != nil {
		t.Fatal(err)
	}

	results, err := recall.Search(s, "部署", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := ids(results)
	if len(got) == 0 || got[0] != deploy.ID {
		t.Fatalf("词法命中的那条应排第一，得到 %v", got)
	}
	if !contains(got, nginx.ID) {
		t.Fatalf("1 跳邻居必须被图谱路带出来，得到 %v", got)
	}
	if src := sourcesOf(results, nginx.ID); !contains(src, "graph") {
		t.Fatalf("邻居的来源应标注 graph，得到 %v", src)
	}
	if contains(got, unrelated.ID) {
		t.Fatalf("无关内容不得进入结果，得到 %v", got)
	}
}

// contradicts 边不是「相关内容」，不能当支持证据带进主列表；它只出现在冲突提示里。
func TestContradictionEdgeIsNotSupport(t *testing.T) {
	s := newStore(t)
	a := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "向量检索默认关闭，需要显式开"})
	b := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "向量检索默认开启，装好就能用"})
	if err := s.Link(a.ID, b.ID, "contradicts"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO reviews(id,asset_a,asset_b,conflict_on,detail,created_at) VALUES(?,?,?,?,?,?)`,
		store.ID(), a.ID, b.ID, "annotate", "两条都仍是 active，未裁决", 1); err != nil {
		t.Fatal(err)
	}

	results, err := recall.Search(s, "向量检索", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Asset.ID == b.ID && contains(r.Sources, "graph") {
			t.Fatal("contradicts 边不得把冲突方当作 graph 路的相关内容带进来")
		}
	}

	out, err := recall.Recall(s, "向量检索", "", "", 10, 4000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "⚠ 与 [") {
		t.Fatalf("未裁决冲突必须在召回尾部提示，实际输出：\n%s", out)
	}
	if strings.Count(out, "向量检索默认开启") != 1 {
		t.Fatalf("冲突配对项只应出现一次（不加分、不重复计分），实际输出：\n%s", out)
	}
}

// merged / archived 永远不能出现在召回里，否则同一内容会出现两遍。
func TestMergedAndArchivedExcluded(t *testing.T) {
	s := newStore(t)
	live := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "并发写要用 BEGIN IMMEDIATE"})
	merged := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "并发写要用 BEGIN IMMEDIATE 并且加重试"})
	archived := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "并发写旧方案是加全局锁"})
	if _, err := s.DB.Exec(`UPDATE assets SET status='merged', merged_into=? WHERE id=?`, live.ID, merged.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE assets SET status='archived' WHERE id=?`, archived.ID); err != nil {
		t.Fatal(err)
	}
	results, err := recall.Search(s, "并发", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := ids(results)
	if contains(got, merged.ID) {
		t.Fatal("merged 资产不能出现在召回里（同一内容会出现两遍）")
	}
	if contains(got, archived.ID) {
		t.Fatal("archived 资产不能出现在召回里")
	}
	if !contains(got, live.ID) {
		t.Fatalf("active 的那条应当命中，得到 %v", got)
	}
}

// 时序路：查询带时间线索时才放宽候选集，把「最近/重要」的东西带出来。
func TestTemporalChannelNeedsTimeCue(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "部署一律用 docker compose"})
	hot := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "今天排查了 NAS 磁盘坏道", Importance: 95})

	plain, err := recall.Search(s, "部署", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if contains(ids(plain), hot.ID) {
		t.Fatal("无时间线索时，时序路不应把无关的高重要度资产塞进结果")
	}

	cued, err := recall.Search(s, "最近有什么新情况", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ids(cued), hot.ID) {
		t.Fatalf("带「最近」线索时，时序路应把高重要度资产带出来，得到 %v", ids(cued))
	}
}

// 读语义：项目 scope 必须能看到 global，但看不到别的项目。
func TestProjectScopeSeesGlobalOnly(t *testing.T) {
	s := newStore(t)
	global := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "跨项目偏好：回答用简体中文"})
	mine := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "proj:mine", Body: "本项目回答用简体中文并附依据"})
	other := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "proj:other", Body: "别的项目回答用简体中文但别管我"})

	results, err := recall.Search(s, "简体中文", "proj:mine", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := ids(results)
	if !contains(got, global.ID) {
		t.Fatal("项目 scope 必须能看到 global")
	}
	if !contains(got, mine.ID) {
		t.Fatal("项目 scope 必须能看到自己")
	}
	if contains(got, other.ID) {
		t.Fatal("项目 scope 不得看到别的项目")
	}
}

// 预算裁剪：max_chars 必须真的截断，否则 hook 注入会超宿主上限。
func TestRecallBudgetRespected(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 12; i++ {
		mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: strings.Repeat("预算测试正文内容", 6)})
	}
	out, err := recall.Recall(s, "预算测试", "", "", 12, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(out)) > 200+40 {
		t.Fatalf("输出超出预算过多：%d 字符", len([]rune(out)))
	}
}

// 召回强化只动 importance / use_count / last_used_at，绝不碰 updated_at（硬约束 14）。
func TestStrengthenDoesNotTouchUpdatedAt(t *testing.T) {
	s := newStore(t)
	a := mustCreate(t, s, store.CreateAsset{Kind: "fact", Scope: "global", Body: "强化的边界在哪里"})

	if _, err := s.DB.Exec(`UPDATE assets SET status='merged' WHERE id='nope'`); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(a.ID)
	if _, err := recall.Recall(s, "强化的边界", "", "", 5, 2000); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get(a.ID)
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatal("召回强化碰了 updated_at：水位线会每轮都变，导致重复注入")
	}
	if after.UseCount != before.UseCount+1 {
		t.Fatalf("use_count 应 +1，得到 %d → %d", before.UseCount, after.UseCount)
	}
	if after.Importance <= before.Importance {
		t.Fatal("importance 应被强化")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
