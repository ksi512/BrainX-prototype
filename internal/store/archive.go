package store

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// 硬约束 11：库内文件引用只存相对路径。
//
// 为什么这条是硬约束：换机器 / 换用户名之后，绝对路径**静默断链**——
// 只有取原件那一刻才失败，而且报的是「文件不存在」，最难排查。
var (
	ErrAbsoluteRef = errors.New("库内引用不得使用绝对路径")
	ErrInvalidRef  = errors.New("库内引用格式非法")
)

// ToRel 把绝对路径转成相对 $BRAINX_HOME 的路径，并断言结果不是绝对路径。
//
// 写入前的最后一道闸：任何一条走进库里、以 / 或 ~ 或盘符开头的字符串，
// 都应该在这里被拦住，而不是等换机器那天才发现。
func (s *Store) ToRel(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ErrInvalidRef
	}
	if strings.HasPrefix(value, "~") {
		return "", fmt.Errorf("%w：%q（不要用 ~，库内不认家目录展开）", ErrAbsoluteRef, value)
	}
	if isAbsoluteRef(value) {
		// 绝对路径的唯一合法来源是「$BRAINX_HOME 之下」——转成相对才能入库。
		rel, err := filepath.Rel(s.Home, value)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("%w：%q 不在 $BRAINX_HOME 内", ErrAbsoluteRef, value)
		}
		value = rel
	}
	value = filepath.ToSlash(value)
	if isAbsoluteRef(value) || !SafeRelativePath(value) {
		return "", fmt.Errorf("%w：%q", ErrAbsoluteRef, value)
	}
	return value, nil
}

// ToAbs 把库内相对路径还原成绝对路径。相对的东西永远拼在 $BRAINX_HOME 上，
// 所以「换机器」只影响 $BRAINX_HOME 一个变量。
func (s *Store) ToAbs(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", ErrInvalidRef
	}
	if isAbsoluteRef(rel) || !SafeRelativePath(rel) {
		return "", fmt.Errorf("%w：%q", ErrAbsoluteRef, rel)
	}
	return filepath.Join(s.Home, filepath.FromSlash(rel)), nil
}

func isAbsoluteRef(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) {
		return true
	}
	if strings.HasPrefix(value, "~") {
		return true
	}
	// Windows 盘符：C: / C:\ / c:/
	if len(value) >= 2 && value[1] == ':' {
		return true
	}
	return false
}

// LocalArchivePath 生成 local 档的原件相对路径：files/<yyyy>/<id><ext>。
//
// 用 id 而不是原文件名：同名文件会互相覆盖，而文件名里的空格/中文/表情
// 在跨文件系统时会变成一堆兼容性问题。原文件名保留在 asset.title 里。
func LocalArchivePath(id, sourceName string) string {
	ext := strings.ToLower(filepath.Ext(sourceName))
	if len(ext) > 12 {
		ext = ""
	}
	return path.Join("files", time.Now().Format("2006"), id+ext)
}

// ArchiveOrigin 把原件落到 archive 指定的位置，返回库内引用。
//
// 三档语义（提示词 §7）：
//   - local：复制进 $BRAINX_HOME/files/，返回 files/<yyyy>/<id>.<ext>
//   - text-only：不留原件，返回 "text-only"
//   - ext:<root>:<relpath>：复制到命名根下（path 型根），返回同一字符串
func (s *Store) ArchiveOrigin(id, sourcePath, archive string) (string, string, error) {
	if archive == "" {
		archive = s.Config.ArchiveDefault
	}
	switch {
	case archive == "text-only":
		return "text-only", "", nil
	case archive == "local":
		rel := LocalArchivePath(id, sourcePath)
		abs, err := s.ToAbs(rel)
		if err != nil {
			return "", "", err
		}
		if err := copyFile(sourcePath, abs); err != nil {
			return "", "", err
		}
		hash, err := fileHash(abs)
		if err != nil {
			return "", "", err
		}
		return rel, hash, nil
	case strings.HasPrefix(archive, "ext:"):
		rootName, relIn, absRoot, err := s.resolveExt(archive)
		if err != nil {
			return "", "", err
		}
		_ = rootName
		if strings.HasPrefix(absRoot, "http://") || strings.HasPrefix(absRoot, "https://") {
			// URL 型根只能在 get --open 时下载，导入阶段不联网（硬约束 12）
			return archive, "", nil
		}
		if err := copyFile(sourcePath, filepath.Join(absRoot, filepath.FromSlash(relIn))); err != nil {
			return "", "", err
		}
		hash, err := fileHash(filepath.Join(absRoot, filepath.FromSlash(relIn)))
		if err != nil {
			return "", "", err
		}
		return archive, hash, nil
	default:
		return "", "", fmt.Errorf("%w：%q", ErrInvalidRef, archive)
	}
}

