// The backend of the full-stack example (examples/fullstack/README.md): a small notes API.
//
// One Worker serves the whole app. Cloudflare serves the frontend (public/) from the Worker's
// static assets, and only requests for /api/* run this code first
// (assets.config.run_worker_first_paths), so the pages of the site cost no Worker invocation.
// Any other path that matches no file gets index.html (not_found_handling:
// single-page-application), so the frontend's own routes such as /notes/42 work as deep links.
// Cloudflare answers only browser navigations (Sec-Fetch-Mode: navigate) that way by itself.
// Any other request that matches no file (a fetch(), curl) still runs this code, and the
// env.ASSETS.fetch below is what returns index.html for it.
//
// The Pages variant (pages/) deploys this same file as its advanced-mode _worker.js, with the
// same binding names.
//
// Bindings (spec.forProvider.bindings of the WorkerScript):
//   env.DB              D1 database: the notes table (schema.sql)
//   env.FILES           R2 bucket: note attachments
//   env.SESSIONS        KV namespace: sessions, and a short-lived cache of each note list
//   env.SESSION_SECRET  secret_text: the HMAC key that signs the session cookie
//   env.ASSETS          the static assets

const SESSION_TTL = 60 * 60 * 24 * 30; // seconds (KV expirationTtl and the cookie's Max-Age)
const LIST_TTL = 60; // seconds; KV's minimum expirationTtl
const MAX_UPLOAD = 10 * 1024 * 1024;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (!url.pathname.startsWith("/api/")) {
      // Required: a non-navigation request that matches no asset lands here (see above), and
      // ASSETS applies the single-page-application fallback to it.
      return env.ASSETS.fetch(request);
    }
    try {
      return await api(request, env, url);
    } catch (err) {
      console.error("api error", err);
      return json({ error: "internal error" }, 500);
    }
  },
};

async function api(request, env, url) {
  const { method } = request;
  const path = url.pathname;
  if (path === "/api/health") {
    return json({ ok: true });
  }

  const session = await getSession(request, env);
  let res;
  let m;
  if (path === "/api/notes" && method === "GET") {
    res = await listNotes(env, session.id);
  } else if (path === "/api/notes" && method === "POST") {
    res = await createNote(request, env, session.id);
  } else if ((m = path.match(/^\/api\/notes\/(\d+)$/)) && method === "DELETE") {
    res = await deleteNote(env, session.id, Number(m[1]));
  } else if ((m = path.match(/^\/api\/notes\/(\d+)\/attachment$/)) && method === "PUT") {
    res = await putAttachment(request, env, session.id, Number(m[1]), url.searchParams.get("name"));
  } else if ((m = path.match(/^\/api\/notes\/(\d+)\/attachment$/)) && method === "GET") {
    res = await getAttachment(env, session.id, Number(m[1]));
  } else {
    res = json({ error: "not found" }, 404);
  }
  if (session.cookie) {
    res.headers.append("Set-Cookie", session.cookie);
  }
  return res;
}

// --- sessions (KV) -----------------------------------------------------------------------

// getSession returns the caller's session, or starts one (with a Set-Cookie value). The cookie
// is "<id>.<HMAC-SHA256 of id>", so a client cannot pick another session's id.
async function getSession(request, env) {
  const raw = readCookie(request, "sid");
  if (raw) {
    const [id, sig] = raw.split(".");
    if (id && sig && (await verify(env, id, sig)) && (await env.SESSIONS.get(`session:${id}`)) !== null) {
      return { id };
    }
  }
  const id = crypto.randomUUID();
  await env.SESSIONS.put(`session:${id}`, JSON.stringify({ created: new Date().toISOString() }), {
    expirationTtl: SESSION_TTL,
  });
  const value = `${id}.${await sign(env, id)}`;
  return { id, cookie: `sid=${value}; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=${SESSION_TTL}` };
}

function readCookie(request, name) {
  for (const part of (request.headers.get("Cookie") || "").split(";")) {
    const [k, ...v] = part.trim().split("=");
    if (k === name) return v.join("=");
  }
  return null;
}

const encoder = new TextEncoder();

function hmacKey(env) {
  return crypto.subtle.importKey("raw", encoder.encode(env.SESSION_SECRET), { name: "HMAC", hash: "SHA-256" }, false, [
    "sign",
    "verify",
  ]);
}

