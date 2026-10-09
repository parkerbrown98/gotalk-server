-- +goose Up

-- Topic voting can be turned off per place; ranking then falls back to the reactions on
-- each topic's opening post.
ALTER TABLE places ADD COLUMN voting_enabled BOOLEAN NOT NULL DEFAULT true;

-- Feed ranking, kept on the topic row and updated in the same transaction as each vote,
-- reply, deletion or reaction (see RefreshTopicRanks). hot_rank is time-independent: newer
-- topics start higher, so stored values stay comparable without periodic recomputation.
ALTER TABLE topics
    ADD COLUMN upvotes     INTEGER          NOT NULL DEFAULT 0,
    ADD COLUMN downvotes   INTEGER          NOT NULL DEFAULT 0,
    ADD COLUMN score       INTEGER          NOT NULL DEFAULT 0,
    ADD COLUMN hot_rank    DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN controversy DOUBLE PRECISION NOT NULL DEFAULT 0;

-- Keep in sync with RefreshTopicRanks in internal/store/queries/feeds.sql.
UPDATE topics t
SET score       = s.score,
    hot_rank    = sign(s.score + 2 * sqrt(GREATEST(t.post_count - 1, 0)))
                      * log(GREATEST(abs(s.score + 2 * sqrt(GREATEST(t.post_count - 1, 0))), 1))
                  + (extract(epoch FROM t.created_at)::float8 - 1704067200) / 45000,
    controversy = 0
FROM (
    SELECT tt.id,
           (CASE WHEN pl.voting_enabled THEN tt.upvotes - tt.downvotes
                 ELSE COALESCE(op.reaction_count, 0) END)::integer AS score
    FROM topics tt
    JOIN places pl ON pl.id = tt.place_id
    LEFT JOIN posts op ON op.topic_id = tt.id AND op.post_number = 1
) s
WHERE t.id = s.id;

CREATE INDEX topics_place_hot_idx ON topics (place_id, hot_rank DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX topics_place_new_idx ON topics (place_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX topics_place_top_idx ON topics (place_id, score DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX topics_place_controversy_idx ON topics (place_id, controversy DESC, id DESC)
    WHERE deleted_at IS NULL AND controversy > 0;
-- Instance-wide feeds (home and all) span places.
CREATE INDEX topics_hot_idx ON topics (hot_rank DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX topics_new_idx ON topics (created_at DESC, id DESC) WHERE deleted_at IS NULL;

-- One up or down vote per person and topic.
CREATE TABLE topic_votes (
    topic_id   UUID        NOT NULL REFERENCES topics (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    value      SMALLINT    NOT NULL CHECK (value IN (-1, 1)),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (topic_id, user_id)
);

CREATE INDEX topic_votes_user_id_idx ON topic_votes (user_id);

-- A topic is "read" once the person has opened it (opened_at set). Marking it unread clears
-- opened_at but keeps the read position. seen_post_number is the topic's last post number
-- at the latest open, so replies posted afterwards can be flagged.
ALTER TABLE topic_reads
    ADD COLUMN opened_at        TIMESTAMPTZ,
    ADD COLUMN first_opened_at  TIMESTAMPTZ,
    ADD COLUMN seen_post_number INTEGER NOT NULL DEFAULT 0;

UPDATE topic_reads
SET opened_at = updated_at, first_opened_at = updated_at, seen_post_number = last_read_post_number;

-- +goose Down
ALTER TABLE topic_reads
    DROP COLUMN seen_post_number,
    DROP COLUMN first_opened_at,
    DROP COLUMN opened_at;
DROP TABLE topic_votes;
DROP INDEX topics_new_idx;
DROP INDEX topics_hot_idx;
DROP INDEX topics_place_controversy_idx;
DROP INDEX topics_place_top_idx;
DROP INDEX topics_place_new_idx;
DROP INDEX topics_place_hot_idx;
ALTER TABLE topics
    DROP COLUMN controversy,
    DROP COLUMN hot_rank,
    DROP COLUMN score,
    DROP COLUMN downvotes,
    DROP COLUMN upvotes;
ALTER TABLE places DROP COLUMN voting_enabled;
