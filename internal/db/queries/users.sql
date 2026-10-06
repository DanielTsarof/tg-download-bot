-- name: UpsertUser :one
-- Records the user on every update, keeping their profile fresh.
INSERT INTO users (id, username, first_name)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE
SET username = EXCLUDED.username,
    first_name = EXCLUDED.first_name,
    updated_at = CASE
        WHEN users.username IS DISTINCT FROM EXCLUDED.username
          OR users.first_name IS DISTINCT FROM EXCLUDED.first_name
        THEN now() ELSE users.updated_at END
RETURNING id, username, first_name, role, status, daily_limit, created_at, updated_at,
          (xmax = 0)::boolean AS inserted;

-- name: EnsureAdmin :exec
-- Bootstraps the admins listed in the configuration.
INSERT INTO users (id, role, status)
VALUES ($1, 'admin', 'active')
ON CONFLICT (id) DO UPDATE
SET role = 'admin', status = 'active', updated_at = now();

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserForUpdate :one
SELECT * FROM users WHERE id = $1 FOR UPDATE;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE lower(username) = lower(@username::text) LIMIT 1;

-- name: ListAdmins :many
SELECT * FROM users WHERE role = 'admin' AND status = 'active' ORDER BY id;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: ListUsersWithUsage :many
SELECT u.*,
       (SELECT count(*) FROM requests r
         WHERE r.user_id = u.id AND r.created_at >= @since::timestamptz) AS used_today
FROM users u
ORDER BY u.status = 'pending' DESC, u.created_at DESC
LIMIT $1 OFFSET $2;

-- name: SetUserStatus :one
UPDATE users SET status = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: SetUserRole :one
UPDATE users SET role = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: SetUserDailyLimit :one
UPDATE users SET daily_limit = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: GrantUser :one
-- Activates a user, creating them if they haven't written to the bot yet.
INSERT INTO users (id, status, daily_limit)
VALUES ($1, 'active', sqlc.narg(daily_limit))
ON CONFLICT (id) DO UPDATE
SET status = 'active',
    daily_limit = COALESCE(EXCLUDED.daily_limit, users.daily_limit),
    updated_at = now()
RETURNING *;
