-- The D1 schema of the full-stack example. The operator creates the database (D1Database
-- notes-db) but never runs SQL in it: apply this once, and every later migration, yourself:
--
--   npx wrangler d1 execute notes-db --remote --file examples/fullstack/schema.sql
--
-- Every statement is idempotent, so running the file again is harmless.

CREATE TABLE IF NOT EXISTS notes (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  session         TEXT NOT NULL,              -- the session id from the signed cookie (KV)
  title           TEXT NOT NULL,
  body            TEXT NOT NULL DEFAULT '',
  attachment_key  TEXT,                       -- the R2 object key, when a file is attached
  attachment_name TEXT,                       -- its file name, for display and download
  created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS notes_by_session ON notes (session, created_at DESC);
