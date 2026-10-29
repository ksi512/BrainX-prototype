package recall

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/index"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

// Result 是一条召回结果。Sources 记录它从哪几路进来——
// 没有这个字段，出问题时分不清「词法命中」和「图谱带出来」，
// 只能靠猜。（提示词 §5.1 第 7 条要求每行标注来源。）
type Result struct {
	Asset   store.Asset
	Snippet string
	Score   float64
	Sources []string
}

const (
	// rrfK 是 RRF 的平滑常数，取 60 是原论文（Cormack 2009）的经验值：
	// 它压住「某一路排第一」的独占优势，让三路都有发言权。
	rrfK = 60.0
	// graphHops：沿 edges 扩 1–2 跳。3 跳开始几乎任何两条资产都能连上，
	// 召回会退化成「整个库」。
	graphHops = 2
	// graphSeedCount：图谱路的种子只取词法前 K 条。
	// 种子取太多，扩出来的邻居会淹没真正的命中。
	graphSeedCount = 5
	// snippetRunes：片段长度上限。片段是给人/Agent 快速判断用的，
	// 不是正文。
	snippetRunes = 300
)

// ───────────────────────── 通道 ─────────────────────────

type chanHit struct {
	id      string
	snippet string
}

// buildQuery 做三件事（提示词 §3.6）：
//   - 单字查询改用前缀匹配 `向*`：索引里是 bigram，单字本身不是 token，
//     不前缀匹配就永远查不到（而且是静默 0 结果）
//   - 同义词表（recall.synonyms_file）里命中任一词时，把同组的其它写法一起丢进去
//   - 普通查询照旧切 bigram
func buildQuery(s *store.Store, query string) string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return ""
	}
	tokens := index.Query(trimmed)
	if tokens == "" {
		return ""
	}
	if len([]rune(trimmed)) == 1 && !strings.Contains(tokens, " ") {
		return tokens + "*"
	}
	extra := synonyms(s, trimmed)
	if len(extra) == 0 {
		return tokens
	}
	parts := []string{tokens}
	for _, word := range extra {
		if expanded := index.Query(word); expanded != "" {
			parts = append(parts, expanded)
		}
	}
	return strings.Join(parts, " ")
}

