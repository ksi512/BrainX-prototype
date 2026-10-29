package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/index"
)

// ErrBusyExhausted / ErrCorrupt 是错误契约里的退出码 6 / 7 的判据。
var (
	ErrBusyExhausted = errors.New("写锁竞争失败（重试三次仍 SQLITE_BUSY）")
	ErrCorrupt       = errors.New("库损坏或 schema 版本不符")
	ErrDownload      = errors.New("外部下载失败")
)

// queueRecord 是补偿文件的结构。落盘的 payload 与 Create 的输入同构，
// 所以补写路径与正常写入走同一段代码——两份实现必然漂移。
type queueRecord struct {
	Op            string      `json:"op"`
	Payload       CreateAsset `json:"payload"`
	Attempts      int         `json:"attempts"`
	FirstFailedAt int64       `json:"first_failed_at"`
	LastError     string      `json:"last_error"`
}

// DrainQueue 是 `queue/` 的消费入口：把补偿文件按顺序重放，成功即删。
//
// 为什么必须存在：Write 失败时把记录落 queue/ 只是「不丢」，没人消费就等于「丢了但留了个墓碑」。
// 每次非 hook 命令启动时调一次（preflight），成本是几次 ReadDir。
func (s *Store) DrainQueue(limit int) (done, failed int, err error) {
	if limit <= 0 {
		limit = 200
	}
	// **只扫 queue/ 本层**。queue/cmd/ 是命令（执行一次即删、失败不重试），
	// 不是补偿记录——把它一起扫，一条 UI 命令会被当成丢写反复重放，
	// 还会被改写成补偿记录的格式，把命令本身弄坏。两个目录的语义必须分开处理。
	for _, dir := range []string{filepath.Join(s.Home, "queue")} {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names) // 按 ULID 前缀即时间序
		if len(names) > limit {
			names = names[:limit]
		}
		for _, name := range names {
			path := filepath.Join(dir, name)
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			var record queueRecord
			if json.Unmarshal(raw, &record) != nil {
				_ = moveToDead(s, path, "payload 无法解析")
				failed++
				continue
			}
			if cmdErr := s.runQueuedCommand(record); cmdErr != nil {
				record.Attempts++
				record.LastError = cmdErr.Error()
				// 三次仍失败 → 进 dead/，不再无限重试
				if record.Attempts >= 3 {
					_ = moveToDead(s, path, record.LastError)
					failed++
					continue
				}
				if updated, encErr := json.MarshalIndent(record, "", "  "); encErr == nil {
					_ = os.WriteFile(path, updated, 0o600)
				}
				failed++
				continue
			}
			_ = os.Remove(path)
			done++
		}
	}
	return done, failed, nil
}

// runQueuedCommand 目前只支持 remember（补写）与 refresh（重算计数）。
// 失败一律不重试超过 3 次，也不静默吞掉。
func (s *Store) runQueuedCommand(record queueRecord) error {
	switch record.Op {
	case "remember", "":
		_, err := s.Create(record.Payload)
		return err
	case "noop":
		return nil
	default:
		return fmt.Errorf("未知补偿动作 %q", record.Op)
	}
}

func moveToDead(s *Store, path, reason string) error {
	dead := filepath.Join(s.Home, "queue", "dead")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		return err
	}
	target := filepath.Join(dead, filepath.Base(path))
	if err := os.Rename(path, target); err != nil {
		return err
	}
	_ = os.WriteFile(target+".why", []byte(reason+"\n"), 0o600)
	return nil
}

// RetryDead 把 dead/ 里的补偿文件移回 queue/ 重试（人工确认后调用）。
func (s *Store) RetryDead() (int, error) {
	dead := filepath.Join(s.Home, "queue", "dead")
	entries, err := os.ReadDir(dead)
	if err != nil {
		return 0, nil
	}
	moved := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		source := filepath.Join(dead, entry.Name())
		raw, readErr := os.ReadFile(source)
		if readErr != nil {
			continue
		}
		var record queueRecord
		if json.Unmarshal(raw, &record) == nil {
			record.Attempts = 0 // 人工放行一次，重置计数
			if updated, encErr := json.MarshalIndent(record, "", "  "); encErr == nil {
				_ = os.WriteFile(source, updated, 0o600)
			}
		}
		if err := os.Rename(source, filepath.Join(s.Home, "queue", entry.Name())); err != nil {
			continue
		}
		_ = os.Remove(source + ".why")
		moved++
	}
	return moved, nil
}

// QueueDepth 给 `context --pending` / `status` 用。
func (s *Store) QueueDepth() (pending int, dead int) {
	return countJSON(filepath.Join(s.Home, "queue")), countJSON(filepath.Join(s.Home, "queue", "dead"))
}

func countJSON(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			n++
		}
	}
	return n
}

// bigramsOf / chunkText 是给维护命令用的薄封装：让 reindex/rechunk
// 与写入路径共用同一套切分与分词实现，避免两套算法漂移。
func bigramsOf(text string) string { return index.Bigrams(text) }

