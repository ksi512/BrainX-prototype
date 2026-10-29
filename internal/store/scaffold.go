package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AGENTSBlock 是写入宿主约定文件的**单一真源**（提示词 §10.4）。
//
// 四条职责缺一条这套就瘫：缺「存」→ 用户说记住但没写；缺「取」→ 有记忆不用；
// 缺「整理」→ 待整理提示没人理会；缺「待确认」→ pending 永远沉底。
const AGENTSBlock = `<!-- brainx:begin -->
## brainx 维护职责

> 本段是**兜底**。宿主装了 brainx hook 时，brief 由 SessionStart / UserPromptSubmit 自动注入，
> 你不需要主动去读文件；但下面四条职责仍然适用——尤其是「存」：
> hook 不会替你把用户口述的约定写进库，那一步只能由你判断并调用 brainx。
> 宿主没装 hook（或被跳过）时，就靠这四条 + brainx recall 维持记忆。

### 存
用户说「记住 / 以后都这样 / 别再问了」时，立刻写入：

    brainx remember "<一句话事实>" --kind fact --scope global

项目内的约定用 --scope proj（自动判定当前仓库）。写完只在心里记一条，不要复述。

### 取
不确定既有约定时先查，别猜：

    brainx recall "<关键词>"

### 整理
brief 顶部出现「待整理」提示时，本次会话跑一次 brainx dream --plan，逐组给判断，
用 brainx dream --apply - 传回。**每次会话最多一次**；没出现提示不要主动跑。

### 待确认
brainx context --pending 列出待确认资产与未裁决冲突。确认用 brainx confirm <id>，
拒绝用 brainx confirm --reject <id>（同内容不会被再次捕获）。
<!-- brainx:end -->
`

// EnsureScaffold 补齐 $BRAINX_HOME 的骨架产物：wiki/、manifest.json、AGENTS.block.md。
//
// 三者都是「缺了也能跑，但缺了会少一条链路」的东西：
//   - wiki/ 是投影目录，缺了 wiki 命令会自己建，但先建出来能让备份一眼看全
//   - manifest.json 是迁移与导出的身份证明（哪个格式版本、什么时候建的）
//   - AGENTS.block.md 是宿主约定文件的正文真源，禁止每家宿主各写一份文案
func EnsureScaffold(home string) error {
	for _, dir := range []string{filepath.Join(home, "wiki")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	manifest := filepath.Join(home, "manifest.json")
	if _, err := os.Stat(manifest); os.IsNotExist(err) {
		payload := map[string]any{
			"format":      1,
			"created_at":  time.Now().UnixMilli(),
			"engine":      "brainx",
			"home_format": "single-dir-v1",
		}
		data, _ := json.MarshalIndent(payload, "", "  ")
		if err := os.WriteFile(manifest, data, 0o600); err != nil {
			return err
		}
	}
	block := filepath.Join(home, "AGENTS.block.md")
	if _, err := os.Stat(block); os.IsNotExist(err) {
		if err := os.WriteFile(block, []byte(AGENTSBlock), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// MountWarning 判断 $BRAINX_HOME 是否落在网络/同步目录上。
//
// 为什么只用路径特征而不用 statfs：statfs 的字段在 macOS 与 Linux 上完全两套，
// 要么引 build tag 分平台，要么引 x/sys——为了一个 init 检查不值得。
// 路径特征是启发式，所以**允许 BRAINX_ALLOW_NETWORK_HOME=1 显式放行**，
// 但默认拦下：SQLite WAL 在网络文件系统上会间歇性损坏，这个代价太高。
func MountWarning(home string) string {
	if os.Getenv("BRAINX_ALLOW_NETWORK_HOME") == "1" {
		return ""
	}
	cleaned := filepath.ToSlash(home)
	lower := strings.ToLower(cleaned)
	markers := []struct{ marker, label string }{
		{"/volumes/", "外部卷（可能是 SMB/NFS 挂载）"},
		{"/net/", "网络挂载点"},
		{"/nfs", "NFS 挂载点"},
		{"/smb", "SMB 挂载点"},
		{"/library/mobile documents", "iCloud Drive"},
		{"/dropbox/", "Dropbox 同步目录"},
		{"/onedrive", "OneDrive 同步目录"},
		{"/google drive", "Google Drive 同步目录"},
		{"/nutstore", "坚果云同步目录"},
		{"/baidunetdisk", "百度网盘同步目录"},
		{"//", "UNC 网络路径"},
	}
	for _, m := range markers {
		if strings.Contains(lower, m.marker) {
			return m.label
		}
	}
	return ""
}

// Manifest 读回来给 export / 迁移演练用。
func Manifest(home string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(home, "manifest.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("manifest.json 无法解析: %w", err)
	}
	return out, nil
}
