package store

import (
	"os"
	"path/filepath"
	"testing"
)

// 验证：拿不到写锁时 Create 必须失败并落 queue/，不得静默丢弃
func TestCreateEnqueuesOnBusy(t *testing.T) {
	home := t.TempDir()
	s1, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	// 第二个"进程"：独立连接池
	s2, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.DB.Exec("PRAGMA busy_timeout=50")

	ctx := t.Context()
	holder, err := s1.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}

	_, createErr := s2.Create(CreateAsset{Body: "锁冲突时的一条", Kind: "fact", Scope: "global"})
	if createErr == nil {
		t.Fatal("锁被占住时 Create 竟然成功了")
	}
	t.Logf("预期失败：%v", createErr)

	files, _ := filepath.Glob(filepath.Join(home, "queue", "*.json"))
	if len(files) != 1 {
		t.Fatalf("期望 queue/ 有 1 个补偿文件，实际 %d", len(files))
	}
	data, _ := os.ReadFile(files[0])
	t.Logf("补偿文件 %s：%s", filepath.Base(files[0]), string(data))

	holder.ExecContext(ctx, "ROLLBACK")

	// 队列里有东西后，正常写入仍应成功
	if _, err := s2.Create(CreateAsset{Body: "解锁后的一条", Kind: "fact", Scope: "global"}); err != nil {
		t.Fatalf("解锁后仍写不进：%v", err)
	}
}
