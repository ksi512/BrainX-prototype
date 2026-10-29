// Package dream 实现两段式巩固的第一段：出候选。
//
// 这一层是纯规则：零模型调用、零资产写入。它只回答「哪几条可能是一回事」，
// 不给结论——keep / merge / archive 留给持有模型的宿主 Agent。
package dream

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/index"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

// Candidate 是一组候选。判断单位是「一组」，不是「一条」——
// 单条没有足够上下文，Agent 无法判断。
type Candidate struct {
	Type    string // 可能重复 / 可能同主题
	Reason  string
	Assets  []store.Asset
	Options string

	// 组内「措辞不同但相似」的追加条数。逐条拼字符串会把理由写成复读机，
	// 所以先计数、最后汇总成「另有 N 条…」。
	similarExtra int
}

const (
	MutateOptions = "keep / merge:<id> / archive:<ids> / supersede:<id>"
	DeriveOptions = "keep / cluster / derive / insight / normalize / supersede"

	// 相似度阈值，实测校准过（见 threshold_test.go）：
	//   真同义改写：0.39 / 0.40 / 0.46 / 0.60
	//   明显不同：  0.07 / 0.08
	// 定 0.35 是刻意取宽——**漏报比误报贵**：误报只是让 Agent 多看一眼（它看正文就能否掉），
	// 漏报则意味着两条重复永远躺在库里没人发现。
	//
	// 已知盲区：bigram 无法识别否定。「默认关闭」vs「默认开启」相似度极高但意思相反，
	// 会被列成候选。这不是调参能解决的，也正是两段式存在的理由——引擎只出候选，Agent 出结论。
	similarThreshold = 0.35

	// 太短的正文（<24 字）Jaccard 不可靠：几个 bigram 就占满集合，
	// 「端口是 3333」和「端口是 8080」会被算成高度相似。
	minBodyRunes = 24

	// 单个 kind 内的比较上限，避免 O(n²) 在大库上失控
	maxPairwise = 300
)