// resolveExt 拆 ext:<root>:<relpath> 并查命名根。
// 根找不到时**不报死**：正文已在库里，原件不可达只影响 get --open。
func (s *Store) resolveExt(archive string) (rootName, relIn, rootPath string, err error) {
	rest := strings.TrimPrefix(archive, "ext:")
	rootName, relIn, ok := strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(relIn) == "" {
		return "", "", "", fmt.Errorf("%w：%q（应为 ext:<root>:<relpath>）", ErrInvalidRef, archive)
	}
	rootPath, ok = s.Config.Roots[rootName]
	if !ok {
		return "", "", "", fmt.Errorf("%w：未配置 archive.roots.%s", ErrInvalidRef, rootName)
	}
	if !SafeRelativePath(relIn) {
		return "", "", "", fmt.Errorf("%w：%q 不是安全的相对路径", ErrInvalidRef, relIn)
	}
	return rootName, relIn, rootPath, nil
}

// ResolveOriginal 找原件的绝对路径（或 URL）。取不到时返回 ErrNotReachable，
// 调用方负责转成退出码 4 —— 原件不可达**绝不能**影响检索。
func (s *Store) ResolveOriginal(a Asset) (string, error) {
	switch {
	case a.Archive == "" || a.Archive == "text-only":
		return "", fmt.Errorf("该资产没有归档原件（archive=%q）", a.Archive)
	case strings.HasPrefix(a.Archive, "ext:"):
		_, rel, root, err := s.resolveExt(a.Archive)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(root, "http://") || strings.HasPrefix(root, "https://") {
			return strings.TrimSuffix(root, "/") + "/" + strings.TrimPrefix(rel, "/"), nil
		}
		return filepath.Join(root, filepath.FromSlash(rel)), nil
	default:
		return s.ToAbs(a.Archive)
	}
}

const fetchMaxBytes = 200 << 20

// FetchURL 把 URL 型根上的原件下到本地缓存目录。只在 get --open 时调，
// 其余路径一律不联网（硬约束 6 / 12）。
func (s *Store) FetchURL(url, archiveHash string) (string, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("%w：只允许 http/https", ErrInvalidRef)
	}
	dir := filepath.Join(s.Home, "files", "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := archiveHash
	if !ValidContentHash(name) {
		name = Hash(url)[:16]
	}
	ext := path.Ext(url)
	target := filepath.Join(dir, name+ext)
	if info, err := os.Stat(target); err == nil && info.Size() > 0 {
		return target, nil
	}
	timeout, err := time.ParseDuration(s.Config.FetchTimeout)
	if err != nil || timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("%w：%v", ErrDownload, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w：HTTP %d", ErrDownload, resp.StatusCode)
	}
	temp, err := os.CreateTemp(dir, "fetch-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	limit := int64(s.Config.FetchMaxBytes)
	if limit <= 0 {
		limit = fetchMaxBytes
	}
	// 多读 1 字节才能区分「正好等于上限」和「超过上限」：
	// 前者允许，后者拒绝。配置上限最大 2 GiB，limit+1 不会溢出。
	written, err := io.Copy(temp, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		temp.Close()
		return "", err
	}
	if written > limit {
		temp.Close()
		return "", fmt.Errorf("%w：超过 %d 字节上限（fetch.max_bytes）", ErrDownload, limit)
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	return target, os.Rename(temp.Name(), target)
}

func copyFile(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil
	}
	return os.Chmod(target, info.Mode().Perm())
}

func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return Hash(string(data)), nil
}

