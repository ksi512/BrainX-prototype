package wiki_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/store"
	"github.com/ksi512/BrainX-prototype/internal/wiki"
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

func TestBuildProjectsActiveOnly(t *testing.T) {
	s := newStore(t)
	doc, err := s.Create(store.CreateAsset{Kind: "doc", Scope: "global", Title: "部署手册", Body: "部署使用 docker compose，数据目录在 /vol2/1000/docker。", Tags: []string{"部署"}})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Title: "过期记忆", Body: "这条会被归档，不应出现在投影里", Tags: []string{"部署"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE assets SET status='archived' WHERE id=?`, gone.ID); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	summary, err := wiki.Build(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pages == 0 || summary.Written == 0 {
		t.Fatalf("应当生成页面，得到 %+v", summary)
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "这是投影") {
		t.Fatal("每页头部必须写「这是投影」（否则以后分不清改了哪边）")
	}
	if !strings.Contains(string(index), doc.ID) {
		t.Fatal("active 资产必须进导航")
	}
	if strings.Contains(string(index), gone.ID) {
		t.Fatal("archived 资产不得进导航")
	}
	if _, err := os.Stat(filepath.Join(dir, "doc", doc.ID+".md")); err != nil {
		t.Fatalf("doc 应有单篇投影页：%v", err)
	}
}

// 幂等：内容没变就不重写，否则每次 build 都刷 mtime，同步工具会误判「全变了」。
func TestBuildIsIdempotent(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Title: "端口", Body: "服务端口是 3333，改动前先确认没有别的进程占用", Tags: []string{"运维"}}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := wiki.Build(s, dir); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, "index.md")
	first, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	second, err := wiki.Build(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 {
		t.Fatalf("第二次构建不应重写任何页面，实际写了 %d 个", second.Written)
	}
	after, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(first.ModTime()) {
		t.Fatal("内容未变时 mtime 必须保持不变")
	}
}

// 标签消失后，旧的簇页面要被清理，否则导航里全是幽灵页。
func TestBuildPrunesStalePages(t *testing.T) {
	s := newStore(t)
	asset, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Title: "临时", Body: "这条记忆的标签稍后会被删掉，用于测试清理逻辑", Tags: []string{"临时标签"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := wiki.Build(s, dir); err != nil {
		t.Fatal(err)
	}
	topic := filepath.Join(dir, "topic", "临时标签.md")
	if _, err := os.Stat(topic); err != nil {
		t.Fatalf("簇页面应已生成：%v", err)
	}
	if _, err := s.DB.Exec(`DELETE FROM asset_tags WHERE asset_id=?`, asset.ID); err != nil {
		t.Fatal(err)
	}
	summary, err := wiki.Build(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(topic); !os.IsNotExist(err) {
		t.Fatal("标签删掉后旧簇页面必须被清理")
	}
	if summary.Removed == 0 {
		t.Fatal("清理数量应当 > 0")
	}
}