// synonyms 读同义词表：一行一组，`=` 分隔。文件不存在不影响启动。
func synonyms(s *store.Store, query string) []string {
	path := s.Config.RecallSynonymsFile
	if path == "" {
		path = filepath.Join(s.Home, "dict", "synonyms.txt")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words := strings.Split(line, "=")
		hit := false
		for _, word := range words {
			if strings.TrimSpace(word) == query {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		for _, word := range words {
			if word = strings.TrimSpace(word); word != "" && word != query {
				out = append(out, word)
			}
		}
	}
	return out
}

// lexical：FTS5 BM25。chunk 级而不是 asset 级，长文档也能命中具体的段落。
func lexical(s *store.Store, ctx context.Context, query, scope, kind string, want int) ([]chanHit, error) {
	tokens := buildQuery(s, query)
	if strings.TrimSpace(tokens) == "" {
		return nil, nil
	}
	clause, args := store.ScopeClause("a.scope", scope)
	statement := `SELECT a.id,c.text FROM chunks_fts JOIN chunks c ON c.rowid=chunks_fts.rowid JOIN assets a ON a.id=c.asset_id WHERE chunks_fts MATCH ? AND a.status='active'`
	params := []any{tokens}
	if clause != "" {
		statement += " AND " + clause
		params = append(params, args...)
	}
	if clause, kindArgs := store.KindClause("a.kind", kind); clause != "" {
		statement += " AND " + clause
		params = append(params, kindArgs...)
	}
	statement += " ORDER BY bm25(chunks_fts,5.0,1.0) ASC LIMIT ?"
	params = append(params, want*3)

	rows, err := s.DB.QueryContext(ctx, statement, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var hits []chanHit
	for rows.Next() {
		var id, snippet string
		if err := rows.Scan(&id, &snippet); err != nil {
			return nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		hits = append(hits, chanHit{id: id, snippet: trimSnippet(snippet)})
		if len(hits) >= want {
			break
		}
	}
	// 单字查询的命中通常很稀，补一次 LIKE 兜底（提示词 §3.6）
	if err := rows.Err(); err != nil {
		return hits, err
	}
	if len(hits) < s.Config.RecallSingleCharEnough && len([]rune(strings.TrimSpace(query))) == 1 && s.Config.LikeFallback {
		fallback, _ := likeSearch(s, query, scope, kind, want)
		seen := map[string]bool{}
		for _, hit := range hits {
			seen[hit.id] = true
		}
		for _, result := range fallback {
			if seen[result.Asset.ID] || len(hits) >= want {
				continue
			}
			hits = append(hits, chanHit{id: result.Asset.ID, snippet: result.Snippet})
		}
	}
	return hits, nil
}

// graph：从词法命中的前几条出发沿 edges 扩 1–2 跳。
// 这是「相关内容」的来源：正文里没有同一个词，但 Agent 或用户显式连过边。
func graph(s *store.Store, seeds []string, scope, kind string, want int) ([]chanHit, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	hops, err := s.ExpandEdges(seeds, graphHops)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(hops))
	for id, hop := range hops {
		if hop == 0 {
			continue // 种子本身由词法路负责，不在这里重复计分
		}
		ids = append(ids, id)
	}
	assets, err := s.GetMany(ids)
	if err != nil {
		return nil, err
	}
	wanted := kindSet(kind)
	type ranked struct {
		asset store.Asset
		hop   int
	}
	var list []ranked
	for id, a := range assets {
		if a.Status != "active" || a.MergedInto != "" {
			continue
		}
		if !visible(a.Scope, scope) {
			continue
		}
		if !kindOK(wanted, a.Kind) {
			continue
		}
		list = append(list, ranked{asset: a, hop: hops[id]})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].hop != list[j].hop {
			return list[i].hop < list[j].hop
		}
		if list[i].asset.Importance != list[j].asset.Importance {
			return list[i].asset.Importance > list[j].asset.Importance
		}
		return list[i].asset.ID < list[j].asset.ID
	})
	if len(list) > want {
		list = list[:want]
	}
	out := make([]chanHit, 0, len(list))
	for _, item := range list {
		out = append(out, chanHit{id: item.asset.ID, snippet: trimSnippet(item.asset.Body)})
	}
	return out, nil
}

// temporal：importance × 时间衰减 × (1+ln(1+use_count))。
//
// 候选集的取法分两种，这是刻意的：
//   - 查询里带时间线索（「最近」「上周」…）→ 取时间窗内的资产。这时用户的意图本来就是
//     「最近发生过什么」，词法命中反而是次要的。
//   - 不带时间线索 → 只在词法候选里重排序。否则这条公式不依赖 query，
//     每���查询都会返回同一个「重要资产榜」，把真正的命中挤出预算。
func temporal(s *store.Store, query, scope, kind string, lexicalIDs []string, want int) ([]chanHit, error) {
	lambda := s.Config.DecayLambda
	if lambda <= 0 {
		lambda = 0.005
	}
	var assets []store.Asset
	if days, ok := timeWindow(query); ok {
		since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		list, err := s.ListSince(scope, kind, since, 2000)
		if err != nil {
			return nil, err
		}
		assets = list
	} else {
		fetched, err := s.GetMany(lexicalIDs)
		if err != nil {
			return nil, err
		}
		for _, a := range fetched {
			if a.Status != "active" || a.MergedInto != "" || a.Layer == 0 {
				continue
			}
			if !visible(a.Scope, scope) {
				continue
			}
			assets = append(assets, a)
		}
	}
	nowMillis := float64(time.Now().UnixMilli())
	type scored struct {
		asset store.Asset
		score float64
	}
	list := make([]scored, 0, len(assets))
	for _, a := range assets {
		if a.Status != "active" || a.MergedInto != "" {
			continue
		}
		ageDays := (nowMillis - float64(a.UpdatedAt)) / 86400000.0
		if ageDays < 0 {
			ageDays = 0
		}
		score := a.Importance * math.Exp(-lambda*ageDays) * (1 + math.Log(1+float64(a.UseCount)))
		list = append(list, scored{asset: a, score: score})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].asset.ID < list[j].asset.ID
	})
	if len(list) > want {
		list = list[:want]
	}
	out := make([]chanHit, 0, len(list))
	for _, item := range list {
		out = append(out, chanHit{id: item.asset.ID, snippet: trimSnippet(item.asset.Body)})
	}
	return out, nil
}

