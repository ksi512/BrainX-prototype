package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// KeySpec 是配置键空间的可读定义。**它是唯一真源**：
// `config set` 的校验、`config schema --json` 的输出、看板页面的渲染，三处都读它。
//
// 为什么必须单一真源：两套定义必然漂移，最后变成「页面能设进 CLI 拒绝的值」。
type KeySpec struct {
	Name    string   `json:"name"`
	Kind    string   `json:"type"` // int | float | bool | string | duration
	Group   string   `json:"group"`
	Default any      `json:"default"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Desc    string   `json:"desc"`
}

func num(v float64) *float64 { return &v }

// KeySpace 是原版提示词 §4.4 的 34 个键，一个不多一个不少。
// 未列出的键一律拒绝——拼错一个键名而不报错，等于配置静默失效。
var KeySpace = []KeySpec{
	{"archive.default", "string", "archive", "local", nil, nil, "新导入文档的原件默认去向：local / text-only / ext:<root>:<path>"},
	{"archive.roots", "string", "archive", "", nil, nil, "外部归档命名根（JSON 对象，键为根名）"},
	{"tidy.capacity", "int", "tidy", 5000, num(0), num(1000000), "active 资产超过这个数才开始尾部淘汰"},
	{"tidy.tail_pct", "int", "tidy", 10, num(1), num(50), "每次淘汰尾部的百分之几"},
	{"tidy.decay_lambda", "float", "tidy", 0.005, num(0), num(1), "时间衰减速度，越大掉得越快"},
	{"tidy.interval", "duration", "tidy", "24h", nil, nil, "两次机械巩固的最小间隔"},
	{"tidy.auto", "bool", "tidy", true, nil, nil, "关闭后 Stop 钩子不再触发机械层"},
	{"derive.synthesis_min_docs", "int", "derive", 3, num(2), num(50), "一簇里至少几篇才值得产出综述"},
	{"derive.alias_edit_distance", "int", "derive", 2, num(0), num(10), "别名归一的编辑距离上限"},
	{"dream.plan_timeout_ms", "int", "dream", 2000, num(100), num(60000), "dream --plan 的时间上限（安全网）"},
	{"recall.max_items", "int", "recall", 8, num(1), num(100), "单次召回条数上限"},
	{"recall.max_chars", "int", "recall", 1200, num(100), num(20000), "单次召回字符上限"},
	{"recall.timeout_ms", "int", "recall", 60, num(10), num(5000), "召回总超时"},
	{"recall.expand_alias", "bool", "recall", true, nil, nil, "检索时把命中资产的 alias_of 邻居一并搜"},
	{"recall.synonyms_file", "string", "recall", "", nil, nil, "同义词表路径（一行一组，= 分隔）"},
	{"recall.single_char_enough", "int", "recall", 20, num(1), num(1000), "单字查询前缀命中的条数门槛"},
	{"chunk.target_chars", "int", "chunk", 400, num(100), num(5000), "分块目标长度（rune），改了必须 rechunk"},
	{"chunk.max_chars", "int", "chunk", 600, num(100), num(10000), "单块硬上限"},
	{"chunk.min_chars", "int", "chunk", 120, num(0), num(2000), "最后一片不足这个长度就并入前一片"},
	{"chunk.overlap_chars", "int", "chunk", 80, num(0), num(2000), "相邻片重叠字数"},
	{"context.max_chars", "int", "context", 2000, num(100), num(12000), "brief 长度"},
	{"context.tidy_hint_min_items", "int", "context", 100, num(1), num(10000), "「待整理」提示的条数门槛"},
	{"context.tidy_hint_min_days", "int", "context", 14, num(1), num(365), "「待整理」提示的天数门槛"},
	{"vector.enabled", "bool", "vector", false, nil, nil, "向量检索总开关（唯一会调模型的开关）"},
	{"vector.endpoint", "string", "vector", "", nil, nil, "embeddings 端点（OpenAI 兼容）"},
	{"vector.model", "string", "vector", "", nil, nil, "embeddings 模型标识，写入 chunks.embed_model"},
	{"index.like_fallback", "bool", "index", true, nil, nil, "FTS 零命中时是否降级跑一次 LIKE 兜底"},
	{"pending.max", "int", "pending", 500, num(1), num(100000), "待确认队列上限，超出按 confidence 升序淘汰最旧的"},
	{"pending.auto_confirm_above", "int", "pending", 0, num(0), num(100), ">0 时 auto-capture/import-low 里 confidence 达标的直接 active（insight 永不适用）"},
	{"hook.session_budget_ms", "int", "hook", 100, num(60), num(2000), "session-start / user-prompt 的时间预算"},
	{"hook.stop_budget_ms", "int", "hook", 400, num(100), num(10000), "stop 的时间预算（含补写与机械层）"},
	{"import.workers", "int", "import", 4, num(1), num(32), "文档解析并发数"},
	{"import.max_file_mb", "int", "import", 200, num(1), num(4096), "单文件体积上限，超出跳过并报错"},
	{"fetch.timeout", "duration", "fetch", "30s", nil, nil, "HTTP 直链归档下载超时"},
	{"fetch.max_bytes", "int", "fetch", 209715200, num(1048576), num(2147483648), "单次下载体积上限"},
}

// Schema 输出机器可读的键空间。
func ConfigSchema() []KeySpec { return KeySpace }

// BasicKeyNames 是看板「配置」区允许出现的旋钮。
//
// 页面不是 CLI：这里只放日常真的会调的键。高级调参键（chunk.*、derive.*、
// recall.expand_alias 等）仍可 config set，但不进页面；死键直接不进 KeySpace。
var BasicKeyNames = []string{
	"archive.default",
	"tidy.capacity",
	"tidy.decay_lambda",
	"tidy.interval",
	"recall.max_items",
	"recall.max_chars",
	"recall.timeout_ms",
	"context.max_chars",
	"context.tidy_hint_min_items",
	"context.tidy_hint_min_days",
	"pending.max",
	"pending.auto_confirm_above",
	"import.workers",
	"import.max_file_mb",
	"fetch.max_bytes",
}

// BasicConfigSchema 返回看板要渲染的键，顺序固定。
func BasicConfigSchema() []KeySpec {
	out := make([]KeySpec, 0, len(BasicKeyNames))
	for _, name := range BasicKeyNames {
		if spec, ok := LookupKey(name); ok {
			out = append(out, spec)
		}
	}
	return out
}

// IsBasicKey 判断某个键是否属于看板基础旋钮。
func IsBasicKey(name string) bool {
	for _, basic := range BasicKeyNames {
		if basic == name {
			return true
		}
	}
	return false
}

// BasicKeyLabels 是看板上的中文名。参数名只留给 CLI 和命令预览，不再当标题。
var BasicKeyLabels = map[string]string{
	"archive.default":             "新导入文档的原件默认去向",
	"tidy.capacity":               "资产总量上限",
	"tidy.decay_lambda":           "记忆衰减速度",
	"tidy.interval":               "自动整理最小间隔",
	"recall.max_items":            "单次召回条数",
	"recall.max_chars":            "单次召回字数",
	"recall.timeout_ms":           "召回超时（毫秒）",
	"context.max_chars":           "会话上下文上限（字）",
	"context.tidy_hint_min_items": "「待整理」条数门槛",
	"context.tidy_hint_min_days":  "「待整理」天数门槛",
	"pending.max":                 "待确认队列上限",
	"pending.auto_confirm_above":  "自动确认分数门槛",
	"import.workers":              "解析并发数",
	"import.max_file_mb":          "单文件上限（MB）",
	"fetch.max_bytes":             "下载上限（MB）",
}

// BasicGroupLabels 是分组的中文名。
var BasicGroupLabels = map[string]string{
	"archive": "归档与原件",
	"tidy":    "整理与衰减",
	"recall":  "召回与注入",
	"context": "上下文与提示",
	"pending": "待确认",
	"import":  "导入",
	"fetch":   "下载",
}

// BasicKeyLabel 返回中文名；未登记时退回参数名。
func BasicKeyLabel(name string) string {
	if label, ok := BasicKeyLabels[name]; ok {
		return label
	}
	return name
}

// BasicGroupLabel 返回分组中文名；未登记时退回原名。
func BasicGroupLabel(group string) string {
	if label, ok := BasicGroupLabels[group]; ok {
		return label
	}
	return group
}

// LookupKey 按键名取定义。
func LookupKey(name string) (KeySpec, bool) {
	for _, spec := range KeySpace {
		if spec.Name == name {
			return spec, true
		}
	}
	return KeySpec{}, false
}

// Snapshot 把配置转成 map，键名与 JSON 标签一致。
func (s *Store) ConfigSnapshot() (map[string]any, error) {
	data, err := json.Marshal(s.Config)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if roots, ok := out["archive.roots"].(map[string]any); ok {
		out["archive.roots"] = roots
	} else if _, ok := out["archive.roots"]; !ok {
		out["archive.roots"] = map[string]any{}
	}
	return out, nil
}

// Get 读一个键的当前值。未登记的键直接报错。
func (s *Store) ConfigGet(name string) (any, error) {
	if _, ok := LookupKey(name); !ok {
		return nil, fmt.Errorf("未知配置键 %s（用 brainx config schema 看全部键）", name)
	}
	if name == "archive.roots" {
		return nil, fmt.Errorf("请用 archive.roots.<name> 指定具体命名根")
	}
	if strings.HasPrefix(name, "archive.roots.") {
		root := strings.TrimPrefix(name, "archive.roots.")
		value, ok := s.Config.Roots[root]
		if !ok {
			return nil, fmt.Errorf("命名根 %s 未配置", root)
		}
		return value, nil
	}
	snapshot, err := s.ConfigSnapshot()
	if err != nil {
		return nil, err
	}
	value, ok := snapshot[name]
	if !ok {
		return nil, fmt.Errorf("未知配置键 %s", name)
	}
	return value, nil
}

// Set 校验并写入一个键。未知键、超范围、类型不符一律拒绝（退出码 2）。
//
// 写回用 JSON 往返而不是字段 switch：加一个键只改 KeySpace 一处，
// 不会出现「结构体加了字段但 set 忘了加分支」这种静默失效。
func (s *Store) ConfigSet(name, raw string) error {
	spec, ok := LookupKey(name)
	if name == "archive.roots" {
		return fmt.Errorf("请用 archive.roots.<name> 指定具体命名根")
	}
	if !ok {
		if strings.HasPrefix(name, "archive.roots.") {
			root := strings.TrimPrefix(name, "archive.roots.")
			if strings.TrimSpace(raw) == "" {
				return fmt.Errorf("archive.roots.%s 不能为空", root)
			}
			s.Config.Roots[root] = raw
			return s.SaveConfig()
		}
		return fmt.Errorf("未知配置键 %s（用 brainx config schema 看全部键）", name)
	}
	snapshot, err := s.ConfigSnapshot()
	if err != nil {
		return err
	}
	parsed, err := parseBySpec(spec, raw)
	if err != nil {
		return err
	}
	snapshot[name] = parsed
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	fresh := DefaultConfig()
	if err := json.Unmarshal(encoded, &fresh); err != nil {
		return fmt.Errorf("配置无法应用：%w", err)
	}
	s.Config = fresh
	if err := s.validateConfig(); err != nil {
		return err
	}
	return s.SaveConfig()
}

func parseBySpec(spec KeySpec, raw string) (any, error) {
	switch spec.Kind {
	case "int":
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s 需要整数，给的是 %q", spec.Name, raw)
		}
		if err := checkRange(spec, float64(n)); err != nil {
			return nil, err
		}
		return n, nil
	case "float":
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, fmt.Errorf("%s 需要小数，给的是 %q", spec.Name, raw)
		}
		if err := checkRange(spec, f); err != nil {
			return nil, err
		}
		return f, nil
	case "bool":
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "1", "yes", "on":
			return true, nil
		case "false", "0", "no", "off":
			return false, nil
		}
		return nil, fmt.Errorf("%s 需要 true/false，给的是 %q", spec.Name, raw)
	case "duration":
		if _, err := time.ParseDuration(strings.TrimSpace(raw)); err != nil {
			return nil, fmt.Errorf("%s 需要时长（如 24h / 30s），给的是 %q", spec.Name, raw)
		}
		return strings.TrimSpace(raw), nil
	default:
		return raw, nil
	}
}

func checkRange(spec KeySpec, value float64) error {
	if spec.Min != nil && value < *spec.Min {
		return fmt.Errorf("%s 最小 %v，给的是 %v", spec.Name, *spec.Min, value)
	}
	if spec.Max != nil && value > *spec.Max {
		return fmt.Errorf("%s 最大 %v，给的是 %v", spec.Name, *spec.Max, value)
	}
	return nil
}

// List 打印全部键、当前值与来源。来源只有两种：默认值 / 配置文件（环境变量覆盖尚未支持）。
func (s *Store) ConfigList() ([][3]string, error) {
	snapshot, err := s.ConfigSnapshot()
	if err != nil {
		return nil, err
	}
	fileValues := map[string]any{}
	if raw, err := os.ReadFile(filepath.Join(s.Home, "config.json")); err == nil {
		_ = json.Unmarshal(raw, &fileValues)
	}
	var rows [][3]string
	for _, spec := range KeySpace {
		value := snapshot[spec.Name]
		source := "默认值"
		if _, ok := fileValues[spec.Name]; ok {
			source = "配置文件"
		}
		rows = append(rows, [3]string{spec.Name, fmt.Sprintf("%v", value), source})
	}
	// 命名根不是固定键，单独列出
	roots := make([]string, 0, len(s.Config.Roots))
	for root := range s.Config.Roots {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	for _, root := range roots {
		rows = append(rows, [3]string{"archive.roots." + root, s.Config.Roots[root], "配置文件"})
	}
	return rows, nil
}

// SaveConfig 原子写回 config.json（先写临时文件再 rename，避免半截文件）。
func (s *Store) SaveConfig() error {
	path := filepath.Join(s.Home, "config.json")
	encoded, err := json.MarshalIndent(s.Config, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	// CLI 自己写的改动不该在下次启动被当成「外部改动」审计一遍。
	// 基线跟着一起推进，只有绕过 CLI 的写入才会触发 op=config。
	return s.setMetaRaw(configAuditKey, string(encoded))
}
