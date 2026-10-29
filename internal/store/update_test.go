package store

import (
	"testing"
	"time"
)

// ftsHits 直接打 chunks_fts，绕开 recall 包（避免测试依赖被测包的调用方）。
func ftsHits(t *testing.T, s *Store, token string) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, token).Scan(&n); err != nil {
		t.Fatalf("FTS 查询失败（token=%q）: %v", token, err)
	}
	return n
}

// M2 验收的核心断言：update 后**新词命中、旧词查不到**。
// 这是 contentless FTS 最容易出错的地方——旧行没删干净，索引里就留下幽灵。
func TestUpdateReindexesFTS(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	created, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: "数据库迁移一律走旧词流程"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ftsHits(t, s, "旧词"); got == 0 {
		t.Fatal("初始写入后旧词应当命中")
	}

	updated, err := s.Update(created.ID, UpdateInput{Body: "数据库迁移一律走新词流程"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ftsHits(t, s, "新词"); got == 0 {
		t.Fatal("update 后新词必须命中")
	}
	if got := ftsHits(t, s, "旧词"); got != 0 {
		t.Fatalf("update 后旧词必须查不到，实际命中 %d 条（contentless FTS 的旧行没删干净）", got)
	}
	// 未改动部分仍要能命中：不能把整条索引删了重建错的。
	// 注意必须用 2 字词——索引里是 bigram token，「数据库」这种 3 字词不是 token。
	if got := ftsHits(t, s, "数据"); got == 0 {
		t.Fatal("未改动的词也应当继续命中")
	}
	if updated.ContentHash == created.ContentHash {
		t.Fatal("正文变了 content_hash 必须跟着变")
	}
	if updated.UpdatedAt <= created.UpdatedAt {
		t.Fatal("正文变了 updated_at 必须前进（增量注入的水位线）")
	}
	if updated.Title != "数据库迁移一律走新词流程" {
		t.Fatalf("自动落回的标题应跟随正文，得到 %q", updated.Title)
	}
}

// 硬约束 10：doc 的正文永不被改写。
func TestUpdateRejectsImmutableDoc(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	doc, err := s.Create(CreateAsset{Kind: "doc", Scope: "global", Body: "原始文档正文不可改"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(doc.ID, UpdateInput{Body: "改一下"}); err == nil {
		t.Fatal("immutable=1 的 doc 必须拒绝改写")
	}
	after, err := s.Get(doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != "原始文档正文不可改" {
		t.Fatal("拒绝之后正文必须原样保留")
	}
}

// 只改标题不动 updated_at，否则水位线每轮都变、brief 重复注入。
func TestUpdateTitleDoesNotTouchWatermark(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	created, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: "端口是 3333", Title: "端口"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	updated, err := s.Update(created.ID, UpdateInput{Title: "容器端口"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "容器端口" {
		t.Fatalf("标题应被改写，得到 %q", updated.Title)
	}
	if updated.UpdatedAt != created.UpdatedAt {
		t.Fatal("只改标题不得动 updated_at（硬约束 14）")
	}
}

// 空操作的 update 不应写 events，避免审计里出现噪声。
func TestUpdateNoopWritesNothing(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	created, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: "同一句话"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(created.ID, UpdateInput{Body: "同一句话"}); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE asset_id=? AND op='update'`, created.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("无变化的 update 不应写事件，实际 %d 条", events)
	}
}
