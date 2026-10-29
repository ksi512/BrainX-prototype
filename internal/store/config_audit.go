package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 配置审计：从「写入时记账」改为「启动时 diff」。
//
// 为什么不能写入时记账：看板页面通过 File System Access 直写 config.json，
// 那条路径完全绕过 CLI。CLI 不可能在写入瞬间记录事件，只能在**下次启动时**
// 比对文件内容与上次已知的快照。
//
// 代价是记不到「谁改的」——个人单机场景不需要这个，也拿不到。

const configAuditKey = "config:last_content"

// auditConfig 在启动加载配置后调用一次。previous 是本次读到的文件内容。
func (s *Store) auditConfig(previous string) error {
	var known string
	err := s.DB.QueryRow(`SELECT v FROM meta WHERE k=?`, configAuditKey).Scan(&known)
	if err != nil && !isNoRows(err) {
		return err
	}
	if known == "" {
		// 第一次启动：只记基线，不产生「变更」事件
		return s.setMetaRaw(configAuditKey, previous)
	}
	if known == previous {
		return nil
	}
	changes := diffConfigJSON(known, previous)
	if len(changes) == 0 {
		// 只有格式/顺序变化（比如页面重排了 JSON），不算改动
		return s.setMetaRaw(configAuditKey, previous)
	}
	reason, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(`INSERT INTO events(id,run_id,seq,op,asset_id,before_hash,after_hash,decision,reason,at) VALUES(?,NULL,1,'config',NULL,?,?,?,?,?)`,
		ID(), Hash(known), Hash(previous), "config-drift", string(reason), now()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "brainx: 检测到 config.json 被外部改动（%d 个键），已记一条 op=config 审计\n", len(changes))
	return s.setMetaRaw(configAuditKey, previous)
}

// ConfigChange 是一条配置差异。
type ConfigChange struct {
	Key string `json:"key"`
	Old any    `json:"old"`
	New any    `json:"new"`
}

// diffConfigJSON 逐键比对两份 JSON。只比对顶层键 + archive.roots 的一层子键，
// 不做任意深递归——配置结构就两层，深层递归只会让输出更难读。
func diffConfigJSON(oldText, newText string) []ConfigChange {
	var oldMap, newMap map[string]any
	if err := json.Unmarshal([]byte(oldText), &oldMap); err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(newText), &newMap); err != nil {
		return nil
	}
	keys := map[string]bool{}
	for key := range oldMap {
		keys[key] = true
	}
	for key := range newMap {
		keys[key] = true
	}
	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)

	var out []ConfigChange
	for _, key := range names {
		oldValue, newValue := oldMap[key], newMap[key]
		if key == "archive.roots" {
			for _, change := range diffRoots(oldValue, newValue) {
				out = append(out, change)
			}
			continue
		}
		if fmt.Sprint(oldValue) != fmt.Sprint(newValue) {
			out = append(out, ConfigChange{Key: key, Old: oldValue, New: newValue})
		}
	}
	return out
}

func diffRoots(oldValue, newValue any) []ConfigChange {
	oldRoots, _ := oldValue.(map[string]any)
	newRoots, _ := newValue.(map[string]any)
	names := map[string]bool{}
	for name := range oldRoots {
		names[name] = true
	}
	for name := range newRoots {
		names[name] = true
	}
	list := make([]string, 0, len(names))
	for name := range names {
		list = append(list, name)
	}
	sort.Strings(list)
	var out []ConfigChange
	for _, name := range list {
		if fmt.Sprint(oldRoots[name]) != fmt.Sprint(newRoots[name]) {
			out = append(out, ConfigChange{Key: "archive.roots." + name, Old: oldRoots[name], New: newRoots[name]})
		}
	}
	return out
}

func (s *Store) setMetaRaw(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
	return err
}

// ConfigFileText 读 config.json 原文，供启动审计比对。
func ConfigFileText(home string) string {
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		return ""
	}
	return string(data)
}

func isNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows")
}
