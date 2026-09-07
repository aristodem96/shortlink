-- +goose Up
-- +goose StatementBegin

-- Owners of links. Only the SHA-256 of the secret is stored, so a database
-- dump never yields a usable credential.
CREATE TABLE api_keys (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT        NOT NULL,
    key_hash   BYTEA       NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

CREATE TABLE links (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code       TEXT        NOT NULL,
    target_url TEXT        NOT NULL,
    owner_id   BIGINT      NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT links_code_not_blank CHECK (length(code) BETWEEN 3 AND 32)
);

-- The redirect hot path looks a link up by code, so this unique index is both
-- the correctness guarantee against collisions and the index that serves reads.
CREATE UNIQUE INDEX links_code_key ON links (code);

-- Supports "list my links, newest first" without a sort.
CREATE INDEX links_owner_id_created_at_idx ON links (owner_id, created_at DESC);

CREATE TABLE clicks (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    link_id    BIGINT      NOT NULL REFERENCES links (id) ON DELETE CASCADE,
    clicked_at TIMESTAMPTZ NOT NULL,
    referer    TEXT,
    user_agent TEXT,
    -- Salted SHA-256 of the visitor IP: enough to count unique visitors,
    -- not enough to identify one.
    ip_hash    BYTEA
);

-- Every stats query is "one link over a time window", which this index serves.
CREATE INDEX clicks_link_id_clicked_at_idx ON clicks (link_id, clicked_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS clicks;
DROP TABLE IF EXISTS links;
DROP TABLE IF EXISTS api_keys;
-- +goose StatementEnd
