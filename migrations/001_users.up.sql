CREATE TABLE tombstone_users (
    user_id uuid PRIMARY KEY,
    deleted_at timestamptz NOT NULL
);
