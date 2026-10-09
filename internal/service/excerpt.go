package service

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ExcerptLength is the maximum length, in characters, of a feed item's excerpt.
const ExcerptLength = 280

var (
	mdEscape     = regexp.MustCompile("\\\\([\\\\`*_{}\\[\\]()#+\\-.!~>|<])")
	mdFence      = regexp.MustCompile("(?s)(^|\n)\\s{0,3}(```|~~~).*?(\n\\s{0,3}(```|~~~)[^\n]*|$)")
	mdImage      = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink       = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	mdRefLink    = regexp.MustCompile(`\[([^\]]+)\]\[[^\]]*\]`)
	mdRefDef     = regexp.MustCompile(`(?m)^\s{0,3}\[[^\]]+\]:\s*\S+.*$`)
	mdAutolink   = regexp.MustCompile(`<((?:https?|mailto):[^>\s]+)>`)
	mdHTML       = regexp.MustCompile(`</?[A-Za-z][^>]*>|<!--.*?-->`)
	mdRule       = regexp.MustCompile(`(?m)^\s{0,3}([-*_]\s*){3,}$`)
	mdLinePrefix = regexp.MustCompile(`(?m)^\s{0,3}(?:>\s?)*(?:#{1,6}\s+|[-*+]\s+(?:\[[ xX]\]\s+)?|\d{1,9}[.)]\s+)?`)
	mdEmphasis   = regexp.MustCompile("[*`]+|~~")
	mdUnderscore = regexp.MustCompile(`(^|[^\p{L}\p{N}_])_+|_+([^\p{L}\p{N}_]|$)`)
)

// plainExcerpt turns Markdown into a short plain-text preview: formatting, links, images,
// code blocks and HTML are reduced to their visible text, whitespace is collapsed, and the
// result is cut at a word boundary to at most n characters (including the ellipsis).
func plainExcerpt(md string, n int) string {
	// Escaped characters are swapped for private-use runes so the steps below leave them alone.
	md = mdEscape.ReplaceAllStringFunc(md, func(m string) string { return string(0xE000 + rune(m[1])) })
	md = mdFence.ReplaceAllString(md, "$1 ")
	md = mdRefDef.ReplaceAllString(md, "")
	md = mdImage.ReplaceAllString(md, "$1")
	md = mdLink.ReplaceAllString(md, "$1")
	md = mdRefLink.ReplaceAllString(md, "$1")
	md = mdAutolink.ReplaceAllString(md, "$1")
	md = mdHTML.ReplaceAllString(md, " ")
	md = mdRule.ReplaceAllString(md, "")
	md = mdLinePrefix.ReplaceAllString(md, "")
	md = mdEmphasis.ReplaceAllString(md, "")
	md = mdUnderscore.ReplaceAllString(md, "$1$2")
	md = strings.Map(func(r rune) rune {
		if r >= 0xE000 && r < 0xE080 {
			return r - 0xE000
		}
		return r
	}, md)
	return truncateWords(strings.Join(strings.Fields(md), " "), n)
}

func truncateWords(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)[:n-1]
	cut := len(runes)
	for i := len(runes) - 1; i >= 0 && i > len(runes)-40; i-- {
		if unicode.IsSpace(runes[i]) {
			cut = i
			break
		}
	}
	out := strings.TrimRightFunc(string(runes[:cut]), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) && r != ')' && r != '"' && r != '\''
	})
	return out + "…"
}