async function sign(env, id) {
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", await hmacKey(env), encoder.encode(id)));
  return btoa(String.fromCharCode(...mac)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function verify(env, id, sig) {
  let mac;
  try {
    const bin = atob(sig.replace(/-/g, "+").replace(/_/g, "/"));
    mac = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  } catch {
    return false;
  }
  return crypto.subtle.verify("HMAC", await hmacKey(env), mac, encoder.encode(id));
}

// --- notes (D1, with the list cached in KV) ------------------------------------------------

const NOTE_COLUMNS = "id, title, body, attachment_name, created_at";

async function listNotes(env, sid) {
  const key = `notes:${sid}`;
  const cached = await env.SESSIONS.get(key, "json");
  if (cached !== null) {
    return json(cached, 200, { "X-Cache": "hit" });
  }
  const { results } = await env.DB.prepare(
    `SELECT ${NOTE_COLUMNS} FROM notes WHERE session = ?1 ORDER BY created_at DESC, id DESC LIMIT 100`,
  )
    .bind(sid)
    .all();
  await env.SESSIONS.put(key, JSON.stringify(results), { expirationTtl: LIST_TTL });
  return json(results, 200, { "X-Cache": "miss" });
}

async function createNote(request, env, sid) {
  let input;
  try {
    input = await request.json();
  } catch {
    return json({ error: "expected a JSON body" }, 400);
  }
  if (input === null || typeof input !== "object" || Array.isArray(input)) {
    return json({ error: "expected a JSON object" }, 400);
  }
  const title = String(input.title ?? "").trim().slice(0, 200);
  const body = String(input.body ?? "").slice(0, 10000);
  if (!title) {
    return json({ error: "title is required" }, 400);
  }
  const note = await env.DB.prepare(`INSERT INTO notes (session, title, body) VALUES (?1, ?2, ?3) RETURNING ${NOTE_COLUMNS}`)
    .bind(sid, title, body)
    .first();
  await env.SESSIONS.delete(`notes:${sid}`);
  return json(note, 201);
}

async function deleteNote(env, sid, id) {
  const note = await env.DB.prepare("DELETE FROM notes WHERE id = ?1 AND session = ?2 RETURNING attachment_key")
    .bind(id, sid)
    .first();
  if (!note) {
    return json({ error: "not found" }, 404);
  }
  if (note.attachment_key) {
    await env.FILES.delete(note.attachment_key);
  }
  await env.SESSIONS.delete(`notes:${sid}`);
  return new Response(null, { status: 204 });
}

// --- attachments (R2) ----------------------------------------------------------------------

async function putAttachment(request, env, sid, id, name) {
  const note = await env.DB.prepare("SELECT attachment_key FROM notes WHERE id = ?1 AND session = ?2").bind(id, sid).first();
  if (!note) {
    return json({ error: "not found" }, 404);
  }
  const size = Number(request.headers.get("Content-Length"));
  if (!request.body || !(size > 0)) {
    return json({ error: "empty upload (a Content-Length is required)" }, 400);
  }
  if (size > MAX_UPLOAD) {
    return json({ error: "attachments are limited to 10 MiB" }, 413);
  }
  const filename = String(name || "attachment").replace(/[^\w.\- ]/g, "_").slice(0, 200);
  const key = `notes/${id}/${crypto.randomUUID()}`;
  await env.FILES.put(key, request.body, {
    httpMetadata: {
      contentType: request.headers.get("Content-Type") || "application/octet-stream",
      contentDisposition: `attachment; filename="${filename}"`,
    },
  });
  await env.DB.prepare("UPDATE notes SET attachment_key = ?1, attachment_name = ?2 WHERE id = ?3")
    .bind(key, filename, id)
    .run();
  if (note.attachment_key) {
    await env.FILES.delete(note.attachment_key); // the replaced attachment
  }
  await env.SESSIONS.delete(`notes:${sid}`);
  return json({ id, attachment_name: filename });
}

async function getAttachment(env, sid, id) {
  const note = await env.DB.prepare("SELECT attachment_key FROM notes WHERE id = ?1 AND session = ?2").bind(id, sid).first();
  const object = note?.attachment_key ? await env.FILES.get(note.attachment_key) : null;
  if (!object) {
    return json({ error: "not found" }, 404);
  }
  const headers = new Headers();
  object.writeHttpMetadata(headers);
  headers.set("ETag", object.httpEtag);
  return new Response(object.body, { headers });
}

function json(value, status = 200, headers = {}) {
  return Response.json(value, { status, headers: { "Cache-Control": "no-store", ...headers } });
}
