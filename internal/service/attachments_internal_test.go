package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/parkerbrown98/gotalk-server/internal/media"
)

func TestLinkURLs(t *testing.T) {
	for content, want := range map[string][]string{
		"no links here":                                                    nil,
		"see https://example.com/a.":                                       {"https://example.com/a"},
		"[docs](https://example.com/docs) and http://x.io":                 {"https://example.com/docs", "http://x.io"},
		"(https://en.wikipedia.org/wiki/Mortise_(joint))":                  {"https://en.wikipedia.org/wiki/Mortise_(joint)"},
		"**https://example.com/bold**":                                     {"https://example.com/bold"},
		"quiet <https://example.com/q> please":                             nil,
		"`https://example.com/code` inline":                                nil,
		"```\nhttps://example.com/fenced\n```\nhttps://example.com/after":  {"https://example.com/after"},
		"https://a.io https://a.io https://b.io":                           {"https://a.io", "https://b.io"},
		"https:// broken and ftp://x.io":                                   nil,
		strings.Repeat("https://e.io/1 https://e.io/2 https://e.io/3 ", 3): {"https://e.io/1", "https://e.io/2", "https://e.io/3"},
		"https://1.io https://2.io https://3.io https://4.io https://5.io https://6.io": {
			"https://1.io", "https://2.io", "https://3.io", "https://4.io", "https://5.io",
		},
	} {
		assert.Equal(t, want, linkURLs(content), content)
	}
}

func TestCleanFilename(t *testing.T) {
	file := media.Info{Ext: "bin"}
	img := media.Info{Ext: "png", Width: 2, Height: 2}
	assert.Equal(t, "plans.pdf", cleanFilename("C:\\Users\\me\\plans.pdf", file))
	assert.Equal(t, "plans.pdf", cleanFilename("../../plans.pdf", file))
	assert.Equal(t, "ab.txt", cleanFilename("a\x00\"b.txt", file))
	assert.Equal(t, "file", cleanFilename("", file))
	assert.Equal(t, "file", cleanFilename("..", file))
	assert.Equal(t, "image.png", cleanFilename("", img))
	long := cleanFilename(strings.Repeat("x", 300)+".jpeg", file)
	assert.Len(t, []rune(long), maxFilenameLen)
	assert.True(t, strings.HasSuffix(long, ".jpeg"))
	assert.Equal(t, "bin", fileExt("noext"))
	assert.Equal(t, "pdf", fileExt("A.PDF"))
	assert.Equal(t, "bin", fileExt("weird.p d f"))
}
