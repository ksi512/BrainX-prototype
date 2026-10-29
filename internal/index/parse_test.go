package index

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestParsePlainText(t *testing.T) {
	text, err := Parse("note.md", []byte("# 标题\n\n正文内容"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "正文内容") {
		t.Fatalf("md 解析丢了正文：%q", text)
	}
}

func TestParseHTMLStripsScriptAndStyle(t *testing.T) {
	source := `<html><head><style>.a{color:red}</style><script>var x=1</script></head>
<body><p>第一段</p><p>第二段 &amp; 实体</p></body></html>`
	text, err := Parse("page.html", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"var x", "color:red", "<p>"} {
		if strings.Contains(text, bad) {
			t.Fatalf("html 解析残留了 %q：%q", bad, text)
		}
	}
	if !strings.Contains(text, "第一段") || !strings.Contains(text, "第二段 & 实体") {
		t.Fatalf("html 正文没抽全：%q", text)
	}
}

// docx 本质是 zip+xml，用最小构造验证 <w:t> 抽取与段落换行。
func TestParseDocx(t *testing.T) {
	buffer := &bytes.Buffer{}
	writer := zip.NewWriter(buffer)
	file, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	document := `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:r><w:t>第一个段落</w:t></w:r></w:p>
<w:p><w:r><w:t>第二个段落</w:t></w:r></w:p>
</w:body></w:document>`
	if _, err := file.Write([]byte(document)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	text, err := Parse("doc.docx", buffer.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "第一个段落") || !strings.Contains(text, "第二个段落") {
		t.Fatalf("docx 没抽到全部段落：%q", text)
	}
	if !strings.Contains(text, "\n") {
		t.Fatalf("docx 段落之间应当有换行：%q", text)
	}
}

// 不支持的扩展名必须明确报错，而不是把二进制塞进正文污染 FTS。
func TestParseRejectsUnknown(t *testing.T) {
	if _, err := Parse("sheet.xlsx", []byte("PK\x03\x04binary")); err == nil {
		t.Fatal("xlsx 应当被拒绝（设计里明确不做表格解析）")
	}
}
