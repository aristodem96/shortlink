-- +goose Up
-- +goose StatementBegin

-- The key the bot acts with when a user shortens a link from the chat. It is a
-- reference, not a secret: the bot issued the user's keys itself, so it knows
-- which ones are theirs and never needs to hold a plaintext secret to use one.
-- ON DELETE SET NULL keeps the row valid if a key is ever deleted outright.
ALTER TABLE subscriptions
    ADD COLUMN active_key_id BIGINT REFERENCES api_keys (id) ON DELETE SET NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE subscriptions DROP COLUMN IF EXISTS active_key_id;
-- +goose StatementEnd
