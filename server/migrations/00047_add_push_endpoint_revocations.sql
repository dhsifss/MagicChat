-- +goose Up
-- No foreign keys: cleanup must survive session/user/grant cascades.
CREATE TABLE mobile_push_endpoint_revocations (
    id UUID PRIMARY KEY,
    installation_id UUID NOT NULL,
    send_token_ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX mobile_push_endpoint_revocations_installation ON mobile_push_endpoint_revocations(installation_id);
CREATE INDEX mobile_push_endpoint_revocations_created ON mobile_push_endpoint_revocations(created_at);

-- +goose StatementBegin
CREATE FUNCTION retain_mobile_push_endpoint_revocation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO mobile_push_endpoint_revocations(id, installation_id, send_token_ciphertext)
    VALUES (OLD.id, OLD.installation_id, OLD.send_token_ciphertext)
    ON CONFLICT (id) DO NOTHING;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER retain_mobile_push_endpoint_revocation
    BEFORE DELETE ON user_push_grants
    FOR EACH ROW EXECUTE FUNCTION retain_mobile_push_endpoint_revocation();

-- +goose Down
DROP TRIGGER retain_mobile_push_endpoint_revocation ON user_push_grants;
DROP FUNCTION retain_mobile_push_endpoint_revocation();
DROP TABLE mobile_push_endpoint_revocations;
