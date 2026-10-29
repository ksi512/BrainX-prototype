// Package wiki 把库投影成一组 Markdown 页面。
//
// 铁律：**只投影，不生成正文**。页面里的每一句话都来自 assets.body 或元数据，
// 不产生新句子——所以它不是「LLM 写 wiki」，而是「换个视图看同一批数据」。
// 这也决定了它可以随时删掉重建。
package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/store"
)

const projectionBanner = "> ⚠ 这是投影，权威数据在 store.db。改这里的文件不会写回库，下次 `brainx wiki` 会被覆盖。\n"

// Summary 是构建结果，CLI 直接打印。
type Summary struct {
	Dir     string
	Pages   int
	Written int
	Skipped int
	Removed int
}

// Build 生成投影。默认写到 $BRAINX_HOME/wiki/，`--out` 可指到外部根。
func Build(s *store.Store, out string) (Summary, error) {
	dir := out
	if dir == "" {
		dir = filepath.Join(s.Home, "wiki")
	}
	assets, err := s.List("", "", "active", 100000)
	if err != nil {
		return Summary{}, err
	}
	tags, err := s.TagsByAsset()
	if err != nil {
		return Summary{}, err
	}
	backlinks, err := backlinksOf(s)
	if err != nil {
		return Summary{}, err
	}

	var active []string
	byID := map[string]store.Asset{}
	for _, a := range assets {
		// 只投影 active：pending / archived 不进导航（规则 1）
		byID[a.ID] = a
		active = append(active, a.ID)
	}

	files := map[string]string{} // 相对路径 → 内容
	hasPage := map[string]string{}

	// 单篇：doc 与 insight 各有一页
	for _, a := range assets {
		if !store.ValidAssetID(a.ID) {
			return Summary{}, fmt.Errorf("资产 id 非法，拒绝投影：%q", a.ID)
		}
		var sub string
		switch a.Kind {
		case "doc":
			sub = "doc"
		case "insight", "synthesis":
			sub = "insight"
		default:
			continue
		}
		rel := filepath.ToSlash(filepath.Join(sub, a.ID+".md"))
		files[rel] = assetPage(a, backlinks[a.ID])
		hasPage[a.ID] = rel
	}

	// 标签簇：一个标签一页
	clusters := map[string][]store.Asset{}
	for _, a := range assets {
		for _, tag := range tags[a.ID] {
			clusters[tag] = append(clusters[tag], a)
		}
	}
	tagNames := make([]string, 0, len(clusters))
	for tag := range clusters {
		tagNames = append(tagNames, tag)
	}
	sort.Strings(tagNames)

	index := &strings.Builder{}
	index.WriteString("# 索引\n\n")
	index.WriteString(projectionBanner)
	index.WriteString("\n")
	index.WriteString(fmt.Sprintf("共 %d 条 active 资产 · 生成于 %s\n\n", len(assets), time.Now().Format("2006-01-02 15:04")))
	if len(assets) == 0 {
		index.WriteString("（库里还没有 active 资产）\n")
	}
	for _, tag := range tagNames {
		members := clusters[tag]
		slug := slugify(tag)
		rel := filepath.ToSlash(filepath.Join("topic", slug+".md"))
		files[rel] = topicPage(tag, members, hasPage)
		index.WriteString(fmt.Sprintf("## %s（%d）\n\n", tag, len(members)))
		for _, a := range members {
			index.WriteString(entryLine(a, hasPage))
		}
		index.WriteString("\n")
	}
	// 无标签的资产也要能导航到，否则它们永远不会出现在 index 里
	var untagged []store.Asset
	for _, a := range assets {
		if len(tags[a.ID]) == 0 {
			untagged = append(untagged, a)
		}
	}
	if len(untagged) > 0 {
		index.WriteString(fmt.Sprintf("## 未打标签（%d）\n\n", len(untagged)))
		for _, a := range untagged {
			index.WriteString(entryLine(a, hasPage))
		}
		index.WriteString("\n")
	}
	files["index.md"] = index.String()

	summary := Summary{Dir: dir, Pages: len(files)}
	for rel, content := range files {
		if !store.SafeRelativePath(rel) {
			return summary, fmt.Errorf("投影路径越界：%q", rel)
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if existing, err := os.ReadFile(full); err == nil && string(existing) == content {
			summary.Skipped++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return summary, err
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			return summary, err
		}
		summary.Written++
	}

	// 清理失效页面：只删我们自己管理的子目录里的 .md，别的文件一律不碰
	removed, err := prune(dir, files)
	if err != nil {
		return summary, err
	}
	summary.Removed = removed
	return summary, nil
}

func entryLine(a store.Asset, hasPage map[string]string) string {
	label := a.Title
	if label == "" {
		label = firstLine(a.Body)
	}
	date := time.UnixMilli(a.CreatedAt).Format("2006-01-02")
	if rel, ok := hasPage[a.ID]; ok {
		return fmt.Sprintf("- [%s](%s) · %s · %s · %s\n", label, rel, a.Kind, a.ID, date)
	}
	return fmt.Sprintf("- %s · %s · %s · %s\n", label, a.Kind, a.ID, date)
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func assetPage(a store.Asset, sources []string) string {
	b := &strings.Builder{}
	b.WriteString("# " + a.Title + "\n\n")
	b.WriteString(projectionBanner)
	b.WriteString("\n")
	body := a.Body
	// 原文摘录：doc 页给全文，其余给前 2000 字——投影不是备份
	runes := []rune(body)
	if a.Kind != "doc" && len(runes) > 2000 {
		body = string(runes[:2000]) + "\n\n…（已截断，全文见 brainx get " + a.ID + "）"
	}
	b.WriteString(body)
	b.WriteString("\n\n---\n\n")
	b.WriteString(fmt.Sprintf("- id：`%s`\n- kind：%s\n- scope：%s\n- status：%s\n- content_hash：`%s`\n",
		a.ID, a.Kind, a.Scope, a.Status, shortHash(a.ContentHash)))
	if len(sources) > 0 {
		b.WriteString("\n## 被以下资产引用\n\n")
		for _, id := range sources {
			b.WriteString("- `" + id + "`\n")
		}
	}
	return b.String()
}

func topicPage(tag string, members []store.Asset, hasPage map[string]string) string {
	b := &strings.Builder{}
	b.WriteString("# " + tag + "\n\n")
	b.WriteString(projectionBanner)
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("该标签下 %d 条 active 资产。\n\n", len(members)))
	for _, a := range members {
		b.WriteString(entryLine(a, hasPage))
	}
	b.WriteString("\n[← 返回索引](../index.md)\n")
	return b.String()
}

// backlinksOf 反向链接：edges 里 to_id = 本页资产的行。
// 不新建索引——idx_edges_to 已经有了。
func backlinksOf(s *store.Store) (map[string][]string, error) {
	rows, err := s.DB.Query(`SELECT to_id,from_id FROM edges ORDER BY to_id,from_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var to, from string
		if err := rows.Scan(&to, &from); err != nil {
			return nil, err
		}
		out[to] = append(out[to], from)
	}
	return out, rows.Err()
}

func prune(dir string, keep map[string]string) (int, error) {
	removed := 0
	for _, sub := range []string{"topic", "doc", "insight"} {
		root := filepath.Join(dir, sub)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(sub, entry.Name()))
			if _, ok := keep[rel]; ok {
				continue
			}
			if err := os.Remove(filepath.Join(root, entry.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// slugify 生成文件名。保留中文（macOS/Linux 都支持），
// 只替换路径分隔符与少数危险字符，避免为了「长得像 URL」把可读性丢掉。
func slugify(tag string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "\x00", "", "..", "_")
	slug := strings.TrimSpace(replacer.Replace(tag))
	if slug == "" {
		slug = "untitled"
	}
	if len([]rune(slug)) > 60 {
		slug = string([]rune(slug)[:60])
	}
	return slug
}

func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			runes := []rune(line)
			if len(runes) > 40 {
				return string(runes[:40]) + "…"
			}
			return line
		}
	}
	return "(无标题)"
}