// ErrDuplicate 表示同一正文已经入库。重复导入不新建——「已归档的重导 = 恢复」。
var ErrDuplicate = errors.New("同一正文已存在")

// ImportFile 是导入的写路径：先查重，再落原件（或记下外部引用），最后写资产。
//
// 顺序不能反：先落原件失败时库里不会留下一条「archive 指向空气」的资产。
func (s *Store) ImportFile(input CreateAsset, sourcePath, archive string) (Asset, error) {
	hash := Hash(input.Body)
	var existingID, existingStatus string
	if err := s.DB.QueryRow(`SELECT id,status FROM assets WHERE content_hash=? LIMIT 1`, hash).Scan(&existingID, &existingStatus); err == nil {
		switch existingStatus {
		case "active", "pending":
			return Asset{}, fmt.Errorf("%w：%s", ErrDuplicate, existingID)
		default:
			// 已归档的重导 = 恢复：只改状态，不碰正文
			if _, err := s.DB.Exec(`UPDATE assets SET status='active',archive_reason=NULL,updated_at=? WHERE id=?`, now(), existingID); err != nil {
				return Asset{}, err
			}
			restored, err := s.Get(existingID)
			if err != nil {
				return Asset{}, err
			}
			return restored, nil
		}
	}
	id := ID()
	archiveRef, archiveHash, err := s.ArchiveOrigin(id, sourcePath, archive)
	if err != nil {
		return Asset{}, err
	}
	input.ID = id
	input.Archive = archiveRef
	// origin 也必须是库内相对引用：`file:files/2026/xxx.md`
	input.Origin = "file:" + archiveRef
	asset, err := s.Create(input)
	if err != nil {
		return Asset{}, err
	}
	if archiveHash != "" {
		if _, err := s.DB.Exec(`UPDATE assets SET archive_hash=? WHERE id=?`, archiveHash, asset.ID); err != nil {
			return Asset{}, err
		}
		asset.ArchiveHash = archiveHash
	}
	return asset, nil
}

// ResolveOut 解析 `--out` 这类**命令行出口**参数。
//
// 注意与库内引用的区别：出口是本次调用的一次性参数，允许绝对路径；
// 库内引用（archive/origin）才禁止绝对路径。两者不能混为一谈。
func (s *Store) ResolveOut(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "ext:") {
		_, rel, root, err := s.resolveExt(value)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(root, "http://") || strings.HasPrefix(root, "https://") {
			return "", fmt.Errorf("%w：wiki 的 --out 不支持 URL 型根", ErrInvalidRef)
		}
		if !SafeRelativePath(rel) {
			return "", fmt.Errorf("%w：%q 不是安全的相对路径", ErrInvalidRef, rel)
		}
		return filepath.Join(root, filepath.FromSlash(rel)), nil
	}
	return value, nil
}

// ProbeReachable 做 1 秒可达性探测。
//
// 为什么不能直接 os.Stat：网络挂载点掉线时 stat 可能阻塞数秒到数十秒，
// 而 open 是交互命令，用户会以为程序挂了。
func ProbeReachable(path string) error {
	done := make(chan error, 1)
	go func() {
		info, err := os.Stat(path)
		if err != nil {
			done <- err
			return
		}
		if info.IsDir() {
			done <- fmt.Errorf("%q 是目录不是原件", path)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		return fmt.Errorf("原件在 1 秒内不可达（根可能未挂载）：%s", path)
	}
}