// vector 是第四路：只在 vector.enabled 且配好端点时参与。
//
// 失败一律**静默降级**（返回空、不报错）：向量是补丁不是主路径，
// 端点挂了不该让整个召回失败——正文与 FTS 还在。
func vector(s *store.Store, ctx context.Context, query, scope, kind string, want int) ([]chanHit, error) {
	embedder := s.Embedder()
	if !embedder.Enabled() {
		return nil, nil
	}
	vectors, err := embedder.Embed(ctx, []string{query})
	if err != nil || len(vectors) == 0 {
		return nil, nil
	}
	queryVector := vectors[0]
	// 按当前模型过滤：旧模型的向量不参与计算（混用不报错，只是结果静默变差）
	chunks, err := s.LoadEmbeddings(scope, kind, embedder.ModelTag(len(queryVector)))
	if err != nil {
		return nil, nil
	}
	type scored struct {
		assetID string
		text    string
		score   float64
	}
	list := make([]scored, 0, len(chunks))
	for _, chunk := range chunks {
		list = append(list, scored{assetID: chunk.AssetID, text: chunk.Text, score: index.Cosine(queryVector, chunk.Vector)})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].assetID < list[j].assetID
	})
	seen := map[string]bool{}
	var hits []chanHit
	for _, item := range list {
		if seen[item.assetID] {
			continue
		}
		seen[item.assetID] = true
		hits = append(hits, chanHit{id: item.assetID, snippet: trimSnippet(item.text)})
		if len(hits) >= want {
			break
		}
	}
	return hits, nil
}

// ───────────────────────── 融合 ─────────────────────────

type fused struct {
	id      string
	snippet string
	score   float64
	ranks   map[string]int
}

// Fuse 执行 RRF：score(d) = Σ 1/(60 + rank_r(d))。
//
// 三路顺序执行而不是并发：连接池是单连接（SetMaxOpenConns(1)），
// 并发只会变成排队，还会让同一输入的结果顺序不稳定。并发在这里是伪优化。
func fuse(s *store.Store, ctx context.Context, query, scope, kind string, want int) ([]fused, error) {
	lexHits, err := lexical(s, ctx, query, scope, kind, want)
	if err != nil && ctx.Err() == nil {
		return nil, err
	}
	seeds := make([]string, 0, graphSeedCount)
	lexIDs := make([]string, 0, len(lexHits))
	for i, hit := range lexHits {
		lexIDs = append(lexIDs, hit.id)
		if i < graphSeedCount {
			seeds = append(seeds, hit.id)
		}
	}

	// alias 展开：同一实体的别名写法可能完全不含查询词（「NAS」vs「飞牛」），
	// 不展开就会漏。只扩一跳，且权重靠后（rank 排在词法命中之后）。
	if s.Config.RecallExpandAlias && len(lexHits) > 0 {
		aliasIDs := []string{}
		for _, hit := range lexHits {
			if len(aliasIDs) >= graphSeedCount {
				break
			}
			aliasIDs = append(aliasIDs, hit.id)
		}
		if neighbors, err := s.ExpandEdges(aliasIDs, 1); err == nil {
			for id := range neighbors {
				if id == "" {
					continue
				}
				occupied := false
				for _, hit := range lexHits {
					if hit.id == id {
						occupied = true
						break
					}
				}
				if !occupied {
					lexHits = append(lexHits, chanHit{id: id})
				}
			}
		}
	}

	entries := map[string]*fused{}
	order := make([]string, 0, len(lexHits))
	for rank, hit := range lexHits {
		e := entries[hit.id]
		if e == nil {
			e = &fused{id: hit.id, snippet: hit.snippet, ranks: map[string]int{}}
			entries[hit.id] = e
			order = append(order, hit.id)
		}
		e.ranks["lexical"] = rank + 1
		e.score += 1.0 / (rrfK + float64(rank+1))
	}

	if ctx.Err() == nil {
		graphHits, err := graph(s, seeds, scope, kind, want)
		if err != nil {
			return nil, err
		}
		for rank, hit := range graphHits {
			e := entries[hit.id]
			if e == nil {
				e = &fused{id: hit.id, snippet: hit.snippet, ranks: map[string]int{}}
				entries[hit.id] = e
				order = append(order, hit.id)
			}
			e.ranks["graph"] = rank + 1
			e.score += 1.0 / (rrfK + float64(rank+1))
		}
	}

	if ctx.Err() == nil {
		vectorHits, err := vector(s, ctx, query, scope, kind, want)
		if err != nil {
			return nil, err
		}
		for rank, hit := range vectorHits {
			e := entries[hit.id]
			if e == nil {
				e = &fused{id: hit.id, snippet: hit.snippet, ranks: map[string]int{}}
				entries[hit.id] = e
				order = append(order, hit.id)
			}
			e.ranks["vector"] = rank + 1
			e.score += 1.0 / (rrfK + float64(rank+1))
		}
	}

	if ctx.Err() == nil {
		tempHits, err := temporal(s, query, scope, kind, lexIDs, want)
		if err != nil {
			return nil, err
		}
		for rank, hit := range tempHits {
			e := entries[hit.id]
			if e == nil {
				e = &fused{id: hit.id, snippet: hit.snippet, ranks: map[string]int{}}
				entries[hit.id] = e
				order = append(order, hit.id)
			}
			e.ranks["temporal"] = rank + 1
			e.score += 1.0 / (rrfK + float64(rank+1))
		}
	}

	out := make([]fused, 0, len(order))
	for _, id := range order {
		out = append(out, *entries[id])
	}
	return out, nil
}

