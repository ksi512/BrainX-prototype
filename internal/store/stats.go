package store

import (
	"database/sql"
	"time"
)

// ───────────────────────────── 召回日志 ─────────────────────────────

// LogRecall 记录一次召回：一条 recalls 汇总行 + 每条命中一行 recall_hits。
//
// 为什么必须单独立表而不是靠 use_count：
//   - `use_count` 是**累计值**，一条三年前的老记忆天然比上周的新记忆高，
//     拿它排「近 30 天最常用」等于排「谁先入库」。
//   - 看板要的是**时间窗内**的活跃度，只有逐次记录才能按窗口切。
//
// 零命中也要记：查询没命中说明「这个词库里没有」，是缺口的信号，
// 只记命中就等于永远不会发现这类缺口。
func (s *Store) LogRecall(scope, query, sources string, hitIDs []string) error {
	at := now()
	id := ID()
	if _, err := s.DB.Exec(`INSERT INTO recalls(id,at,scope,query,hits,sources) VALUES(?,?,?,?,?,?)`,
		id, at, scope, query, len(hitIDs), sources); err != nil {
		return err
	}
	if len(hitIDs) == 0 {
		return nil
	}
	stmt, err := s.DB.Prepare(`INSERT INTO recall_hits(recall_id,asset_id,at) VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, assetID := range hitIDs {
		if _, err := stmt.Exec(id, assetID, at); err != nil {
			return err
		}
	}
	return nil
}

// LogSource 落一行召回汇总、不写命中明细。给 context / hook 注入用：
// 那两处每次注入十几条，写明细会把 hook 的 100ms 预算吃光，
// 而它们只需要「今天注入过几次」这个计数。
func (s *Store) LogSource(scope, kind string, hits int) error {
	_, err := s.DB.Exec(`INSERT INTO recalls(id,at,scope,query,hits,sources) VALUES(?,?,?,?,?,?)`,
		ID(), now(), scope, "", hits, kind)
	return err
}

// ───────────────────────────── 看板数据 ─────────────────────────────

// StatAsset 是看板里的一行资产。只带看板会用到的字段，不塞整个 Asset。
type StatAsset struct {
	ID, Kind, Title, TitleSource string
	Importance                   float64
	UseCount                     int
	CreatedAt, LastUsedAt        int64
	Recalls30                    int
}

// DayStat 是 30 天图里的一个点。
type DayStat struct {
	Label   string // MM-DD
	Imports int
	Recalls int
}

// KindStat 是资产构成的一段。
type KindStat struct {
	Name  string
	Count int
	Share int
}

// PendingGroup 是「待确认」按原因分组。
type PendingGroup struct {
	Reason string
	Count  int
	Items  []StatAsset
}

// ReviewItem 是一条未裁决冲突。
type ReviewItem struct {
	ID, A, B, ATitle, BTitle, Conflict, Detail string
	CreatedAt                                  int64
}

// Stats 是看板的全部数据。**一次快照**：页面上的所有数字来自同一时刻，
// 不能这一块查一次、那一块再查一次——中间写入会让页面自相矛盾。
type Stats struct {
	Generated                 string
	Home                      string
	Total, Active, Archived   int
	Pending, PendingInsight   int
	Today, Last7, Prev7       int
	WeekDelta                 int // 近 7 日对上一 7 日的环比（百分比，正负）
	OpenReviews               int
	QueuePending, QueueDead   int
	Days                      []DayStat
	MaxImports, MaxRecalls    int
	ImportMean, RecallMean    float64
	Kinds                     []KindStat
	PendingGroups             []PendingGroup
	Reviews                   []ReviewItem
	MostUsed, NearDecay       []StatAsset
	NeverUsed                 []StatAsset
	RecallCount30             int
	ZeroHitRecalls30          int
	Chunks, Edges             int
	WALBytes, DBBytes, FileMB int
	ConfigJSON                string
	SchemaJSON                string
	Roots                     map[string]string
}

const dayMillis = int64(24 * time.Hour / time.Millisecond)

func localDayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// BuildStats 收集看板需要的全部数字。只读，不写库。
func (s *Store) BuildStats() (Stats, error) {
	st := Stats{
		Generated: time.Now().Format("2006-01-02 15:04"),
		Home:      s.Home,
		Roots:     s.Config.Roots,
	}
	if st.Roots == nil {
		st.Roots = map[string]string{}
	}

	// ── 计数 ──
	rows, err := s.DB.Query(`SELECT status,COUNT(*) FROM assets GROUP BY status`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return st, err
		}
		st.Total += count
		switch status {
		case "active":
			st.Active = count
		case "archived":
			st.Archived = count
		case "pending":
			st.Pending = count
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='pending' AND kind='insight'`).Scan(&st.PendingInsight); err != nil {
		return st, err
	}

	nowT := time.Now()
	todayStart := localDayStart(nowT)
	st.Today = s.countSince(todayStart.UnixMilli())
	st.Last7 = s.countSince(todayStart.AddDate(0, 0, -6).UnixMilli())
	st.Prev7 = s.countBetween(todayStart.AddDate(0, 0, -13).UnixMilli(), todayStart.AddDate(0, 0, -6).UnixMilli())
	if st.Prev7 > 0 {
		st.WeekDelta = (st.Last7 - st.Prev7) * 100 / st.Prev7
	} else if st.Last7 > 0 {
		st.WeekDelta = 100
	}

	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM reviews WHERE resolved_at IS NULL`).Scan(&st.OpenReviews); err != nil {
		return st, err
	}
	st.QueuePending, st.QueueDead = s.QueueDepth()
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&st.Chunks); err != nil {
		return st, err
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&st.Edges); err != nil {
		return st, err
	}

	// ── 30 天系列 ──
	since := todayStart.AddDate(0, 0, -29)
	imports, err := s.dailyCounts(`SELECT date(created_at/1000,'unixepoch','localtime') d,COUNT(*) FROM assets WHERE created_at>=? GROUP BY d`, since.UnixMilli())
	if err != nil {
		return st, err
	}
	// 折线 = 「一次读」的总次数，**含 context/hook 注入**。
	// 排除注入会让重度用 hook 的人看到一条恒为 0 的线，误以为检索没被用过。
	recalls, err := s.dailyCounts(`SELECT date(at/1000,'unixepoch','localtime') d,COUNT(*) FROM recalls WHERE at>=? GROUP BY d`, since.UnixMilli())
	if err != nil {
		return st, err
	}
	sumImports, sumRecalls := 0, 0
	for i := 0; i < 30; i++ {
		day := since.AddDate(0, 0, i)
		key := day.Format("2006-01-02")
		point := DayStat{Label: day.Format("01-02"), Imports: imports[key], Recalls: recalls[key]}
		sumImports += point.Imports
		sumRecalls += point.Recalls
		if point.Imports > st.MaxImports {
			st.MaxImports = point.Imports
		}
		if point.Recalls > st.MaxRecalls {
			st.MaxRecalls = point.Recalls
		}
		st.Days = append(st.Days, point)
	}
	st.ImportMean = float64(sumImports) / 30
	st.RecallMean = float64(sumRecalls) / 30

	// 查询型召回（query 非空）单独统计：零命中才是有意义的缺口信号，
	// 注入没有「命中/未命中」之分，混进来只会把口径搅乱。
	if err := s.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN hits=0 THEN 1 ELSE 0 END),0) FROM recalls WHERE at>=? AND query<>''`, since.UnixMilli()).Scan(&st.RecallCount30, &st.ZeroHitRecalls30); err != nil {
		return st, err
	}

	// ── 资产构成 ──
	kindRows, err := s.DB.Query(`SELECT kind,COUNT(*) FROM assets WHERE status='active' GROUP BY kind ORDER BY 2 DESC,1`)
	if err != nil {
		return st, err
	}
	activeSum := 0
	for kindRows.Next() {
		var item KindStat
		if err := kindRows.Scan(&item.Name, &item.Count); err != nil {
			kindRows.Close()
			return st, err
		}
		activeSum += item.Count
		st.Kinds = append(st.Kinds, item)
	}
	kindRows.Close()
	for i := range st.Kinds {
		if activeSum > 0 {
			st.Kinds[i].Share = st.Kinds[i].Count * 100 / activeSum
		}
	}

	// ── 待办三栏 ──
	if st.PendingGroups, err = s.pendingGroups(); err != nil {
		return st, err
	}
	if st.Reviews, err = s.reviewItems(); err != nil {
		return st, err
	}

	// ── 三条资产榜 ──
	if st.MostUsed, err = s.mostUsed(since.UnixMilli()); err != nil {
		return st, err
	}
	if st.NearDecay, err = s.nearDecay(); err != nil {
		return st, err
	}
	if st.NeverUsed, err = s.neverUsed(); err != nil {
		return st, err
	}

	st.DBBytes = int(fileSize(s.Home + "/store.db"))
	st.WALBytes = int(fileSize(s.Home + "/store.db-wal"))
	st.FileMB = int(dirSize(s.Home+"/files") / (1 << 20))
	return st, nil
}

