package index

import (
	"strings"
	"unicode"
)

// Bigrams creates overlapping CJK character tokens while preserving contiguous
// Latin and numeric words. SQLite FTS sees the returned whitespace-delimited tokens.
func Bigrams(text string) string {
	var tokens []string
	var cjk []rune
	var latin []rune
	flushCJK := func() {
		for i := 0; i < len(cjk); i++ {
			if i+1 < len(cjk) {
				tokens = append(tokens, string(cjk[i:i+2]))
			} else if len(cjk) == 1 {
				tokens = append(tokens, string(cjk[i]))
			}
		}
		cjk = nil
	}
	flushLatin := func() {
		if len(latin) > 0 {
			tokens = append(tokens, string(latin))
			latin = nil
		}
	}
	for _, char := range []rune(text) {
		switch {
		case unicode.Is(unicode.Han, char):
			flushLatin()
			cjk = append(cjk, char)
		case unicode.IsLetter(char) || unicode.IsDigit(char) || char == '_' || char == '-':
			flushCJK()
			latin = append(latin, unicode.ToLower(char))
		default:
			flushCJK()
			flushLatin()
		}
	}
	flushCJK()
	flushLatin()
	return strings.Join(tokens, " ")
}

func Query(text string) string { return Bigrams(text) }