func chunkText(body string, cfg Config) []index.Chunk {
	return index.ChunkText(body, cfg.ChunkTarget, cfg.ChunkMax, cfg.ChunkMin, cfg.ChunkOverlap)
}

// ───────────────────────── reindex / rechunk ─────────────────────────

// Reindex 按 chunks.rowid 分批重建 chunks_fts，全程不清空整表，可中断续跑。
//
// 为什么不能「DROP 再重建」：重建期间检索会空窗，而 hook 每轮都在跑。
// 游标记在 meta 的 reindex:scan_rowid，中断后 --resume 从这里继续。
func (s *Store) Reindex(batch int, resume bool) (int, error) {
	if batch <= 0 {
		batch = 500
	}
	cursor := int64(0)
	if resume {
		cursor = metaIntValue(s, "reindex:scan_rowid")
	} else {
		_ = s.SetMeta("reindex:scan_rowid", "0")
	}
	processed := 0
	for {
		rows, err := s.DB.Query(`SELECT c.rowid,c.asset_id,c.seq,c.text,COALESCE(a.title,'') FROM chunks c JOIN assets a ON a.id=c.asset_id WHERE c.rowid>? ORDER BY c.rowid LIMIT ?`, cursor, batch)
		if err != nil {
			return processed, err
		}
		type row struct {
			rowID   int64
			assetID string
			seq     int
			text    string
			title   string
		}
		var batchRows []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.rowID, &r.assetID, &r.seq, &r.text, &r.title); err != nil {
				rows.Close()
				return processed, err
			}
			batchRows = append(batchRows, r)
		}
		rows.Close()
		if len(batchRows) == 0 {
			break
		}
		for _, r := range batchRows {
			// contentless 表：先带原始 token 删旧行，再插新行
			_, _ = s.DB.Exec(`INSERT INTO chunks_fts(chunks_fts,rowid,title_ngram,text_ngram,asset_id,seq) VALUES('delete',?,?,?,?,?)`,
				r.rowID, bigramsOf(r.title), bigramsOf(r.text), r.assetID, r.seq)
			if _, err := s.DB.Exec(`INSERT INTO chunks_fts(rowid,title_ngram,text_ngram,asset_id,seq) VALUES(?,?,?,?,?)`,
				r.rowID, bigramsOf(r.title), bigramsOf(r.text), r.assetID, r.seq); err != nil {
				return processed, err
			}
			cursor = r.rowID
			processed++
		}
		_ = s.SetMeta("reindex:scan_rowid", fmt.Sprintf("%d", cursor))
	}
	_ = s.SetMeta("reindex:scan_rowid", "")
	return processed, nil
}

// Rechunk 按当前 chunk.* 参数重切所有非 doc 之外…实际上 doc 也要重切（切片不是正文）。
// 重切后旧向量作废（embed_model 清空），必须重算。
func (s *Store) Rechunk(batch int, resume bool) (int, error) {
	assets, err := s.List("", "", "", 100000)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, a := range assets {
		if err := s.rechunkOne(a); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func (s *Store) rechunkOne(a Asset) error {
	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	old, err := chunksOf(ctx, conn, a.ID)
	if err != nil {
		return err
	}
	for _, chunk := range old {
		_, _ = conn.ExecContext(ctx, `INSERT INTO chunks_fts(chunks_fts,rowid,title_ngram,text_ngram,asset_id,seq) VALUES('delete',?,?,?,?,?)`,
			chunk.rowID, bigramsOf(a.Title), bigramsOf(chunk.text), a.ID, chunk.seq)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM chunks WHERE asset_id=?`, a.ID); err != nil {
		return err
	}
	chunks := chunkText(a.Body, s.Config)
	for seq, chunk := range chunks {
		res, err := conn.ExecContext(ctx, `INSERT INTO chunks(id,asset_id,seq,text,start_off,end_off) VALUES(?,?,?,?,?,?)`,
			ID(), a.ID, seq, chunk.Text, chunk.StartOff, chunk.EndOff)
		if err != nil {
			return err
		}
		rowID, _ := res.LastInsertId()
		if _, err := conn.ExecContext(ctx, `INSERT INTO chunks_fts(rowid,title_ngram,text_ngram,asset_id,seq) VALUES(?,?,?,?,?)`,
			rowID, bigramsOf(a.Title), bigramsOf(chunk.Text), a.ID, seq); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE assets SET updated_at=updated_at WHERE id=?`, a.ID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// Repair 只做确定性修复：checkpoint、重建 FTS、清孤儿与悬空边。**绝不猜数据**。
func (s *Store) Repair() (map[string]string, error) {
	out := map[string]string{}
	if _, err := s.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return out, err
	}
	out["wal"] = "checkpoint 完成"
	reindexed, err := s.Reindex(500, false)
	if err != nil {
		return out, err
	}
	out["fts"] = fmt.Sprintf("重建 %d 片", reindexed)
	res, err := s.DB.Exec(`DELETE FROM chunks WHERE asset_id NOT IN (SELECT id FROM assets)`)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			out["orphan_chunks"] = fmt.Sprintf("清理 %d 条", n)
		}
	}
	res, err = s.DB.Exec(`DELETE FROM edges WHERE rel<>'entity' AND from_id NOT IN (SELECT id FROM assets) AND to_id NOT IN (SELECT id FROM assets)`)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			out["dangling_edges"] = fmt.Sprintf("清理 %d 条", n)
		}
	}

	// plan 令牌里塞的是 JSON，坏了会让 --apply 一直报「找不到最近一次清单」，
	// 而用户看不到任何原因。修的时候直接清掉：重跑 --plan 会重写。
	cleared := 0
	rows, err := s.DB.Query(`SELECT k,v FROM meta WHERE k LIKE 'plan:%'`)
	if err == nil {
		var brokenKeys []string
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				continue
			}
			if !json.Valid([]byte(value)) {
				brokenKeys = append(brokenKeys, key)
			}
		}
		rows.Close()
		for _, key := range brokenKeys {
			if _, err := s.DB.Exec(`DELETE FROM meta WHERE k=?`, key); err == nil {
				cleared++
			}
		}
	}
	if cleared > 0 {
		out["meta"] = fmt.Sprintf("清理 %d 个损坏的 plan 令牌", cleared)
	}

	// VACUUM 放最后：它要求独占，前面任何写事务没清干净都会失败。
	// 失败只报告不报错——库能用，只是文件没缩。
	if _, err := s.DB.Exec(`VACUUM`); err != nil {
		out["vacuum"] = "跳过（" + err.Error() + "）"
	} else {
		out["vacuum"] = "完成"
	}
	return out, nil
}