func (s *Store) countSince(since int64) int {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM assets WHERE created_at>=?`, since).Scan(&n)
	return n
}

func (s *Store) countBetween(from, to int64) int {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM assets WHERE created_at>=? AND created_at<?`, from, to).Scan(&n)
	return n
}

func (s *Store) dailyCounts(query string, since int64) (map[string]int, error) {
	rows, err := s.DB.Query(query, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			return nil, err
		}
		out[day] = count
	}
	return out, rows.Err()
}

// statAssetColumns 是三条资产榜共用的列。
const statAssetColumns = `a.id,a.kind,COALESCE(a.title,''),COALESCE(a.title_source,''),a.importance,a.use_count,a.created_at,COALESCE(a.last_used_at,0)`

func scanStatAssets(rows *sql.Rows) ([]StatAsset, error) {
	defer rows.Close()
	var out []StatAsset
	for rows.Next() {
		var item StatAsset
		if err := rows.Scan(&item.ID, &item.Kind, &item.Title, &item.TitleSource,
			&item.Importance, &item.UseCount, &item.CreatedAt, &item.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// mostUsed 是「近 30 天最常用」：按查询时间窗内的**命中次数**排。
func (s *Store) mostUsed(since int64) ([]StatAsset, error) {
	rows, err := s.DB.Query(`SELECT `+statAssetColumns+`,COUNT(h.asset_id) c
		FROM recall_hits h JOIN assets a ON a.id=h.asset_id
		WHERE h.at>=? AND a.status='active'
		GROUP BY h.asset_id ORDER BY c DESC,a.id LIMIT 10`, since)
	if err != nil {
		return nil, err
	}
	out, err := scanStatAssets2(rows)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanStatAssets2 处理带额外计数列的形态：多读一列丢掉。
func scanStatAssets2(rows *sql.Rows) ([]StatAsset, error) {
	defer rows.Close()
	var out []StatAsset
	for rows.Next() {
		var item StatAsset
		var extra int
		if err := rows.Scan(&item.ID, &item.Kind, &item.Title, &item.TitleSource,
			&item.Importance, &item.UseCount, &item.CreatedAt, &item.LastUsedAt, &extra); err != nil {
			return nil, err
		}
		item.Recalls30 = extra
		out = append(out, item)
	}
	return out, rows.Err()
}

// nearDecay 是「快被衰减杀掉」：重要度高、但 60 天没被召回，且落在会衰减的层。
// 排除 doc——文档不参与衰减（机械层的口径，两处必须一致）。
func (s *Store) nearDecay() ([]StatAsset, error) {
	cutoff := time.Now().AddDate(0, 0, -60).UnixMilli()
	rows, err := s.DB.Query(`SELECT `+statAssetColumns+` FROM assets a
		WHERE a.status='active' AND a.kind<>'doc' AND a.layer>0 AND a.importance>=50
		  AND (a.last_used_at IS NULL OR a.last_used_at<?)
		ORDER BY a.importance DESC,a.use_count ASC,a.id LIMIT 10`, cutoff)
	if err != nil {
		return nil, err
	}
	return scanStatAssets(rows)
}

// neverUsed 是「存了但搜不到」：入库超过 7 天、一次都没被召回。
func (s *Store) neverUsed() ([]StatAsset, error) {
	cutoff := time.Now().AddDate(0, 0, -7).UnixMilli()
	rows, err := s.DB.Query(`SELECT `+statAssetColumns+` FROM assets a
		WHERE a.status='active' AND a.use_count=0 AND a.created_at<?
		ORDER BY a.created_at ASC,a.id LIMIT 10`, cutoff)
	if err != nil {
		return nil, err
	}
	return scanStatAssets(rows)
}

// pendingGroups 按 pending_reason 分组，每组带最多 8 条明细。
func (s *Store) pendingGroups() ([]PendingGroup, error) {
	rows, err := s.DB.Query(`SELECT COALESCE(NULLIF(pending_reason,''),'(未标注)'),COUNT(*)
		FROM assets WHERE status='pending' GROUP BY 1 ORDER BY 2 DESC,1`)
	if err != nil {
		return nil, err
	}
	var groups []PendingGroup
	for rows.Next() {
		var group PendingGroup
		if err := rows.Scan(&group.Reason, &group.Count); err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, group)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range groups {
		items, err := s.pendingItems(groups[i].Reason)
		if err != nil {
			return nil, err
		}
		groups[i].Items = items
	}
	return groups, nil
}

func (s *Store) pendingItems(reason string) ([]StatAsset, error) {
	query := `SELECT ` + statAssetColumns + ` FROM assets a WHERE a.status='pending' AND COALESCE(NULLIF(a.pending_reason,''),'(未标注)')=? ORDER BY a.created_at DESC LIMIT 8`
	rows, err := s.DB.Query(query, reason)
	if err != nil {
		return nil, err
	}
	return scanStatAssets(rows)
}

func (s *Store) reviewItems() ([]ReviewItem, error) {
	rows, err := s.DB.Query(`SELECT r.id,r.asset_a,r.asset_b,COALESCE(a.title,''),COALESCE(b.title,''),COALESCE(r.conflict_on,''),COALESCE(r.detail,''),r.created_at
		FROM reviews r LEFT JOIN assets a ON a.id=r.asset_a LEFT JOIN assets b ON b.id=r.asset_b
		WHERE r.resolved_at IS NULL ORDER BY r.created_at DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewItem
	for rows.Next() {
		var item ReviewItem
		if err := rows.Scan(&item.ID, &item.A, &item.B, &item.ATitle, &item.BTitle, &item.Conflict, &item.Detail, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// TitleSourceLabel 把存储值翻成人话。看板上直接显示英文键名等于没解释。
func TitleSourceLabel(source string) string {
	switch source {
	case "flag":
		return "--title"
	case "filename":
		return "文件名"
	case "body-first-line":
		return "正文首句"
	case "h1":
		return "标题行 H1"
	case "agent":
		return "Agent 自攒"
	case "":
		return "未知（旧数据）"
	}
	return source
}
