package store

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// 硬约束 11：库内引用只存相对路径。这里既测断言，也测转换。
func TestToRelRejectsAbsolute(t *testing.T) {
	s := tempStore(t)
	for _, bad := range []string{"/tmp/x.md", "~/x.md", "C:/x.md", `C:\x.md`} {
		if _, err := s.ToRel(bad); err == nil {
			t.Fatalf("绝对路径 %q 应当被拒", bad)
		}
	}
	rel, err := s.ToRel(filepath.Join(s.Home, "files", "2026", "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != "files/2026/a.md" {
		t.Fatalf("应转成相对路径，得到 %q", rel)
	}
	if _, err := s.ToAbs(rel); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ToAbs("/etc/passwd"); err == nil {
		t.Fatal("ToAbs 也必须拒绝绝对路径")
	}
}

// 导入 local 档：原件真的落进 files/，archive 记的是相对路径。
func TestImportFileArchivesLocally(t *testing.T) {
	s := tempStore(t)
	source := filepath.Join(t.TempDir(), "论文.md")
	body := "# 论文\n\n这是一篇用于测试导入与归档的文档，正文足够长。"
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	asset, err := s.ImportFile(CreateAsset{Kind: "doc", Scope: "global", Title: "论文", Body: body}, source, "local")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(asset.Archive, "files/") {
		t.Fatalf("local 档的 archive 应是 files/… 相对路径，得到 %q", asset.Archive)
	}
	abs, err := s.ToAbs(asset.Archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("原件没有真的落盘：%v", err)
	}
	if asset.ArchiveHash == "" {
		t.Fatal("应当记录 archive_hash")
	}
	// origin 也必须是相对引用
	if got := asset.Origin; got != "file:"+asset.Archive {
		t.Fatalf("origin 应指向库内相对路径，得到 %q", got)
	}
}

// doctor 的绝对路径扫描是硬约束 11 的唯一守门人。
func TestDoctorFindsAbsolutePaths(t *testing.T) {
	s := tempStore(t)
	asset, err := s.Create(CreateAsset{Kind: "doc", Scope: "global", Body: "手工塞进来的文档正文内容"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE assets SET archive='/Users/someone/paper.pdf', origin='file:/Users/someone/paper.pdf' WHERE id=?`, asset.ID); err != nil {
		t.Fatal(err)
	}
	checks, err := Doctor(s)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range checks {
		if c.Name == "absolute_paths" {
			found = true
			if c.Status != "fail" {
				t.Fatalf("绝对路径必须判 fail，得到 %s（%s）", c.Status, c.Detail)
			}
		}
	}
	if !found {
		t.Fatal("doctor 缺少 absolute_paths 检查项")
	}
	if !HasFailure(checks) {
		t.Fatal("有 fail 项时 HasFailure 必须为 true")
	}
}

// 空库也必须七项全过，否则「装完就能用」不成立。
func TestDoctorCleanOnFreshStore(t *testing.T) {
	s := tempStore(t)
	if _, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: "一条正常的中文记忆内容，用来占位"}); err != nil {
		t.Fatal(err)
	}
	checks, err := Doctor(s)
	if err != nil {
		t.Fatal(err)
	}
	if HasFailure(checks) {
		for _, c := range checks {
			if c.Status == "fail" {
				t.Fatalf("干净库不该失败：%s %s", c.Name, c.Detail)
			}
		}
	}
	if len(checks) < 7 {
		t.Fatalf("体检项应≥7，实际 %d", len(checks))
	}
}

func TestFetchURLAllowsExactLimit(t *testing.T) {
	s := tempStore(t)
	s.Config.FetchMaxBytes = 4
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("1234"))
	}))
	defer server.Close()

	path, err := s.FetchURL(server.URL+"/exact.txt", "")
	if err != nil {
		t.Fatalf("正好等于上限应允许：%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1234" {
		t.Fatalf("下载内容不对：%q", data)
	}
}

func TestFetchURLRejectsOverLimit(t *testing.T) {
	s := tempStore(t)
	s.Config.FetchMaxBytes = 4
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("12345"))
	}))
	defer server.Close()

	if _, err := s.FetchURL(server.URL+"/over.txt", ""); !errors.Is(err, ErrDownload) {
		t.Fatalf("超过上限应报 ErrDownload，实际：%v", err)
	}
}
