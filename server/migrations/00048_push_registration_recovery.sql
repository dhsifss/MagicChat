-- +goose Up
ALTER TABLE user_push_grants DROP CONSTRAINT user_push_grants_status_check;
ALTER TABLE user_push_grants ADD CONSTRAINT user_push_grants_status_check CHECK (status IN ('active','disabled','registering'));
ALTER TABLE mobile_push_endpoint_revocations ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE mobile_push_endpoint_revocations ADD COLUMN attempts BIGINT NOT NULL DEFAULT 0;
CREATE INDEX mobile_push_endpoint_revocations_due ON mobile_push_endpoint_revocations(next_attempt_at,created_at,id);

-- +goose Down
DELETE FROM user_push_grants WHERE status='registering';
ALTER TABLE user_push_grants DROP CONSTRAINT user_push_grants_status_check;
ALTER TABLE user_push_grants ADD CONSTRAINT user_push_grants_status_check CHECK (status IN ('active','disabled'));
DROP INDEX mobile_push_endpoint_revocations_due;
ALTER TABLE mobile_push_endpoint_revocations DROP COLUMN attempts;
ALTER TABLE mobile_push_endpoint_revocations DROP COLUMN next_attempt_at;
