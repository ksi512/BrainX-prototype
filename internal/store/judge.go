package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Verdict 是写入质量判断的结论（提示词 §5.2）。
//
// 刻意做成「扣分制」而不是「分类器」：规则少、可解释、零模型。
// 分数只是给三档划线的工具，真正重要的是 Reason——它要能说清为什么。
type Verdict struct {
	Score  int
	Status string // active | pending | reject
	Reason []string
}

const (
	statusActive  = "active"
	statusPending = "pending"
	statusReject  = "reject"
	baseScore     = 70
	shortRunes    = 20
)

// Judge 给一条待写入的记忆打分。
//
// 三档：≥70 直接 active，40–69 进 pending 等确认，<40 丢弃。
// 判据都来自「这条写进去会不会变成噪音」：太短、纯自指、和已有的完全一样、被明确拒过。
func (s *Store) Judge(body, kind string) (Verdict, error) {
	verdict := Verdict{Score: baseScore, Status: statusActive}
	trimmed := strings.TrimSpace(body)
	runes := utf8.RuneCountInString(trimmed)
	hash := Hash(body)

	if runes < shortRunes {
		verdict.Score -= 25
		verdict.Reason = append(verdict.Reason, fmt.Sprintf("过短（%d 字，<%d）", runes, shortRunes))
	}
	if selfReferential(trimmed) {
		verdict.Score -= 20
		verdict.Reason = append(verdict.Reason, "纯自指（说的是会话本身，不是知识）")
	}
	var duplicates int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM assets WHERE content_hash=? AND status<>'archived'`, hash).Scan(&duplicates); err != nil {
		return verdict, err
	}
	if duplicates > 0 {
		verdict.Score -= 50
		verdict.Reason = append(verdict.Reason, "与既有资产正文完全相同")
	}
	if suppressed, err := s.IsSuppressed(hash); err != nil {
		return verdict, err
	} else if suppressed {
		verdict.Score -= 100
		verdict.Reason = append(verdict.Reason, "曾被 confirm --reject 明确拒绝")
	}
	// insight 的入库门槛另有一套（必须人工确认），不按分数放行
	if kind == "insight" {
		verdict.Status = statusPending
		verdict.Reason = append(verdict.Reason, "insight 一律先落 pending，等人工确认")
		return verdict, nil
	}

	switch {
	case verdict.Score < 40:
		verdict.Status = statusReject
	case verdict.Score < 70:
		verdict.Status = statusPending
	}
	if len(verdict.Reason) == 0 {
		verdict.Reason = append(verdict.Reason, "无扣分项")
	}
	return verdict, nil
}

// selfReferential 抓「说的是这次会话，不是知识」的句子。
// 只看开头与几个固定短语：真正的语义自指需要模型判断，而引擎不调模型。
func selfReferential(text string) bool {
	if text == "" {
		return false
	}
	head := []rune(text)
	if len(head) > 12 {
		head = head[:12]
	}
	prefix := string(head)
	for _, cue := range []string{"我刚刚", "我刚才", "本次会话", "上一条消息", "用户说", "如上所述"} {
		if strings.HasPrefix(prefix, cue) {
			return true
		}
	}
	return false
}

// Suppress 记一条抑制：`confirm --reject` 之后，同 content_hash 不再被捕获。
// 存在 meta 里而不是新表：这是「配置型」数据，量级远小于资产。
func (s *Store) Suppress(hash, reason string) error {
	return s.SetMeta("suppress:"+hash, fmt.Sprintf("%d|%s", time.Now().UnixMilli(), reason))
}

func (s *Store) IsSuppressed(hash string) (bool, error) {
	value, err := s.Meta("suppress:" + hash)
	if err != nil {
		return false, err
	}
	return value != "", nil
}

// TidyHint 生成 brief 最前那一行「待整理」。
//
// 三条实现规则（提示词 §5.4）：
//   - 用「条数 + 天数」做代理指标，**不算候选组数**（自连接 1 万资产要 62ms，放不进 hook 预算）
//   - 条数与天数都不达标时**整行不输出**，不许输出「待整理 0 条」
//   - 连续 7 天没处理 → 降级：不再进 brief，只在 status 里显示
func (s *Store) TidyHint() (string, bool, error) {
	lastTidy := metaIntValue(s, "tidy:last_run")
	lastPlan := metaIntValue(s, "dream:last_plan_at")
	ref := lastTidy
	if lastPlan > ref {
		ref = lastPlan
	}
	var count int
	var err error
	if ref > 0 {
		err = s.DB.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='active' AND updated_at>?`, ref).Scan(&count)
	} else {
		err = s.DB.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='active'`).Scan(&count)
	}
	if err != nil {
		return "", false, err
	}
	nowMillis := time.Now().UnixMilli()
	days := 0
	if ref > 0 {
		days = int((nowMillis - ref) / 86400000)
	} else {
		oldest := nowMillis
		if err := s.DB.QueryRow(`SELECT COALESCE(MIN(created_at),0) FROM assets WHERE status='active'`).Scan(&oldest); err != nil {
			return "", false, err
		}
		if oldest > 0 && oldest < nowMillis {
			days = int((nowMillis - oldest) / 86400000)
		}
	}
	minItems, minDays := s.Config.TidyHintMinItems, s.Config.TidyHintMinDays
	if minItems <= 0 {
		minItems = 100
	}
	if minDays <= 0 {
		minDays = 14
	}
	if count < minItems && days < minDays {
		// 已经收敛：把「首次提示」的时钟也归零，下次重新计
		_ = s.SetMeta("tidy_hint:first_shown_at", "")
		return "", false, nil
	}
	firstShown := metaIntValue(s, "tidy_hint:first_shown_at")
	if firstShown == 0 {
		_ = s.SetMeta("tidy_hint:first_shown_at", strconv.FormatInt(nowMillis, 10))
	} else if nowMillis-firstShown > int64(7*86400000) {
		// 连续 7 天没处理：降级，别再占 brief 的位置
		_ = s.SetMeta("tidy_hint:degraded_at", strconv.FormatInt(nowMillis, 10))
		return "", false, nil
	}
	line := fmt.Sprintf("[brainx] 待整理 %d 条 / %d 天 → brainx dream --plan", count, days)
	return line, true, nil
}

func metaIntValue(s *Store, key string) int64 {
	value, err := s.Meta(key)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(value, 10, 64)
	return n
}

// Confirm 把 pending 资产转 active。它是 pending 的唯一出口。
func (s *Store) Confirm(id string) error {
	asset, err := s.Get(id)
	if err != nil {
		return err
	}
	if asset.Status != "pending" {
		return fmt.Errorf("资产 %s 的状态是 %q，只有 pending 需要确认", id, asset.Status)
	}
	if _, err := s.DB.Exec(`UPDATE assets SET status='active',pending_reason=NULL WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := s.DB.Exec(`INSERT INTO events(id,run_id,seq,op,asset_id,before_hash,after_hash,decision,at) VALUES(?,NULL,1,'confirm',?,?,?,?,?)`,
		ID(), id, asset.ContentHash, asset.ContentHash, "keep", now()); err != nil {
		return err
	}
	return s.resolveReviewsFor(id, "annotate")
}

