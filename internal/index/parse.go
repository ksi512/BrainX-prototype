package index

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// ErrUnsupported 表示这个扩展名不在导入范围内。
// 明确报错比「原样塞进正文」好得多：PDF/图片的二进制灌进 FTS 会污染整个检索面。
var ErrUnsupported = errors.New("不支持的文件类型")

// Parse 按扩展名把原件抽成纯文本。正文必须是人可读的文本，
// 原件（容器格式）另存（见 store.ArchiveOrigin）。
func Parse(filename string, data []byte) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".md", ".markdown", ".txt", ".text", ".rst", ".log", ".go", ".py", ".js", ".ts", ".sh", ".json", ".yaml", ".yml", ".toml", ".csv":
		if !utf8.Valid(data) {
			return "", fmt.Errorf("%w：%s 不是合法的 UTF-8 文本", ErrUnsupported, ext)
		}
		return strings.TrimSpace(string(data)), nil
	case ".html", ".htm":
		return htmlText(data), nil
	case ".docx":
		return docxText(data)
	case ".pdf":
		return pdfText(data)
	case ".xlsx":
		return ParseXLSX(data)
	default:
		return "", fmt.Errorf("%w：%s（支持 md/txt/html/docx/pdf/xlsx；xls 请另存为 xlsx）", ErrUnsupported, ext)
	}
}

// htmlText 去掉标签、脚本、样式并解码实体。
// 不用 x/net/html：它只是为了一件事（取文本）引入一棵完整的树，
// 而这里的输入是「已经被用户另存过一次的网页」，宽容的线性扫描足够。
func htmlText(data []byte) string {
	source := string(data)
	var out strings.Builder
	skip := "" // 正在跳过的块：script / style
	for i := 0; i < len(source); {
		if source[i] != '<' {
			end := strings.IndexByte(source[i:], '<')
			if end < 0 {
				end = len(source) - i
			}
			if skip == "" {
				out.WriteString(source[i : i+end])
			}
			i += end
			continue
		}
		close := strings.IndexByte(source[i:], '>')
		if close < 0 {
			break
		}
		tag := strings.ToLower(strings.TrimSpace(source[i+1 : i+close]))
		i += close + 1
		switch {
		case strings.HasPrefix(tag, "script"):
			skip = "script"
		case strings.HasPrefix(tag, "style"):
			skip = "style"
		case strings.HasPrefix(tag, "/script"), strings.HasPrefix(tag, "/style"):
			skip = ""
		case strings.HasPrefix(tag, "br"), strings.HasPrefix(tag, "/p"), strings.HasPrefix(tag, "/div"),
			strings.HasPrefix(tag, "/li"), strings.HasPrefix(tag, "/tr"), strings.HasPrefix(tag, "/h1"),
			strings.HasPrefix(tag, "/h2"), strings.HasPrefix(tag, "/h3"), strings.HasPrefix(tag, "/table"):
			out.WriteByte('\n')
		}
	}
	text := html.UnescapeString(out.String())
	return collapseBlankLines(text)
}

// docxText 解 zip 里的 word/document.xml，取全部 <w:t> 文本。
// docx 本质就是 zip+xml，用 archive/zip + encoding/xml 自解，不引重依赖。
func docxText(data []byte) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx 不是合法的 zip 容器: %w", err)
	}
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		stream, err := file.Open()
		if err != nil {
			return "", err
		}
		defer stream.Close()
		decoder := xml.NewDecoder(stream)
		var out strings.Builder
		inText := false
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			switch element := token.(type) {
			case xml.StartElement:
				switch element.Name.Local {
				case "t":
					inText = true
				case "p":
					out.WriteByte('\n')
				}
			case xml.EndElement:
				switch element.Name.Local {
				case "t":
					inText = false
				case "p":
					out.WriteByte('\n')
				}
			case xml.CharData:
				if inText {
					out.Write([]byte(element))
				}
			}
		}
		return collapseBlankLines(out.String()), nil
	}
	return "", errors.New("docx 里找不到 word/document.xml")
}

// pdfText 抽 PDF 文本。纯 Go 实现，不引 cgo（硬约束 5）。
func pdfText(data []byte) (string, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("pdf 解析失败: %w", err)
	}
	plain, err := reader.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("pdf 取文本失败: %w", err)
	}
	buffer, err := io.ReadAll(io.LimitReader(plain, 32<<20))
	if err != nil {
		return "", err
	}
	return collapseBlankLines(string(buffer)), nil
}

