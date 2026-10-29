package store

import (
	"archive/zip"
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DerivedStats 是生成物导出的结果。
type DerivedStats struct {
	Root    string
	Pages   int
	Written int
	Skipped int
	Pruned  int
	Orphans []string
}

// defaultDerivedKinds：只导「做梦的产物」。memory/fact/skill/profile 是运行时认知，
// 导出到 NAS 没有意义（要看用 export 全库包）。
var defaultDerivedKinds = []string{"synthesis", "insight", "note"}

// ExportDerived 把生成物导成一棵 Markdown 树（提示词 §3.7）。
//
// 四条硬约束：① 永不导 doc；② 手动触发；③ 内容未变不重写（保 mtime）；④ index.md 顶部写明「这是快照」。
func (s *Store) ExportDerived(to string, kinds []string, prune bool, yes bool) (DerivedStats, error) {
	stats := DerivedStats{Root: to}
	if strings.TrimSpace(to) == "" {
		return stats, fmt.Errorf("需要 --to 指定导出目录")
	}
	if len(kinds) == 0 {
		kinds = defaultDerivedKinds
	}
	allowed := map[string]bool{}
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if kind == "doc" {
			return stats, fmt.Errorf("拒绝导出 doc：原件本来就在外部位置，导出会制造两份真相")
		}
		allowed[kind] = true
	}

	assets, err := s.List("", "", "active", 100000)
	if err != nil {
		return stats, err
	}
	sources, err := s.derivedFromMap()
	if err != nil {
		return stats, err
	}

	keep := map[string]bool{}
	var index strings.Builder
	index.WriteString("# brainx 生成物快照\n\n")
	index.WriteString("> ⚠ 这是快照，权威数据在本地 `store.db`。改这里的文件不会回流，下次导出会被覆盖。\n\n")
	index.WriteString(fmt.Sprintf("导出时间：%s · kind：%s\n\n", time.Now().Format("2006-01-02 15:04"), strings.Join(kinds, ", ")))

	for _, a := range assets {
		if !allowed[a.Kind] {
			continue
		}
		if !ValidAssetID(a.ID) {
			return stats, fmt.Errorf("资产 id 非法，拒绝导出：%q", a.ID)
		}
		rel := filepath.ToSlash(filepath.Join(a.Kind, time.UnixMilli(a.CreatedAt).Format("2006/01"), a.ID+".md"))
		if !SafeRelativePath(rel) {
			return stats, fmt.Errorf("导出路径越界：%q", rel)
		}
		keep[rel] = true
		content := derivedPage(a, sources[a.ID])
		full := filepath.Join(to, filepath.FromSlash(rel))
		stats.Pages++
		if existing, readErr := os.ReadFile(full); readErr == nil && string(existing) == content {
			stats.Skipped++
		} else {
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				return stats, err
			}
			if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
				return stats, err
			}
			stats.Written++
		}
		title := a.Title
		if title == "" {
			title = firstNonEmptyLine(a.Body)
		}
		index.WriteString(fmt.Sprintf("- [%s](%s) · %s · %s · %s\n", title, rel, a.Kind, strings.Join(sources[a.ID], ","), time.UnixMilli(a.CreatedAt).Format("2006-01-02")))
	}

	// 孤儿：导出目录里存在、库里已不存在或已归档的文件。默认不删。
	orphans, err := s.derivedOrphans(to, kinds, keep)
	if err != nil {
		return stats, err
	}
	stats.Orphans = orphans
	if prune && len(orphans) > 0 {
		if !yes {
			return stats, fmt.Errorf("--prune 会删除 %d 个文件，先逐条看：\n%s\n确认后加 --yes 重跑", len(orphans), strings.Join(orphans, "\n"))
		}
		for _, rel := range orphans {
			if !SafeRelativePath(rel) {
				return stats, fmt.Errorf("拒绝删除越界路径：%q", rel)
			}
			if err := os.Remove(filepath.Join(to, filepath.FromSlash(rel))); err != nil {
				return stats, err
			}
			stats.Pruned++
		}
	}
	if len(orphans) > 0 && !prune {
		index.WriteString(fmt.Sprintf("\n---\n\n未清理的孤儿文件：%d 个（`export --derived --prune --yes` 可清理）\n", len(orphans)))
	}

	indexPath := filepath.Join(to, "index.md")
	if existing, readErr := os.ReadFile(indexPath); readErr == nil && string(existing) == index.String() {
		// 内容没变就不写，保 mtime
	} else {
		if err := os.MkdirAll(to, 0o700); err != nil {
			return stats, err
		}
		if err := os.WriteFile(indexPath, []byte(index.String()), 0o600); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// derivedPage 渲染单页。YAML frontmatter 是为了直接喂给 Notion / Obsidian。
func derivedPage(a Asset, sources []string) string {
	b := &strings.Builder{}
	b.WriteString("---\n")
	b.WriteString("id: " + a.ID + "\n")
	b.WriteString("kind: " + a.Kind + "\n")
	if len(sources) > 0 {
		b.WriteString("derived_from: [" + strings.Join(sources, ", ") + "]\n")
	}
	b.WriteString("created_at: " + time.UnixMilli(a.CreatedAt).Format(time.RFC3339) + "\n")
	b.WriteString("brainx_export_v: 1\n")
	b.WriteString("---\n\n")
	if a.Title != "" {
		b.WriteString("# " + a.Title + "\n\n")
	}
	b.WriteString(a.Body)
	b.WriteString("\n")
	return b.String()
}

// derivedFromMap 取每条资产的 derived_from 来源（出边）。
func (s *Store) derivedFromMap() (map[string][]string, error) {
	rows, err := s.DB.Query(`SELECT from_id,to_id FROM edges WHERE rel='derived_from' ORDER BY from_id,to_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, err
		}
		out[from] = append(out[from], to)
	}
	return out, rows.Err()
}

func (s *Store) derivedOrphans(to string, kinds []string, keep map[string]bool) ([]string, error) {
	var orphans []string
	for _, kind := range kinds {
		root := filepath.Join(to, kind)
		err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			rel, relErr := filepath.Rel(to, path)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if !keep[rel] {
				orphans = append(orphans, rel)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// isHex 只接受指定长度的十六进制串。
func isHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ValidAssetID 是资产 id 的唯一合法形态（26 位小写 hex）。
// 库内 id 会被拼进文件路径，不能接受 `../`、引号或任意字符串。
func ValidAssetID(id string) bool { return isHex(id, 26) }

// ValidContentHash 是 sha256 的十六进制形态。
func ValidContentHash(hash string) bool { return isHex(hash, 64) }

// SafeRelativePath 判断一个相对路径是否落在某个根之下。
// 拒绝绝对路径、盘符、`..` 段与空路径。
func SafeRelativePath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	value = filepath.FromSlash(value)
	if filepath.IsAbs(value) {
		return false
	}
	if filepath.VolumeName(value) != "" {
		return false
	}
	if len(value) >= 2 && value[1] == ':' { // Windows 盘符在 Unix 上 VolumeName 为空
		return false
	}
	if strings.HasPrefix(value, `\`) || strings.HasPrefix(value, "//") {
		return false
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

var bundleKinds = map[string]bool{
	"memory": true, "fact": true, "skill": true, "profile": true,
	"note": true, "doc": true, "synthesis": true, "insight": true,
}

var bundleRels = map[string]bool{
	"semantic": true, "contradicts": true, "supersedes": true,
	"derived_from": true, "cites": true, "alias_of": true,
	"temporal": true, "causal": true, "entity": true,
}

func validArchiveRef(ref string) bool {
	switch {
	case ref == "" || ref == "text-only":
		return true
	case strings.HasPrefix(ref, "files/"):
		return SafeRelativePath(ref)
	case strings.HasPrefix(ref, "ext:"):
		_, rel, ok := strings.Cut(strings.TrimPrefix(ref, "ext:"), ":")
		return ok && SafeRelativePath(rel)
	default:
		return false
	}
}

// validateBundleDB 在合并/替换前校验包内数据库。
// .mbz 是外部输入：asset id / kind / archive / content_hash 都会被拼进
// 文件路径或 HTML，必须先证明它们是合法值，否则拒绝整个包。
func validateBundleDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	rows, err := db.Query(`SELECT id,kind,scope,archive,content_hash FROM assets`)
	if err != nil {
		return fmt.Errorf("包内 assets 表不可读：%w", err)
	}
	for rows.Next() {
		var id, kind, scope, archive, hash string
		if err := rows.Scan(&id, &kind, &scope, &archive, &hash); err != nil {
			rows.Close()
			return err
		}
		if !ValidAssetID(id) {
			rows.Close()
			return fmt.Errorf("包内资产 id 非法：%q", id)
		}
		if !bundleKinds[kind] {
			rows.Close()
			return fmt.Errorf("包内资产 %s 的 kind 非法：%q", id, kind)
		}
		if scope != "global" && scope != "proj" && !strings.HasPrefix(scope, "proj:") {
			rows.Close()
			return fmt.Errorf("包内资产 %s 的 scope 非法：%q", id, scope)
		}
		if !validArchiveRef(archive) {
			rows.Close()
			return fmt.Errorf("包内资产 %s 的 archive 非法：%q", id, archive)
		}
		if !ValidContentHash(hash) {
			rows.Close()
			return fmt.Errorf("包内资产 %s 的 content_hash 非法（应为 64 位 hex）", id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	checks := []struct {
		query string
		label string
	}{
		{`SELECT asset_id FROM chunks`, "chunk.asset_id"},
		{`SELECT asset_a FROM reviews`, "review.asset_a"},
		{`SELECT asset_b FROM reviews`, "review.asset_b"},
	}
	for _, check := range checks {
		rows, err := db.Query(check.query)
		if err != nil {
			return fmt.Errorf("包内 %s 不可读：%w", check.label, err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !ValidAssetID(id) {
				rows.Close()
				return fmt.Errorf("包内 %s 非法：%q", check.label, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	rows, err = db.Query(`SELECT from_id,to_id,rel FROM edges`)
	if err != nil {
		return fmt.Errorf("包内 edges 不可读：%w", err)
	}
	for rows.Next() {
		var from, to, rel string
		if err := rows.Scan(&from, &to, &rel); err != nil {
			rows.Close()
			return err
		}
		if !bundleRels[rel] {
			rows.Close()
			return fmt.Errorf("包内关系非法：%q", rel)
		}
		// entity 型边允许指向库外实体；其余两端必须是资产。
		if rel != "entity" && (!ValidAssetID(from) || !ValidAssetID(to)) {
			rows.Close()
			return fmt.Errorf("包内边 %q 的端点非法：%q -> %q", rel, from, to)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return nil
}

// ImportBundle 从 .mbz 恢复。merge = 按 id 去重合并（缺的补上，不覆盖本地改动）；
// replace = 先把当前库改名备份，再整体换成包里的库。
func (s *Store) ImportBundle(path string, replace bool) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp("", "brainx-import-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := unzipTo(path, stage); err != nil {
		return "", err
	}
	incoming := filepath.Join(stage, "store.db")
	if _, err := os.Stat(incoming); err != nil {
		return "", fmt.Errorf("包里没有 store.db：%w", err)
	}
	if err := validateBundleDB(incoming); err != nil {
		return "", fmt.Errorf("拒绝导入：%w", err)
	}

	if replace {
		s.Close()
		stamp := time.Now().UnixMilli()
		backup := filepath.Join(s.Home, fmt.Sprintf("store.db.bak-%d", stamp))
		if err := os.Rename(filepath.Join(s.Home, "store.db"), backup); err != nil {
			return "", err
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			_ = os.Remove(filepath.Join(s.Home, "store.db"+suffix))
		}
		// 旧原件不是安装物，但也不是新库的一部分：整体移到 files.bak-<stamp>，
		// 避免替换后 files/ 里残留上一库的文件。备份保留，由用户自行清理。
		files := filepath.Join(s.Home, "files")
		if _, err := os.Stat(files); err == nil {
			if err := os.Rename(files, filepath.Join(s.Home, fmt.Sprintf("files.bak-%d", stamp))); err != nil {
				return "", err
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := copyFile(incoming, filepath.Join(s.Home, "store.db")); err != nil {
			return "", err
		}
		if err := copyTree(filepath.Join(stage, "files"), files); err != nil {
			return "", err
		}
		return backup, nil
	}

	// merge：ATTACH 后 INSERT OR IGNORE，本地已有的 id 一律不覆盖
	if _, err := s.DB.Exec(`ATTACH DATABASE ? AS incoming`, incoming); err != nil {
		return "", err
	}
	defer s.DB.Exec(`DETACH DATABASE incoming`)
	tables := []string{"assets", "asset_tags", "chunks", "edges", "runs", "events", "reviews"}
	for _, table := range tables {
		if _, err := s.DB.Exec(fmt.Sprintf(`INSERT OR IGNORE INTO main.%s SELECT * FROM incoming.%s`, table, table)); err != nil {
			return "", fmt.Errorf("合并 %s 失败：%w", table, err)
		}
	}
	if err := copyTree(filepath.Join(stage, "files"), filepath.Join(s.Home, "files")); err != nil {
		return "", err
	}
	// 合并进来的 chunk 没有 FTS 行，必须重建，否则「导入的文档搜不到」
	if _, err := s.Reindex(500, false); err != nil {
		return "", err
	}
	return "", nil
}

func unzipTo(archive, dir string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	for _, file := range reader.File {
		target := filepath.Join(dir, filepath.FromSlash(file.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("包内路径越界：%s", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			source.Close()
			return err
		}
		if _, err := io.Copy(out, source); err != nil {
			out.Close()
			source.Close()
			return err
		}
		out.Close()
		source.Close()
	}
	return nil
}

func copyTree(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return copyFile(source, target)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyTree(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func firstNonEmptyLine(body string) string {
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			return line
		}
	}
	return "(无标题)"
}
