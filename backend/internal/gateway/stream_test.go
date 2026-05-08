package gateway

import (
	"testing"
)

func TestFindThinkingStartTag(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"<thinking>hello", 0},
		{"some text<thinking>hello", 9},
		{"`<thinking>`", -1},        // 反引号包裹
		{"\"<thinking>\"", -1},       // 双引号包裹
		{"no tag here", -1},
	}

	for _, tt := range tests {
		got := findThinkingStartTag(tt.input)
		if got != tt.want {
			t.Errorf("findThinkingStartTag(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFindThinkingEndTag(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"end here</thinking>\n\nmore", 8},
		{"`</thinking>`", -1},                // 反引号
		{"about </thinking> tag", -1},         // 后面不是 \n\n
		{"</thinking>", 0},                    // 缓冲区末尾
		{"</thinking>  ", 0},                  // 末尾空白
	}

	for _, tt := range tests {
		got := findThinkingEndTag(tt.input)
		if got != tt.want {
			t.Errorf("findThinkingEndTag(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input string
		min   int
	}{
		{"Hello", 1},
		{"Hello World, this is a test", 6},
		{"你好世界", 4}, // 4 chars × 4.0 units / 4 = 4
		{"", 0},
	}

	for _, tt := range tests {
		got := estimateTokens(tt.input)
		if got < tt.min {
			t.Errorf("estimateTokens(%q) = %d, expected >= %d", tt.input, got, tt.min)
		}
	}
}

func TestIsQuoteChar(t *testing.T) {
	quoteChars := "`\"'\\"
	for _, c := range quoteChars {
		if !isQuoteChar(byte(c)) {
			t.Errorf("expected %q to be a quote char", string(c))
		}
	}

	nonQuote := "abcABC 09"
	for _, c := range nonQuote {
		if isQuoteChar(byte(c)) {
			t.Errorf("expected %q to NOT be a quote char", string(c))
		}
	}
}
