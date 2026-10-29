// The playground page. It never touches the image itself: it posts coarse
// requests (open, list, read) to the worker and puts the answers in the DOM
// with textContent only -- every name comes from an untrusted image.
"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const TEXT_MAX = 1 << 20;      // view text up to 1 MiB
  const PREVIEW = 64 << 10;      // otherwise show the first 64 KiB
  const IMG_MAX = 8 << 20;       // view images up to 8 MiB
  const CHUNK = 4 << 20;         // the worker's largest read
  const TIMEOUT = 30000;         // per request: a corrupt image must not hang the page
  const IMAGE_TYPES = {
    png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif",
    webp: "image/webp", bmp: "image/bmp", ico: "image/x-icon", avif: "image/avif",
  };

  // Exposed for the end-to-end test and for anyone curious in a console.
  const state = { ready: false, info: null, name: "", lastList: null, timings: {}, error: "" };
  window.__fsplay = state;

  let worker = null;
  let seq = 0;
  let readyP = null;
  const pending = new Map();
  let sampleBlob = null;

  function status(text, isError) {
    const el = $("status");
    el.textContent = text;
    el.classList.toggle("error", !!isError);
    if (isError) state.error = text;
  }

  function startWorker() {
    if (worker) worker.terminate();
    for (const p of pending.values()) {
      clearTimeout(p.timer);
      p.reject(new Error("the engine was restarted"));
    }
    pending.clear();
    state.ready = false;
    worker = new Worker("worker.js");
    readyP = new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error("the engine did not start within 60 s")), 60000);
      worker.onmessage = (e) => {
        const m = e.data || {};
        if (m.id === 0) {
          clearTimeout(t);
          if (m.hello) {
            state.ready = true;
            resolve();
          } else {
            reject(new Error("the engine failed to start: " + m.error));
          }
          return;
        }
        const p = pending.get(m.id);
        if (!p) return;
        pending.delete(m.id);
        clearTimeout(p.timer);
        let r;
        try {
          r = JSON.parse(m.resp);
        } catch (_) {
          p.reject(new Error("the engine sent an unreadable answer"));
          return;
        }
        if (!r.ok) {
          const err = new Error(r.error || "failed");
          err.info = r.info;
          p.reject(err);
          if (/already exited/.test(r.error || "")) restart("the engine stopped");
          return;
        }
        r.data = m.data || null;
        p.resolve(r);
      };
      worker.onerror = (e) => {
        clearTimeout(t);
        const msg = "the engine failed: " + (e.message || "error");
        reject(new Error(msg));
        for (const p of pending.values()) {
          clearTimeout(p.timer);
          p.reject(new Error(msg));
        }
        pending.clear();
      };
    });
    readyP.catch((err) => status(err.message, true));
    return readyP;
  }

  function restart(why) {
    startWorker();
    state.info = null;
    $("volume").hidden = true;
    status(why + "; the engine was restarted. Open the image again.", true);
  }

  function call(msg, timeout) {
    return readyP.then(() => new Promise((resolve, reject) => {
      const id = ++seq;
      const ms = timeout || TIMEOUT;
      const timer = setTimeout(() => {
        pending.delete(id);
        reject(new Error(`no answer after ${ms / 1000} s (the image may be damaged)`));
        restart("A request took too long");
      }, ms);
      pending.set(id, { resolve, reject, timer });
      worker.postMessage(Object.assign({ id }, msg));
    }));
  }
  const req = (o, timeout) => call({ kind: "req", req: JSON.stringify(o) }, timeout);
  state.req = req;

  // ---- formatting ---------------------------------------------------------

  function human(n) {
    if (!(n >= 0)) return "";
    const u = ["B", "KiB", "MiB", "GiB", "TiB"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + " " + u[i];
  }

  // safeName makes a download file name from an untrusted one.
  function safeName(n) {
    const s = String(n)
      .replace(/[\u0000-\u001f\u007f<>:"/\\|?*]/g, "_")
      .replace(/^[.\s]+/, "")
      .replace(/[.\s]+$/, "")
      .slice(0, 200);
    return s || "file";
  }
  state.safeName = safeName;

  function ext(name) {
    const i = name.lastIndexOf(".");
    return i > 0 ? name.slice(i + 1).toLowerCase() : "";
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  const join = (dir, name) => (dir === "/" ? "/" : dir + "/") + name;

  // ---- opening ------------------------------------------------------------

  function showVolume(info, name, ms) {
    state.info = info;
    state.name = name;
    $("fstype").textContent = info.type;
    $("fstype").title = info.forced ? "chosen in the Filesystem list" : "detected";
    $("fsname").textContent = name;
    $("fssize").textContent = human(info.size);
    $("fslabel").textContent = info.label || "(none)";
    $("fsopen").textContent = `${info.openMs.toFixed(1)} ms in the engine, ${ms.toFixed(1)} ms end to end` +
      (info.cached ? ", block cache on" : ", block cache off");
    $("volume").hidden = false;
    $("view").replaceChildren(el("p", "hint", "Select a file to view it. Text up to 1 MiB and images up to 8 MiB are shown here; any file can be downloaded."));
    const tree = $("tree");
    tree.replaceChildren();
    const ul = el("ul", "dir");
    tree.append(ul);
    return fill(ul, "/");
  }

  async function openFile(file) {
    if (!file) return;
    sampleBlob = null;
    $("samplerow").hidden = true;
    status(`Opening ${file.name} (${human(file.size)})…`);
    const t0 = performance.now();
    try {
      const r = await call({ kind: "open", file, cache: $("cache").checked, fstype: $("kind").value });
      const ms = performance.now() - t0;
      state.timings.open = { engineMs: r.info.openMs, endToEndMs: ms };
      const t1 = performance.now();
      await showVolume(r.info, file.name, ms);
      state.timings.listRoot = performance.now() - t1;
      status(`${file.name}: ${r.info.type}.`);
    } catch (err) {
      $("volume").hidden = true;
      const t = err.info && err.info.type && err.info.type !== "unknown" ? ` (looks like ${err.info.type})` : "";
      status(`Could not open ${file.name}${t}: ${err.message}`, true);
    }
  }

  async function makeSample() {
    status("Formatting a FAT32 image in your browser…");
    const t0 = performance.now();
    try {
      const r = await req({ op: "sample", cache: $("cache").checked });
      sampleBlob = new Blob([r.data], { type: "application/octet-stream" });
      $("samplerow").hidden = false;
      const ms = performance.now() - t0;
      await showVolume(r.info, "sample-fat32.img", ms);
      status(`Sample FAT32 image made and opened (${human(sampleBlob.size)}).`);
    } catch (err) {
      status("Could not make the sample: " + err.message, true);
    }
  }

  // ---- the tree -----------------------------------------------------------

  async function fill(ul, path) {
    const t0 = performance.now();
    let r;
    try {
      r = await req({ op: "list", path });
    } catch (err) {
      ul.replaceChildren(el("li", "error", "Cannot list: " + err.message));
      return;
    }
    const l = r.listing;
    state.lastList = { path: l.path, names: l.entries.map((e) => e.name), ms: performance.now() - t0, engineMs: r.ms };
    const items = [];
    for (const e of l.entries) items.push(entryNode(path, e));
    if (l.entries.length === 0) items.push(el("li", "hint", "(empty)"));
    if (l.truncated) items.push(el("li", "hint", `… ${l.total - l.entries.length} more entries not shown`));
    ul.replaceChildren(...items);
  }

  function entryNode(dir, e) {
    const li = el("li");
    const p = join(dir, e.name);
    if (e.kind === "dir") {
      const d = el("details");
      const s = el("summary", "name", e.name);
      const ul = el("ul", "dir");
      d.append(s, ul);
      let loaded = false;
      d.addEventListener("toggle", () => {
        if (d.open && !loaded) {
          loaded = true;
          ul.replaceChildren(el("li", "hint", "loading…"));
          fill(ul, p);
        }
      });
      li.append(d);
      return li;
    }
    const b = el("button", "name file", e.name);
    b.type = "button";
    b.title = p;
    b.addEventListener("click", () => view(p, e));
    li.append(b, el("span", "size", e.kind === "file" ? human(e.size) : e.kind));
    if (e.kind === "file") {
      const dl = el("button", "dl", "download");
      dl.type = "button";
      dl.setAttribute("aria-label", "download " + e.name);
      dl.addEventListener("click", () => download(p, e));
      li.append(dl);
    }
    return li;
  }

  // ---- reading ------------------------------------------------------------

  async function readRange(path, limit, progress) {
    const parts = [];
    let off = 0;
    let size = 0;
    for (;;) {
      const r = await req({ op: "read", path, off, n: Math.min(CHUNK, limit - off) });
      size = r.size;
      const n = r.data ? r.data.byteLength : 0;
      if (n > 0) parts.push(r.data);
      off += n;
      if (progress) progress(off, Math.min(size, limit));
      if (n === 0 || off >= Math.min(size, limit)) break;
    }
    return { parts, size, read: off };
  }

  async function view(path, e) {
    const v = $("view");
    const head = el("h2", "", e.name);
    if (e.kind !== "file") {
      v.replaceChildren(head, el("p", "hint", `This is a ${e.kind}; there is nothing to show.`));
      return;
    }
    v.replaceChildren(head, el("p", "hint", "reading…"));
    const mime = IMAGE_TYPES[ext(e.name)];
    try {
      if (mime) {
        if (e.size > IMG_MAX) {
          v.replaceChildren(head, el("p", "hint", `${human(e.size)}: too large to show here; download it.`));
          return;
        }
        const r = await readRange(path, IMG_MAX);
        const img = el("img");
        img.alt = e.name;
        img.src = URL.createObjectURL(new Blob(r.parts, { type: mime }));
        img.onload = img.onerror = () => URL.revokeObjectURL(img.src);
        v.replaceChildren(head, img);
        return;
      }
      const limit = e.size <= TEXT_MAX ? TEXT_MAX : PREVIEW;
      const r = await readRange(path, limit);
      const bytes = new Uint8Array(await new Blob(r.parts).arrayBuffer());
      let text = null;
      if (!bytes.includes(0)) {
        try {
          text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
        } catch (_) {
          text = r.read < r.size ? new TextDecoder("utf-8").decode(bytes) : null;
        }
      }
      const items = [head];
      if (text === null) {
        items.push(el("p", "hint", `Binary file, ${human(r.size)}. First bytes:`), el("pre", "hex", hexdump(bytes.subarray(0, 512))));
      } else {
        if (r.read < r.size) items.push(el("p", "hint", `First ${human(r.read)} of ${human(r.size)}.`));
        items.push(el("pre", "text", text));
      }
      v.replaceChildren(...items);
      state.lastView = { path, text, size: r.size };
    } catch (err) {
      v.replaceChildren(head, el("p", "error", "Cannot read: " + err.message));
    }
  }

  function hexdump(b) {
    const lines = [];
    for (let i = 0; i < b.length; i += 16) {
      const row = b.subarray(i, i + 16);
      const hex = Array.from(row, (x) => x.toString(16).padStart(2, "0")).join(" ");
      const asc = Array.from(row, (x) => (x >= 32 && x < 127 ? String.fromCharCode(x) : ".")).join("");
      lines.push(i.toString(16).padStart(8, "0") + "  " + hex.padEnd(48) + "  " + asc);
    }
    return lines.join("\n");
  }

  function save(blob, name) {
    const a = el("a");
    a.href = URL.createObjectURL(blob);
    a.download = safeName(name);
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 60000);
  }

  async function download(path, e) {
    status(`Reading ${e.name}…`);
    try {
      const r = await readRange(path, Number.MAX_SAFE_INTEGER, (done, total) =>
        status(`Reading ${e.name}: ${human(done)} of ${human(total)}…`));
      save(new Blob(r.parts, { type: "application/octet-stream" }), e.name);
      status(`${e.name}: ${human(r.read)} saved.`);
    } catch (err) {
      status(`Cannot download ${e.name}: ${err.message}`, true);
    }
  }

  // ---- wiring -------------------------------------------------------------

  const drop = $("drop");
  drop.addEventListener("dragover", (ev) => { ev.preventDefault(); drop.classList.add("over"); });
  drop.addEventListener("dragleave", () => drop.classList.remove("over"));
  drop.addEventListener("drop", (ev) => {
    ev.preventDefault();
    drop.classList.remove("over");
    const f = ev.dataTransfer && ev.dataTransfer.files && ev.dataTransfer.files[0];
    openFile(f);
  });
  drop.addEventListener("keydown", (ev) => {
    if (ev.target === drop && (ev.key === "Enter" || ev.key === " ")) { ev.preventDefault(); $("pick").click(); }
  });
  $("pick").addEventListener("change", (ev) => { openFile(ev.target.files[0]); ev.target.value = ""; });
  $("sample").addEventListener("click", makeSample);
  $("sampledl").addEventListener("click", () => { if (sampleBlob) save(sampleBlob, "sample-fat32.img"); });

  startWorker().then(() => status("Ready. Drop an image, choose one, or make a sample."), () => {});
})();
