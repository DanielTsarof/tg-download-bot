# tg-file-downloader

A Telegram bot that downloads audio and video by link or search name via
[ytdl-service](openapi.json). It returns MP3, MP4 or WAV files in chat or in
inline mode. Built with `github.com/go-telegram/bot`, PostgreSQL (`pgx`, `sqlc`,
`goose`) and Docker.

## How it works

The bot only uses the service's asynchronous task API (no webhooks):

1. `POST /v1/tasks` with `Idempotency-Key: tg-request-<id>`, so a retry never starts a second task.
2. It polls `GET /v1/tasks/{id}` every `POLL_INTERVAL` until the task succeeds or fails, or `TASK_TIMEOUT` runs out. It honours `Retry-After`.
3. It streams `GET /v1/tasks/{id}/file` straight into a Telegram upload: MP3 as audio, MP4 as video, WAV as a document.

Every uploaded file's Telegram `file_id` is cached per link/name and format. A
repeat request is answered instantly and doesn't touch the service. Requests
that are still running when the bot stops resume on the next start.

## Usage

| Command | |
|---|---|
| `/mp3 <link or name>`, `/wav …` | Download audio in that format |
| `/mp4 [360\|480\|720\|1080\|best] <link or name>` | Download video; the resolution is optional and defaults to 480p (e.g. `/mp4 720 https://…`) |
| any link or name as a message | Pick the format with buttons |
| `@your_bot <link>` in any chat | Inline: pick a format, and the posted message turns into the file (video in 480p) |
| `/history [page]`, `/me` | Your requests, role and today's usage |

Admin commands. `<user>` is a Telegram ID or `@username`:

| Command | |
|---|---|
| `/users [page]` | All users with status, role and today's usage |
| `/user <user>` | Details, statistics and recent requests |
| `/requests [user] [page]` | Request history, everyone's or one user's |
| `/grant <user> [limit]` | Grant access, optionally with a daily limit. Accepts the numeric ID of someone who hasn't written yet |
| `/revoke <user>` | Block a user |
| `/limit <user> <n\|default>` | Set or reset the daily limit |
| `/promote <user>`, `/demote <user>` | Change the role |

New users start as *pending*. Admins get a message with **Grant / Deny**
buttons when someone new writes to the bot. Limits count requests per UTC day,
including cached ones. Admins are unlimited. `ADMIN_IDS` from `.env` are made
admins on every start.

## Deployment

This assumes the ytdl-service stack is running in Docker, with containers
`ytdl-service-postgres-1` and `ytdl-service-app-1` on network `ytdl-service_default`.

1. **Database.** Create a separate database for the bot in the existing PostgreSQL:
   ```sh
   docker exec ytdl-service-postgres-1 createdb -U <postgres user> tgbot
   ```
   Migrations run automatically on start.
2. **API key.** A ytdl-service admin issues one:
   `POST /v1/users` (new user plus key) or `POST /v1/users/{id}/api-keys`.
3. **BotFather.** Create the bot, then turn on `/setinline` and `/setinlinefeedback` (set it to 100%).
   Without inline feedback, Telegram never reports which inline result was picked.
4. **Storage chat**, needed for inline mode. Create a private channel, add the bot as an admin and put the
   channel ID (`-100…`) into `STORAGE_CHAT_ID`. Telegram doesn't allow uploading
   a new file into an inline message. The bot uploads there first and then attaches the file by `file_id`.
5. **Configure and start:**
   ```sh
   cp .env.example .env   # fill in token, API key, DATABASE_URL, ADMIN_IDS, STORAGE_CHAT_ID
   docker compose up -d --build
   docker compose logs -f bot
   ```
   If your ytdl-service network has another name, set `YTDL_NETWORK=<name>`.

The public Bot API limits uploads to 50 MB. Larger files fail with a message
that suggests MP3. To lift the limit to 2000 MB, run a
[local Bot API server](https://github.com/tdlib/telegram-bot-api) and set `TELEGRAM_API_URL`.

## Development

```sh
go test ./...
# The pipeline test needs a scratch PostgreSQL (it recreates the schema):
docker run -d --rm --name tgbot-test-pg -p 127.0.0.1:55432:5432 -e POSTGRES_PASSWORD=test postgres:17-alpine
TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:55432/postgres go test -race ./...

# After editing internal/db/queries or migrations:
docker run --rm -u $(id -u):$(id -g) -v "$PWD":/src -w /src sqlc/sqlc:1.30.0 generate
```

Layout: `cmd/bot` (wiring), `internal/ytdl` (service client),
`internal/service` (request pipeline, limits, cache), `internal/bot`
(Telegram handlers), `internal/db` (migrations, queries, generated code).
