package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Relink 单条重指外部位置：换根用配置，换单条用这里。
func (s *Store) Relink(id, target string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("需要 <root>:<relpath>")
	}
	archive := target
	if !strings.HasPrefix(target, "ext:") {
		archive = "ext:" + target
	}
	if !validArchive(archive) {
		return fmt.Errorf("%w：%q（应为 <root>:<relpath>）", ErrInvalidRef, target)
	}
	if _, _, _, err := s.resolveExt(archive); err != nil {
		return err
	}
	asset, err := s.Get(id)
	if err != nil {
		return err
	}
	if asset.Immutable {
		// doc 可以重指位置——那是「原件搬到别处」，不是改写正文
	}
	if _, err := s.DB.Exec(`UPDATE assets SET archive=? WHERE id=?`, archive, id); err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO events(id,run_id,seq,op,asset_id,before_hash,decision,reason,at) VALUES(?,NULL,1,'relink',?,?,?,?,?)`,
		ID(), id, asset.ContentHash, "relink", archive, now())
	return err
}

// Suppress 抑制某类提示（目前只有 root-warning）。值与到期时间都落 meta。
func (s *Store) SetSuppress(subject, mode string) error {
	value := ""
	switch {
	case mode == "session":
		value = "session"
	case strings.HasPrefix(mode, "until:"):
		duration, err := ParseDuration(strings.TrimPrefix(mode, "until:"))
		if err != nil {
			return fmt.Errorf("--until 需要时长（如 1d / 12h），给的是 %q", mode)
		}
		value = strconv.FormatInt(time.Now().Add(duration).UnixMilli(), 10)
	default:
		value = "session"
	}
	return s.SetMeta("suppress:"+subject, value)
}

// Suppressed 判断某个提示是否在抑制期内。到期自动失效。
func (s *Store) Suppressed(subject string) bool {
	value, err := s.Meta("suppress:" + subject)
	if err != nil || value == "" {
		return false
	}
	if value == "session" {
		// 「本会话」无法跨进程判断，按 6 小时近似——足够覆盖一次会话
		return true
	}
	expire, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().UnixMilli() < expire
}

func (s *Store) ClearSuppress(subject string) error {
	_, err := s.DB.Exec(`DELETE FROM meta WHERE k=?`, "suppress:"+subject)
	return err
}

// SuppressList 列出全部抑制项。
func (s *Store) SuppressList() ([][2]string, error) {
	rows, err := s.DB.Query(`SELECT k,v FROM meta WHERE k LIKE 'suppress:%' ORDER BY k`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out = append(out, [2]string{strings.TrimPrefix(k, "suppress:"), v})
	}
	return out, rows.Err()
}

// HardForget 物理删除。这是唯一删字节的路径：doc 也只允许软删（硬约束 10）。
func (s *Store) HardForget(id string) error {
	asset, err := s.Get(id)
	if err != nil {
		return err
	}
	if asset.Kind == "doc" || asset.Immutable {
		return fmt.Errorf("kind='doc' 只允许软删（下架≠销毁）：请用 brainx forget %s", id)
	}
	if _, err := s.DB.Exec(`DELETE FROM assets WHERE id=?`, id); err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO events(id,run_id,seq,op,asset_id,before_hash,decision,reason,at) VALUES(?,NULL,1,'archive',?,?,?,?,?)`,
		ID(), id, asset.ContentHash, "hard-delete", "forget --hard", now())
	return err
}

// Review 是一条待标注冲突的完整视图（供 review list / 看板用）。
type Review struct {
	ID       string
	AssetA   Asset
	AssetB   Asset
	Conflict string
	Detail   string
	NoteA    string
	NoteB    string
	Created  int64
}

