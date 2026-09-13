-- +goose Up
-- +goose StatementBegin

-- Payments now sell two different things, so a row has to say which. The
-- default makes the change safe for rows that already exist: everything sold
-- before this migration was API access.
ALTER TABLE payments
    ADD COLUMN product TEXT NOT NULL DEFAULT 'access'
        CHECK (product IN ('access', 'domain'));

-- A domain purchase buys no days, so access_days can no longer be NOT NULL.
-- The replacement constraint keeps the two products honest: an access payment
-- must carry a positive number of days, a domain payment must carry none.
ALTER TABLE payments ALTER COLUMN access_days DROP NOT NULL;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_access_days_check;
ALTER TABLE payments ADD CONSTRAINT payments_access_days_check CHECK (
    (product = 'access' AND access_days IS NOT NULL AND access_days > 0)
 OR (product = 'domain' AND access_days IS NULL));

-- A domain a buyer brings of their own. hostname is UNIQUE across every user,
-- not just within one: two buyers must never both claim the same host, or the
-- on-demand TLS gate could not decide whose certificate to request.
CREATE TABLE custom_domains (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    telegram_user_id BIGINT      NOT NULL,
    hostname         TEXT        NOT NULL UNIQUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL until the domain's A record was seen pointing at this server.
    verified_at      TIMESTAMPTZ,

    CONSTRAINT custom_domains_hostname_shape CHECK (
        hostname = lower(hostname) AND length(hostname) BETWEEN 4 AND 253)
);

CREATE INDEX custom_domains_user_idx ON custom_domains (telegram_user_id);

ALTER TABLE subscriptions
    -- How many domains this buyer has paid for. One $5 payment buys one slot,
    -- and slots stack the way paid days do.
    ADD COLUMN domain_slots     INT NOT NULL DEFAULT 0 CHECK (domain_slots >= 0),
    -- The domain new links are stamped with, mirroring active_key_id.
    ADD COLUMN active_domain_id BIGINT REFERENCES custom_domains (id) ON DELETE SET NULL;

-- Which domain a link is served on. NULL means the operator's own domain,
-- which is every link that existed before this migration. ON DELETE SET NULL
-- rather than CASCADE: removing a domain must not destroy the buyer's links,
-- it moves them back to the default host.
ALTER TABLE links
    ADD COLUMN domain_id BIGINT REFERENCES custom_domains (id) ON DELETE SET NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE links DROP COLUMN IF EXISTS domain_id;
ALTER TABLE subscriptions
    DROP COLUMN IF EXISTS active_domain_id,
    DROP COLUMN IF EXISTS domain_slots;
DROP TABLE IF EXISTS custom_domains;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_access_days_check;
UPDATE payments SET access_days = 1 WHERE access_days IS NULL;
ALTER TABLE payments ALTER COLUMN access_days SET NOT NULL;
ALTER TABLE payments ADD CONSTRAINT payments_access_days_check CHECK (access_days > 0);
ALTER TABLE payments DROP COLUMN IF EXISTS product;
-- +goose StatementEnd
