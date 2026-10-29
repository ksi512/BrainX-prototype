package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/index"
)

// UpdateInput 描述一次改写。空字符串表示「这一项不动」。
type UpdateInput struct {
	Body    string
	Title   string
	AddTags []string
}

// Update 改写一条记忆型资产的正文 / 标题 / 标签，并**同步重建它的 chunk 与 FTS 行**。
//
// 三条约束：
//   - `immutable=1`（kind='doc'）拒绝改写。文档是 ground truth，改它等于毁掉溯源（硬约束 10）。
//   - `updated_at` 只在正文真的变了时更新——它是「变化才注入」的水位线（硬约束 14）。
//   - FTS 是 contentless 表，删行必须用 'delete' 命令**带原始 token**，
//     不能 DELETE。漏掉这一步会出现「旧词仍然命中」的幽灵索引。
func (s *Store) Update(id string, input UpdateInput) (Asset, error) {
	if strings.TrimSpace(id) == "" {
		return Asset{}, errors.New("需要资产 id")
	}
	old, err := s.Get(id)
	if err != nil {
		return Asset{}, err
	}
	if old.Immutable {
		return Asset{}, errors.New("kind='doc' 的正文不可改写（immutable=1）：请改为产出派生资产或只改 status")
	}
	body := old.Body
	if input.Body != "" && input.Body != old.Body {
		body = input.Body
	}
	title := old.Title
	if strings.TrimSpace(input.Title) != "" && input.Title != old.Title {
		title = strings.TrimSpace(input.Title)
	} else if body != old.Body && (old.Title == "" || old.Title == titleFallback(old.Body)) {
		// 标题此前是自动回落的，正文变了就跟着变；手写标题不动。
		title = titleFallback(body)
	}
	bodyChanged := body != old.Body
	titleChanged := title != old.Title
	if !bodyChanged && !titleChanged && len(input.AddTags) == 0 {
		return old, nil
	}

	waits := []time.Duration{0, 20 * time.Millisecond, 60 * time.Millisecond, 180 * time.Millisecond}
	var lastErr error
	for attempt, wait := range waits {
		if wait > 0 {
			time.Sleep(wait)
		}
		if lastErr = s.updateTx(old, body, title, input.AddTags, bodyChanged); lastErr == nil {
			updated, err := s.Get(id)
			if err != nil {
				return Asset{}, err
			}
			return updated, nil
		}
		if !isBusy(lastErr) {
			return Asset{}, lastErr
		}
		if attempt == len(waits)-1 {
			break
		}
	}
	return Asset{}, lastErr
}

func (s *Store) updateTx(old Asset, body, title string, addTags []string, bodyChanged bool) error {
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

	// 1) 清掉旧 FTS 行。contentless 表只能用 'delete' 命令并带上原始列值，
	//    所以先取旧 chunk 文本 + 旧标题，按当初写入时的同一套 bigram 重算。
	oldChunks, err := chunksOf(ctx, conn, old.ID)
	if err != nil {
		return err
	}
	oldTitleNgram := index.Bigrams(old.Title)
	for _, chunk := range oldChunks {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO chunks_fts(chunks_fts, rowid, title_ngram, text_ngram, asset_id, seq) VALUES('delete',?,?,?,?,?)`,
			chunk.rowID, oldTitleNgram, index.Bigrams(chunk.text), old.ID, chunk.seq); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM chunks WHERE asset_id=?`, old.ID); err != nil {
		return err
	}

	// 2) 改资产本体
	//
	// 时间戳必须严格前进：同一毫秒内改两次会让 updated_at 相等，
	// 而增量注入的水位线是 `> since`，相等就等于「永远不注入」。
	nowMillis := now()
	if nowMillis <= old.UpdatedAt {
		nowMillis = old.UpdatedAt + 1
	}
	if bodyChanged {
		if _, err := conn.ExecContext(ctx,
			`UPDATE assets SET body=?,title=?,content_hash=?,updated_at=? WHERE id=?`,
			body, title, Hash(body), nowMillis, old.ID); err != nil {
			return err
		}
	} else if title != old.Title {
		// 只改标题不动 updated_at：它是「变化才注入」的水位线，硬约束 14 只认正文变化。
		if _, err := conn.ExecContext(ctx, `UPDATE assets SET title=? WHERE id=?`, title, old.ID); err != nil {
			return err
		}
	}
	for _, tag := range addTags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO asset_tags(asset_id,tag) VALUES(?,?)`, old.ID, tag); err != nil {
			return err
		}
	}

	// 3) 重新切块 + 重建 FTS
	chunks := index.ChunkText(body, s.Config.ChunkTarget, s.Config.ChunkMax, s.Config.ChunkMin, s.Config.ChunkOverlap)
	if len(chunks) == 0 {
		return errors.New("无法生成切片")
	}
	for seq, chunk := range chunks {
		chunkID := ID()
		result, err := conn.ExecContext(ctx, `INSERT INTO chunks(id,asset_id,seq,text,start_off,end_off) VALUES(?,?,?,?,?,?)`,
			chunkID, old.ID, seq, chunk.Text, chunk.StartOff, chunk.EndOff)
		if err != nil {
			return err
		}
		rowID, _ := result.LastInsertId()
		if _, err := conn.ExecContext(ctx, `INSERT INTO chunks_fts(rowid,title_ngram,text_ngram,asset_id,seq) VALUES(?,?,?,?,?)`,
			rowID, index.Bigrams(title), index.Bigrams(chunk.Text), old.ID, seq); err != nil {
			return err
		}
	}

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO events(id,run_id,seq,op,asset_id,before_hash,after_hash,decision,at) VALUES(?,NULL,1,'update',?,?,?,?,?)`,
		ID(), old.ID, old.ContentHash, Hash(body), "keep", nowMillis); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

type storedChunk struct {
	rowID int64
	seq   int
	text  string
}

// chunksOf 走传入的 conn 而不是 s.DB：
// 连接池是单连接（MaxOpenConns(1)），事务已经占着它，再用 s.DB 查会自锁。
func chunksOf(ctx context.Context, conn *sql.Conn, assetID string) ([]storedChunk, error) {
	rows, err := conn.QueryContext(ctx, `SELECT rowid,seq,text FROM chunks WHERE asset_id=? ORDER BY seq`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedChunk
	for rows.Next() {
		var c storedChunk
		if err := rows.Scan(&c.rowID, &c.seq, &c.text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// titleFallback 是标题回落规则：正文首个非空行前 40 字（§9.4.3）。
func titleFallback(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#> "))
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > 40 {
			return string(runes[:40]) + "…"
		}
		return line
	}
	return ""
}
