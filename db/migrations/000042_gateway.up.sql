CREATE TABLE payment_gateway_settings (
    society_id         uuid PRIMARY KEY REFERENCES societies(id) ON DELETE CASCADE,
    provider           text NOT NULL DEFAULT 'razorpay' CHECK (provider IN ('razorpay','fake')),
    key_id             text NOT NULL DEFAULT '',
    key_secret_enc     text,
    webhook_secret_enc text,
    active             boolean NOT NULL DEFAULT false,
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payment_orders (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    society_id        uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    unit_id           uuid NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    amount_paise      bigint NOT NULL CHECK (amount_paise > 0),
    provider          text NOT NULL,
    provider_order_id text,
    status            text NOT NULL DEFAULT 'created' CHECK (status IN ('created','paid','failed','expired')),
    token_hash        text NOT NULL,
    idem_key          text,
    created_by        uuid REFERENCES memberships(id),
    expires_at        timestamptz NOT NULL,
    paid_at           timestamptz,
    payment_id        uuid REFERENCES payments(id),
    note              text,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_orders_idem_uq ON payment_orders (society_id, idem_key) WHERE idem_key IS NOT NULL;
CREATE UNIQUE INDEX payment_orders_provider_uq ON payment_orders (provider, provider_order_id) WHERE provider_order_id IS NOT NULL;
CREATE INDEX payment_orders_unit_idx ON payment_orders (unit_id, created_at DESC);

CREATE TABLE payment_events (
    provider    text NOT NULL,
    event_id    text NOT NULL,
    society_id  uuid NOT NULL REFERENCES societies(id) ON DELETE CASCADE,
    payload     jsonb NOT NULL,
    processed   boolean NOT NULL DEFAULT false,
    note        text,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, event_id)
);
