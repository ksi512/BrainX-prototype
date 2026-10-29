package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ScopeClause 返回读语义下的 scope 过滤条件。
//
// 读语义（提示词 §4.3）：project scope 同时可见 global——用户记在 global 的
// 跨项目偏好必须在任何项目里都能被召回，否则「记了等于没记」。
// 写语义不在这里，写入必须显式 scope。
func ScopeClause(column, scope string) (string, []any) {
	scope = strings.TrimSpace(scope)
	switch {
	case scope == "":
		return "", nil
	case scope == "global":
		return column + "='global'", nil
	case strings.HasPrefix(scope, "proj:"):
		return "(" + column + "='global' OR " + column + "=?)", []any{scope}
	default:
		return column + "=?", []any{scope}
	}
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// GetMany 批量取资产。返回 map 而不是切片：调用方拿到的是一组 id，
// 顺序由调用方的排序决定，不能被 SQL 的返回顺序偷偷带走。
func (s *Store) GetMany(ids []string) (map[string]Asset, error) {
	out := map[string]Asset{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	query := `SELECT id,kind,scope,layer,COALESCE(title,''),body,importance,confidence,status,COALESCE(origin,''),archive,content_hash,COALESCE(merged_into,''),immutable,created_at,updated_at,use_count FROM assets WHERE id IN (` + placeholders(len(ids)) + `)`
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

// ExpandEdges 从 seeds 出发沿 edges 扩 hops 跳，返回 id → 最小跳数。
//
// contradicts 边被排除：冲突方不是「相关内容」，它是「需要人裁决的对立项」，
// 由 OpenConflicts 单独处理。混进来会让冲突互相抬分，形成自激。
func (s *Store) ExpandEdges(seeds []string, hops int) (map[string]int, error) {
	visited := map[string]int{}
	frontier := make([]string, 0, len(seeds))
	for _, id := range seeds {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := visited[id]; ok {
			continue
		}
		visited[id] = 0
		frontier = append(frontier, id)
	}
	for hop := 1; hop <= hops && len(frontier) > 0; hop++ {
		marks := placeholders(len(frontier))
		args := make([]any, 0, len(frontier)*2+1)
		for _, id := range frontier {
			args = append(args, id)
		}
		for _, id := range frontier {
			args = append(args, id)
		}
		args = append(args, "contradicts")
		query := `SELECT from_id,to_id FROM edges WHERE (from_id IN (` + marks + `) OR to_id IN (` + marks + `)) AND rel<>?`
		rows, err := s.DB.Query(query, args...)
		if err != nil {
			return nil, err
		}
		var next []string
		for rows.Next() {
			var from, to string
			if err := rows.Scan(&from, &to); err != nil {
				rows.Close()
				return nil, err
			}
			for _, id := range []string{from, to} {
				if _, ok := visited[id]; ok {
					continue
				}
				visited[id] = hop
				next = append(next, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		frontier = next
	}
	return visited, nil
}

// Conflict 是一条未解决的冲突，A 是召回命中的那条，B 是配对项。
type Conflict struct {
	A        string
	B        string
	Detail   string
	NoteA    string
	NoteB    string
	Conflict string
}

// OpenConflicts 取命中项挂着的未解决冲突。
//
// 双来源：reviews（标注队列，人工裁决的入口）+ edges(rel='contradicts')（Agent 直接连的边）。
// 只看 reviews 会漏掉「连了边但没建 review」的情况，只看 edges 会漏掉 review 里记的裁决线索。
func (s *Store) OpenConflicts(ids []string) ([]Conflict, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	marks := placeholders(len(ids))
	args := make([]any, 0, len(ids)*2)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	query := `SELECT asset_a,asset_b,COALESCE(conflict_on,''),COALESCE(detail,''),COALESCE(note_a,''),COALESCE(note_b,'') FROM reviews WHERE resolved_at IS NULL AND (asset_a IN (` + marks + `) OR asset_b IN (` + marks + `))`
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var out []Conflict
	seen := map[string]bool{}
	for rows.Next() {
		var a, b, on, detail, noteA, noteB string
		if err := rows.Scan(&a, &b, &on, &detail, &noteA, &noteB); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, Conflict{A: a, B: b, Conflict: on, Detail: detail, NoteA: noteA, NoteB: noteB})
		seen[a+"|"+b] = true
		seen[b+"|"+a] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	edgeArgs := append([]any{}, args...)
	edgeQuery := `SELECT from_id,to_id FROM edges WHERE rel='contradicts' AND (from_id IN (` + marks + `) OR to_id IN (` + marks + `))`
	edgeRows, err := s.DB.Query(edgeQuery, edgeArgs...)
	if err != nil {
		return nil, err
	}
	defer edgeRows.Close()
	for edgeRows.Next() {
		var from, to string
		if err := edgeRows.Scan(&from, &to); err != nil {
			return nil, err
		}
		if seen[from+"|"+to] {
			continue
		}
		out = append(out, Conflict{A: from, B: to, Conflict: "annotate", Detail: "两条都仍是 active，未裁决"})
		seen[from+"|"+to] = true
	}
	return out, edgeRows.Err()
}

// ListSince 取 scope 内某时间点之后更新的 active 资产，按时序路的需要排序前先全取。
// 上限用于兜底：时序路是「重要 + 近期」的偏好排序，不需要全库。
func (s *Store) ListSince(scope, kind string, since int64, limit int) ([]Asset, error) {
	clause, args := ScopeClause("scope", scope)
	query := `SELECT id,kind,scope,layer,COALESCE(title,''),body,importance,confidence,status,COALESCE(origin,''),archive,content_hash,COALESCE(merged_into,''),immutable,created_at,updated_at,use_count FROM assets WHERE status='active' AND layer>0`
	if clause != "" {
		query += " AND " + clause
	}
	if clause, kindArgs := KindClause("kind", kind); clause != "" {
		query += " AND " + clause
		args = append(args, kindArgs...)
	}
	if since > 0 {
		query += " AND updated_at>=?"
		args = append(args, since)
	}
	query += " ORDER BY importance DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var assets []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

// OpenReviewCount 是 status/context 用的待裁决冲突计数。
func (s *Store) OpenReviewCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM reviews WHERE resolved_at IS NULL`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Strengthen 是召回强化：只动 importance / use_count / last_used_at 三个字段。
//
// **绝不能碰 updated_at**——它是「变化才注入」的水位线，每轮都变会导致
// 每轮重复注入同一批内容，白烧 token（硬约束 14）。
func (s *Store) Strengthen(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	stmt := `UPDATE assets SET importance=MIN(100,importance+2),use_count=use_count+1,last_used_at=? WHERE id=?`
	for _, id := range ids {
		if _, err := s.DB.ExecContext(ctx, stmt, now(), id); err != nil {
			return err
		}
	}
	return nil
}

// KindClause 支持 `--kind fact,skill` 这种多选。单值走等号，多值走 IN，
// 避免为了「支持多选」把所有查询都写成子查询。
func KindClause(column, kind string) (string, []any) {
	parts := strings.Split(kind, ",")
	values := make([]any, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			values = append(values, part)
		}
	}
	switch len(values) {
	case 0:
		return "", nil
	case 1:
		return column + "=?", values
	default:
		return column + " IN (" + placeholders(len(values)) + ")", values
	}
}

// OpenReviewList 列全部未裁决冲突，供 `confirm --pending` / `context --pending` 使用。
func (s *Store) OpenReviewList(limit int) ([]Conflict, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.Query(`SELECT asset_a,asset_b,COALESCE(conflict_on,''),COALESCE(detail,''),COALESCE(note_a,''),COALESCE(note_b,'') FROM reviews WHERE resolved_at IS NULL ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conflict
	for rows.Next() {
		var c Conflict
		if err := rows.Scan(&c.A, &c.B, &c.Conflict, &c.Detail, &c.NoteA, &c.NoteB); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Link 是一条关系。Direction 用 out/in 表示「从本资产出发」还是「指向本资产」。
type Link struct {
	Direction string
	Rel       string
	Other     string
}

// Neighbors 取 1 跳邻居：出边与入边都要。只看出边会漏掉「谁引用了我」，
// 而反向链接是判断一条记忆是否还有用的主要线索。
func (s *Store) Neighbors(id string) ([]Link, error) {
	rows, err := s.DB.Query(`SELECT 'out',rel,to_id FROM edges WHERE from_id=? UNION ALL SELECT 'in',rel,from_id FROM edges WHERE to_id=? ORDER BY 1,2,3`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Direction, &l.Rel, &l.Other); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
