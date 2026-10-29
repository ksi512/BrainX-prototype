package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ksi512/BrainX-prototype/internal/hook"
	"github.com/ksi512/BrainX-prototype/internal/scope"
	"github.com/ksi512/BrainX-prototype/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSessionStartRendersThreeFormats(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Title: "部署", Body: "部署使用 SQLite 单文件"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ format, key string }{
		{"claude-code", "hookSpecificOutput"},
		{"codex", "hookSpecificOutput"},
		{"cursor", "additional_context"},
	} {
		out, err := hook.Run(s, "session-start", tc.format, strings.NewReader(`{"cwd":"/tmp/project","hook_event_name":"SessionStart"}`))
		if err != nil {
			t.Fatalf("%s: %v", tc.format, err)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("%s: invalid JSON %q: %v", tc.format, out, err)
		}
		if _, ok := parsed[tc.key]; !ok {
			t.Fatalf("%s: missing %s in %s", tc.format, tc.key, out)
		}
		if !strings.Contains(out, "SQLite") {
			t.Fatalf("%s: brief lost the memory: %s", tc.format, out)
		}
	}
}

func TestStopWritesNothing(t *testing.T) {
	s := newStore(t)
	out, err := hook.Run(s, "stop", "claude-code", strings.NewReader(`{"cwd":"/tmp/project"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("stop must keep stdout empty, got %q", out)
	}
}

func TestUserPromptOnlyInjectsChanges(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Body: "仓库使用 SQLite"}); err != nil {
		t.Fatal(err)
	}
	if out, err := hook.Run(s, "session-start", "claude-code", strings.NewReader(`{"cwd":"/tmp/project"}`)); err != nil || out == "" {
		t.Fatalf("session-start should emit the full brief: %q, %v", out, err)
	}
	if out, err := hook.Run(s, "user-prompt", "claude-code", strings.NewReader(`{"cwd":"/tmp/project","prompt":"继续"}`)); err != nil || out != "" {
		t.Fatalf("unchanged user-prompt must emit nothing: %q, %v", out, err)
	}

	time.Sleep(5 * time.Millisecond)
	if _, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Body: "新增记忆：部署走单文件"}); err != nil {
		t.Fatal(err)
	}
	out, err := hook.Run(s, "user-prompt", "claude-code", strings.NewReader(`{"cwd":"/tmp/project","prompt":"继续"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "新增记忆") {
		t.Fatalf("changed user-prompt should inject the delta: %q", out)
	}
	if again, err := hook.Run(s, "user-prompt", "claude-code", strings.NewReader(`{"cwd":"/tmp/project"}`)); err != nil || again != "" {
		t.Fatalf("delta must not repeat: %q, %v", again, err)
	}
}

func TestEventCanComeFromStdin(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(store.CreateAsset{Kind: "fact", Scope: "global", Body: "事件可以从 stdin 推断"}); err != nil {
		t.Fatal(err)
	}
	out, err := hook.Run(s, "", "claude-code", strings.NewReader(`{"hook_event_name":"SessionStart","cwd":"/tmp/project"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "additionalContext") {
		t.Fatalf("stdin event was not honoured: %q", out)
	}
}

// 子目录必须与仓库根目录共用同一份 brief。
// 用 cwd 当 scope 时，这条测试会失败——那正是「在子目录里静默失忆」的现场。
func TestSubdirectoryGetsSameBriefAsRepoRoot(t *testing.T) {
	s := newStore(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = git@github.com:L/hookdemo.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "internal", "store")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	scope := scope.Detect(root).Scope
	if _, err := s.Create(store.CreateAsset{Kind: "fact", Scope: scope, Body: "本项目测试命令是 go test ./..."}); err != nil {
		t.Fatal(err)
	}

	for _, cwd := range []string{root, deep} {
		payload := `{"cwd":"` + cwd + `","hook_event_name":"SessionStart"}`
		out, err := hook.Run(s, "session-start", "claude-code", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "go test") {
			t.Fatalf("cwd=%s 时 brief 丢了项目记忆：%q", cwd, out)
		}
	}
}
