-- +goose Up
-- M4-13: constrain the audit actor id format (FIN-33; spec section 4.1).
--
-- The id half of the canonical "type:id" actor is 1-128 characters and
-- contains no whitespace, control characters, or colons. The Go constructors
-- enforce the same ASCII subset; PostgreSQL's POSIX classes are the
-- authoritative Unicode backstop.
--
-- The pre-check fails closed before the constraint is added. It never updates,
-- trims, or normalizes rows, so existing row hashes and the chain stay valid.
-- The exception reports only the number of offending rows, never actor ids.
-- +goose StatementBegin
DO $$
DECLARE
    invalid_count BIGINT;
BEGIN
    SELECT count(*) INTO invalid_count
      FROM audit_log
     WHERE actor_id IS NULL
        OR actor_id !~ '^[^[:space:][:cntrl:]:]{1,128}$';
    IF invalid_count > 0 THEN
        RAISE EXCEPTION 'audit_log_actor_id_format: % existing audit_log row(s) have an invalid actor_id; refusing to add the constraint', invalid_count
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_actor_id_format
    CHECK (actor_id ~ '^[^[:space:][:cntrl:]:]{1,128}$');

-- +goose Down
ALTER TABLE audit_log
    DROP CONSTRAINT IF EXISTS audit_log_actor_id_format;
