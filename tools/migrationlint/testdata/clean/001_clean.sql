-- Clean migration fixture: no floating-point column types.
-- Amounts are integer minor units; rates use exact NUMERIC.
CREATE TABLE clean_fixture (
    amount_minor BIGINT NOT NULL,
    rate NUMERIC(24, 12) NOT NULL,
    note TEXT
);

INSERT INTO clean_fixture (amount_minor, rate, note)
VALUES (1000, 1.234567890123, 'REAL and FLOAT in a string do not trigger');
