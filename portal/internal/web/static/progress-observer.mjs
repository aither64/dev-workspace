export function progressPresentation(value) {
  const concern = value?.concern;
  return {
    enabled: value?.enabled === true,
    available: value?.available === true,
    warning: value?.warning === true && Boolean(concern),
    message: concern ? `${concern.summary}\nSuggested next step: ${concern.recommendation}${concern.response ? `\nLead response: ${concern.response}` : "\nAwaiting the lead's response."}` : "",
    status: value?.unavailable ? `Progress observation unavailable: ${value.unavailable}` :
      !value?.available ? "Add an observer member to enable progress checks." :
      value.running ? "A progress check is running." :
      !value.enabled ? "Automatic progress checks are paused." : `Checks run every ${value.intervalMinutes || 15} minutes while someone is working. Idle sessions stay quiet.`,
  };
}

export function mountProgressObserver({request, slug, policy, createNotice, readOnly, document: page = document, window: browser = window}) {
  const panel = page.getElementById("progress-observer"), toggle = page.getElementById("progress-observer-enabled");
  if (!panel || !toggle) return;
  const status = page.getElementById("progress-observer-status"), concern = page.getElementById("progress-observer-concern");
  const intervalInput = page.getElementById("progress-observer-interval"), intervalForm = page.getElementById("progress-observer-interval-form");
  const save = page.getElementById("progress-observer-save"), checkNow = page.getElementById("progress-observer-now");
  const banner = page.getElementById("progress-observer-warning");
  const warningToggle = page.getElementById("progress-observer-warning-toggle"), warningHide = page.getElementById("progress-observer-warning-hide");
  const warningMessage = page.getElementById("progress-observer-warning-message"), announcement = page.getElementById("progress-observer-announcement");
  const preference = `workspace-portal.progress-concern-hidden.${slug}`;
  let hiddenKey = "", hiddenWithoutKey = false, observedKey = null;
  try { hiddenKey = browser.sessionStorage.getItem(preference) || ""; } catch {}
  const rememberHidden = key => {
    hiddenKey = key;
    try {
      if (key) browser.sessionStorage.setItem(preference, key);
      else browser.sessionStorage.removeItem(preference);
    } catch {}
  };
  const concernKey = () => typeof saved?.concern?.key === "string" ? saved.concern.key : "";
  const text = (element, value) => { if (element && element.textContent !== value) element.textContent = value; };
  const path = `/api/sessions/${encodeURIComponent(slug)}/progress`;
  let saved = null, pending = null, busy = false, stopped = false, intervalDirty = false;
  const notice = createNotice({render: state => {
    if (state.warning) status.textContent = state.error.message;
  }});
  const render = () => {
    const view = progressPresentation(saved);
    toggle.checked = view.enabled;
    toggle.disabled = readOnly || busy || !saved || !view.available;
    if (intervalInput) {
      intervalInput.disabled = toggle.disabled;
      if (!intervalDirty) intervalInput.value = String(saved?.intervalMinutes || 15);
    }
    if (save) save.disabled = toggle.disabled || !intervalDirty;
    if (checkNow) checkNow.disabled = toggle.disabled || saved?.running === true;
    text(status, view.status);
    text(concern, view.message); concern.hidden = !view.message;
    concern.classList.toggle("warning", view.warning);
    if (banner && warningToggle) {
      const key = concernKey();
      if (saved && !view.warning) {
        if (hiddenKey) rememberHidden("");
        hiddenWithoutKey = false; observedKey = null;
        text(announcement, "");
      } else if (view.warning && key !== observedKey) {
        if (hiddenKey && hiddenKey !== key) rememberHidden("");
        hiddenWithoutKey = false; observedKey = key;
        if (announcement) announcement.textContent = "New progress concern";
      }
      banner.hidden = !view.warning || (key ? hiddenKey === key : hiddenWithoutKey);
      warningToggle.hidden = !view.warning;
      warningToggle.setAttribute("aria-expanded", String(!banner.hidden));
      text(warningMessage, view.warning ? view.message : "");
    }
  };
  const hideWarning = hidden => {
    if (!progressPresentation(saved).warning) return;
    const key = concernKey();
    rememberHidden(hidden ? key : "");
    hiddenWithoutKey = hidden && !key;
    render();
    if (hidden) warningToggle?.focus();
  };
  warningToggle?.addEventListener("click", () => hideWarning(!banner.hidden));
  warningHide?.addEventListener("click", () => hideWarning(true));
  const refresh = async () => {
    if (stopped || busy || pending || page.hidden) return;
    pending = new AbortController();
    const controller = pending, timer = browser.setTimeout(() => controller.abort(), policy.readDeadlineMs);
    notice.begin();
    try {
      const result = await request(path, {signal: controller.signal});
      if (stopped || controller.signal.aborted) return;
      if (typeof result.available !== "boolean" || typeof result.enabled !== "boolean") throw new Error("Progress status is unavailable.");
      saved = result; render(); notice.success();
    } catch (error) { if (!stopped && !page.hidden) notice.failure(error); }
    finally { browser.clearTimeout(timer); if (pending === controller) pending = null; }
  };
  const write = async body => {
    if (busy || readOnly || !saved?.available) return;
    pending?.abort(); pending = null; busy = true; render();
    const controller = new AbortController(), timer = browser.setTimeout(() => controller.abort(), policy.readDeadlineMs);
    try {
      const result = await request(path, {method: "POST", body: JSON.stringify(body), signal: controller.signal});
      if (stopped) return;
      if (body.intervalMinutes !== undefined) intervalDirty = false;
      saved = result; notice.success();
    } catch (error) { if (!stopped) notice.failure(error, {immediate: true}); }
    finally { browser.clearTimeout(timer); busy = false; if (!stopped) render(); }
    // Observe an uncertain write rather than retrying it.
    void refresh();
  };
  toggle.addEventListener("change", () => write({enabled: toggle.checked}));
  intervalInput?.addEventListener("input", () => { intervalDirty = true; render(); });
  intervalForm?.addEventListener("submit", event => {
    event.preventDefault();
    if (!intervalForm.reportValidity()) return;
    void write({intervalMinutes: Number(intervalInput.value)});
  });
  checkNow?.addEventListener("click", () => { if (!saved?.running) void write({checkNow: true}); });
  const visible = () => { if (page.hidden) { pending?.abort(); } else void refresh(); };
  const hide = () => { stopped = true; pending?.abort(); browser.clearInterval(interval); notice.destroy(); };
  page.addEventListener("visibilitychange", visible);
  browser.addEventListener("focus", refresh);
  browser.addEventListener("pagehide", hide, {once: true});
  const interval = browser.setInterval(refresh, policy.detailsMs);
  render(); void refresh();
}
