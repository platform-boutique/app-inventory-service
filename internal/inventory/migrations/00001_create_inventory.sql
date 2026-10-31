-- +goose Up
-- Stock per product and the reservations that hold part of it. `available` is what can still be reserved:
-- a reservation takes its quantities out of `available` when it is made and gives them back when it is
-- released or expires; a committed reservation keeps them taken (sold). The CHECK is the last line of
-- defence against overselling: the code also decrements with a conditional UPDATE.
CREATE TABLE stock (
    product_id text    PRIMARY KEY CHECK (char_length(product_id) BETWEEN 1 AND 128),
    available  integer NOT NULL CHECK (available >= 0)
);

CREATE TABLE reservations (
    order_id   text        PRIMARY KEY CHECK (char_length(order_id) BETWEEN 1 AND 64),
    status     text        NOT NULL CHECK (status IN ('RESERVED', 'COMMITTED', 'RELEASED')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at)
);

-- the sweep and the lazy expiry only look at holds that can still expire
CREATE INDEX reservations_expiry_idx ON reservations (expires_at) WHERE status = 'RESERVED';

CREATE TABLE reservation_items (
    order_id   text    NOT NULL REFERENCES reservations (order_id) ON DELETE CASCADE,
    product_id text    NOT NULL REFERENCES stock (product_id),
    quantity   integer NOT NULL CHECK (quantity BETWEEN 1 AND 1000),
    PRIMARY KEY (order_id, product_id)
);

-- "which holds touch this product" (lazy expiry before Reserve/GetStock)
CREATE INDEX reservation_items_product_idx ON reservation_items (product_id);

-- +goose Down
DROP TABLE reservation_items;
DROP TABLE reservations;
DROP TABLE stock;