// Reject 归档并写抑制记录：同 content_hash 之后不再被捕获。
//
// 「拒绝」比「删除」重要：删除只清掉这一条，抑制才防住下一次同样的自动捕获。
func (s *Store) Reject(id, reason string) error {
	asset, err := s.Get(id)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "rejected"
	}
	if _, err := s.DB.Exec(`UPDATE assets SET status='archived',archive_reason=?,pending_reason=NULL WHERE id=?`, reason, id); err != nil {
		return err
	}
	if _, err := s.DB.Exec(`INSERT INTO events(id,run_id,seq,op,asset_id,before_hash,decision,reason,at) VALUES(?,NULL,1,'archive',?,?,?,?,?)`,
		ID(), id, asset.ContentHash, "reject", reason, now()); err != nil {
		return err
	}
	if err := s.Suppress(asset.ContentHash, reason); err != nil {
		return err
	}
	return s.resolveReviewsFor(id, "reject")
}

func (s *Store) resolveReviewsFor(id, resolution string) error {
	_, err := s.DB.Exec(`UPDATE reviews SET resolved_at=?,resolution=? WHERE resolved_at IS NULL AND (asset_a=? OR asset_b=?)`,
		now(), resolution, id, id)
	return err
}

// enforcePendingCap 是待确认队列的泄压阀：超过 pending.max 就按 confidence
// 升序淘汰最旧的（archive_reason='evicted'，可找回）。
//
// 为什么必须有：pending 只增不减会让「待确认」变成一个永远清不完的列表，
// 最后没人看——这比直接拒绝写入更糟。
func (s *Store) enforcePendingCap() error {
	max := s.Config.PendingMax
	if max <= 0 {
		max = 500
	}
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM assets WHERE status='pending'`).Scan(&total); err != nil {
		return err
	}
	if total <= max {
		return nil
	}
	// insight 是唯一不能自动淘汰的 pending：它必须人工确认（硬约束）
	_, err := s.DB.Exec(`UPDATE assets SET status='archived',archive_reason='evicted'
		WHERE id IN (
		  SELECT id FROM assets WHERE status='pending' AND kind<>'insight'
		  ORDER BY confidence ASC, created_at ASC LIMIT ?
		)`, total-max)
	return err
}