// Plan 产出候选。mode: mutate / derive / both
//
// 返回 truncated 表示撞到了 dream.plan_timeout_ms：这是**安全网**，不是机制——
// 正常库不该触发；触发时输出部分结果并明确告知，不能假装完整。
func Plan(s *store.Store, scope, mode string, limit int) ([]Candidate, bool, error) {
	deadline := time.Now().Add(time.Duration(s.Config.DreamPlanTimeoutMs) * time.Millisecond)
	truncated := false
	expired := func() bool {
		if truncated {
			return true
		}
		if time.Now().After(deadline) {
			truncated = true
		}
		return truncated
	}
	assets, err := s.List(scope, "", "active", 5000)
	if err != nil {
		return nil, false, err
	}
	if limit <= 0 {
		limit = 20
	}
	wantMutate := mode == "" || mode == "mutate" || mode == "both"
	wantDerive := mode == "derive" || mode == "both"

	var out []Candidate
	groupOf := map[string]int{} // assetID → out 中的组下标

	if wantMutate {
		// ── A. 正文完全相同（content_hash 一致）──
		byHash := map[string][]store.Asset{}
		var order []string
		for _, a := range assets {
			if _, ok := byHash[a.ContentHash]; !ok {
				order = append(order, a.ContentHash)
			}
			byHash[a.ContentHash] = append(byHash[a.ContentHash], a)
		}
		for _, h := range order {
			g := byHash[h]
			if len(g) < 2 {
				continue
			}
			short := h
			if len(short) > 8 {
				short = short[:8]
			}
			out = append(out, Candidate{
				Type:    "可能重复",
				Reason:  fmt.Sprintf("正文内容完全相同（content_hash %s）", short),
				Assets:  g,
				Options: MutateOptions,
			})
			for _, a := range g {
				groupOf[a.ID] = len(out) - 1
			}
		}

		// ── B. 高度相似（同 kind 内做 bigram Jaccard）──
		//
		// 关键：**不能跳过已分组的资产**。
		// 否则「两条完全相同 + 一条近似」会被切成「一组重复（2 条）」，
		// 而近似那条因为同类邻居已被划走、自己凑不成对，直接消失。
		// 正确做法是允许把近似项**并入已有的组**。
		byKind := map[string][]store.Asset{}
		for _, a := range assets {
			if len([]rune(a.Body)) < minBodyRunes {
				continue
			}
			byKind[a.Kind] = append(byKind[a.Kind], a)
		}
		kinds := make([]string, 0, len(byKind))
		for k := range byKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds) // 固定顺序，保证同一输入两次跑结果一致
		for _, kind := range kinds {
			list := byKind[kind]
			if len(list) > maxPairwise {
				list = list[:maxPairwise]
			}
			sets := make([]map[string]bool, len(list))
			for i, a := range list {
				sets[i] = bigramSet(a.Body)
			}
			for i := 0; i < len(list); i++ {
				if expired() {
					break
				}
				for j := i + 1; j < len(list); j++ {
					if jaccard(sets[i], sets[j]) < similarThreshold {
						continue
					}
					ai, aj := list[i], list[j]
					gi, iIn := groupOf[ai.ID]
					gj, jIn := groupOf[aj.ID]
					switch {
					case iIn && jIn:
						// 同组：已经在一起；不同组：不自动合并两组，交给 Agent
						continue
					case iIn:
						out[gi].Assets = append(out[gi].Assets, aj)
						out[gi].similarExtra++
						groupOf[aj.ID] = gi
					case jIn:
						out[gj].Assets = append(out[gj].Assets, ai)
						out[gj].similarExtra++
						groupOf[ai.ID] = gj
					default:
						out = append(out, Candidate{
							Type:    "可能重复",
							Reason:  fmt.Sprintf("措辞不同但高度相似（同属 %s，bigram 相似度 ≥ %.0f%%）", kind, similarThreshold*100),
							Assets:  []store.Asset{ai, aj},
							Options: MutateOptions,
						})
						groupOf[ai.ID] = len(out) - 1
						groupOf[aj.ID] = len(out) - 1
					}
				}
			}
		}
	}

	// ── C. 同主题文档簇（共享 ≥2 个标签）──
	if wantDerive {
		tags, err := s.TagsByAsset()
		if err != nil {
			return nil, false, err
		}
		tagIDs := map[string][]string{} // tag → asset ids
		byID := map[string]store.Asset{}
		for _, a := range assets {
			for _, t := range tags[a.ID] {
				tagIDs[t] = append(tagIDs[t], a.ID)
				byID[a.ID] = a
			}
		}
		shared := map[string]int{}
		sharedTags := map[string][]string{}
		tagNames := make([]string, 0, len(tagIDs))
		for t := range tagIDs {
			tagNames = append(tagNames, t)
		}
		sort.Strings(tagNames)
		for _, tag := range tagNames {
			ids := tagIDs[tag]
			// 少于 2 个共现就不构成簇；超过 200 个属超高频标签，没有区分度
			if len(ids) < 2 || len(ids) > 200 {
				continue
			}
			sort.Strings(ids)
			for i := 0; i < len(ids); i++ {
				for j := i + 1; j < len(ids); j++ {
					k := ids[i] + "|" + ids[j]
					shared[k]++
					sharedTags[k] = append(sharedTags[k], tag)
				}
			}
		}
		type pair struct {
			k string
			n int
		}
		var pairs []pair
		for k, n := range shared {
			if n >= 2 {
				pairs = append(pairs, pair{k, n})
			}
		}
		sort.Slice(pairs, func(i, j int) bool {
			if pairs[i].n != pairs[j].n {
				return pairs[i].n > pairs[j].n
			}
			return pairs[i].k < pairs[j].k
		})
		// 别名归一：标题编辑距离 ≤ derive.alias_edit_distance 视为同一实体的不同写法。
		// 这类重复的内容相似度未必高（「NAS 部署」vs「飞牛部署」），只能靠标题判。
		maxDistance := s.Config.DeriveAliasEditDistance
		if maxDistance > 0 {
			var docs []store.Asset
			for _, a := range assets {
				if len([]rune(strings.TrimSpace(a.Title))) >= 2 {
					docs = append(docs, a)
				}
			}
			if len(docs) > 300 {
				docs = docs[:300]
			}
			for i := 0; i < len(docs) && !expired(); i++ {
				for j := i + 1; j < len(docs); j++ {
					if docs[i].Kind != docs[j].Kind {
						continue
					}
					distance := editDistance(docs[i].Title, docs[j].Title)
					if distance == 0 || distance > maxDistance {
						continue
					}
					out = append(out, Candidate{
						Type:    "可能同一实体",
						Reason:  fmt.Sprintf("标题近似（编辑距离 %d）：%q ≈ %q", distance, docs[i].Title, docs[j].Title),
						Assets:  []store.Asset{docs[i], docs[j]},
						Options: DeriveOptions,
					})
				}
			}
		}

		for _, p := range pairs {
			parts := strings.Split(p.k, "|")
			a, ok1 := byID[parts[0]]
			b, ok2 := byID[parts[1]]
			if !ok1 || !ok2 {
				continue
			}
			out = append(out, Candidate{
				Type:    "可能同主题",
				Reason:  fmt.Sprintf("共享标签「%s」", strings.Join(sharedTags[p.k], "」「")),
				Assets:  []store.Asset{a, b},
				Options: DeriveOptions,
			})
		}
	}

	for i := range out {
		if out[i].similarExtra > 0 {
			out[i].Reason += fmt.Sprintf("；另有 %d 条措辞不同但高度相似", out[i].similarExtra)
			out[i].similarExtra = 0
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, truncated, nil
}

// ThresholdPercent 给命令行显示用，避免文档/提示里写死一个会漂移的数字。
func ThresholdPercent() int { return int(similarThreshold * 100) }

func bigramSet(text string) map[string]bool {
	set := map[string]bool{}
	for _, token := range strings.Fields(index.Bigrams(text)) {
		set[token] = true
	}
	return set
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// editDistance 是标准 Levenshtein（按 rune，不按字节）。
// 只用来判「标题是不是同一个东西的两种写法」，不需要更聪明的算法。
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	previous := make([]int, len(rb)+1)
	current := make([]int, len(rb)+1)
	for j := 0; j <= len(rb); j++ {
		previous[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		current[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			minimum := previous[j] + 1
			if current[j-1]+1 < minimum {
				minimum = current[j-1] + 1
			}
			if previous[j-1]+cost < minimum {
				minimum = previous[j-1] + cost
			}
			current[j] = minimum
		}
		previous, current = current, previous
	}
	return previous[len(rb)]
}