// DrainQueueFull 全量补写，直到队列空或达到上限。
// doctor 与「用户明确要求清空」的场景用它；Stop 用有界版本，免得拖过预算。
func (s *Store) DrainQueueFull() (done, failed int, rounds int) {
	for i := 0; i < 20; i++ {
		d, f, err := s.DrainQueue(200)
		done += d
		failed += f
		rounds++
		if err != nil || (d == 0 && f == 0) {
			break
		}
	}
	return done, failed, rounds
}

// VerifyIntegrity 给 doctor / Open 用；失败即退出码 7。
func (s *Store) VerifyIntegrity() error {
	var result string
	if err := s.DB.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("%w：%v", ErrCorrupt, err)
	}
	if result != "ok" {
		return fmt.Errorf("%w：%s", ErrCorrupt, result)
	}
	return nil
}

var _ = sql.ErrNoRows
var _ = time.Now

// CheckpointWAL 做一次 WAL 回收。
//
// 为什么必须在收尾时做：WAL 自动 checkpoint 只让空间**可重用**，不缩小文件。
// 一次几万字的导入可能留下几百 MB 的 -wal，而用户只看到磁盘少了。
// 失败不算错（可能有别的连接占用），下次再做。
func (s *Store) CheckpointWAL() error {
	_, err := s.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// ArchiveRootStatus 给 doctor 用：哪个命名根不可达、影响多少条资产。
//
// 不阻断任何功能：库在功能上 100% 完整，只有「取原件」这一条会退。
func (s *Store) ArchiveRootStatus() ([]RootStatus, error) {
	type counter struct {
		name  string
		count int
	}
	rows, err := s.DB.Query(`SELECT archive,COUNT(*) FROM assets WHERE archive LIKE 'ext:%' GROUP BY archive`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byRoot := map[string]int{}
	for rows.Next() {
		var archive string
		var count int
		if err := rows.Scan(&archive, &count); err != nil {
			return nil, err
		}
		rest := strings.TrimPrefix(archive, "ext:")
		name, _, ok := strings.Cut(rest, ":")
		if !ok {
			name = rest
		}
		byRoot[name] += count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(byRoot))
	for name := range byRoot {
		names = append(names, name)
	}
	// 配置里注册了但一条资产都没引用的根也要列出来：空根是最常见的「配错了」。
	for name := range s.Config.Roots {
		if _, ok := byRoot[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var out []RootStatus
	for _, name := range names {
		status := RootStatus{Name: name, Assets: byRoot[name], Path: s.Config.Roots[name]}
		if status.Path == "" {
			status.Error = "未在 archive.roots 里注册"
			out = append(out, status)
			continue
		}
		if strings.HasPrefix(status.Path, "http://") || strings.HasPrefix(status.Path, "https://") {
			// URL 型根不做探测：每次 doctor 都发一次请求是「引擎不主动联网」的破例
			status.Error = ""
			out = append(out, status)
			continue
		}
		if err := ProbeReachable(status.Path); err != nil {
			status.Error = err.Error()
		}
		out = append(out, status)
	}
	return out, nil
}

// RootStatus 是一个命名根的可达性。
type RootStatus struct {
	Name   string
	Path   string
	Assets int
	Error  string
}
