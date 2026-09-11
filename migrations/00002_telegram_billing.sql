-- +goose Up
-- +goose StatementBegin

-- Keys issued through the Telegram bot belong to a paying user and stop working
-- when that user's paid access ends. Keys issued with shortlinkctl keep both
-- columns NULL: an operator key with no owner and no expiry.
ALTER TABLE api_keys
    ADD COLUMN expires_at       TIMESTAMPTZ,
    ADD COLUMN telegram_user_id BIGINT;

CREATE INDEX api_keys_telegram_user_idx
    ON api_keys (telegram_user_id)
    WHERE telegram_user_id IS NOT NULL;

-- One row per Telegram user who has ever paid. paid_until only ever moves
-- forward: a renewal stacks on top of whatever time is left.
CREATE TABLE subscriptions (
    telegram_user_id BIGINT      PRIMARY KEY,
    paid_until       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE payments (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The payment provider's invoice id. UNIQUE is what makes confirming a
    -- payment idempotent: a webhook delivered twice cannot grant access twice.
    invoice_id       BIGINT        NOT NULL UNIQUE,
    telegram_user_id BIGINT        NOT NULL,
    amount           NUMERIC(12,2) NOT NULL,
    currency         TEXT          NOT NULL,
    access_days      INT           NOT NULL CHECK (access_days > 0),
    status           TEXT          NOT NULL CHECK (status IN ('pending', 'paid', 'expired')),
    pay_url          TEXT          NOT NULL,
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    paid_at          TIMESTAMPTZ
);

-- The reconciler scans only pending payments, so index only those.
CREATE INDEX payments_pending_idx ON payments (created_at) WHERE status = 'pending';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS subscriptions;
DROP INDEX IF EXISTS api_keys_telegram_user_idx;
ALTER TABLE api_keys DROP COLUMN IF EXISTS telegram_user_id;
ALTER TABLE api_keys DROP COLUMN IF EXISTS expires_at;
-- +goose StatementEnd
