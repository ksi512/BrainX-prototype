package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeRelativePathAndIDs(t *testing.T) {
	if !SafeRelativePath("files/2026/abc.md") || !SafeRelativePath("ext-root/a/b") {
		t.Fatal("合法相对路径被拒绝")
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/../../x", "/abs/x", "C:/x"} {
		if SafeRelativePath(bad) {
			t.Fatalf("危险路径被放行：%q", bad)
		}
	}
	if ValidAssetID("deadbeef") || !ValidAssetID(strings.Repeat("a", 26)) {
		t.Fatal("资产 id 校验不符合 26 位 hex")
	}
	if ValidContentHash("deadbeef") || !ValidContentHash(strings.Repeat("b", 64)) {
		t.Fatal("content_hash 校验不符合 64 位 hex")
	}
}

func TestValidateBundleRejectsUnsafeAsset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE assets(id TEXT,kind TEXT,scope TEXT,archive TEXT,content_hash TEXT)`,
		`CREATE TABLE chunks(asset_id TEXT)`,
		`CREATE TABLE edges(from_id TEXT,to_id TEXT,rel TEXT)`,
		`CREATE TABLE reviews(asset_a TEXT,asset_b TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	id := strings.Repeat("a", 26)
	hash := strings.Repeat("b", 64)
	if _, err := db.Exec(`INSERT INTO assets VALUES(?,?,?,?,?)`, id, "fact", "global", "text-only", hash); err != nil {
		t.Fatal(err)
	}
	if err := validateBundleDB(path); err != nil {
		t.Fatalf("合法库被拒绝：%v", err)
	}
	if _, err := db.Exec(`UPDATE assets SET id='../../evil'`); err != nil {
		t.Fatal(err)
	}
	if err := validateBundleDB(path); err == nil {
		t.Fatal("危险 asset id 应被拒绝")
	}
}
