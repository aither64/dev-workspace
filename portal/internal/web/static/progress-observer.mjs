export function progressPresentation(value) {
  const concern = value?.concern;
  return {
    enabled: value?.enabled === true,
    available: value?.available === true,
    warning: value?.warning === true && Boolean(concern),
    message: concern ? `${concern.summary}\nSuggested next step: ${concern.recommendation}${concern.response ? `\nLead response: ${concern.response}` : "\nAwaiting the lead's response."}` : "",
    status: value?.unavailable ? `Progress observation unavailable: ${value.unavailable}` :
      !value?.available ? "Add an observer member to enable progress checks." :
      !value.enabled ? "Automatic progress checks are paused." :
      value.running ? "A progress check is running." : "Checks run every 15 minutes while someone is working. Idle sessions stay quiet.",
  };
}

export function mountProgressObserver({request, slug, policy, createNotice, readOnly, document: page = document, window: browser = window}) {
  const panel = page.getElementById("progress-observer"), toggle = page.getElementById("progress-observer-enabled");
  if (!panel || !toggle) return;
  const status = page.getElementById("progress-observer-status"), concern = page.getElementById("progress-observer-concern");
  const banner = page.getElementById("progress-observer-warning");
  const path = `/api/sessions/${encodeURIComponent(slug)}/progress`;
  let saved = null, pending = null, busy = false, stopped = false;
  const notice = createNotice({render: state => {
    if (state.warning) status.textContent = state.error.message;
  }});
  const render = () => {
    const view = progressPresentation(saved);
    toggle.checked = view.enabled;
    toggle.disabled = readOnly || busy || !saved || !view.available;
    status.textContent = view.status;
    concern.textContent = view.message; concern.hidden = !view.message;
    concern.classList.toggle("warning", view.warning);
    if (banner) { banner.hidden = !view.warning; banner.textContent = view.warning ? `Progress concern: ${view.message}` : ""; }
  };
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
  toggle.addEventListener("change", async () => {
    if (busy || readOnly || !saved?.available) return;
    const enabled = toggle.checked;
    pending?.abort(); pending = null; busy = true; render();
    const controller = new AbortController(), timer = browser.setTimeout(() => controller.abort(), policy.readDeadlineMs);
    try {
      const result = await request(path, {method: "POST", body: JSON.stringify({enabled}), signal: controller.signal});
      if (stopped) return;
      saved = result; notice.success();
    } catch (error) { if (!stopped) notice.failure(error, {immediate: true}); }
    finally { browser.clearTimeout(timer); busy = false; if (!stopped) render(); }
    // Observe an uncertain write rather than retrying it.
    void refresh();
  });
  const visible = () => { if (page.hidden) { pending?.abort(); } else void refresh(); };
  const hide = () => { stopped = true; pending?.abort(); browser.clearInterval(interval); notice.destroy(); };
  page.addEventListener("visibilitychange", visible);
  browser.addEventListener("focus", refresh);
  browser.addEventListener("pagehide", hide, {once: true});
  const interval = browser.setInterval(refresh, policy.detailsMs);
  render(); void refresh();
}