// collapseBlankLines 压掉连续空行与行尾空白：
// 抽出来的文本往往是「每个视觉行一个换行」，原样进库会让 chunk 切得很碎。
func collapseBlankLines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\u00a0")
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ───────────────────────── xlsx ─────────────────────────

// ParseXLSX 把工作簿转成「每张表一棵 Markdown 表格」。
//
// 原版 §3.5 的规则：**保留行列关系，不是把单元格拍平成一串字**；
// 分块时表头必须随每片重复（由 chunk 侧负责，这里只保证表头在第一行）。
//
// 为什么自解 zip+xml 而不引 excelize：只为读文本，引一个带样式/公式/图表的库
// 会把二进制从 12MB 推到 30MB+，而我们需要的信息只在 sharedStrings 与 sheet 里。
func ParseXLSX(data []byte) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("xlsx 不是合法的 zip 容器: %w", err)
	}
	shared, err := xlsxSharedStrings(reader)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	sheets := 0
	for _, file := range reader.File {
		if !strings.HasPrefix(file.Name, "xl/worksheets/sheet") || !strings.HasSuffix(file.Name, ".xml") {
			continue
		}
		table, err := xlsxSheet(file, shared)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(table) == "" {
			continue
		}
		sheets++
		out.WriteString(table)
		out.WriteString("\n\n")
	}
	if sheets == 0 {
		return "", errors.New("xlsx 里没有可读的工作表内容")
	}
	return collapseBlankLines(out.String()), nil
}

func xlsxSharedStrings(reader *zip.Reader) ([]string, error) {
	for _, file := range reader.File {
		if file.Name != "xl/sharedStrings.xml" {
			continue
		}
		stream, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		var payload struct {
			Items []struct {
				Text string `xml:"t"`
				Runs []struct {
					Text string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		if err := xml.NewDecoder(stream).Decode(&payload); err != nil {
			return nil, err
		}
		out := make([]string, 0, len(payload.Items))
		for _, item := range payload.Items {
			text := item.Text
			if text == "" {
				var parts []string
				for _, run := range item.Runs {
					parts = append(parts, run.Text)
				}
				text = strings.Join(parts, "")
			}
			out = append(out, text)
		}
		return out, nil
	}
	return nil, nil
}

func xlsxSheet(file *zip.File, shared []string) (string, error) {
	stream, err := file.Open()
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var payload struct {
		Rows []struct {
			Cells []struct {
				Ref    string `xml:"r,attr"`
				Type   string `xml:"t,attr"`
				Value  string `xml:"v"`
				Inline struct {
					Text string `xml:"t"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.NewDecoder(stream).Decode(&payload); err != nil {
		return "", err
	}
	var rows [][]string
	width := 0
	for _, row := range payload.Rows {
		var cells []string
		column := 0
		for _, cell := range row.Cells {
			// r="C5"：按列字母定位，跳过的空列要补齐，否则表头与数据错位
			if col := xlsxColumn(cell.Ref); col > column {
				for len(cells) < col {
					cells = append(cells, "")
				}
				column = col
			}
			value := cell.Value
			if cell.Type == "s" {
				index, convErr := strconv.Atoi(strings.TrimSpace(cell.Value))
				if convErr == nil && index >= 0 && index < len(shared) {
					value = shared[index]
				}
			} else if cell.Type == "inlineStr" {
				value = cell.Inline.Text
			}
			cells = append(cells, strings.ReplaceAll(strings.TrimSpace(value), "|", "/"))
			column++
		}
		if len(cells) > width {
			width = len(cells)
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		return "", nil
	}
	b := &strings.Builder{}
	for i, row := range rows {
		for len(row) < width {
			row = append(row, "")
		}
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
		if i == 0 {
			// 表头分隔行：Markdown 表格靠它区分表头与数据
			sep := make([]string, width)
			for j := range sep {
				sep[j] = "---"
			}
			b.WriteString("| " + strings.Join(sep, " | ") + " |\n")
		}
	}
	return b.String(), nil
}

// xlsxColumn 把 "C5" 解析成 0-based 列号（C→2）。
func xlsxColumn(ref string) int {
	col := 0
	for _, char := range ref {
		if char < 'A' || char > 'Z' {
			break
		}
		col = col*26 + int(char-'A'+1)
	}
	if col == 0 {
		return 0
	}
	return col - 1
}
