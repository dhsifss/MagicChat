-- +goose Up
ALTER TABLE mobile_push_jobs ADD COLUMN submission_token_ciphertext BYTEA;

-- +goose Down
ALTER TABLE mobile_push_jobs DROP COLUMN submission_token_ciphertext;