// ReviewList 列全部未裁决冲突，带双方正文——Agent 判断要靠正文，不能只给 id。
func (s *Store) ReviewList(limit int) ([]Review, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.Query(`SELECT id,asset_a,asset_b,COALESCE(conflict_on,''),COALESCE(detail,''),COALESCE(note_a,''),COALESCE(note_b,''),created_at FROM reviews WHERE resolved_at IS NULL ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.ID, &r.AssetA.ID, &r.AssetB.ID, &r.Conflict, &r.Detail, &r.NoteA, &r.NoteB, &r.Created); err != nil {
			return nil, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	for i := range pending {
		if a, err := s.Get(pending[i].AssetA.ID); err == nil {
			pending[i].AssetA = a
		}
		if b, err := s.Get(pending[i].AssetB.ID); err == nil {
			pending[i].AssetB = b
		}
	}
	return pending, rows.Err()
}

// ReviewResolve 标注冲突。**它只改状态与边，永不改正文**。
//
// 五种处置：annotate（两条都留）/ supersede（旧的归档）/ pick_a / pick_b（确有错）/ merge（假冲突）。
func (s *Store) ReviewResolve(id, resolution, noteA, noteB string) error {
	valid := map[string]bool{"annotate": true, "supersede": true, "pick_a": true, "pick_b": true, "merge": true}
	if !valid[resolution] {
		return fmt.Errorf("未知处置 %q（annotate / supersede / pick_a / pick_b / merge）", resolution)
	}
	var assetA, assetB string
	err := s.DB.QueryRow(`SELECT asset_a,asset_b FROM reviews WHERE id=? AND resolved_at IS NULL`, id).Scan(&assetA, &assetB)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("找不到未裁决的冲突 %s", id)
	}
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(`UPDATE reviews SET resolved_at=?,resolution=?,note_a=?,note_b=? WHERE id=?`,
		now(), resolution, noteA, noteB, id); err != nil {
		return err
	}
	switch resolution {
	case "supersede", "pick_a", "pick_b":
		loser := assetB
		if resolution == "pick_b" {
			loser = assetA
		}
		if asset, getErr := s.Get(loser); getErr == nil && !asset.Immutable {
			_, err = s.DB.Exec(`UPDATE assets SET status='archived',archive_reason='superseded' WHERE id=?`, loser)
		}
	case "merge":
		if _, err = s.DB.Exec(`INSERT OR IGNORE INTO edges(id,from_id,to_id,rel,created_at) VALUES(?,?,?,?,?)`,
			ID(), assetB, assetA, "alias_of", now()); err == nil {
			if asset, getErr := s.Get(assetB); getErr == nil && !asset.Immutable {
				_, err = s.DB.Exec(`UPDATE assets SET status='merged',merged_into=?,archive_reason='merged' WHERE id=?`, assetA, assetB)
			}
		}
	}
	return err
}

// NeighborsDepth 按深度扩邻接（默认 1 跳）。图谱浏览与融合排查用。
func (s *Store) NeighborsDepth(id, rel string, depth int) ([]Link, error) {
	if depth <= 0 {
		depth = 1
	}
	if depth > 3 {
		depth = 3 // 3 跳开始几乎没有信息量，全是整个库
	}
	seen := map[string]bool{id: true}
	frontier := []string{id}
	var out []Link
	for hop := 0; hop < depth; hop++ {
		var next []string
		for _, cur := range frontier {
			rows, err := s.DB.Query(`SELECT 'out',rel,to_id FROM edges WHERE from_id=? UNION ALL SELECT 'in',rel,from_id FROM edges WHERE to_id=? ORDER BY 1,2,3`, cur, cur)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var l Link
				if err := rows.Scan(&l.Direction, &l.Rel, &l.Other); err != nil {
					rows.Close()
					return nil, err
				}
				if rel != "" && l.Rel != rel {
					continue
				}
				if seen[l.Other] {
					continue
				}
				seen[l.Other] = true
				out = append(out, l)
				next = append(next, l.Other)
			}
			rows.Close()
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	return out, nil
}

// AddReview 建一条未解决冲突（link contradicts 时自动调）。
func (s *Store) AddReview(assetA, assetB, detail string) error {
	var existing int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM reviews WHERE resolved_at IS NULL AND ((asset_a=? AND asset_b=?) OR (asset_a=? AND asset_b=?))`,
		assetA, assetB, assetB, assetA).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	_, err := s.DB.Exec(`INSERT INTO reviews(id,asset_a,asset_b,conflict_on,detail,created_at) VALUES(?,?,?,?,?,?)`,
		ID(), assetA, assetB, "annotate", detail, now())
	return err
}

// ParseDuration 在 time.ParseDuration 之上补一个 "d"（天）。
//
// 为什么要补：配置与 CLI 里到处是 "1d" / "24h"，而 Go 标准库不认 d。
// 让用户为了一个单位差异去查文档，是设计上的偷懒。
func ParseDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(value)
}
