let worker, active, serial = 0;
const pending = new Map(), queue = [];
const MAX_PENDING = 17; // One warmup plus the two sides of eight mounted files.

function unavailable() { return new Error("Syntax highlighting is unavailable."); }
function finish(error, result) {
  if (!active) return;
  const task = active;
  clearTimeout(task.timer);
  active = null;
  pending.delete(task.key);
  for (const listener of task.listeners) {
    listener.cleanup();
    if (error) listener.reject(error); else listener.resolve(result);
  }
  drain();
}
function reset() {
  worker?.terminate();
  worker = null;
}
function ensureWorker() {
  if (worker) return;
  worker = new Worker(new URL("./review-highlight-worker.js", import.meta.url), {type: "module"});
  const instance = worker;
  worker.onmessage = ({data}) => {
    if (worker !== instance || !active || active.id !== data.id) return;
    finish(data.error ? unavailable() : null, data.result);
  };
  worker.onerror = event => {
    event.preventDefault();
    if (worker !== instance) return;
    reset();
    finish(unavailable());
  };
}
function drain() {
  if (active || !queue.length) return;
  active = queue.shift();
  if (!active.listeners.size) { finish(unavailable()); return; }
  try {
    ensureWorker();
    active.timer = setTimeout(() => { reset(); finish(unavailable()); }, 8000);
    worker.postMessage({id: active.id, text: active.text, language: active.language, warmup: active.warmup});
  } catch { reset(); finish(unavailable()); }
}
function request(text, language, warmup = false, signal) {
  if (signal?.aborted) return Promise.reject(unavailable());
  const key = warmup ? "warmup" : `${language}\0${text}`;
  let task = pending.get(key);
  if (!task) {
    if (pending.size >= MAX_PENDING) return Promise.reject(unavailable());
    task = {id: ++serial, key, text, language, warmup, listeners: new Set()};
    pending.set(key, task);
    queue.push(task);
  }
  return new Promise((resolve, reject) => {
    const listener = {resolve, reject, cleanup: () => signal?.removeEventListener("abort", abort)};
    function abort() {
      listener.cleanup();
      task.listeners.delete(listener);
      reject(unavailable());
      if (!task.listeners.size && task === active) { reset(); finish(unavailable()); }
      else if (!task.listeners.size) {
        const index = queue.indexOf(task);
        if (index >= 0) queue.splice(index, 1);
        pending.delete(task.key);
      }
    }
    task.listeners.add(listener);
    signal?.addEventListener("abort", abort, {once: true});
    drain();
  });
}

export function preloadReviewEditor() {
  return request("", "", true).catch(() => {});
}
export function highlightSource(text, language, signal) {
  return language ? request(text, language, false, signal) : Promise.resolve({tokens: [], styles: []});
}
