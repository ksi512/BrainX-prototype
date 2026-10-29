package recall_test

import (
	"fmt"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/recall"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

// 千级/万级实测：M2 验收要求「万级资产 P95 < 60ms」。
//
// 用 testing.Short() 跳过：跑一次要几十秒造库，不该拖慢日常 go test。
// 需要实测时：go test ./internal/recall/ -run TestRecallP95 -v -timeout 900s
func TestRecallP95(t *testing.T) {
	if raceEnabled {
		t.Skip("go test -race 会插桩放大延迟，P95 断言只在普通 go test 下跑")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows CI 的 SQLite 文件性能不代表目标平台，P95 在 macOS/Linux 测")
	}
	if testing.Short() {
		t.Skip("造万级库很慢，-short 下跳过")
	}
	const total = 10000
	const queries = 200

	s := newStore(t)
	topics := []string{"部署", "向量", "并发", "缓存", "迁移", "索引", "权限", "日志", "回滚", "监控"}
	regions := []string{"数据目录在 /vol2/1000/docker", "默认关闭需要显式开启", "用 compose 而不是裸 docker", "超时时间六十毫秒", "失败要落队列不能静默"}
	start := time.Now()
	for i := 0; i < total; i++ {
		body := fmt.Sprintf("%s 相关经验第 %d 条：%s", topics[i%len(topics)], i, regions[i%len(regions)])
		if _, err := s.Create(store.CreateAsset{
			Kind:       "fact",
			Scope:      "global",
			Body:       body,
			Importance: float64(30 + i%70),
		}); err != nil {
			t.Fatalf("第 %d 条写入失败: %v", i, err)
		}
	}
	build := time.Since(start)
	t.Logf("造库：%d 条资产，耗时 %s", total, build.Round(time.Millisecond))

	var samples []time.Duration
	for i := 0; i < queries; i++ {
		query := topics[i%len(topics)]
		begin := time.Now()
		if _, err := recall.Search(s, query, "", "", s.Config.RecallMaxItems); err != nil {
			t.Fatalf("第 %d 次召回失败: %v", i, err)
		}
		samples = append(samples, time.Since(begin))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 := samples[len(samples)*50/100]
	p95 := samples[len(samples)*95/100]
	p99 := samples[len(samples)*99/100]
	t.Logf("召回延迟（%d 次）：p50=%s p95=%s p99=%s max=%s",
		queries, p50.Round(time.Microsecond), p95.Round(time.Microsecond), p99.Round(time.Microsecond), samples[len(samples)-1].Round(time.Microsecond))

	if p95 > 60*time.Millisecond {
		t.Fatalf("万级 P95 超出 60ms 预算：%s", p95.Round(time.Microsecond))
	}
}
