ALTER TABLE paper_orders DROP CONSTRAINT paper_orders_status_check;
ALTER TABLE paper_orders ADD CONSTRAINT paper_orders_status_check
    CHECK (status IN ('reserved', 'unknown', 'partially_filled', 'cancelled', 'filled'));

ALTER TABLE paper_fills ADD COLUMN quantity_raw numeric(38,0);
-- Every fill in the preceding schema disposed of the original order quantity.
UPDATE paper_fills f SET quantity_raw=(o.request->>'quantity_raw')::numeric
    FROM paper_orders o WHERE (f.account_id, f.order_id)=(o.account_id, o.order_id);
ALTER TABLE paper_fills ALTER COLUMN quantity_raw SET NOT NULL;
ALTER TABLE paper_fills ADD CONSTRAINT paper_fills_quantity_raw_check
    CHECK (quantity_raw > 0 AND quantity_raw < 'Infinity'::numeric);
ALTER TABLE paper_fills DROP CONSTRAINT paper_fills_pkey;
ALTER TABLE paper_fills ADD PRIMARY KEY (account_id, order_id, event_id);
ALTER TABLE paper_fills ADD UNIQUE (account_id, event_id);
