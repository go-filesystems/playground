// The playground's worker: runs the Go engine (fsplay.wasm) and answers the
// page's messages. FileReaderSync exists only in workers, which is why the
// engine lives here and not on the page.
"use strict";
importScripts("wasm_exec.js");

const go = new Go();
const ready = WebAssembly.instantiateStreaming(fetch("fsplay.wasm"), go.importObject)
  .then((r) => { go.run(r.instance); });

self.onmessage = async (e) => {
  const { id, kind, file, cache, fstype, req } = e.data || {};
  try {
    await ready;
    if (kind === "open") {
      const resp = self.fsplay.open(file, !!cache, typeof fstype === "string" ? fstype : "auto");
      self.postMessage({ id, resp });
      return;
    }
    const [resp, data] = self.fsplay.request(String(req));
    if (data) {
      self.postMessage({ id, resp, data }, [data.buffer]);
    } else {
      self.postMessage({ id, resp });
    }
  } catch (err) {
    self.postMessage({ id, resp: JSON.stringify({ ok: false, error: String(err && err.message || err) }) });
  }
};
// Say hello once the engine runs, or say why it cannot.
ready.then(
  () => self.postMessage({ id: 0, hello: true }),
  (err) => self.postMessage({ id: 0, hello: false, error: String(err && err.message || err) }),
);
