// The frontend of the full-stack example: a single-page app with two routes, / (the note list)
// and /notes/<id> (one note). Cloudflare answers a deep link such as /notes/42 with index.html
// (not_found_handling: single-page-application), and this script renders the route.

const app = document.getElementById("app");

async function api(path, options = {}) {
  const res = await fetch(`/api${path}`, { credentials: "same-origin", ...options });
  if (!res.ok) {
    const { error } = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(error || `HTTP ${res.status}`);
  }
  return res.status === 204 ? null : res.json();
}

function render(templateId) {
  app.replaceChildren(document.getElementById(templateId).content.cloneNode(true));
}

async function showList() {
  render("list-view");
  const form = document.getElementById("new-note");
  const error = form.querySelector(".error");
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    error.hidden = true;
    const data = new FormData(form);
    try {
      const note = await api("/notes", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title: data.get("title"), body: data.get("body") }),
      });
      const file = data.get("file");
      if (file && file.size > 0) {
        await api(`/notes/${note.id}/attachment?name=${encodeURIComponent(file.name)}`, {
          method: "PUT",
          headers: { "Content-Type": file.type || "application/octet-stream" },
          body: file,
        });
      }
      form.reset();
      await fillList();
    } catch (err) {
      error.textContent = err.message;
      error.hidden = false;
    }
  });
  await fillList();
}

async function fillList() {
  const list = document.getElementById("notes");
  const notes = await api("/notes");
  list.replaceChildren(
    ...notes.map((note) => {
      const li = document.createElement("li");
      const a = document.createElement("a");
      a.href = `/notes/${note.id}`;
      a.dataset.link = "";
      a.textContent = note.title;
      li.append(a);
      if (note.attachment_name) {
        li.append(" ", Object.assign(document.createElement("span"), { className: "muted", textContent: "(attachment)" }));
      }
      return li;
    }),
  );
  if (notes.length === 0) {
    list.innerHTML = '<li class="muted">No notes yet.</li>';
  }
}

async function showNote(id) {
  const note = (await api("/notes")).find((n) => n.id === id);
  if (!note) {
    app.innerHTML = '<p>Note not found. <a href="/" data-link>Back</a></p>';
    return;
  }
  render("note-view");
  app.querySelector("h2").textContent = note.title;
  app.querySelector(".body").textContent = note.body;
  app.querySelector(".created").textContent = new Date(note.created_at).toLocaleString();
  if (note.attachment_name) {
    const p = app.querySelector(".attachment");
    p.hidden = false;
    p.querySelector("a").href = `/api/notes/${id}/attachment`;
    p.querySelector("a").textContent = `Download ${note.attachment_name}`;
  }
  app.querySelector("[data-action=delete]").addEventListener("click", async () => {
    await api(`/notes/${id}`, { method: "DELETE" });
    navigate("/");
  });
}

async function route() {
  const m = location.pathname.match(/^\/notes\/(\d+)$/);
  try {
    await (m ? showNote(Number(m[1])) : showList());
  } catch (err) {
    app.textContent = `Error: ${err.message}`;
  }
}

function navigate(path) {
  history.pushState(null, "", path);
  route();
}

document.addEventListener("click", (event) => {
  const a = event.target.closest("a[data-link]");
  if (a && a.origin === location.origin) {
    event.preventDefault();
    navigate(a.pathname);
  }
});
window.addEventListener("popstate", route);
route();
