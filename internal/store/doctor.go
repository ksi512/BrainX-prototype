package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProbeFTS 是三步探针（提示词 §8）：建表 → 写入切好的 bigram → 用 **2 字词** 查回来。
//
// 为什么要探：FTS5 是 SQLite 的编译期选项，驱动层没开就调不出来，
// 而失败方式是「查询返回 0 条且不报错」——最隐蔽的一种。
// 三步探针把它变成「启动即拦下」。
func ProbeFTS(s *Store) error {
	if _, err := s.DB.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS _fts_probe USING fts5(x, tokenize='unicode61')`); err != nil {
		return fmt.Errorf("FTS5 不可用（建表失败）：%w", err)
	}
	defer s.DB.Exec(`DROP TABLE IF EXISTS _fts_probe`)
	if _, err := s.DB.Exec(`INSERT INTO _fts_probe(x) VALUES('向量 量检 检索')`); err != nil {
		return fmt.Errorf("FTS5 不可用（写入失败）：%w", err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM _fts_probe WHERE _fts_probe MATCH ?`, "向量").Scan(&n); err != nil {
		return fmt.Errorf("FTS5 不可用（查询失败）：%w", err)
	}
	if n == 0 {
		return fmt.Errorf("FTS5 存在但 2 字词查询失效（『向量』返回 0 条）——不要用 3 字词验证，3 字会掩盖缺陷")
	}
	return nil
}

// Check 是一条诊断结果。
type Check struct {
	Name   string
	Status string // ok | warn | fail
	Detail string
}

// Doctor 跑全套体检。任一条 fail 时调用方应当以非零退出码结束。
//
// 设计里明确要求的七项：库完整性、FTS 一致性、孤儿 chunk、悬空边、
// -wal 大小、embed_model 混用、**全库绝对路径扫描**。
// 最后一项是硬约束 11 的唯一守门人：绝对路径在换机器那一刻才炸，平时完全静默。
func Doctor(s *Store) ([]Check, error) {
	var checks []Check
	add := func(name, status, detail string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail})
	}

	var integrity string
	if err := s.DB.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		add("sqlite", "fail", err.Error())
	} else if integrity != "ok" {
		add("sqlite", "fail", integrity)
	} else {
		add("sqlite", "ok", "integrity_check=ok")
	}

	if err := ProbeFTS(s); err != nil {
		add("fts5", "fail", err.Error())
	} else {
		var chunks, indexed int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunks)
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM chunks_fts`).Scan(&indexed)
		if chunks != indexed {
			add("fts5", "fail", fmt.Sprintf("chunks=%d 但索引行=%d（不一致，索引漏行或多行）", chunks, indexed))
		} else {
			add("fts5", "ok", fmt.Sprintf("探针通过，chunks=%d 全部有索引行", chunks))
		}
	}

	var orphans int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM chunks c LEFT JOIN assets a ON a.id=c.asset_id WHERE a.id IS NULL`).Scan(&orphans)
	if orphans > 0 {
		add("orphan_chunks", "fail", fmt.Sprintf("%d 条 chunk 找不到宿主资产", orphans))
	} else {
		add("orphan_chunks", "ok", "0")
	}

	// 悬空边：entity 型边故意不建外键；其余关系任意一端缺失都要报，
	// 只数「两端都缺失」会漏掉 link 打错 id 的情况。
	var dangling int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM edges e WHERE e.rel<>'entity'
		AND (NOT EXISTS(SELECT 1 FROM assets a WHERE a.id=e.from_id)
		OR NOT EXISTS(SELECT 1 FROM assets a WHERE a.id=e.to_id))`).Scan(&dangling)
	if dangling > 0 {
		add("dangling_edges", "warn", fmt.Sprintf("%d 条边指向不存在的资产（可能是 entity 型或历史残留）", dangling))
	} else {
		add("dangling_edges", "ok", "0")
	}

	wal := fileSize(filepath.Join(s.Home, "store.db-wal"))
	switch {
	case wal > 64<<20:
		add("wal", "warn", fmt.Sprintf("%.1fMB（偏大，建议 brainx export 或手动 checkpoint）", float64(wal)/(1<<20)))
	default:
		add("wal", "ok", fmt.Sprintf("%.1fMB", float64(wal)/(1<<20)))
	}

	// 向量模型混用是唯一会造成**静默数据损坏**的一项：余弦距离失去意义但不报错
	currentTag := ""
	if embedder := s.Embedder(); embedder.Enabled() {
		currentTag = embedder.Model
	}
	stale, models, err := s.StaleEmbeddingStats(currentTag)
	if err != nil {
		add("embed_model", "warn", err.Error())
	} else if len(models) > 1 {
		add("embed_model", "fail", fmt.Sprintf("%d 种向量模型混用：%s（必须重算）", len(models), strings.Join(models, ", ")))
	} else if stale > 0 && currentTag != "" {
		add("embed_model", "fail", fmt.Sprintf("%d 个 chunk 的向量来自旧模型（当前 %s），rechunk 后需重算", stale, currentTag))
	} else {
		add("embed_model", "ok", "无混用")
	}

	// 归档根可达性：迁移后最常出问题的一项。
	// **不算 fail**：拔掉根只影响取原件，检索与写入全在库里，功能 100% 完整。
	roots, err := s.ArchiveRootStatus()
	if err != nil {
		add("archive_roots", "warn", err.Error())
	} else {
		var broken []string
		var healthy int
		for _, root := range roots {
			if root.Error != "" {
				broken = append(broken, fmt.Sprintf("%s（%d 条资产）：%s", root.Name, root.Assets, root.Error))
				continue
			}
			healthy++
		}
		switch {
		case len(roots) == 0:
			add("archive_roots", "ok", "未使用外部归档")
		case len(broken) > 0:
			add("archive_roots", "warn", strings.Join(broken, "；")+"——只影响取原件，检索不受影响")
		default:
			add("archive_roots", "ok", fmt.Sprintf("%d 个根全部可达", healthy))
		}
	}

	absAssets, absDocs := absoluteRefs(s)
	if absAssets > 0 || absDocs > 0 {
		add("absolute_paths", "fail", fmt.Sprintf("%d 条资产的 archive/origin 是绝对路径——换机器会静默断链，必须改成相对路径或 ext:<root>:<relpath>", absAssets+absDocs))
	} else {
		add("absolute_paths", "ok", "全库无绝对路径引用")
	}

	return checks, nil
}

// absoluteRefs 扫全库的路径型字段。
// 同时看 archive 与 origin：origin 里 `file:/abs/path` 也是绝对路径。
func absoluteRefs(s *Store) (assets, docs int) {
	rows, err := s.DB.Query(`SELECT COALESCE(archive,''),COALESCE(origin,'') FROM assets`)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var archive, origin string
		if err := rows.Scan(&archive, &origin); err != nil {
			return assets, docs
		}
		if isAbsoluteRef(archive) {
			assets++
		}
		if ref := strings.TrimPrefix(origin, "file:"); ref != origin && isAbsoluteRef(ref) {
			docs++
		}
	}
	return assets, docs
}

// 供 CLI 判断退出码用
func HasFailure(checks []Check) bool {
	for _, c := range checks {
		if c.Status == "fail" {
			return true
		}
	}
	return false
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
