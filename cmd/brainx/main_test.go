package main

import (
	"errors"
	"strings"
	"testing"
)

func TestVersionAndHelpDoNotNeedStore(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatalf("--version 不应依赖数据库：%v", err)
	}
	if err := run([]string{"--help"}); err != nil {
		t.Fatalf("--help 不应依赖数据库：%v", err)
	}
	if !strings.Contains(versionText(), "brainx") {
		t.Fatalf("版本输出异常：%q", versionText())
	}
}

func TestUnknownCommandIsArgsError(t *testing.T) {
	err := run([]string{"definitely-not-a-command"})
	if !errors.Is(err, errArgs) {
		t.Fatalf("未知子命令应返回参数错误，实际：%v", err)
	}
}
