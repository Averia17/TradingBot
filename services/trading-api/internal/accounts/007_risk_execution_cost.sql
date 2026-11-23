ALTER TABLE paper_risk_refusals
    DROP CONSTRAINT paper_risk_refusals_reason_check,
    ADD CONSTRAINT paper_risk_refusals_reason_check CHECK (reason IN (
        'instrument_not_allowed', 'order_notional', 'maximum_fee', 'maximum_execution_cost',
        'held_quantity', 'unresolved_order', 'daily_turnover', 'minimum_free_cash',
        'instrument_concentration', 'group_concentration', 'gross_exposure'
    ));
