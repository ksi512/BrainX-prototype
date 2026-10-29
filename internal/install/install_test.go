package install_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ksi512/BrainX-prototype/internal/install"
)

func TestInstallIsIdempotentAndUninstallable(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := install.Options{Exe: "/opt/brainx", Home: home, WorkDir: home}
	path := filepath.Join(home, ".claude", "settings.json")

	if _, err := install.Run(opts); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "hook --event session-start") {
		t.Fatalf("session-start hook missing: %s", first)
	}

	if result, err := install.Run(opts); err != nil {
		t.Fatal(err)
	} else if result[0].Action != "unchanged" {
		t.Fatalf("second install should be a no-op, got %q", result[0].Action)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("install rewrote identical content")
	}
	if !firstInfo.ModTime().Equal(secondInfo.ModTime()) {
		t.Fatal("install touched mtime without changing content")
	}

	if _, err := install.Run(install.Options{Exe: "/opt/brainx", Home: home, WorkDir: home, Uninstall: true}); err != nil {
		t.Fatal(err)
	}
	remaining, err := os.ReadFile(path)
	if err == nil && strings.Contains(string(remaining), "hook --event") {
		t.Fatalf("uninstall left brainx hooks behind: %s", remaining)
	}
}

func TestCursorUsesFlatCamelCaseConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Run(install.Options{Only: []string{"cursor"}, Exe: "/opt/brainx", Home: home, WorkDir: home}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["version"] != float64(1) {
		t.Fatalf("cursor config needs version 1: %s", data)
	}
	hooks, _ := config["hooks"].(map[string]any)
	for _, event := range []string{"sessionStart", "beforeSubmitPrompt", "stop"} {
		entries, _ := hooks[event].([]any)
		if len(entries) == 0 {
			t.Fatalf("cursor event %s missing: %s", event, data)
		}
		entry, _ := entries[0].(map[string]any)
		if _, ok := entry["command"]; !ok {
			t.Fatalf("cursor entry must be flat: %s", data)
		}
	}
}

func TestExplicitHostIsCreatedWhenMissing(t *testing.T) {
	home := t.TempDir()
	if _, err := install.Run(install.Options{Only: []string{"codex"}, Exe: "/opt/brainx", Home: home, WorkDir: home}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"additionalContextLimit": 5000`) {
		t.Fatalf("codex hook needs an explicit context limit: %s", data)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	results, err := install.Run(install.Options{DryRun: true, Only: []string{"claude-code"}, Exe: "/opt/brainx", Home: home, WorkDir: home})
	if err != nil {
		t.Fatal(err)
	}
	// 自举那一条（Host=self）在 dry-run 下可能是 unchanged，所以不能只看 results[0]
	wrote := false
	for _, result := range results {
		if result.Action == "would-write" {
			wrote = true
		}
		// 关键断言：dry-run 不得在 Home 之外留下任何东西
		if result.Path != "" && !strings.HasPrefix(result.Path, home) {
			t.Fatalf("dry-run 的落点跑到了 Home 之外：%s", result.Path)
		}
	}
	if !wrote {
		t.Fatalf("dry-run should report would-write: %#v", results)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote a file")
	}
}

func TestUninstallSelfRemovesBinaryAndPath(t *testing.T) {
	home := t.TempDir()
	prefix := filepath.Join(home, "brainx", "bin")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryName := "brainx"
	if runtime.GOOS == "windows" {
		binaryName = "brainx.exe"
	}
	binary := filepath.Join(prefix, binaryName)
	if err := os.WriteFile(binary, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Windows 的 PATH 不是文件，UninstallSelf 不改 PATH；这里只验证二进制回收。
	if runtime.GOOS == "windows" {
		if _, err := install.UninstallSelf(install.Options{Home: home, Prefix: prefix}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(binary); !os.IsNotExist(err) {
			t.Fatal("卸载后自装二进制仍在")
		}
		return
	}
	// shellPathFile 在 Linux 按 $SHELL 选 .zshenv/.profile；固定成 zsh，测试才不依赖 CI shell。
	t.Setenv("SHELL", "/bin/zsh")
	zshenv := filepath.Join(home, ".zshenv")
	block := "export FOO=1\n# brainx\nexport PATH=\"" + prefix + "\":$PATH\n"
	if err := os.WriteFile(zshenv, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := install.UninstallSelf(install.Options{Home: home, Prefix: prefix}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); !os.IsNotExist(err) {
		t.Fatal("卸载后自装二进制仍在")
	}
	data, err := os.ReadFile(zshenv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "brainx") || !strings.Contains(string(data), "export FOO=1") {
		t.Fatalf("PATH 行没有正确清理: %q", data)
	}
	if _, err := os.Stat(filepath.Join(home, "brainx")); !os.IsNotExist(err) {
		t.Fatal("卸载后空目录仍在")
	}
}
