-- +goose Up
CREATE TABLE users (
    id          BIGINT PRIMARY KEY, -- Telegram user ID
    username    TEXT NOT NULL DEFAULT '',
    first_name  TEXT NOT NULL DEFAULT '',
    role        TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active', 'blocked')),
    daily_limit INTEGER CHECK (daily_limit >= 0), -- NULL: use the configured default
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX users_username_idx ON users (lower(username));

CREATE TABLE requests (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind              TEXT NOT NULL CHECK (kind IN ('command', 'inline')),
    query             TEXT NOT NULL,
    is_url            BOOLEAN NOT NULL,
    format            TEXT NOT NULL CHECK (format IN ('mp3', 'mp4', 'wav')),
    task_id           TEXT,
    status            TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    error             TEXT NOT NULL DEFAULT '',
    title             TEXT NOT NULL DEFAULT '',
    source_id         TEXT NOT NULL DEFAULT '',
    from_cache        BOOLEAN NOT NULL DEFAULT false,
    chat_id           BIGINT,         -- target chat for command requests
    status_message_id INTEGER,        -- "downloading…" message in that chat
    inline_message_id TEXT,           -- target for inline requests
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ
);

CREATE INDEX requests_user_created_idx ON requests (user_id, created_at DESC);
CREATE INDEX requests_created_idx ON requests (created_at DESC);
CREATE INDEX requests_unfinished_idx ON requests (id) WHERE status IN ('queued', 'running');

-- Telegram file IDs of files already uploaded, so repeats skip the service.
CREATE TABLE file_cache (
    lookup_key TEXT NOT NULL, -- normalized URL or lowercased search name
    format     TEXT NOT NULL,
    tg_file_id TEXT NOT NULL,
    media_kind TEXT NOT NULL CHECK (media_kind IN ('audio', 'video', 'document')),
    title      TEXT NOT NULL DEFAULT '',
    source_id  TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (lookup_key, format)
);

-- +goose Down
DROP TABLE file_cache;
DROP TABLE requests;
DROP TABLE users;
