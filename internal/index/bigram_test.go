package index

import "testing"

func TestBigramsPreservesChineseOverlapAndLatinWords(t *testing.T) {
	got := Bigrams("记忆库与向量 docker compose")
	want := "记忆 忆库 库与 与向 向量 docker compose"
	if got != want {
		t.Fatalf("Bigrams() = %q, want %q", got, want)
	}
}

func TestChunkTextUsesRunes(t *testing.T) {
	chunks := ChunkText("甲乙丙丁戊己庚辛壬癸", 4, 6, 0, 1)
	if len(chunks) < 2 || chunks[0].EndOff != 6 || chunks[1].StartOff != 5 {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}
