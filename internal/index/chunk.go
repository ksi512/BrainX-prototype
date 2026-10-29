package index

import "strings"

type Chunk struct {
	Text     string
	StartOff int
	EndOff   int
}

// ChunkText 把正文切成片。两条规则来自提示词 §3.6 / §3.5：
//
//   - 普通文本：按目标字数切，优先落在段落/句末，尾部保留有限重叠（跨片边界也能搜到）
//   - **Markdown 表格：不按字数硬切，按行组分块，并且每片都带上表头**
//
// 表格那条最容易被忽略：切开后只剩数字、没有列名，检索出来就无法理解。
// xlsx 导入的产物全是表格，所以这不是边角场景。
func ChunkText(body string, target, max, min, overlap int) []Chunk {
	runes := []rune(body)
	if len(runes) == 0 {
		return nil
	}
	if target <= 0 {
		target = 400
	}
	if max < target {
		max = target + 200
	}
	if min < 0 {
		min = 0
	}
	blocks := splitBlocks(body)
	// 没有表格就走原来的纯文本路径，保证既有行为不变
	if len(blocks) == 1 && blocks[0].table == nil {
		return chunkProse(runes, 0, target, max, min, overlap)
	}

	var chunks []Chunk
	var pending []Chunk // 累积中的普通文本片，按需与相邻合并
	flush := func() {
		chunks = append(chunks, pending...)
		pending = nil
	}
	for _, block := range blocks {
		switch {
		case block.table != nil:
			flush()
			chunks = append(chunks, chunkTable(block.table, target)...)
		default:
			prose := chunkProse(runes[block.start:block.end], block.start, target, max, min, overlap)
			pending = append(pending, prose...)
		}
	}
	flush()
	if len(chunks) > 1 && len([]rune(chunks[len(chunks)-1].Text)) < min {
		previous := &chunks[len(chunks)-2]
		previous.Text += chunks[len(chunks)-1].Text
		previous.EndOff = chunks[len(chunks)-1].EndOff
		chunks = chunks[:len(chunks)-1]
	}
	return chunks
}

type block struct {
	start, end int // rune 偏移
	table      *tableBlock
}

type tableBlock struct {
	header []string // 表头行 + 分隔行
	rows   []string
}

// splitBlocks 把正文按「表格块 / 普通块」切开。表格行的判据是行首（去空白后）为 |。
func splitBlocks(body string) []block {
	lines := strings.Split(body, "\n")
	var blocks []block
	runeOffset := 0
	i := 0
	for i < len(lines) {
		line := lines[i]
		if isTableLine(line) {
			start := runeOffset
			var table tableBlock
			var tableLines []string
			for i < len(lines) && isTableLine(lines[i]) {
				tableLines = append(tableLines, lines[i])
				runeOffset += len([]rune(lines[i])) + 1
				i++
			}
			if len(tableLines) >= 2 {
				// 前两行是表头与分隔行，其余是数据行
				table.header = append([]string{}, tableLines[0], tableLines[1])
				table.rows = tableLines[2:]
			} else {
				// 只有一行（不构成 Markdown 表格）当作普通文本
				blocks = append(blocks, block{start: start, end: runeOffset - 1})
				continue
			}
			blocks = append(blocks, block{start: start, end: runeOffset - 1, table: &table})
			continue
		}
		start := runeOffset
		for i < len(lines) && !isTableLine(lines[i]) {
			runeOffset += len([]rune(lines[i])) + 1
			i++
		}
		blocks = append(blocks, block{start: start, end: runeOffset - 1})
	}
	return blocks
}

func isTableLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2
}

// chunkTable 按行组分块，**每片都带表头**。表头不计入 target 预算的一半以上，
// 否则窄表会退化成「每片只有表头 + 一行」。
func chunkTable(table *tableBlock, target int) []Chunk {
	header := strings.Join(table.header, "\n")
	headerRunes := len([]rune(header))
	budget := target - headerRunes - 1
	if budget < 40 {
		budget = 40
	}
	var chunks []Chunk
	var group []string
	size := 0
	flush := func() {
		if len(group) == 0 {
			return
		}
		text := header + "\n" + strings.Join(group, "\n")
		chunks = append(chunks, Chunk{Text: text, StartOff: 0, EndOff: len([]rune(text))})
		group = nil
		size = 0
	}
	for _, row := range table.rows {
		rowSize := len([]rune(row)) + 1
		if size+rowSize > budget && len(group) > 0 {
			flush()
		}
		group = append(group, row)
		size += rowSize
	}
	flush()
	if len(chunks) == 0 {
		// 只有表头没有数据行：表头本身也是一片（空表也要能被搜到列名）
		text := header
		chunks = append(chunks, Chunk{Text: text, StartOff: 0, EndOff: len([]rune(text))})
	}
	return chunks
}

// chunkProse 是原来的纯文本切分，接收 rune 切片与基准偏移，便于在混合文档里复用。
func chunkProse(runes []rune, base, target, max, min, overlap int) []Chunk {
	if len(runes) == 0 {
		return nil
	}
	var chunks []Chunk
	start := 0
	for start < len(runes) {
		end := start + target
		if end >= len(runes) {
			end = len(runes)
		} else {
			limit := start + max
			if limit > len(runes) {
				limit = len(runes)
			}
			best := -1
			for pos := end; pos < limit; pos++ {
				if runes[pos] == '\n' || strings.ContainsRune("。！？；.?!", runes[pos]) {
					best = pos + 1
					if runes[pos] == '\n' {
						break
					}
				}
			}
			if best > start {
				end = best
			} else if limit < len(runes) {
				end = limit
			}
		}
		chunks = append(chunks, Chunk{Text: string(runes[start:end]), StartOff: base + start, EndOff: base + end})
		if end == len(runes) {
			break
		}
		next := end - overlap
		if next <= start {
			next = end
		}
		start = next
	}
	if len(chunks) > 1 && len([]rune(chunks[len(chunks)-1].Text)) < min {
		previous := &chunks[len(chunks)-2]
		previous.Text += chunks[len(chunks)-1].Text
		previous.EndOff = chunks[len(chunks)-1].EndOff
		chunks = chunks[:len(chunks)-1]
	}
	return chunks
}
