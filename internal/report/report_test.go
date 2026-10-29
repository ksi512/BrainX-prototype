package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ksi512/BrainX-prototype/internal/store"
)

// M10 的核心验收：**单文件、零外部资源**。
// 引一个 CDN 上的图表库，页面一断网就只剩空白——而看板恰恰是
// 「磁盘/网络出问题时」最需要打开的东西。
func TestReportIsSelfContained(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Body: "部署一律用 docker compose，数据目录固定"}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "report.html")
	path, _, err := Generate(s, out)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)

	for _, bad := range []string{`src="http`, `href="http`, "//cdn.", "@import url("} {
		if strings.Contains(html, bad) {
			t.Fatalf("看板里出现了外部资源 %q：单文件看板必须断网可用", bad)
		}
	}
	// 六区块都在：少一块就是「看板看着像有，其实没有」
	for _, block := range []string{"总资产", "近 30 天", "资产构成", "待办", "资产榜", "配置"} {
		if !strings.Contains(html, block) {
			t.Fatalf("缺少区块 %q", block)
		}
	}
	// 三个资产榜 tab 与两条曲线
	for _, marker := range []string{"series-imports", "series-recalls", "tab-used", "tab-decay", "tab-never"} {
		if !strings.Contains(html, marker) {
			t.Fatalf("缺少 %q", marker)
		}
	}
}

// 配置区只允许出现基础旋钮：高级键仍可 CLI 设置，但不该糊到看板上。
func TestConfigPanelShowsOnlyBasicKeys(t *testing.T) {
	s := newStore(t)
	out := filepath.Join(t.TempDir(), "report.html")
	path, _, err := Generate(s, out)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	start := strings.Index(html, `<script id="payload"`)
	if start < 0 {
		t.Fatal("缺少 payload")
	}
	start += len(`<script id="payload" type="application/json">`)
	end := strings.Index(html[start:], `</script>`)
	payload := html[start : start+end]

	var parsed struct {
		Keys []struct {
			Name     string  `json:"name"`
			Label    string  `json:"label"`
			Control  string  `json:"control"`
			Scale    float64 `json:"scale"`
			Value    any     `json:"value"`
			Readonly bool    `json:"readonly"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("payload 不是合法 JSON：%v", err)
	}
	if len(parsed.Keys) != len(store.BasicConfigSchema()) {
		t.Fatalf("页面拿到 %d 个键，基础 schema 有 %d 个", len(parsed.Keys), len(store.BasicConfigSchema()))
	}
	seen := map[string]bool{}
	for _, key := range parsed.Keys {
		seen[key.Name] = true
		if key.Readonly {
			t.Fatalf("%s 不该以只读行出现，基础面板只放可调旋钮", key.Name)
		}
	}
	for _, name := range []string{"archive.default", "tidy.capacity", "recall.max_chars", "context.max_chars", "pending.max", "import.workers", "fetch.max_bytes"} {
		if !seen[name] {
			t.Fatalf("基础键 %s 不在看板里", name)
		}
	}
	for _, name := range []string{"chunk.target_chars", "derive.synthesis_min_docs", "tidy.auto", "index.like_fallback", "fetch.timeout", "recall.expand_alias", "vector.enabled", "hook.session_budget_ms", "dream.auto", "fetch.cache_dir"} {
		if seen[name] {
			t.Fatalf("高级/死键 %s 不应出现在看板里", name)
		}
	}
	for _, key := range parsed.Keys {
		switch key.Name {
		case "fetch.max_bytes":
			if key.Label != "下载上限（MB）" || key.Scale != 1048576 || key.Value != float64(200) {
				t.Fatalf("fetch.max_bytes 应按 MB 展示：%+v", key)
			}
		case "archive.default":
			if key.Control != "archive-default" {
				t.Fatalf("archive.default 应是三档选择：%+v", key)
			}
		case "tidy.capacity":
			if key.Label == key.Name || key.Label == "" {
				t.Fatalf("基础旋钮必须用中文名：%+v", key)
			}
		}
	}
}

// 没有数据时也必须能生成：一个空库打不开看板，等于「刚装好就报错」。
func TestReportOnEmptyStore(t *testing.T) {
	s := newStore(t)
	out := filepath.Join(t.TempDir(), "report.html")
	path, stats, err := Generate(s, out)
	if err != nil {
		t.Fatalf("空库应能生成看板：%v", err)
	}
	if stats.Total != 0 {
		t.Fatalf("空库统计应为 0，得到 %d", stats.Total)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) < 2000 {
		t.Fatalf("生成物过小或读取失败：%v", err)
	}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
