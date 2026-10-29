package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/index"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS assets (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL, scope TEXT NOT NULL, layer INTEGER NOT NULL DEFAULT 1,
 title TEXT, body TEXT NOT NULL, summary TEXT, importance REAL NOT NULL DEFAULT 50,
 confidence REAL NOT NULL DEFAULT 70, status TEXT NOT NULL DEFAULT 'active', pending_reason TEXT,
 archive_reason TEXT, origin TEXT, archive TEXT NOT NULL DEFAULT 'text-only', archive_hash TEXT,
 content_hash TEXT NOT NULL, merged_into TEXT, immutable INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, last_used_at INTEGER, use_count INTEGER NOT NULL DEFAULT 0,
 token_est INTEGER
);
CREATE INDEX IF NOT EXISTS idx_assets_scope ON assets(scope,status,layer);
CREATE INDEX IF NOT EXISTS idx_assets_hash ON assets(content_hash);
CREATE INDEX IF NOT EXISTS idx_assets_used ON assets(last_used_at);
CREATE TABLE IF NOT EXISTS asset_tags (asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE, tag TEXT NOT NULL, PRIMARY KEY(asset_id,tag));
CREATE INDEX IF NOT EXISTS idx_asset_tags_tag ON asset_tags(tag);
CREATE TABLE IF NOT EXISTS chunks (id TEXT PRIMARY KEY, asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE, seq INTEGER NOT NULL, text TEXT NOT NULL, start_off INTEGER, end_off INTEGER, embedding BLOB, dim INTEGER, embed_model TEXT, token_est INTEGER);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chunks_asset_seq ON chunks(asset_id,seq);
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(title_ngram,text_ngram,asset_id UNINDEXED,seq UNINDEXED,content='',tokenize='unicode61');
CREATE TABLE IF NOT EXISTS edges (id TEXT PRIMARY KEY, from_id TEXT NOT NULL, to_id TEXT NOT NULL, rel TEXT NOT NULL, weight REAL NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, UNIQUE(from_id,to_id,rel));
CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(from_id); CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id);
CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, trigger TEXT NOT NULL, scope TEXT, started_at INTEGER NOT NULL, finished_at INTEGER, input_snapshot_hash TEXT NOT NULL, decisions_json TEXT, status TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY, run_id TEXT REFERENCES runs(id) ON DELETE SET NULL, seq INTEGER NOT NULL, op TEXT NOT NULL, asset_id TEXT, before_hash TEXT, after_hash TEXT, decision TEXT, reason TEXT, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id,seq); CREATE INDEX IF NOT EXISTS idx_events_asset ON events(asset_id,at);
CREATE TABLE IF NOT EXISTS reviews (id TEXT PRIMARY KEY, asset_a TEXT NOT NULL, asset_b TEXT NOT NULL, conflict_on TEXT, detail TEXT, note_a TEXT, note_b TEXT, created_at INTEGER NOT NULL, resolved_at INTEGER, resolution TEXT);
CREATE INDEX IF NOT EXISTS idx_reviews_open ON reviews(resolved_at) WHERE resolved_at IS NULL;
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT);
-- 召回日志：看板的「近 30 天召回次数」与「资产榜」都靠它。
-- 不能拿 use_count 代替：那是累计值，新旧资产不可比（§5 看板明确禁止用它排序）。
CREATE TABLE IF NOT EXISTS recalls (id TEXT PRIMARY KEY, at INTEGER NOT NULL, scope TEXT, query TEXT, hits INTEGER NOT NULL DEFAULT 0, sources TEXT);
CREATE INDEX IF NOT EXISTS idx_recalls_at ON recalls(at);
CREATE TABLE IF NOT EXISTS recall_hits (recall_id TEXT NOT NULL, asset_id TEXT NOT NULL, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_recall_hits_at ON recall_hits(at);
CREATE INDEX IF NOT EXISTS idx_recall_hits_asset ON recall_hits(asset_id,at);`

type Config struct {
	ArchiveDefault string            `json:"archive.default"`
	Roots          map[string]string `json:"archive.roots,omitempty"`

	TidyCapacity int     `json:"tidy.capacity"`
	TidyTailPct  int     `json:"tidy.tail_pct"`
	DecayLambda  float64 `json:"tidy.decay_lambda"`
	TidyInterval string  `json:"tidy.interval"`
	TidyAuto     bool    `json:"tidy.auto"`

	DeriveSynthesisMinDocs  int `json:"derive.synthesis_min_docs"`
	DeriveAliasEditDistance int `json:"derive.alias_edit_distance"`

	DreamPlanTimeoutMs int `json:"dream.plan_timeout_ms"`

	RecallMaxItems         int    `json:"recall.max_items"`
	RecallMaxChars         int    `json:"recall.max_chars"`
	RecallTimeoutMs        int    `json:"recall.timeout_ms"`
	RecallExpandAlias      bool   `json:"recall.expand_alias"`
	RecallSynonymsFile     string `json:"recall.synonyms_file"`
	RecallSingleCharEnough int    `json:"recall.single_char_enough"`

	ChunkTarget  int `json:"chunk.target_chars"`
	ChunkMax     int `json:"chunk.max_chars"`
	ChunkMin     int `json:"chunk.min_chars"`
	ChunkOverlap int `json:"chunk.overlap_chars"`

	ContextMaxChars  int `json:"context.max_chars"`
	TidyHintMinItems int `json:"context.tidy_hint_min_items"`
	TidyHintMinDays  int `json:"context.tidy_hint_min_days"`

	VectorEnabled  bool   `json:"vector.enabled"`
	VectorEndpoint string `json:"vector.endpoint"`
	VectorModel    string `json:"vector.model"`

	LikeFallback bool `json:"index.like_fallback"`

	PendingMax              int `json:"pending.max"`
	PendingAutoConfirmAbove int `json:"pending.auto_confirm_above"`

	HookSessionBudgetMs int `json:"hook.session_budget_ms"`
	HookStopBudgetMs    int `json:"hook.stop_budget_ms"`

	ImportWorkers   int `json:"import.workers"`
	ImportMaxFileMB int `json:"import.max_file_mb"`

	FetchTimeout  string `json:"fetch.timeout"`
	FetchMaxBytes int    `json:"fetch.max_bytes"`
}

func DefaultConfig() Config {
	// 具名字面量：加一个配置键时不能悄悄把后面的值错位。
	return Config{
		ArchiveDefault:          "local",
		Roots:                   map[string]string{},
		TidyCapacity:            5000,
		TidyTailPct:             10,
		DecayLambda:             0.005,
		TidyInterval:            "24h",
		TidyAuto:                true,
		DeriveSynthesisMinDocs:  3,
		DeriveAliasEditDistance: 2,
		DreamPlanTimeoutMs:      2000,
		RecallMaxItems:          8,
		RecallMaxChars:          1200,
		RecallTimeoutMs:         60,
		RecallExpandAlias:       true,
		RecallSingleCharEnough:  20,
		ChunkTarget:             400,
		ChunkMax:                600,
		ChunkMin:                120,
		ChunkOverlap:            80,
		ContextMaxChars:         2000,
		TidyHintMinItems:        100,
		TidyHintMinDays:         14,
		LikeFallback:            true,
		PendingMax:              500,
		HookSessionBudgetMs:     100,
		HookStopBudgetMs:        400,
		ImportWorkers:           4,
		ImportMaxFileMB:         200,
		FetchTimeout:            "30s",
		FetchMaxBytes:           209715200,
	}
}

type Store struct {
	DB     *sql.DB
	Home   string
	Config Config
}

type Asset struct {
	ID, Kind, Scope, Title, Body, Status, Origin, Archive, ArchiveHash, ContentHash, MergedInto string
	Layer                                                                                       int
	Importance, Confidence                                                                      float64
	Immutable                                                                                   bool
	CreatedAt, UpdatedAt                                                                        int64
	UseCount                                                                                    int
	TitleSource                                                                                 string
}

type CreateAsset struct {
	// ID 留空则由 Create 生成；ImportFile 需要先知道 id 才能落原件，所以允许外部给。
	ID                                        string
	Kind, Scope, Title, Body, Origin, Archive string
	// TitleSource 记录标题是谁给的（flag / filename / body-first-line / agent），
	// 看板「从未被召回」那一列要显示它：存了搜不到时，先要能看清标题怎么来的。
	TitleSource            string
	Tags                   []string
	Layer                  int
	Importance, Confidence float64
	Status                 string
	PendingReason          string
}

func Home() string {
	if path := os.Getenv("BRAINX_HOME"); path != "" {
		return path
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".brainx")
}
func now() int64 { return time.Now().UnixMilli() }
func ID() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid()))))[:26]
}
func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func Init(home string) (*Store, error) {
	for _, dir := range []string{home, filepath.Join(home, "files"), filepath.Join(home, "queue"), filepath.Join(home, "queue", "dead"), filepath.Join(home, "queue", "cmd"), filepath.Join(home, "dict"), filepath.Join(home, "cache", "fetch")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	// busy_timeout 必须走 DSN（连接级），不能只写在 schema 里——
	// schema 的第一条语句（journal_mode=WAL）自己就要抢锁，那时 pragma 还没执行。
	dsn := filepath.Join(home, "store.db") +
		"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// 多进程同时首次打开时会并发跑建表语句，这里也要退避重试
	var schemaErr error
	for _, wait := range []time.Duration{0, 20 * time.Millisecond, 60 * time.Millisecond, 180 * time.Millisecond} {
		if wait > 0 {
			time.Sleep(wait)
		}
		if _, schemaErr = db.Exec(schema); schemaErr == nil {
			break
		}
		if !isBusy(schemaErr) {
			break
		}
	}
	if schemaErr != nil {
		db.Close()
		return nil, schemaErr
	}
	// 列级迁移：SQLite 的 ADD COLUMN 是幂等的（重复加会报 duplicate column name），
	// 所以这里吞掉那一个错误即可，不需要版本号表。
	if err := addColumnIfMissing(db, "assets", "title_source", "TEXT"); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db, Home: home, Config: DefaultConfig()}
	if err := s.LoadConfig(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func Open(home string) (*Store, error) {
	if _, err := os.Stat(filepath.Join(home, "store.db")); err != nil {
		return nil, fmt.Errorf("BrainX 尚未初始化：%w", err)
	}
	return Init(home)
}
func (s *Store) Close() error { return s.DB.Close() }

// addColumnIfMissing 是列级迁移的唯一入口。
// 只吞「duplicate column name」这一种错——把别的错误也吞掉，迁移就会静默半成品。
func addColumnIfMissing(db *sql.DB, table, column, kind string) error {
	_, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + kind)
	if err == nil || strings.Contains(err.Error(), "duplicate column name") {
		return nil
	}
	return err
}

func (s *Store) LoadConfig() error {
	path := filepath.Join(s.Home, "config.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data, _ = json.MarshalIndent(DefaultConfig(), "", "  ")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return err
		}
		if err := s.validateConfig(); err != nil {
			return err
		}
		return s.auditConfig(string(data))
	}
	if err != nil {
		return err
	}
	config := DefaultConfig()
	if err := json.Unmarshal(data, &config); err != nil {
		// 硬约束 15：配置坏了要回退默认值并告警，不能启动即崩
		fmt.Fprintf(os.Stderr, "brainx: config.json 无法解析，已回退默认值：%v\n", err)
		s.Config = DefaultConfig()
		if err := s.validateConfig(); err != nil {
			return err
		}
		return s.auditConfig(string(data))
	}
	s.Config = config
	if err := s.validateConfig(); err != nil {
		return err
	}
	if s.Config.Roots == nil {
		s.Config.Roots = map[string]string{}
	}
	// 防线②：看板页面绕过了 CLI 的校验函数，这里是唯一兜底。
	// 越界值已被拉回默认，但**必须说出是哪个键**——静默修正比报错更难查。
	s.reportConfigDrift(string(data))
	return s.auditConfig(string(data))
}

// reportConfigDrift 把「文件里的值 ≠ 生效值」逐个打到 stderr。
//
// 为什么必须报：页面直写 config.json，一个越界值会让系统行为变化，
// 而文件看起来完全「正常」。不报的话，用户只会觉得「brainx 今天不太对劲」。
func (s *Store) reportConfigDrift(rawText string) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawText), &raw); err != nil {
		return
	}
	snapshot, err := s.ConfigSnapshot()
	if err != nil {
		return
	}
	var drift []string
	for _, spec := range ConfigSchema() {
		value, ok := raw[spec.Name]
		if !ok || spec.Name == "archive.roots" {
			continue
		}
		if fmt.Sprint(value) != fmt.Sprint(snapshot[spec.Name]) {
			drift = append(drift, fmt.Sprintf("%s=%v（越界或类型不符，已按 %v 生效）", spec.Name, value, snapshot[spec.Name]))
		}
	}
	if len(drift) > 0 {
		fmt.Fprintf(os.Stderr, "brainx: config.json 有 %d 个值不可用，已回退默认：%s\n", len(drift), strings.Join(drift, "；"))
	}
}

// validateConfig 把越界值拉回默认。它是「配置永远可用」的最后一道保险。
func (s *Store) validateConfig() error {
	c := &s.Config
	if c.Roots == nil {
		c.Roots = map[string]string{}
	}
	if !validArchive(c.ArchiveDefault) {
		c.ArchiveDefault = "local"
	}
	clampInt(&c.TidyCapacity, 0, 1000000, 5000)
	clampInt(&c.TidyTailPct, 1, 50, 10)
	clampFloat(&c.DecayLambda, 0, 1, 0.005)
	if _, err := time.ParseDuration(c.TidyInterval); err != nil {
		c.TidyInterval = "24h"
	}
	clampInt(&c.DeriveSynthesisMinDocs, 2, 50, 3)
	clampInt(&c.DeriveAliasEditDistance, 0, 10, 2)
	clampInt(&c.DreamPlanTimeoutMs, 100, 60000, 2000)
	clampInt(&c.RecallMaxItems, 1, 100, 8)
	clampInt(&c.RecallMaxChars, 100, 20000, 1200)
	clampInt(&c.RecallTimeoutMs, 10, 5000, 60)
	clampInt(&c.RecallSingleCharEnough, 1, 1000, 20)
	clampInt(&c.ChunkTarget, 100, 5000, 400)
	clampInt(&c.ChunkMax, 100, 10000, 600)
	clampInt(&c.ChunkMin, 0, 2000, 120)
	clampInt(&c.ChunkOverlap, 0, 2000, 80)
	if c.ChunkMax < c.ChunkTarget {
		c.ChunkMax = 600
	}
	if c.ChunkMin > c.ChunkTarget {
		c.ChunkMin = 120
	}
	if c.ChunkOverlap >= c.ChunkTarget {
		c.ChunkOverlap = 80
	}
	clampInt(&c.ContextMaxChars, 100, 12000, 2000)
	clampInt(&c.TidyHintMinItems, 1, 10000, 100)
	clampInt(&c.TidyHintMinDays, 1, 365, 14)
	clampInt(&c.PendingMax, 1, 100000, 500)
	clampInt(&c.PendingAutoConfirmAbove, 0, 100, 0)
	clampInt(&c.HookSessionBudgetMs, 60, 2000, 100)
	clampInt(&c.HookStopBudgetMs, 100, 10000, 400)
	clampInt(&c.ImportWorkers, 1, 32, 4)
	clampInt(&c.ImportMaxFileMB, 1, 4096, 200)
	if _, err := time.ParseDuration(c.FetchTimeout); err != nil {
		c.FetchTimeout = "30s"
	}
	clampInt(&c.FetchMaxBytes, 1048576, 2147483648, 209715200)
	return nil
}

func clampInt(value *int, min, max, fallback int) {
	if *value < min || *value > max {
		*value = fallback
	}
}

func clampFloat(value *float64, min, max, fallback float64) {
	if *value < min || *value > max {
		*value = fallback
	}
}

// validArchive 接受四种形态：
//   - local：放进 $BRAINX_HOME/files/（写入时会替换成具体相对路径）
//   - text-only：不留原件
//   - files/<yyyy>/<id>.<ext>：local 落盘后的具体相对路径
//   - ext:<root>:<relpath>：外部命名根下的相对路径
func validArchive(value string) bool {
	return value == "local" || value == "text-only" ||
		strings.HasPrefix(value, "files/") || strings.HasPrefix(value, "ext:")
}

func (s *Store) Create(input CreateAsset) (Asset, error) {
	if input.Scope == "" {
		return Asset{}, errors.New("写入必须明确指定 --scope")
	}
	if input.Body == "" {
		return Asset{}, errors.New("正文不能为空")
	}
	if input.Kind == "" {
		input.Kind = "memory"
	}
	if input.Status == "" {
		input.Status = "active"
	}
	if input.Archive == "" {
		input.Archive = "text-only"
	}
	if !validArchive(input.Archive) {
		return Asset{}, errors.New("archive 必须为 local、text-only 或 ext:<root>:<relpath>")
	}
	if input.Kind == "doc" {
		input.Layer = 0
	} else if input.Layer == 0 {
		switch input.Kind {
		case "synthesis", "insight":
			input.Layer = 2
		case "profile":
			input.Layer = 3
		default:
			input.Layer = 1
		}
	}
	if input.Importance == 0 {
		input.Importance = 50
	}
	if input.Confidence == 0 {
		input.Confidence = 70
	}
	if input.Title == "" {
		// 标题回落：正文首个非空行前 40 字（§9.4.3），让看板与召回都有可读标签
		input.Title = titleFallback(input.Body)
	}
	assetID := input.ID
	if assetID == "" {
		assetID = ID()
	}
	titleSource := input.TitleSource
	if titleSource == "" {
		if strings.TrimSpace(input.Title) == "" || input.Title == titleFallback(input.Body) {
			titleSource = "body-first-line"
		} else {
			titleSource = "flag"
		}
	}
	asset := Asset{ID: assetID, Kind: input.Kind, Scope: input.Scope, Title: input.Title, Body: input.Body, Status: input.Status, Origin: input.Origin, Archive: input.Archive, ContentHash: Hash(input.Body), Layer: input.Layer, Importance: input.Importance, Confidence: input.Confidence, Immutable: input.Kind == "doc", CreatedAt: now(), UpdatedAt: now(), TitleSource: titleSource}

	// 并发模型（硬约束 10）：短事务 + BEGIN IMMEDIATE + 退避重试。
	// 不能依赖 database/sql 默认的 DEFERRED —— 它先拿读锁、写时升级，
	// 两个连接同时这么做会互相等待成死锁，报 SQLITE_BUSY 且重试也解不开。
	waits := []time.Duration{0, 20 * time.Millisecond, 60 * time.Millisecond, 180 * time.Millisecond}
	var lastErr error
	for attempt, wait := range waits {
		if wait > 0 {
			time.Sleep(wait)
		}
		if lastErr = s.createTx(asset, input); lastErr == nil {
			// 写成功后再泄压：放进事务里会让「淘汰」与「写入」互相拖慢
			_ = s.enforcePendingCap()
			// 向量是可选增强：算不出来就留空，该片检索时只走 FTS5
			_ = s.EmbedAsset(asset.ID)
			return asset, nil
		}
		if !isBusy(lastErr) {
			return Asset{}, lastErr
		}
		if attempt == len(waits)-1 {
			break
		}
	}
	if err := s.enqueueFailure("remember", input, lastErr); err != nil {
		return Asset{}, fmt.Errorf("%w（且落 queue/ 也失败：%v）", lastErr, err)
	}
	return Asset{}, fmt.Errorf("%w（已落 queue/ 待下次补偿）", lastErr)
}

// createTx 执行一次写入事务。用独立 Conn 显式 BEGIN IMMEDIATE，
// 先拿到写锁再开始读，避免 DEFERRED 的锁升级竞争。
func (s *Store) createTx(asset Asset, input CreateAsset) error {
	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	if _, err = conn.ExecContext(ctx, `INSERT INTO assets(id,kind,scope,layer,title,body,importance,confidence,status,pending_reason,origin,archive,content_hash,immutable,created_at,updated_at,use_count,title_source) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?)`, asset.ID, asset.Kind, asset.Scope, asset.Layer, asset.Title, asset.Body, asset.Importance, asset.Confidence, asset.Status, input.PendingReason, asset.Origin, asset.Archive, asset.ContentHash, asset.Immutable, asset.CreatedAt, asset.UpdatedAt, asset.TitleSource); err != nil {
		return err
	}
	for _, tag := range input.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO asset_tags(asset_id,tag) VALUES(?,?)`, asset.ID, tag); err != nil {
			return err
		}
	}
	chunks := index.ChunkText(asset.Body, s.Config.ChunkTarget, s.Config.ChunkMax, s.Config.ChunkMin, s.Config.ChunkOverlap)
	if len(chunks) == 0 {
		return errors.New("无法生成切片")
	}
	for sequence, chunk := range chunks {
		chunkID := ID()
		result, err := conn.ExecContext(ctx, `INSERT INTO chunks(id,asset_id,seq,text,start_off,end_off) VALUES(?,?,?,?,?,?)`, chunkID, asset.ID, sequence, chunk.Text, chunk.StartOff, chunk.EndOff)
		if err != nil {
			return err
		}
		rowID, _ := result.LastInsertId()
		if _, err = conn.ExecContext(ctx, `INSERT INTO chunks_fts(rowid,title_ngram,text_ngram,asset_id,seq) VALUES(?,?,?,?,?)`, rowID, index.Bigrams(asset.Title), index.Bigrams(chunk.Text), asset.ID, sequence); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO events(id,run_id,seq,op,asset_id,after_hash,decision,at) VALUES(?,NULL,1,'create',?,?,?,?)`, ID(), asset.ID, asset.ContentHash, "keep", now()); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "database is locked")
}

// enqueueFailure 把失败的写入落进 queue/。文件名带 ID 前缀，
// 避免并发写者互相覆盖——固定文件名会让「补偿机制自己制造丢写」。
func (s *Store) enqueueFailure(op string, input CreateAsset, cause error) error {
	dir := filepath.Join(s.Home, "queue")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	record := map[string]any{
		"op":              op,
		"payload":         input,
		"attempts":        0,
		"first_failed_at": now(),
		"last_error":      cause.Error(),
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ID()+"-"+op+".json"), data, 0o600)
}

func scanAsset(row interface{ Scan(...any) error }) (Asset, error) {
	var a Asset
	var immutable int
	err := row.Scan(&a.ID, &a.Kind, &a.Scope, &a.Layer, &a.Title, &a.Body, &a.Importance, &a.Confidence, &a.Status, &a.Origin, &a.Archive, &a.ContentHash, &a.MergedInto, &immutable, &a.CreatedAt, &a.UpdatedAt, &a.UseCount)
	a.Immutable = immutable == 1
	return a, err
}

// notFoundError 让调用方既能拿到干净的「找不到资产 x」，又能用 errors.Is 判成 os.ErrNotExist。
type notFoundError struct{ id string }

func (e notFoundError) Error() string { return "找不到资产 " + e.id }
func (e notFoundError) Unwrap() error { return os.ErrNotExist }

func (s *Store) Get(id string) (Asset, error) {
	asset, err := scanAsset(s.DB.QueryRow(`SELECT id,kind,scope,layer,COALESCE(title,''),body,importance,confidence,status,COALESCE(origin,''),archive,content_hash,COALESCE(merged_into,''),immutable,created_at,updated_at,use_count FROM assets WHERE id=? OR id LIKE ? ORDER BY created_at DESC LIMIT 1`, id, id+"%"))
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, notFoundError{id}
	}
	return asset, err
}
func (s *Store) List(scope, kind, status string, limit int) ([]Asset, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id,kind,scope,layer,COALESCE(title,''),body,importance,confidence,status,COALESCE(origin,''),archive,content_hash,COALESCE(merged_into,''),immutable,created_at,updated_at,use_count FROM assets WHERE 1=1`
	var args []any
	if scope != "" {
		q += " AND scope=?"
		args = append(args, scope)
	}
	if clause, kindArgs := KindClause("kind", kind); clause != "" {
		q += " AND " + clause
		args = append(args, kindArgs...)
	}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	q += " ORDER BY updated_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var assets []Asset
	for rows.Next() {
		a, e := scanAsset(rows)
		if e != nil {
			return nil, e
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}
func (s *Store) SoftForget(id string) error {
	a, err := s.Get(id)
	if err != nil {
		return err
	}
	if _, err = s.DB.Exec(`UPDATE assets SET status='archived',archive_reason='forgotten' WHERE id=?`, a.ID); err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO events(id,run_id,seq,op,asset_id,before_hash,decision,at) VALUES(?,NULL,1,'archive',?,?,?,?)`, ID(), a.ID, a.ContentHash, "archive", now())
	return err
}
func (s *Store) Link(from, to, rel string) error {
	valid := map[string]bool{"semantic": true, "contradicts": true, "supersedes": true, "derived_from": true, "cites": true, "alias_of": true, "temporal": true, "causal": true, "entity": true}
	if !valid[rel] {
		return fmt.Errorf("未知关系 %q（semantic / contradicts / supersedes / derived_from / cites / alias_of / temporal / causal / entity）", rel)
	}
	// entity 型边允许指向库外实体；其余关系必须两端都是真实资产，
	// 否则邻居查询与 dream 会拿着悬空 id 当依据。
	if rel != "entity" {
		for _, id := range []string{from, to} {
			var exists int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, id).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return notFoundError{id}
			}
		}
	}
	if _, err := s.DB.Exec(`INSERT OR IGNORE INTO edges(id,from_id,to_id,rel,created_at) VALUES(?,?,?,?,?)`, ID(), from, to, rel, now()); err != nil {
		return err
	}
	// 矛盾不只是连条边：必须同时进 reviews 队列，否则「未裁决冲突」永远没人看得到。
	// 两条都保持 active——取消 frozen 的理由见提示词 §3.3 B。
	if rel == "contradicts" {
		return s.AddReview(from, to, "手工标注的冲突，两条都仍是 active")
	}
	return nil
}

// Meta reads a value from the meta key-value table. Missing keys return "".
func (s *Store) Meta(key string) (string, error) {
	var value string
	err := s.DB.QueryRow(`SELECT v FROM meta WHERE k=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// SetMeta upserts a value in the meta key-value table.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
	return err
}

// MaxUpdatedAt returns the newest updated_at among active assets visible to
// scope. A project scope also sees global assets, matching read semantics.
func (s *Store) MaxUpdatedAt(scope string) (int64, error) {
	query := `SELECT COALESCE(MAX(updated_at),0) FROM assets WHERE status='active'`
	var args []any
	if strings.HasPrefix(scope, "proj:") {
		query += ` AND (scope='global' OR scope=?)`
		args = append(args, scope)
	} else if scope != "" {
		query += ` AND scope=?`
		args = append(args, scope)
	}
	var value int64
	err := s.DB.QueryRow(query, args...).Scan(&value)
	return value, err
}

func (s *Store) Status() (map[string]int, error) {
	out := map[string]int{}
	rows, err := s.DB.Query(`SELECT kind||':'||status,COUNT(*) FROM assets GROUP BY kind,status`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			rows.Close()
			return nil, err
		}
		out[key] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 分层分布：brief 与召回的行为都由 layer 决定，看不见分布就没法判断库长什么样
	layerRows, err := s.DB.Query(`SELECT layer,COUNT(*) FROM assets WHERE status='active' GROUP BY layer ORDER BY layer`)
	if err != nil {
		return nil, err
	}
	for layerRows.Next() {
		var layer, count int
		if err := layerRows.Scan(&layer, &count); err != nil {
			layerRows.Close()
			return nil, err
		}
		out[fmt.Sprintf("layer%d:active", layer)] = count
	}
	layerRows.Close()

	counts := []struct {
		key   string
		query string
	}{
		{"pending:total", `SELECT COUNT(*) FROM assets WHERE status='pending'`},
		{"conflicts:open", `SELECT COUNT(*) FROM reviews WHERE resolved_at IS NULL`},
		{"chunks:total", `SELECT COUNT(*) FROM chunks`},
		{"edges:total", `SELECT COUNT(*) FROM edges`},
	}
	for _, c := range counts {
		var n int
		if err := s.DB.QueryRow(c.query).Scan(&n); err != nil {
			return nil, err
		}
		out[c.key] = n
	}

	out["queue"] = 0
	// queue 深度只数**文件**：queue/ 下还有 dead/ 与 cmd/ 两个子目录，
	// 数条目会把空队列报成 2，看板上的「待补偿」就永远是假的。
	for _, dir := range []string{filepath.Join(s.Home, "queue"), filepath.Join(s.Home, "queue", "dead")} {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if !entry.IsDir() {
				out["queue"]++
			}
		}
	}
	out["db_bytes"] = int(fileSize(filepath.Join(s.Home, "store.db")))
	out["files_bytes"] = int(dirSize(filepath.Join(s.Home, "files")))
	return out, nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func dirSize(path string) int64 {
	var total int64
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		full := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			total += dirSize(full)
			continue
		}
		total += fileSize(full)
	}
	return total
}

// TagsByAsset 返回 asset_id → 标签列表。dream --plan 用它算「共享标签」候选，
// 独立成一次查询而不是 N+1，避免资产多了之后退化成全表扫。
func (s *Store) TagsByAsset() (map[string][]string, error) {
	rows, err := s.DB.Query(`SELECT asset_id, tag FROM asset_tags ORDER BY asset_id, tag`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}

// Snapshot 计算当前 scope 下 active 资产的指纹，用于 --plan 的幂等重放依据。
func (s *Store) Snapshot(scope string) (string, error) {
	assets, err := s.List(scope, "", "active", 100000)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(assets))
	for _, a := range assets {
		lines = append(lines, a.ID+":"+a.ContentHash)
	}
	sort.Strings(lines)
	return Hash(strings.Join(lines, "\n")), nil
}

// NowMillis 给其它包（看板的「几天前」等）一个统一的时间源。
func NowMillis() int64 { return now() }