func collect(s *store.Store, fusedList []fused, scope, kind string, limit int) ([]Result, error) {
	ids := make([]string, 0, len(fusedList))
	for _, item := range fusedList {
		ids = append(ids, item.id)
	}
	assets, err := s.GetMany(ids)
	if err != nil {
		return nil, err
	}
	wanted := kindSet(kind)
	results := make([]Result, 0, len(fusedList))
	for _, item := range fusedList {
		a, ok := assets[item.id]
		if !ok {
			continue
		}
		// 双保险：候选必须排除非 active（merged 尤其要排，否则同一内容出现两遍）
		if a.Status != "active" || a.MergedInto != "" {
			continue
		}
		if !visible(a.Scope, scope) {
			continue
		}
		if !kindOK(wanted, a.Kind) {
			continue
		}
		sources := make([]string, 0, len(item.ranks))
		for _, name := range []string{"lexical", "graph", "vector", "temporal"} {
			if _, ok := item.ranks[name]; ok {
				sources = append(sources, name)
			}
		}
		results = append(results, Result{Asset: a, Snippet: item.snippet, Score: item.score, Sources: sources})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].Asset.Importance != results[j].Asset.Importance {
			return results[i].Asset.Importance > results[j].Asset.Importance
		}
		return results[i].Asset.UpdatedAt > results[j].Asset.UpdatedAt
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// Search 是纯检索入口（`brainx search`）：融合三路、按 RRF 排序，不做字符预算。
func Search(s *store.Store, query, scope, kind string, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = s.Config.RecallMaxItems
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config.RecallTimeoutMs)*time.Millisecond)
	defer cancel()
	fusedList, err := fuse(s, ctx, query, scope, kind, limit)
	if err != nil {
		if s.Config.LikeFallback {
			return likeSearch(s, query, scope, kind, limit)
		}
		return nil, err
	}
	results, err := collect(s, fusedList, scope, kind, limit)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 && s.Config.LikeFallback {
		return likeSearch(s, query, scope, kind, limit)
	}
	return results, nil
}

