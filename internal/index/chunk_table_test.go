package index

import (
	"strings"
	"testing"
)

// 表格切开后每片都必须带表头，否则检索出来只有数字没有列名。
func TestChunkTableRepeatsHeader(t *testing.T) {
	var b strings.Builder
	b.WriteString("| 端口 | 用途 |\n| --- | --- |\n")
	for i := 0; i < 200; i++ {
		b.WriteString("| 3333 | 很长的用途说明用来把表格撑开，确保会被切成多片 |\n")
	}
	chunks := ChunkText(b.String(), 200, 300, 50, 20)
	if len(chunks) < 2 {
		t.Fatalf("表格应当被切成多片，实际 %d 片", len(chunks))
	}
	for i, chunk := range chunks {
		if !strings.Contains(chunk.Text, "| 端口 | 用途 |") {
			t.Fatalf("第 %d 片丢了表头：%q", i, chunk.Text)
		}
	}
}

// 混合文档：普通段落照常切，表格单独成片且带表头。
func TestChunkMixedDocument(t *testing.T) {
	body := "# 标题\n\n这是一段普通说明文字，长度不大。\n\n| 名称 | 值 |\n| --- | --- |\n| a | 1 |\n| b | 2 |\n\n结尾再写一段普通文字。"
	chunks := ChunkText(body, 100, 150, 20, 10)
	if len(chunks) == 0 {
		t.Fatal("应当有切片")
	}
	foundTable := false
	for _, chunk := range chunks {
		if strings.Contains(chunk.Text, "| 名称 | 值 |") {
			foundTable = true
			if !strings.Contains(chunk.Text, "| a | 1 |") {
				t.Fatalf("表格片应包含数据行：%q", chunk.Text)
			}
		}
	}
	if !foundTable {
		t.Fatal("表格片丢了")
	}
}

func TestChunkPlainStillWorks(t *testing.T) {
	chunks := ChunkText(strings.Repeat("普通句子。", 100), 100, 150, 20, 10)
	if len(chunks) < 2 {
		t.Fatalf("长文本应被切开，实际 %d", len(chunks))
	}
}
