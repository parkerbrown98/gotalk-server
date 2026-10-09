package service

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPlainExcerpt(t *testing.T) {
	for in, want := range map[string]string{
		"**Hello** _there_, [world](https://example.com)!":          "Hello there, world!",
		"# Title\n\nSome *text*\n> quoted\n- item one\n2. item two": "Title Some text quoted item one item two",
		"Before\n```go\nfunc main() {}\n```\nAfter":                 "Before After",
		"![a cat](https://x/cat.png) and `inline code`":             "a cat and inline code",
		"snake_case stays, but __bold__ does not":                   "snake_case stays, but bold does not",
		"<b>raw</b> html <script>alert(1)</script> gone":            "raw html alert(1) gone",
		"Escaped \\*stars\\* and \\_underscores\\_":                 "Escaped *stars* and _underscores_",
		"See <https://example.com/x> or [ref][1]\n\n[1]: https://x": "See https://example.com/x or ref",
		"---\n~~struck~~\n- [x] done":                               "struck done",
	} {
		require.Equal(t, want, plainExcerpt(in, ExcerptLength), "input %q", in)
	}

	long := strings.Repeat("word ", 100)
	got := plainExcerpt(long, 40)
	require.LessOrEqual(t, utf8.RuneCountInString(got), 40)
	require.True(t, strings.HasSuffix(got, "word…"), got)
	require.Equal(t, "short", plainExcerpt("short", 40))
	require.Equal(t, "", plainExcerpt("```\nonly code\n```", 40))
	require.Equal(t, "ééééé…", truncateWords(strings.Repeat("é", 50), 6))
}

func TestFeedCursor(t *testing.T) {
	rank := 1.0 / 3
	c := feedCursor{Sort: FeedSortHot, AsOf: time.Now().UTC().Truncate(time.Microsecond), ID: uuid.New(), Rank: &rank}
	raw := encodeCursor(c)

	got, err := decodeCursor(raw, FeedQuery{Sort: FeedSortHot})
	require.NoError(t, err)
	require.Equal(t, rank, *got.Rank, "floats survive the round trip exactly")
	require.True(t, c.AsOf.Equal(got.AsOf))

	for _, fq := range []FeedQuery{{Sort: FeedSortNew}, {Sort: FeedSortTop, Window: "week"}} {
		_, err := decodeCursor(raw, fq)
		require.Error(t, err, "a cursor only works with its own sort")
	}
	for _, bad := range []string{"%%%", "e30", strings.Repeat("a", maxCursorLen+1)} {
		_, err := decodeCursor(bad, FeedQuery{Sort: FeedSortHot})
		require.Error(t, err)
	}
	none, err := decodeCursor("", FeedQuery{Sort: FeedSortHot})
	require.NoError(t, err)
	require.Nil(t, none)
}

func TestFeedQueryNormalize(t *testing.T) {
	fq := FeedQuery{}
	require.NoError(t, fq.normalize())
	require.Equal(t, FeedSortHot, fq.Sort)
	require.EqualValues(t, DefaultFeedPageSize, fq.Limit)

	fq = FeedQuery{Sort: FeedSortTop, Limit: 1000}
	require.NoError(t, fq.normalize())
	require.Equal(t, DefaultFeedWindow, fq.Window)
	require.EqualValues(t, MaxFeedPageSize, fq.Limit)

	fq = FeedQuery{Sort: FeedSortNew, Window: "day"}
	require.NoError(t, fq.normalize())
	require.Empty(t, fq.Window, "only top and controversial use a window")

	require.Error(t, (&FeedQuery{Sort: "best"}).normalize())
	require.Error(t, (&FeedQuery{Sort: FeedSortTop, Window: "decade"}).normalize())
}

func TestVoteDelta(t *testing.T) {
	for _, tc := range []struct {
		prev, next int16
		up, down   int32
	}{
		{0, 1, 1, 0}, {0, -1, 0, 1}, {1, -1, -1, 1}, {-1, 1, 1, -1}, {1, 0, -1, 0}, {-1, 0, 0, -1}, {1, 1, 0, 0},
	} {
		up, down := voteDelta(tc.prev, tc.next)
		require.Equal(t, [2]int32{tc.up, tc.down}, [2]int32{up, down}, "%d -> %d", tc.prev, tc.next)
	}
}
