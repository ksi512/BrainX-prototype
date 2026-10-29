package install_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ksi512/BrainX-prototype/internal/install"
)

// 约定文件的正文来自 $BRAINX_HOME/AGENTS.block.md（单一真源）。
// 测试必须把它准备好，否则 install 会如实报「读不到正文真源」并跳过。
func prepareBlock(t *testing.T, home string) {
	t.Helper()
	t.Setenv("BRAINX_HOME", home)
	t.Setenv("BRAINX_AGENTS_BLOCK", "")
	if err := os.WriteFile(filepath.Join(home, "AGENTS.block.md"),
		[]byte("<!-- brainx:begin -->\n## brainx 维护职责\n<!-- brainx:end -->\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Codex 每层**只读第一个非空文件**：AGENTS.override.md 存在时 AGENTS.md 是死代码。
// 所以 auto 必须写到 override 里，而且**不能动用户已经写在里面的内容**。
func TestAgentsBlockGoesIntoOverrideWhenPresent(t *testing.T) {
	home := t.TempDir()
	prepareBlock(t, home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(codexDir, "AGENTS.override.md")
	const userText = "# 我的临时覆盖\n\n这段时间只改前端，别动后端。\n"
	if err := os.WriteFile(override, []byte(userText), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := install.Options{Exe: "/opt/brainx", Home: home, WorkDir: home, Only: []string{"codex"}}
	if _, err := install.Run(opts); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(override)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, userText) {
		t.Fatalf("用户原有内容被改动了：\n%s", content)
	}
	if !strings.Contains(content, "<!-- brainx:begin -->") {
		t.Fatalf("brainx 段没写进 override：\n%s", content)
	}

	// AGENTS.md 不该再被创建：两份都写会让人以为装了两遍
	if _, err := os.Stat(filepath.Join(codexDir, "AGENTS.md")); err == nil {
		t.Fatal("有 override 时不应再写 AGENTS.md")
	}
}

// override 为空文件时不算「存在」——Codex 跳过空文件，会回落到 AGENTS.md。
func TestEmptyOverrideFallsBackToMain(t *testing.T) {
	home := t.TempDir()
	prepareBlock(t, home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "AGENTS.override.md"), []byte("  \n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := install.Options{Exe: "/opt/brainx", Home: home, WorkDir: home, Only: []string{"codex"}}
	if _, err := install.Run(opts); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(codexDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("空 override 时应回落到 AGENTS.md：%v", err)
	}
	if !strings.Contains(string(data), "<!-- brainx:begin -->") {
		t.Fatal("AGENTS.md 里缺少 brainx 段")
	}
}

// --agents-target=main 是显式覆盖：宁可写死代码也要落在 AGENTS.md。
func TestAgentsTargetMainForcesMain(t *testing.T) {
	home := t.TempDir()
	prepareBlock(t, home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "AGENTS.override.md"), []byte("# 覆盖\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := install.Options{Exe: "/opt/brainx", Home: home, WorkDir: home,
		Only: []string{"codex"}, AgentsTarget: "main"}
	if _, err := install.Run(opts); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(codexDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("强制 main 时应写 AGENTS.md：%v", err)
	}
	if !strings.Contains(string(data), "<!-- brainx:begin -->") {
		t.Fatal("AGENTS.md 里缺少 brainx 段")
	}
}

// 卸载必须两侧都清：用户装完之后删掉 override，auto 会指向 main，
// 只按 pick 走就会留下永远清不掉的残留段。
func TestUninstallClearsBothCandidates(t *testing.T) {
	home := t.TempDir()
	prepareBlock(t, home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(codexDir, "AGENTS.override.md")
	main := filepath.Join(codexDir, "AGENTS.md")
	const userText = "# 用户自己的覆盖\n"
	if err := os.WriteFile(override, []byte(userText), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := install.Options{Exe: "/opt/brainx", Home: home, WorkDir: home, Only: []string{"codex"}}
	if _, err := install.Run(opts); err != nil {
		t.Fatal(err)
	}
	// 再强制装一份到 main，制造「两个文件都有我们的段」的局面
	forced := opts
	forced.AgentsTarget = "main"
	if _, err := install.Run(forced); err != nil {
		t.Fatal(err)
	}

	uninstall := opts
	uninstall.Uninstall = true
	if _, err := install.Run(uninstall); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{override, main} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue // 只剩空文件时整个删掉也是对的
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "<!-- brainx:begin -->") {
			t.Fatalf("%s 里还留着 brainx 段：\n%s", path, data)
		}
	}
	data, err := os.ReadFile(override)
	if err != nil {
		t.Fatalf("override 里的用户内容不该被删：%v", err)
	}
	if !strings.Contains(string(data), userText) {
		t.Fatalf("用户内容丢了：\n%s", data)
	}
}

// 重复 install 必须字节级不变：Codex 的 hook 信任绑定命令 hash，
// 而约定文件反复重写会让同步工具误判全变。
func TestOverrideInstallIsByteStable(t *testing.T) {
	home := t.TempDir()
	prepareBlock(t, home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(codexDir, "AGENTS.override.md")
	if err := os.WriteFile(override, []byte("# 覆盖\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := install.Options{Exe: "/opt/brainx", Home: home, WorkDir: home, Only: []string{"codex"}}
	if _, err := install.Run(opts); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(override)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.Run(opts); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(override)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("第二次 install 改动了 override 的内容")
	}
}
