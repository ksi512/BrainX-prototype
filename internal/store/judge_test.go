package store

import (
	"strconv"
	"strings"
	"testing"
)

// 写入质量三档：过短进 pending、完全重复被拒、insight 一律 pending。
func TestJudgeThreeTiers(t *testing.T) {
	s := tempStore(t)

	short, err := s.Judge("端口是 3333", "fact")
	if err != nil {
		t.Fatal(err)
	}
	if short.Status != "pending" {
		t.Fatalf("过短应进 pending，得到 %s（%d 分：%v）", short.Status, short.Score, short.Reason)
	}

	long := "部署一律用 docker compose，数据目录在 /vol2/1000/docker，不要用裸 docker run"
	ok, err := s.Judge(long, "fact")
	if err != nil {
		t.Fatal(err)
	}
	if ok.Status != "active" {
		t.Fatalf("足够长的记忆应直接 active，得到 %s（%v）", ok.Status, ok.Reason)
	}
	if _, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: long}); err != nil {
		t.Fatal(err)
	}
	dup, err := s.Judge(long, "fact")
	if err != nil {
		t.Fatal(err)
	}
	if dup.Status != "reject" {
		t.Fatalf("与既有资产完全相同应被拒，得到 %s（%d 分）", dup.Status, dup.Score)
	}

	insight, err := s.Judge("这是一条化合结论，正文足够长，但 insight 必须先人工确认", "insight")
	if err != nil {
		t.Fatal(err)
	}
	if insight.Status != "pending" {
		t.Fatalf("insight 一律落 pending，得到 %s", insight.Status)
	}
}

// confirm --reject 之后，同一内容不再被捕获（抑制记录）。
func TestRejectSuppressesSameContent(t *testing.T) {
	s := tempStore(t)
	body := "这条记忆会被明确拒绝，之后同样的内容不该再被自动捕获"
	asset, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: body, Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reject(asset.ID, "rejected"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get(asset.ID)
	if after.Status != "archived" {
		t.Fatalf("拒绝应归档，得到 %s", after.Status)
	}
	verdict, err := s.Judge(body, "fact")
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Status != "reject" {
		t.Fatalf("被拒过的内容必须再次被拒，得到 %s（%v）", verdict.Status, verdict.Reason)
	}
}

func TestConfirmOnlyForPending(t *testing.T) {
	s := tempStore(t)
	asset, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: "一条已经 active 的记忆，不需要确认"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Confirm(asset.ID); err == nil {
		t.Fatal("非 pending 资产不应被 confirm")
	}
}

// 待整理提示：条数达标就出现；跑过 --plan 后必须消失（第二水位线生效）。
func TestTidyHintAppearsThenClearsAfterPlan(t *testing.T) {
	s := tempStore(t)
	s.Config.TidyHintMinItems = 2
	s.Config.TidyHintMinDays = 3650
	for i := 0; i < 3; i++ {
		if _, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: "用于测试待整理提示的记忆内容，足够长以避免扣分"}); err != nil {
			t.Fatal(err)
		}
	}
	line, ok, err := s.TidyHint()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("条数达标时应当出现提示")
	}
	if !strings.Contains(line, "待整理") || !strings.Contains(line, "dream --plan") {
		t.Fatalf("提示行格式不对：%q", line)
	}

	// 模拟 --plan 跑过
	if err := s.SetMeta("dream:last_plan_at", strconv.FormatInt(now(), 10)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.TidyHint(); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("跑过 --plan 之后提示必须消失（否则永远清不掉）")
	}
}