// Recall 组装成 Markdown 片段（`brainx recall` 与 hook 之外的主动召回）。
func Recall(s *store.Store, query, scope, kind string, limit, maxChars int) (string, error) {
	if maxChars <= 0 {
		maxChars = s.Config.RecallMaxChars
	}
	results, err := Search(s, query, scope, kind, limit)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "（没有找到相关记忆）", nil
	}

	ids := make([]string, 0, len(results))
	var lines []string
	used := 0
	for _, result := range results {
		line := fmt.Sprintf("- %s\n  [id=%s; kind=%s; conf=%.0f; used=%d; from=%s]",
			strings.TrimSpace(result.Snippet), result.Asset.ID, result.Asset.Kind,
			result.Asset.Confidence, result.Asset.UseCount, strings.Join(result.Sources, "+"))
		if used+len([]rune(line)) > maxChars {
			break
		}
		lines = append(lines, line)
		used += len([]rune(line))
		ids = append(ids, result.Asset.ID)
	}
	if err := s.Strengthen(context.Background(), ids); err != nil {
		return "", err
	}
	// 召回日志：看板的「近 30 天召回次数」与资产榜都读它。
	// 落日志失败**不**让召回失败——它是遥测，不是召回的一部分；
	// 但也不能静默：打到 stderr，否则看板上的曲线悄悄变成假数据。
	sourceSet := map[string]bool{}
	for _, result := range results {
		for _, source := range result.Sources {
			sourceSet[source] = true
		}
	}
	sourceList := make([]string, 0, len(sourceSet))
	for name := range sourceSet {
		sourceList = append(sourceList, name)
	}
	sort.Strings(sourceList)
	if err := s.LogRecall(scope, query, strings.Join(sourceList, "+"), ids); err != nil {
		fmt.Fprintf(os.Stderr, "brainx: 召回日志写入失败（不影响结果）：%v\n", err)
	}

	// 冲突配对：追加到尾部，**不参与排序、不强化**——否则冲突双方互相抬分，形成自激。
	conflictLines, err := conflictAppendix(s, ids, &used, maxChars)
	if err != nil {
		return "", err
	}
	lines = append(lines, conflictLines...)
	if len(lines) == 0 {
		return "（没有找到相关记忆）", nil
	}
	return strings.Join(lines, "\n"), nil
}

