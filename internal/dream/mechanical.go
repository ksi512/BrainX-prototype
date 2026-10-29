package dream

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/store"
)

// 机械层：纯规则、零模型、可定时。它只做四件事——衰减、淘汰、去重、留痕。
//
// 与判断层的分界：**机械层永远不改变任何正文**，只动
// importance / use_count / status / merged_into 与 edges。
// 「合并两条措辞不同的记忆」是语义判断，属于 --plan/--apply。
const (
	metaTidyLastRun = "tidy:last_run"
	evictImportance = 15.0
	evictIdleDays   = 90.0
)

// Mechanical 跑一次机械层。返回摘要，没什么可做时返回空串。
//
// interval 未到期且 force=false 时直接返回：Stop hook 每次都会调它，
// 不设闸门就变成「每轮都全表扫」。
func Mechanical(s *store.Store, force bool) (string, error) {
	last := metaInt(s, metaTidyLastRun)
	nowMillis := time.Now().UnixMilli()
	interval := time.Duration(24) * time.Hour
	if elapsed := nowMillis - last; !force && last > 0 && time.Duration(elapsed)*time.Millisecond < interval {
		return "", nil
	}

	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	elapsedDays := 1.0
	if last > 0 {
		elapsedDays = math.Max(float64(nowMillis-last)/86400000.0, 1.0/24.0)
	}
	lambda := s.Config.DecayLambda
	if lambda <= 0 {
		lambda = 0.005
	}
	factor := math.Exp(-lambda * elapsedDays)

	var lines []string

	// ── 1. 衰减：只降不升。召回强化（importance+2）是它的对冲，
	// 两者必须同时存在，否则常用记忆会被衰减慢慢杀掉。
	decayed := 0
	// 单轮上限 2000 条：Stop 的预算是 400ms，10 万条库上「一次全衰减」会直接超时。
	// 按最久未用的先衰减，剩下的下一轮继续——衰减本来就是渐进过程，不需要一次做完。
	rows, err := conn.QueryContext(ctx,
		`SELECT id,importance,COALESCE(last_used_at,0) FROM assets WHERE status='active' AND layer>0
		 ORDER BY COALESCE(last_used_at,0), updated_at LIMIT 2000`)
	if err != nil {
		return "", err
	}
	type pending struct {
		id     string
		before float64
		after  float64
	}
	var updates []pending
	for rows.Next() {
		var id string
		var importance float64
		var lastUsed int64
		if err := rows.Scan(&id, &importance, &lastUsed); err != nil {
			rows.Close()
			return "", err
		}
		// 上次整理之后被召回过的，不衰减：使用即投票。
		if lastUsed > last && last > 0 {
			continue
		}
		after := math.Max(5, importance*factor)
		if after >= importance-0.01 {
			continue
		}
		updates = append(updates, pending{id: id, before: importance, after: after})
	}
	rows.Close()
	for _, item := range updates {
		if _, err := conn.ExecContext(ctx, `UPDATE assets SET importance=? WHERE id=?`, item.after, item.id); err != nil {
			return "", err
		}
		decayed++
	}
	if decayed > 0 {
		lines = append(lines, fmt.Sprintf("衰减 %d 条（%.2f 天 × λ=%.3f）", decayed, elapsedDays, lambda))
	}

	// ── 2. 去重：content_hash 完全相同才合并。近似重复留给判断层。
	merged := 0
	dupRows, err := conn.QueryContext(ctx,
		`SELECT content_hash FROM assets WHERE status='active' AND kind<>'doc' GROUP BY content_hash HAVING COUNT(*)>1`)
	if err != nil {
		return "", err
	}
	var hashes []string
	for dupRows.Next() {
		var h string
		if err := dupRows.Scan(&h); err != nil {
			dupRows.Close()
			return "", err
		}
		hashes = append(hashes, h)
	}
	dupRows.Close()
	for _, h := range hashes {
		ids, err := idsByHash(ctx, conn, h)
		if err != nil {
			return "", err
		}
		if len(ids) < 2 {
			continue
		}
		keep := ids[0]
		for _, id := range ids[1:] {
			if _, err := conn.ExecContext(ctx, `UPDATE assets SET status='merged',merged_into=?,archive_reason='merged' WHERE id=?`, keep, id); err != nil {
				return "", err
			}
			if _, err := conn.ExecContext(ctx,
				`INSERT OR IGNORE INTO edges(id,from_id,to_id,rel,created_at) VALUES(?,?,?,?,?)`,
				store.ID(), id, keep, "alias_of", nowMillis); err != nil {
				return "", err
			}
			merged++
		}
	}
	if merged > 0 {
		lines = append(lines, fmt.Sprintf("去重合并 %d 条（content_hash 完全相同）", merged))
	}

	// ── 3. 尾部淘汰：容量驱动，不是「重要度低于某个值」驱动。
	//
	// 原版口径：active 总量 > tidy.capacity（默认 5000）才开始，每次淘汰尾部 tidy.tail_pct（10%），
	// **kind='doc' 一律排除**——文档是知识本体，不该因为「最近没人看」被下架。
	capacity := s.Config.TidyCapacity
	tailPct := s.Config.TidyTailPct
	if capacity <= 0 {
		capacity = 5000
	}
	if tailPct <= 0 || tailPct > 50 {
		tailPct = 10
	}
	var activeCount int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets WHERE status='active' AND kind<>'doc'`).Scan(&activeCount); err != nil {
		return "", err
	}
	if activeCount > capacity {
		target := activeCount * tailPct / 100
		if target < 1 {
			target = 1
		}
		// use_count=0 的优先被淘汰，其次 importance 最低、最久没动的
		res, err := conn.ExecContext(ctx,
			`UPDATE assets SET status='archived',archive_reason='evicted'
			 WHERE id IN (
			   SELECT id FROM assets WHERE status='active' AND kind<>'doc'
			   ORDER BY use_count ASC, importance ASC, COALESCE(last_used_at,0) ASC
			   LIMIT ?
			 )`, target)
		if err != nil {
			return "", err
		}
		if evicted, _ := res.RowsAffected(); evicted > 0 {
			lines = append(lines, fmt.Sprintf("尾部淘汰 %d 条（active %d > 容量 %d，淘汰 %d%%）", evicted, activeCount, capacity, tailPct))
		}
	}

	if _, err := conn.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		metaTidyLastRun, strconv.FormatInt(nowMillis, 10)); err != nil {
		return "", err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", err
	}
	committed = true
	if len(lines) == 0 {
		return "机械层：无可做项", nil
	}
	return "机械层：" + strings.Join(lines, "；"), nil
}

// idsByHash 按 content_hash 取 active 资产，**最早创建的排第一**——
// 合并时保留最早的，是因为「先记下的那条」通常带着当时的上下文。
func idsByHash(ctx context.Context, conn *sql.Conn, hash string) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT id FROM assets WHERE content_hash=? AND status='active' AND kind<>'doc' ORDER BY created_at, id`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func metaInt(s *store.Store, key string) int64 {
	value, err := s.Meta(key)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(value, 10, 64)
	return n
}
