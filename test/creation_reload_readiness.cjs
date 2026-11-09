"use strict";

// Options normally stays collapsed after reload. Observe the new document's
// catalog and the DOM that loadModels/populateEfforts produce, without edits.
function reloadState({expected, catalog, catalogError, onlyReady = false}) {
  const form = document.getElementById("new-session-form");
  const fields = ["provider", "team", "catalogDigest", "model", "effort"];
  const values = Object.fromEntries(fields.map(key => [key, form?.elements[key]?.value ?? null]));
  let draft = null;
  try {
    const key = globalThis.sessionPreparation?.draftKey;
    const saved = key && JSON.parse(sessionStorage.getItem(key));
    if (saved) draft = Object.fromEntries(["requestId", ...fields].map(key => [key, saved[key] ?? null]));
  } catch (_) { /* Missing or unreadable storage remains unknown. */ }
  const controls = Object.fromEntries(["provider", "model", "effort"].map(key => {
    const control = form?.elements[key], box = control?.getBoundingClientRect();
    return [key, {present: Boolean(control), disabled: control?.disabled ?? null,
      visible: Boolean(box?.width && box?.height && getComputedStyle(control).visibility !== "hidden" &&
        (!control.closest("details") || control.closest("details").open)),
      value: control?.value ?? null, selected: control?.selectedOptions?.[0]?.value ?? null}];
  }));
  const models = catalog?.map(item => item.model);
  const selected = catalog?.find(item => item.model === expected.model);
  const efforts = selected?.supportedReasoningEfforts?.map(item => item.reasoningEffort);
  const options = key => Array.from(form?.elements[key]?.options || [], option => option.value);
  const same = (a, b) => Array.isArray(a) && Array.isArray(b) && JSON.stringify(a) === JSON.stringify(b);
  const modelOptionsMatch = same(models, options("model"));
  const effortOptionsMatch = same(efforts, options("effort"));
  const identityAndPairMatch = ["requestId", ...fields].every(key =>
    typeof expected[key] === "string" && (expected[key] !== "" || key === "provider")) &&
    draft?.requestId === expected.requestId &&
    fields.every(key => values[key] === expected[key] && draft?.[key] === expected[key]);
  const ready = !catalogError && Boolean(catalog && selected) && modelOptionsMatch && effortOptionsMatch &&
    Object.values(controls).every(control => control.present && control.disabled === false && control.selected === control.value) &&
    identityAndPairMatch;
  return onlyReady ? ready : {ready, detailsOpen: form?.querySelector("details")?.open ?? null,
    controls, values, draft, catalogComplete: catalog !== null, catalogError,
    modelOptionsMatch, effortOptionsMatch, identityAndPairMatch};
}

function watchCreationReload(page, expected) {
  const deadline = Date.now() + 30000;
  let navigated = false, catalog = null, catalogError = null;
  const requests = new WeakSet();
  const navigatedFrame = frame => { if (frame === page.mainFrame()) navigated = true; };
  const requestStarted = request => {
    const url = new URL(request.url());
    if (navigated && request.frame() === page.mainFrame() && request.method() === "GET" &&
        url.origin === new URL(page.url()).origin && url.pathname === "/api/models" &&
        (url.searchParams.get("provider") || "") === expected.provider) requests.add(request);
  };
  page.on("framenavigated", navigatedFrame);
  page.on("request", requestStarted);
  const completed = page.waitForResponse(response => requests.has(response.request())).then(async response => {
    if (!response.ok()) throw Error(`reload model catalog status ${response.status()}`);
    const error = await response.finished();
    if (error) throw error;
    const body = await response.json();
    if (!Array.isArray(body) || !body.length || body.some(item => typeof item.model !== "string" ||
        !Array.isArray(item.supportedReasoningEfforts) || item.supportedReasoningEfforts.some(
          effort => typeof effort.reasoningEffort !== "string"))) throw Error("reload model catalog shape");
    catalog = body.map(item => ({model: item.model, supportedReasoningEfforts:
      item.supportedReasoningEfforts.map(effort => ({reasoningEffort: effort.reasoningEffort}))}));
  }).catch(error => { catalogError = error.message; });
  const argumentsFor = onlyReady => ({expected, catalog, catalogError, onlyReady});
  return {
    snapshot: () => page.evaluate(reloadState, argumentsFor(false)),
    async wait() {
      let timer;
      try {
        return await Promise.race([(async () => {
          await completed;
          if (catalogError) throw Error(catalogError);
          const handle = await page.waitForFunction(reloadState, argumentsFor(true),
            {timeout: Math.max(1, deadline - Date.now())});
          await handle.dispose();
          return page.evaluate(reloadState, argumentsFor(false));
        })(), new Promise((_, reject) => {
          timer = setTimeout(() => reject(Error("creation reload readiness deadline")),
            Math.max(1, deadline - Date.now()));
        })]);
      } finally { clearTimeout(timer); }
    },
    dispose() {
      page.off("framenavigated", navigatedFrame);
      page.off("request", requestStarted);
    },
  };
}

module.exports = {watchCreationReload};