// conflictAppendix 处理「冲突配对」：最多列 2 对，超出的折成一行计数。
//
// 两条规则：
//   - 配对项**不参与排序、不强化**，否则冲突双方互相抬分，形成自激。
//   - 配对项如果已经在主列表里，就只补一行 ⚠ 提示、不再输出一遍正文——
//     正文重复会让同一内容出现两次，还把字符预算吃掉一半。
func conflictAppendix(s *store.Store, ids []string, used *int, maxChars int) ([]string, error) {
	conflicts, err := s.OpenConflicts(ids)
	if err != nil || len(conflicts) == 0 {
		return nil, err
	}
	hit := map[string]bool{}
	for _, id := range ids {
		hit[id] = true
	}
	type pair struct{ self, other, detail string }
	var pairs []pair
	seen := map[string]bool{}
	for _, c := range conflicts {
		var self, other string
		switch {
		case hit[c.A]:
			self, other = c.A, c.B
		case hit[c.B]:
			self, other = c.B, c.A
		default:
			continue
		}
		if seen[self+"|"+other] {
			continue
		}
		seen[self+"|"+other] = true
		detail := "两条都仍是 active，未裁决"
		if strings.TrimSpace(c.Detail) != "" {
			detail = c.Detail
		}
		pairs = append(pairs, pair{self: self, other: other, detail: detail})
	}
	if len(pairs) == 0 {
		return nil, nil
	}

	var lines []string
	shown := 0
	for _, p := range pairs {
		if shown >= 2 {
			break
		}
		entry := []string{fmt.Sprintf("⚠ 与 [%s] 存在冲突：%s", shortID(p.other), p.detail)}
		if !hit[p.other] {
			asset, err := s.Get(p.other)
			if err != nil {
				continue
			}
			entry = append([]string{
				fmt.Sprintf("- %s", trimSnippet(asset.Body)),
				fmt.Sprintf("  [id=%s; kind=%s; conf=%.0f; used=%d; from=conflict]", asset.ID, asset.Kind, asset.Confidence, asset.UseCount),
			}, entry...)
		}
		size := 0
		for _, line := range entry {
			size += len([]rune(line)) + 1
		}
		if *used+size > maxChars {
			break
		}
		lines = append(lines, entry...)
		*used += size
		shown++
	}
	if rest := len(pairs) - shown; rest > 0 {
		line := fmt.Sprintf("⚠ 另有 %d 对冲突，见 brainx confirm --pending", rest)
		if *used+len([]rune(line)) <= maxChars {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// ───────────────────────── context ─────────────────────────

// Context assembles the session brief for a scope.
func Context(s *store.Store, scope string) (string, error) {
	return ContextBrief(s, scope, 0, true)
}

// ContextSince assembles the brief, optionally limited to assets updated after
// since. A project scope also reads global assets. Documents (layer 0) stay out
// of the brief: it is a summary, not a document dump.
//
// context 一律不强化（§5.1 第 6 条）：它每轮都跑，强化会让 brief 里那几条
// 富者愈富，最后库里只看得到最早几条。
func ContextSince(s *store.Store, scope string, since int64) (string, error) {
	return ContextBrief(s, scope, since, true)
}

// ContextBrief 是真正的拼装入口。hints 决定要不要带 brief 最前那一行「待整理」。
//
// 为什么单独一个开关：SessionStart 带、UserPromptSubmit 不带。
// 增量注入每轮都带同一行提示，等于每轮烧一次 token 提醒同一件事（§5.4 频控）。
func ContextBrief(s *store.Store, scope string, since int64, hints bool) (string, error) {
	query := `SELECT id,kind,scope,layer,COALESCE(title,''),body,importance,confidence,status,COALESCE(origin,''),archive,content_hash,COALESCE(merged_into,''),immutable,created_at,updated_at,use_count FROM assets WHERE status='active' AND layer>0`
	var args []any
	clause, clauseArgs := store.ScopeClause("scope", scope)
	if clause != "" {
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if since > 0 {
		query += ` AND updated_at > ?`
		args = append(args, since)
	}
	query += ` ORDER BY layer DESC, importance DESC, updated_at DESC LIMIT 40`
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	used := 0
	for rows.Next() {
		var a store.Asset
		var immutable int
		if err := rows.Scan(&a.ID, &a.Kind, &a.Scope, &a.Layer, &a.Title, &a.Body, &a.Importance, &a.Confidence, &a.Status, &a.Origin, &a.Archive, &a.ContentHash, &a.MergedInto, &immutable, &a.CreatedAt, &a.UpdatedAt, &a.UseCount); err != nil {
			return "", err
		}
		a.Immutable = immutable == 1
		// 标题与正文不重复：标题为空时回落正文首句由 store 写入保证，
		// 两者相同时只输出一次，否则 brief 的字符预算被同一句话吃掉两遍。
		body := strings.TrimSpace(a.Body)
		text := body
		title := strings.TrimSpace(a.Title)
		// 自动回落的标题是正文前 40 字 + "…"，直接比较前缀会判成「不同」，
		// 于是 brief 里出现「标题：正文」的复读。去掉省略号再比。
		if title != "" && !strings.HasPrefix(body, strings.TrimSuffix(title, "…")) {
			text = title + "：" + body
		}
		if len([]rune(text)) > 240 {
			text = string([]rune(text)[:240]) + "…"
		}
		line := fmt.Sprintf("- [%s] %s", a.Kind, text)
		if used+len([]rune(line)) > s.Config.ContextMaxChars {
			break
		}
		lines = append(lines, line)
		used += len([]rune(line))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	// 注入计数：只落一行汇总，不写命中明细——hook 的预算是 100ms，
	// 十几条明细的逐行插入会把预算吃穿，而看板只需要「今天注入过几次」。
	if len(lines) > 0 {
		kind := "context"
		if since > 0 {
			kind = "context-incremental"
		}
		_ = s.LogSource(scope, kind, len(lines))
	}

	hint := ""
	if hints {
		if line, ok, err := s.TidyHint(); err != nil {
			return "", err
		} else if ok {
			hint = line + "\n"
		}
	}
	if len(lines) == 0 && hint == "" {
		return "", nil
	}
	body := "## 已知上下文\n" + strings.Join(lines, "\n")
	if len(lines) == 0 {
		body = ""
	}
	return hint + body, nil
}

// ───────────────────────── 工具 ─────────────────────────

// likeSearch 是 FTS 不可用时的兜底。它没有排序能力，只保证「不炸」。
func likeSearch(s *store.Store, query, scope, kind string, limit int) ([]Result, error) {
	deadline := time.Now().Add(300 * time.Millisecond)
	assets, err := s.List(scope, kind, "active", 10000)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(query)
	var results []Result
	for _, asset := range assets {
		if time.Now().After(deadline) {
			break
		}
		if strings.Contains(strings.ToLower(asset.Title+"\n"+asset.Body), needle) {
			results = append(results, Result{Asset: asset, Snippet: trimSnippet(asset.Body), Sources: []string{"like"}})
			if len(results) == limit {
				break
			}
		}
	}
	return results, nil
}

// kindSet 解析 `--kind fact,skill`。
func kindSet(kind string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(kind, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out[part] = true
		}
	}
	return out
}

func kindOK(wanted map[string]bool, kind string) bool {
	return len(wanted) == 0 || wanted[kind]
}

func visible(assetScope, queryScope string) bool {
	switch {
	case queryScope == "":
		return true
	case queryScope == "global":
		return assetScope == "global"
	case strings.HasPrefix(queryScope, "proj:"):
		return assetScope == "global" || assetScope == queryScope
	default:
		return assetScope == queryScope
	}
}

func trimSnippet(text string) string {
	trimmed := strings.TrimSpace(text)
	runes := []rune(trimmed)
	if len(runes) <= snippetRunes {
		return trimmed
	}
	return string(runes[:snippetRunes]) + "…"
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// timeWindow 从查询里抽时间线索。命中第一个即返回，天数取窗口长度。
//
// 这是提示词 §5.1 第 1 条「识别时间线索」的实现：没有它，时序路就退化成
// 「重要资产榜」，每个查询都返回同一批东西。
func timeWindow(query string) (int, bool) {
	cues := []struct {
		words []string
		days  int
	}{
		{[]string{"今天", "今日", "刚才", "刚刚"}, 1},
		{[]string{"昨天", "昨晚"}, 2},
		{[]string{"最近", "近期", "这几天", "前几天", "本周", "这周", "上周", "上个星期"}, 14},
		{[]string{"本月", "这个月", "这月"}, 31},
		{[]string{"上个月", "上月"}, 62},
		{[]string{"今年"}, 366},
		{[]string{"去年"}, 732},
	}
	for _, cue := range cues {
		for _, word := range cue.words {
			if strings.Contains(query, word) {
				return cue.days, true
			}
		}
	}
	return 0, false
}

// SearchLexical 是 `brainx search` 的实现：**纯检索，不融合、不注入、不出 brief**。
//
// 与 Recall 的区别是刻意的：search 是「我要找东西」，recall 是「给我上下文」。
// 前者要可预测（只按关键词），后者要尽量不漏（三路融合）。把两者合成一个，
// 用户就再也无法拿到「只匹配这个词」的确定结果。
func SearchLexical(s *store.Store, query, scope, kind string, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = s.Config.RecallMaxItems
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config.RecallTimeoutMs)*time.Millisecond)
	defer cancel()
	hits, err := lexical(s, ctx, query, scope, kind, limit)
	if err != nil {
		if s.Config.LikeFallback {
			return likeSearch(s, query, scope, kind, limit)
		}
		return nil, err
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.id)
	}
	assets, err := s.GetMany(ids)
	if err != nil {
		return nil, err
	}
	wanted := kindSet(kind)
	var results []Result
	for _, hit := range hits {
		a, ok := assets[hit.id]
		if !ok || a.Status != "active" || a.MergedInto != "" {
			continue
		}
		if !kindOK(wanted, a.Kind) {
			continue
		}
		results = append(results, Result{Asset: a, Snippet: hit.snippet, Sources: []string{"lexical"}})
	}
	if len(results) == 0 && s.Config.LikeFallback {
		return likeSearch(s, query, scope, kind, limit)
	}
	return results, nil
}
