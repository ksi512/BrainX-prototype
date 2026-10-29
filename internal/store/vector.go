package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/ksi512/BrainX-prototype/internal/index"
)

// Embedder 从配置造一个 embeddings 客户端。只在 vector.enabled 时才真的会连。
func (s *Store) Embedder() *index.Embedder {
	if !s.Config.VectorEnabled {
		return nil
	}
	return index.NewEmbedder(s.Config.VectorEndpoint, s.Config.VectorModel, "")
}

// EmbedAsset 给一条资产的切片算向量。**失败不阻断写入**——
// 向量是可选的增强，正文与 FTS 才是主路径；算不出来就留空，检索时该片只走 FTS5。
func (s *Store) EmbedAsset(assetID string) error {
	embedder := s.Embedder()
	if !embedder.Enabled() {
		return nil
	}
	rows, err := s.DB.Query(`SELECT rowid,text FROM chunks WHERE asset_id=? ORDER BY seq`, assetID)
	if err != nil {
		return err
	}
	type chunkRow struct {
		rowID int64
		text  string
	}
	var chunks []chunkRow
	for rows.Next() {
		var c chunkRow
		if err := rows.Scan(&c.rowID, &c.text); err != nil {
			rows.Close()
			return err
		}
		chunks = append(chunks, c)
	}
	rows.Close()
	if len(chunks) == 0 {
		return nil
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.text
	}
	// 一次批量请求：分片大小受服务端限制，这里按 32 条一批
	const batch = 32
	for start := 0; start < len(texts); start += batch {
		end := start + batch
		if end > len(texts) {
			end = len(texts)
		}
		vectors, err := embedder.Embed(context.Background(), texts[start:end])
		if err != nil {
			return err
		}
		for i, vector := range vectors {
			chunk := chunks[start+i]
			tag := embedder.ModelTag(len(vector))
			if _, err := s.DB.Exec(`UPDATE chunks SET embedding=?,dim=?,embed_model=? WHERE rowid=?`,
				index.EncodeVector(vector), len(vector), tag, chunk.rowID); err != nil {
				return err
			}
		}
	}
	return nil
}

// EmbeddedChunk 是向量检索的候选。
type EmbeddedChunk struct {
	AssetID string
	Text    string
	Vector  []float32
}

// LoadEmbeddings 取当前模型下的全部向量。按 embed_model 精确过滤：
// 换过模型的旧向量**不能参与**计算，否则余弦失去意义且不报错（§3.4）。
func (s *Store) LoadEmbeddings(scope, kind, modelTag string) ([]EmbeddedChunk, error) {
	query := `SELECT c.asset_id,c.text,c.embedding FROM chunks c JOIN assets a ON a.id=c.asset_id
		WHERE a.status='active' AND c.embedding IS NOT NULL AND c.embed_model=?`
	args := []any{modelTag}
	if clause, clauseArgs := ScopeClause("a.scope", scope); clause != "" {
		query += " AND " + clause
		args = append(args, clauseArgs...)
	}
	if clause, kindArgs := KindClause("a.kind", kind); clause != "" {
		query += " AND " + clause
		args = append(args, kindArgs...)
	}
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmbeddedChunk
	for rows.Next() {
		var chunk EmbeddedChunk
		var blob []byte
		if err := rows.Scan(&chunk.AssetID, &chunk.Text, &blob); err != nil {
			return nil, err
		}
		chunk.Vector = index.DecodeVector(blob)
		if len(chunk.Vector) == 0 {
			continue
		}
		out = append(out, chunk)
	}
	return out, rows.Err()
}

// StaleEmbeddingStats 给 doctor 用：有多少 chunk 的向量来自旧模型。
//
// current 传的是**模型名**（不含维度）：embed_model 里存的是 `model:dim`，
// 维度可能因服务端配置变化，不该被当成「换了模型」。
func (s *Store) StaleEmbeddingStats(current string) (stale int, models []string, err error) {
	rows, err := s.DB.Query(`SELECT COALESCE(embed_model,''),COUNT(*) FROM chunks WHERE embedding IS NOT NULL GROUP BY embed_model`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tag string
		var count int
		if err := rows.Scan(&tag, &count); err != nil {
			return 0, nil, err
		}
		models = append(models, fmt.Sprintf("%s×%d", tag, count))
		if current != "" && tag != current && !strings.HasPrefix(tag, current+":") {
			stale += count
		}
	}
	sort.Strings(models)
	return stale, models, rows.Err()
}

var _ = sql.ErrNoRows
var _ = strings.TrimSpace
