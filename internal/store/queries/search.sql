-- name: SearchPosts :many
-- Full-text search over live posts. Scope is either the explicit board_ids (per-place
-- search) or, with public_only, boards readable by signed-out visitors (instance-wide).
-- Relevance blends text rank with reactions and recency (90-day decay scale).
WITH q AS (
    SELECT websearch_to_tsquery('simple', @query::text) AS tsq
),
hits AS (
    SELECT p.id,
           p.created_at,
           (ts_rank_cd(d.vector, q.tsq)::float8
                * (1 + ln(1 + p.reaction_count::float8) / 5)
                / (1 + extract(epoch FROM now() - p.created_at)::float8 / 7776000))::float8 AS rank
    FROM search_documents d
    CROSS JOIN q
    JOIN posts p ON p.id = d.post_id
    JOIN topics t ON t.id = p.topic_id
    JOIN boards b ON b.id = p.board_id
    JOIN places pl ON pl.id = p.place_id
    WHERE d.vector @@ q.tsq
      AND p.deleted_at IS NULL
      AND t.deleted_at IS NULL
      AND pl.deleted_at IS NULL
      AND ((@public_only::boolean AND b.is_public)
           OR (NOT @public_only::boolean AND p.board_id = ANY(@board_ids::uuid[])))
      AND (sqlc.narg('place_id')::uuid IS NULL OR p.place_id = sqlc.narg('place_id')::uuid)
      AND (sqlc.narg('board_id')::uuid IS NULL OR p.board_id = sqlc.narg('board_id')::uuid)
      AND (sqlc.narg('author_id')::uuid IS NULL OR p.author_id = sqlc.narg('author_id')::uuid)
      AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag')::text = ANY(t.tags))
      AND (sqlc.narg('solved')::boolean IS NULL
           OR (t.solution_post_id IS NOT NULL) = sqlc.narg('solved')::boolean)
      AND (sqlc.narg('after')::timestamptz IS NULL OR p.created_at >= sqlc.narg('after')::timestamptz)
      AND (sqlc.narg('before')::timestamptz IS NULL OR p.created_at < sqlc.narg('before')::timestamptz)
      AND (NOT @topics_only::boolean OR p.post_number = 1)
),
page AS (
    SELECT * FROM hits
    ORDER BY CASE WHEN @sort::text = 'newest' THEN hits.created_at END DESC,
             CASE WHEN @sort::text = 'oldest' THEN hits.created_at END ASC,
             hits.rank DESC,
             hits.id DESC
    LIMIT @lim OFFSET @off
)
SELECT sqlc.embed(p),
       sqlc.embed(t),
       page.rank::float8 AS rank,
       ts_headline('simple', p.content, q.tsq,
                   'StartSel="**", StopSel="**", MaxWords=35, MinWords=15, MaxFragments=2, FragmentDelimiter=" … "')::text AS snippet
FROM page
CROSS JOIN q
JOIN posts p ON p.id = page.id
JOIN topics t ON t.id = p.topic_id
ORDER BY CASE WHEN @sort::text = 'newest' THEN page.created_at END DESC,
         CASE WHEN @sort::text = 'oldest' THEN page.created_at END ASC,
         page.rank DESC,
         page.id DESC;
