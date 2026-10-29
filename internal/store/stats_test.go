package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 召回日志是看板两条曲线与资产榜的唯一数据源。
// 这里守两件事：零命中也记；命中明细能按时间窗统计。
func TestLogRecallFeedsStats(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	hot, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: "部署一律用 docker compose，数据目录固定"})
	if err != nil {
		t.Fatal(err)
	}
	cold, err := s.Create(CreateAsset{Kind: "fact", Scope: "global", Body: "权限模型按最小授权设计，不做超管"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.LogRecall("global", "部署", "lexical", []string{hot.ID}); err != nil {
			t.Fatal(err)
		}
	}
	// 零命中也要留痕：它说明「这个词库里没有」，是缺口的信号
	if err := s.LogRecall("global", "不存在的词", "lexical", nil); err != nil {
		t.Fatal(err)
	}

	stats, err := s.BuildStats()
	if err != nil {
		t.Fatal(err)
	}
	// 口径：查询型召回 = 有查询词的行（含零命中）；注入不算查询
	if stats.RecallCount30 != 4 {
		t.Fatalf("查询型召回应为 4（3 次命中 + 1 次零命中），得到 %d", stats.RecallCount30)
	}
	if stats.ZeroHitRecalls30 != 1 {
		t.Fatalf("零命中应计 1，得到 %d", stats.ZeroHitRecalls30)
	}
	if len(stats.MostUsed) == 0 || stats.MostUsed[0].ID != hot.ID {
		t.Fatalf("最常用榜第一条应是召回三次的那条，得到 %#v", stats.MostUsed)
	}
	if stats.MostUsed[0].Recalls30 != 3 {
		t.Fatalf("30 天召回次数应为 3，得到 %d", stats.MostUsed[0].Recalls30)
	}
	// use_count 是累计值，可以比 30 天窗口大；看板排序只能看窗口内次数
	if hotAsset, err := s.Get(cold.ID); err != nil || hotAsset.UseCount != 0 {
		t.Fatalf("没被召回的那条 use_count 应为 0：%v", err)
	}

	// 注入只落汇总、不落明细：hook 的预算不允许写十几条
	if err := s.LogSource("global", "context", 6); err != nil {
		t.Fatal(err)
	}
	after, err := s.BuildStats()
	if err != nil {
		t.Fatal(err)
	}
	if after.RecallCount30 != 4 {
		t.Fatalf("注入不该计入查询召回数，得到 %d", after.RecallCount30)
	}
	today := after.Days[len(after.Days)-1]
	if today.Recalls != 5 {
		t.Fatalf("当日读次数应含 4 次查询 + 1 次注入，得到 %d", today.Recalls)
	}
}

// 注入的汇总行必须出现在 30 天图里——否则重度使用 hook 的人会看到一条永远为 0 的线。
func TestInjectionCountsAsReadInChart(t *testing.T) {
	s, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.LogSource("global", "context", 4); err != nil {
		t.Fatal(err)
	}
	stats, err := s.BuildStats()
	if err != nil {
		t.Fatal(err)
	}
	today := stats.Days[len(stats.Days)-1]
	if today.Recalls == 0 {
		t.Fatal("今日召回计数应包含 context 注入")
	}
}

// 外部改 config.json 必须留下 op=config 审计，且只记真正的差异。
func TestConfigAuditRecordsExternalChange(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config["context.max_chars"] = 1600
	// 顺手塞一个 CLI 不认识的键：页面必须原样保留，审计不该把它当错误
	config["future.key"] = "keep-me"
	encoded, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var reason string
	if err := reopened.DB.QueryRow(`SELECT reason FROM events WHERE op='config' ORDER BY at DESC LIMIT 1`).Scan(&reason); err != nil {
		t.Fatalf("缺少 op=config 审计：%v", err)
	}
	var changes []ConfigChange
	if err := json.Unmarshal([]byte(reason), &changes); err != nil {
		t.Fatalf("reason 不是差异数组：%v", err)
	}
	found := false
	for _, change := range changes {
		if change.Key == "context.max_chars" && change.New == float64(1600) {
			found = true
		}
	}
	if !found {
		t.Fatalf("差异里没有 context.max_chars：%s", reason)
	}
	if reopened.Config.ContextMaxChars != 1600 {
		t.Fatalf("改动应生效，得到 %d", reopened.Config.ContextMaxChars)
	}
}

// CLI 自己写的改动不该在下次启动被当成外部改动再审计一遍。
func TestConfigSetDoesNotAuditItself(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigSet("context.max_chars", "1500"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.DB.QueryRow(`SELECT COUNT(*) FROM events WHERE op='config'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("config set 不该产生审计事件，得到 %d 条", count)
	}
}

// 越界值要被拉回默认，并且**说出是哪个键**——静默修正是最难查的故障。
func TestOutOfRangeConfigIsReported(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	raw, _ := os.ReadFile(path)
	var config map[string]any
	_ = json.Unmarshal(raw, &config)
	config["recall.max_items"] = 999 // 上限 100
	encoded, _ := json.MarshalIndent(config, "", "  ")
	_ = os.WriteFile(path, encoded, 0o600)
	s.Close()

	reopened, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Config.RecallMaxItems != 8 {
		t.Fatalf("越界值应回退默认 8，得到 %d", reopened.Config.RecallMaxItems)
	}
}

// 看板按钮排的队：能解析的执行一次即删，坏命令当场丢弃。
func TestQueueCommandRoundTrip(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	dir := filepath.Join(home, "queue", "cmd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0001-confirm.json"),
		[]byte(`{"verb":"confirm","ids":["abc"],"at":"now"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0002-bogus.json"), []byte(`{"verb":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	commands, broken, err := s.ReadCommands()
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Verb != "confirm" || commands[0].IDs[0] != "abc" {
		t.Fatalf("只应解析出那条 confirm：%#v", commands)
	}
	if len(broken) != 1 {
		t.Fatalf("未知 verb 应被丢弃，得到 %#v", broken)
	}
	if _, err := os.Stat(filepath.Join(dir, "0002-bogus.json")); !os.IsNotExist(err) {
		t.Fatal("坏命令应被删掉：它永远不可能被执行，留着只会每次启动都报一遍")
	}
	if err := s.RemoveCommand(commands[0]); err != nil {
		t.Fatal(err)
	}
	left, _, _ := s.ReadCommands()
	if len(left) != 0 {
		t.Fatalf("执行后应清空，剩余 %d", len(left))
	}
}

// queue/ 与 queue/cmd/ 必须是两套语义：
// 一条 UI 命令**不能**被补偿队列当成丢写去重放——那会把它改写坏。
func TestDrainQueueIgnoresCommandDir(t *testing.T) {
	home := t.TempDir()
	s, err := Init(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	dir := filepath.Join(home, "queue", "cmd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	commandFile := filepath.Join(dir, "0001-tidy.json")
	if err := os.WriteFile(commandFile, []byte(`{"verb":"tidy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DrainQueue(200); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(commandFile)
	if err != nil {
		t.Fatalf("命令文件被补偿队列吃掉了：%v", err)
	}
	if string(raw) != `{"verb":"tidy"}` {
		t.Fatalf("命令文件被改写：%s", raw)
	}
}
