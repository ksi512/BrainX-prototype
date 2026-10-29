package store_test

import (
	"testing"

	"github.com/ksi512/BrainX-prototype/internal/recall"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

func TestDocumentIsImmutableAndSearchableByTwoCharacterTerm(t *testing.T) {
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	asset, err := s.Create(store.CreateAsset{Kind: "doc", Scope: "global", Title: "向量说明", Body: "BrainX 使用向量检索作为可选能力。", Archive: "text-only"})
	if err != nil {
		t.Fatal(err)
	}
	if !asset.Immutable || asset.Layer != 0 {
		t.Fatalf("document contract broken: %#v", asset)
	}
	results, err := recall.Search(s, "向量", "global", "doc", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Asset.ID != asset.ID {
		t.Fatalf("two-character search missed document: %#v", results)
	}
}

func TestCreateRequiresExplicitScope(t *testing.T) {
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Create(store.CreateAsset{Body: "not allowed"}); err == nil {
		t.Fatal("expected missing scope to fail")
	}
}

func TestLayersFollowKind(t *testing.T) {
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cases := []struct {
		kind  string
		layer int
	}{{"doc", 0}, {"fact", 1}, {"synthesis", 2}, {"insight", 2}, {"profile", 3}}
	for _, tc := range cases {
		asset, err := s.Create(store.CreateAsset{Kind: tc.kind, Scope: "global", Body: "分层测试 " + tc.kind})
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if asset.Layer != tc.layer {
			t.Fatalf("%s: layer = %d, want %d", tc.kind, asset.Layer, tc.layer)
		}
	}
}

func TestLinkRequiresExistingAssets(t *testing.T) {
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Body: "第一条足够长的记忆内容用于关系测试"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Body: "第二条足够长的记忆内容用于关系测试"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Link(a.ID, "missing", "cites"); err == nil {
		t.Fatal("link 指向不存在的资产应该报错")
	}
	if err := s.Link(a.ID, b.ID, "cites"); err != nil {
		t.Fatalf("合法 link 失败: %v", err)
	}
}

func TestIDUnique(t *testing.T) {
	seen := make(map[string]bool, 20000)
	for i := 0; i < 20000; i++ {
		id := store.ID()
		if len(id) != 26 {
			t.Fatalf("id 长度应为 26：%q", id)
		}
		if seen[id] {
			t.Fatalf("重复 id：%s", id)
		}
		seen[id] = true
	}
}
