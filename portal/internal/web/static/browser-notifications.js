(() => {
  "use strict";
  const key = "workspace-browser-notifications.v1";
  const cleanupKey = `${key}.cleanup`;
  const elements = name => [...document.querySelectorAll(`[data-notification-${name}]`)];
  const statuses = elements("status"), enables = elements("enable"), disables = elements("disable"), tests = elements("test");
  const testStatus = elements("test-status"), providerStatus = elements("provider-status"), workerStatus = elements("worker-status");
  const soundControls = elements("sound"), soundStatus = elements("sound-status");
  const deliveryControls = elements("delivery"), menuErrors = elements("menu-error");
  if (!statuses.length && !deliveryControls.length && !soundControls.length) return;
  let menuValue = null, menuReading = false, menuAvailable = null;
  let deliveryError = null, soundError = null, workerError = false, epochReady = false, deliveryUncertain = false;
  const menuMessages = {
    setup: "Notification setup needs attention.", read: "Could not check notifications.",
    blocked: "Notifications are blocked.", unsupported: "Notifications are unavailable here.",
    unavailable: "Notification delivery is unavailable.", action: "Could not update notifications.",
    soundRead: "Could not read the sound setting.", soundSave: "Could not save the sound setting.",
    soundUnsupported: "Sound control is unavailable.", worker: "Notification update unconfirmed.",
  };
  const renderMenu = () => {
    deliveryControls.forEach(control => {
      control.checked = menuValue === true;
      control.indeterminate = menuValue === null;
      control.disabled = busy || soundBusy || menuReading || pageClosed || observationRetired || menuValue === null ||
        !supported || Notification.permission === "denied" || menuAvailable === false;
    });
    const problem = deliveryError || (!silentSupported ? "soundUnsupported" : soundError) || (workerError ? "worker" : null);
    setText(menuErrors, menuMessages[problem] || "");
  };
  const setStatus = text => setText(statuses, text);
  const store = globalThis.workspaceAttentionStore;
  const silentSupported = Boolean(globalThis.Notification?.prototype && "silent" in Notification.prototype);
  let soundGeneration = 0, soundBusy = false, savedSound, soundReady = false, soundFailed = false;
  const renderSound = () => {
    soundControls.forEach(control => {
      control.disabled = busy || !silentSupported || soundBusy || typeof savedSound !== "boolean";
      control.checked = savedSound === true;
    });
    const text = !silentSupported ? "This browser does not support visual-only notifications." : soundBusy ? "Saving sound preference…" : soundFailed ?
      "The sound preference could not be saved. Check the setting and try again." : typeof savedSound !== "boolean" ?
      "The sound setting could not be read. The browser will be asked to show notifications without sound." : !soundReady ?
      "The notification update is unconfirmed. This sound setting may not take effect yet." : "";
    setText(soundStatus, text);
    renderMenu();
  };
  async function refreshSound() {
    if (!soundControls.length || soundBusy || pageClosed) return;
    const generation = ++soundGeneration;
    let value;
    try { value = await store.readSound(); } catch (_) {}
    if (generation !== soundGeneration || pageClosed || soundBusy) return;
    savedSound = value; soundFailed = false; soundError = typeof value === "boolean" ? null : "soundRead"; renderSound();
  }
  soundControls.forEach(soundControl => soundControl.addEventListener("change", async () => {
    if (busy || soundBusy || pageClosed || !silentSupported) return;
    const generation = ++soundGeneration, selected = soundControl.checked;
    soundError = null; soundBusy = true; buttons();
    try {
      const value = await store.writeSound(selected);
      if (generation === soundGeneration && !pageClosed) {savedSound = value; soundFailed = false; soundError = null;}
    } catch (_) {
      // A timeout may leave an uncertain commit. Re-read rather than presenting
      // the attempted value as saved; the helper aborts overdue writes.
      let value;
      try {value = await store.readSound();} catch (_) {}
      if (generation === soundGeneration && !pageClosed) {savedSound = value; soundFailed = true; soundError = "soundSave";}
    } finally {
      soundBusy = false;
      if (!pageClosed) {
        buttons();
        if (generation !== soundGeneration) void refreshSound();
      }
    }
  }));
  let pendingTest = null, pollTimer = null, pollGeneration = 0, pageClosed = false, workerUpdate = null, resumeRetired = false;
  let refreshGeneration = 0, observationRetired = false, cancelReadiness = null;
  const updateUnconfirmed = "The notification update could not be confirmed. Refresh this page to try again. Test results may be limited.";
  const invalidateRefresh = (action = false) => {
    refreshGeneration++;
    if (action) menuReading = false;
    if (cancelReadiness) {
      soundReady = false; renderSound();
      cancelReadiness();
      if (action) setText(workerStatus, updateUnconfirmed);
    }
  };
  const setText = (elements, text) => elements.forEach(element => { element.textContent = text; element.hidden = !text; });
  const localResult = async promise => {
    let timer;
    try { return await Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error()), 1000); })]); }
    finally { clearTimeout(timer); }
  };
  const stopTest = () => { pollGeneration++; clearTimeout(pollTimer); pollTimer = null; };
  const cancelTest = () => {
    stopTest();
    resumeRetired = true;
    const descriptor = pendingTest;
    pendingTest = null;
    setText(testStatus, "");
    if (!descriptor || !store) return Promise.resolve();
    const retirement = store.update(descriptor.identity, value => {
      if (store.testMatches(value.testPending, descriptor)) delete value.testPending;
      return value;
    });
    // Enable/Disable compensation must not wait on diagnostic storage. A new
    // explicit Test does await retirement before submitting another request.
    void retirement.catch(() => {});
    return retirement;
  };
  const lastProvider = value => ({provider_accepted: "The push service accepted a notification.", retrying: "The push service is unavailable; delivery will retry.",
    provider_refused: "The push service refused a notification.", delivery_failed: "A notification could not be sent.", expired: "A notification expired."})[value];
  async function workerReadiness(registration, update = false, current = () => true) {
    if (!registration) return;
    cancelReadiness?.();
    const generation = refreshGeneration, deadline = Date.now() + 10000;
    const valid = () => current() && generation === refreshGeneration && !pageClosed && !observationRetired;
    if (!valid()) return;
    workerError = false; soundReady = false; epochReady = false; renderSound();
    setText(workerStatus, "Updating browser notifications… You can keep your tabs open.");
    if (update && !workerUpdate) workerUpdate = Promise.resolve().then(() => registration.update());
    const activated = await new Promise(resolve => {
      const workers = new Set();
      let timer, finished = false, checkingUpdate = Boolean(workerUpdate);
      const finish = value => {
        if (finished) return;
        finished = true; clearTimeout(timer);
        registration.removeEventListener?.("updatefound", inspect);
        navigator.serviceWorker.removeEventListener?.("controllerchange", inspect);
        workers.forEach(worker => worker.removeEventListener?.("statechange", inspect));
        if (cancelReadiness === cancel) cancelReadiness = null;
        resolve(value);
      };
      const cancel = () => finish(false);
      const inspect = () => {
        if (!valid()) {finish(false); return;}
        for (const worker of [registration.installing, registration.waiting, registration.active]) {
          if (worker && !workers.has(worker)) {workers.add(worker); worker.addEventListener?.("statechange", inspect);}
        }
        const controller = navigator.serviceWorker.controller;
        if (!checkingUpdate && !registration.installing && !registration.waiting && controller &&
            controller === registration.active && controller.state === "activated") finish(true);
      };
      cancelReadiness = cancel;
      timer = setTimeout(() => finish(false), Math.max(0, deadline - Date.now()));
      registration.addEventListener?.("updatefound", inspect);
      navigator.serviceWorker.addEventListener?.("controllerchange", inspect);
      if (checkingUpdate) {
        workerUpdate.then(() => {checkingUpdate = false; if (!finished) inspect();}, () => finish(false));
      }
      inspect();
    });
    if (!valid()) return;
    if (!activated) {workerError = true; renderMenu(); setText(workerStatus, updateUnconfirmed); return;}
    const controller = navigator.serviceWorker.controller;
    if (!globalThis.MessageChannel) {
      workerError = true; renderMenu();
      setText(workerStatus, "Detailed test results are unavailable. Refresh this page to check for a notification update.");
      return;
    }
    const supported = await new Promise(resolve => {
      const channel = new MessageChannel(); let finished = false, timer;
      const finish = value => {
        if (finished) return;
        finished = true; clearTimeout(timer); channel.port1.close(); channel.port2.close?.();
        if (cancelReadiness === cancel) cancelReadiness = null;
        resolve(value);
      };
      const cancel = () => finish(false);
      cancelReadiness = cancel;
      timer = setTimeout(() => finish(false), Math.max(0, Math.min(1500, deadline - Date.now())));
      channel.port1.onmessage = event => finish(event.data?.version === 1 ? event.data : null);
      try {controller.postMessage({type: "notification-test-capabilities"}, [channel.port2]);}
      catch (_) {finish(false);}
    });
    if (valid() && controller === navigator.serviceWorker.controller && controller === registration.active && !registration.waiting && !registration.installing) {
      workerError = !supported; soundReady = Boolean(supported); epochReady = supported?.deliveryEpoch === 1; renderSound();
      setText(workerStatus, supported ? "" : "Detailed test results are unavailable. Refresh this page to check for a notification update.");
      return epochReady;
    }
  }
  async function pollTest() {
    stopTest();
    const generation = pollGeneration, descriptor = pendingTest;
    if (!descriptor || pageClosed || !registered || descriptor.identity !== `test-${registrationId}`) return;
    let record, storageFailed = false;
    try { record = await localResult(store?.read(descriptor.identity)); } catch (_) { storageFailed = true; }
    if (generation !== pollGeneration || pageClosed) return;
    const current = store?.testDescriptor?.(record?.testPending), latest = store?.testDescriptor?.(record?.testOutcome);
    if ([current, latest].some(item => item && item.revision >= descriptor.revision && !store.testMatches(item, descriptor))) {
      setText(testStatus, "Another test replaced this check. Send a new test to check this page.");
      pendingTest = null;
      return;
    }
    const outcome = record?.testOutcome, matches = store?.testMatches?.(outcome, descriptor);
    let text = "Waiting for this test to reach the browser.";
    let complete = false;
    if (matches && outcome.received === true) {
      if (outcome.display === "rejected") {
        text = `This test reached the browser, but the browser rejected the display request (${store.testError({name: outcome.error})}).`;
        complete = true;
      } else if (outcome.display === "resolved") {
        text = "This test reached the browser and the browser accepted the display request. ";
        text += outcome.browserRecord === "present" ? "A matching notification is recorded. Check your screen to confirm it appeared." :
          outcome.browserRecord === "absent" ? "No matching notification is recorded. Appearance on your screen is unconfirmed." :
          "The notification record could not be checked. Appearance on your screen is unconfirmed.";
        complete = true;
      } else text = "This test reached the browser. Waiting for its display result.";
    }
    if (storageFailed || !store) text = "This browser could not read the test result. Appearance on your screen is unconfirmed.";
    const expired = Date.now() >= descriptor.expiresAt + 10000;
    if (expired && !complete && !storageFailed) text = matches && outcome.received ?
      "This test reached the browser, but its display result was not confirmed before the check ended." :
      "This test was not confirmed by the browser before the check ended. Appearance on your screen is unconfirmed.";
    setText(testStatus, text);
    setStatus("Enabled on this browser.");
    // The provider status is registration-wide. It never certifies this test.
    try {
      const state = await api(`?registrationId=${encodeURIComponent(registrationId)}`);
      if (generation !== pollGeneration || pageClosed) return;
      setText(providerStatus, lastProvider(state.result) ? `Last push-service result; it may be from another notification: ${lastProvider(state.result)}` : "");
    } catch (_) {}
    if (complete || expired || !store) return;
    if (generation === pollGeneration && !pageClosed) pollTimer = setTimeout(() => void pollTest(), 2000);
  }
  async function resumeTest() {
    if (!tests.length || !store?.read || !registered || busy || pageClosed || pendingTest || resumeRetired) return;
    const generation = pollGeneration, identity = `test-${registrationId}`;
    try {
      const record = await localResult(store.read(identity)), saved = store.testDescriptor(record?.testPending);
      if (generation !== pollGeneration || pageClosed || !registered || busy || resumeRetired || identity !== `test-${registrationId}` || pendingTest) return;
      if (saved && saved.identity === identity && saved.expiresAt <= Date.now() + 70000) {
        pendingTest = saved;
        void pollTest();
      }
    } catch (_) {}
  }
  const supported = globalThis.isSecureContext && "serviceWorker" in navigator && "PushManager" in globalThis && "Notification" in globalThis;
  let registrationId = "", busy = false, registered = false, localSubscription = null, localKnown = false, pendingCleanup = null;
  const incomplete = "Browser setup is incomplete. Disable it or enable notifications again.";
  const disablingIncomplete = "Disabling is incomplete. Retry Disable.";
  const serverPending = "Browser subscription removed; server cleanup is still pending. Retry Disable.";
  const digestPattern = /^[0-9a-f]{64}$/;
  const readCleanup = () => {
    try {
      const saved = localStorage.getItem(cleanupKey);
      if (saved && saved.length <= 128) {
        const value = JSON.parse(saved), fields = value && Object.keys(value);
        if (fields?.length === 1 && ["registrationId", "endpointDigest"].includes(fields[0]) &&
            typeof value[fields[0]] === "string" && digestPattern.test(value[fields[0]])) pendingCleanup = value;
      }
    } catch (_) {}
  };
  const saveCleanup = selector => {
    pendingCleanup = selector;
    try { localStorage.setItem(cleanupKey, JSON.stringify(selector)); } catch (_) {}
  };
  const subscriptionSelector = async subscription => {
    const endpoint = subscription.endpoint || subscription.toJSON().endpoint;
    if (typeof endpoint !== "string" || !endpoint) throw new Error();
    const sum = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(endpoint));
    return {endpointDigest: [...new Uint8Array(sum)].map(byte => byte.toString(16).padStart(2, "0")).join("")};
  };
  const clearRegistration = id => {
    if (!id || registrationId !== id) return;
    try {
      localStorage.removeItem(key);
      if (!localStorage.getItem(key)) registrationId = "";
    } catch (_) {}
  };
  const clearCleanup = () => {
    try {
      localStorage.removeItem(cleanupKey);
      if (localStorage.getItem(cleanupKey)) return false;
      pendingCleanup = null;
      return true;
    } catch (_) { return false; }
  };
  const cleanupStatus = () => localKnown && !localSubscription ? serverPending : disablingIncomplete;
  try { registrationId = localStorage.getItem(key) || ""; } catch (_) {}
  readCleanup();
  const api = async (path, body) => {
    const controller = new AbortController(), deadline = setTimeout(() => controller.abort(), 8000);
    try {
      const response = await fetch(`/api/browser-notifications${path}`, {cache: "no-store", credentials: "same-origin", signal: controller.signal,
        ...(body ? {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body)} : {})});
      const value = await response.json(); if (!response.ok) throw Object.assign(new Error(value.error || "Browser notifications are unavailable."), {status: response.status}); return value;
    } finally { clearTimeout(deadline); }
  };
  const buttons = () => {
    enables.forEach(control => {control.disabled = busy || soundBusy || !supported || Notification.permission === "denied" || Boolean(pendingCleanup);});
    disables.forEach(control => {control.disabled = busy || soundBusy || (!localSubscription && !registrationId && !pendingCleanup && !deliveryUncertain);});
    tests.forEach(control => {control.disabled = busy || soundBusy || !registered || Boolean(pendingCleanup);});
    renderSound();
  };
  async function refresh(resume = true) {
    if (busy || pageClosed || observationRetired) return;
    invalidateRefresh();
    const generation = refreshGeneration, identity = registrationId;
    const current = () => generation === refreshGeneration && identity === registrationId && !busy && !pageClosed && !observationRetired;
    menuReading = true; renderMenu();
    if (!supported) { menuReading = false; menuValue = null; deliveryError = "unsupported"; setStatus("This browser build does not support background Web Push. Use a browser with Push API support."); buttons(); return; }
    let observedSubscription = localSubscription, observedKnown = localKnown;
    try {
      const local = await navigator.serviceWorker.getRegistration("/");
      observedSubscription = await local?.pushManager.getSubscription();
      observedKnown = true;
      if (!current()) return;
      const state = await api(`?registrationId=${encodeURIComponent(identity)}`);
      if (!current()) return;
      const gate = await store.readDeliveryGate();
      if (!current()) return;
      const managedReady = !gate?.enabled || state.deliveryEpoch === 1 && Boolean(navigator.locks?.request) &&
        await workerReadiness(local, true, current);
      if (!current()) return;
      localSubscription = observedSubscription; localKnown = observedKnown;
      menuAvailable = state.available;
      const off = !state.registered && localKnown && !localSubscription && !registrationId && !pendingCleanup;
      registered = Boolean(!pendingCleanup && state.registered && localSubscription && Notification.permission === "granted" &&
        (gate === null || gate.enabled && gate.registrationId === identity && managedReady));
      menuValue = registered && state.available ? true : off ? false : null;
      deliveryError = pendingCleanup || (!off && !registered) ? "setup" : Notification.permission === "denied" ? "blocked" : !state.available ? "unavailable" : null;
      menuReading = false;
      setText(providerStatus, lastProvider(state.result) ? `Last push-service result; it may be from another notification: ${lastProvider(state.result)}` : "");
      setStatus(gate?.enabled && !managedReady ? protectionUnavailable : pendingCleanup ? cleanupStatus() : localSubscription && !registered ? incomplete : Notification.permission === "denied" ? "Notifications are blocked. Change this site's notification permission in your browser settings." :
        !state.available ? "Notification delivery is disabled on the server." : state.result === "expired" ? "The browser subscription expired. Enable notifications again." : registered ?
        "Enabled on this browser." : "Disabled on this browser.");
      if (resume) { if (!gate?.enabled) void workerReadiness(local, true, current); void resumeTest(); }
    } catch (error) {
      if (!current()) return;
      localSubscription = observedSubscription; localKnown = observedKnown;
      registered = false; menuValue = null; menuReading = false; deliveryError = pendingCleanup ? "setup" : "read"; setStatus(pendingCleanup ? cleanupStatus() : localSubscription ? incomplete : error.message);
    }
    if (current()) buttons();
  }
  const retiredAction = () => new Error("Notification settings changed in another tab. Check the current setting before trying again.");
  const protectionUnavailable = "Notification protection is unavailable. Reload this page before enabling notifications.";
  const deliveryLock = work => navigator.locks?.request ? navigator.locks.request("workspace-browser-notifications", work) : work();
  // Invoked only while holding the origin's action lock. Keep both old-ID and
  // current-endpoint obligations across lost registration/deletion replies.
  async function removeServerRegistration(subscription, guard) {
    await guard();
    readCleanup();
    let selector = pendingCleanup, currentBound = !subscription;
    if (!registrationId && selector?.registrationId) {
      registrationId = selector.registrationId;
      localStorage.setItem(key, registrationId);
    }
    try {
      if (subscription) {selector = await subscriptionSelector(subscription); currentBound = true;}
    } catch (_) {}
    await guard();
    selector ||= registrationId ? {registrationId} : null;
    if (selector) saveCleanup(selector);
    const knownId = registrationId;
    const selectorRemoval = selector ? api("/disable", selector).then(() => true, () => false) : Promise.resolve(true);
    const idRemoval = knownId ? selector?.registrationId === knownId ? selectorRemoval :
      api("/disable", {registrationId: knownId}).then(() => true, () => false) : Promise.resolve(true);
    const [selectorAbsent, idAbsent] = await Promise.all([selectorRemoval, idRemoval]);
    await guard();
    if (idAbsent && currentBound) clearRegistration(knownId);
    return currentBound && selectorAbsent && idAbsent;
  }
  async function enableNotifications() {
    if (soundBusy || busy || !supported || Notification.permission === "denied") return;
    readCleanup();
    if (pendingCleanup) {menuValue = null; deliveryError = "setup"; setStatus(cleanupStatus()); buttons(); return;}
    deliveryError = null; menuReading = false;
    invalidateRefresh(true); observationRetired = false; cancelTest();
    const generation = refreshGeneration, currentAction = () => generation === refreshGeneration && !pageClosed && !observationRetired;
    busy = true; buttons();
    let token, gate, continuous = false, menuAbsenceConfirmed = menuValue === false;
    const guard = async () => {
      if (!currentAction()) throw retiredAction();
      const latest = await store.readDeliveryGate();
      if (!currentAction() || latest?.actionToken !== token) throw retiredAction();
    };
    try {
      if (!navigator.locks?.request || !store?.writeDeliveryGate) throw new Error(protectionUnavailable);
      // Capture intent before waiting on permission/the lock. A later Disable
      // supersedes it durably, including a delayed registration response.
      gate = await store.readDeliveryGate();
      if (!currentAction()) throw retiredAction();
      continuous = Boolean(gate?.enabled && gate.registrationId === registrationId);
      token = continuous ? gate.actionToken : crypto.randomUUID();
      if (!continuous && !await store.writeDeliveryGate({enabled:false,actionToken:token,registrationId:""}, gate?.actionToken ?? null)) throw retiredAction();
      await guard();
      const permission = await Notification.requestPermission();
      await guard();
      if (permission === "denied") deliveryError = "blocked";
      if (permission !== "granted") throw new Error(permission === "denied" ? "Notifications are blocked. Change this site's notification permission in your browser settings." : "Notification permission was not granted.");
      await deliveryLock(async () => {
        await guard();
        registrationId = localStorage.getItem(key) || ""; readCleanup();
        if (pendingCleanup) throw new Error(disablingIncomplete);
        const state = await api(continuous ? `?registrationId=${encodeURIComponent(registrationId)}` : "");
        await guard(); menuAvailable = state.available;
        if (!state.available) throw new Error("Notification delivery is disabled on the server.");
        if (state.deliveryEpoch !== 1) throw new Error(protectionUnavailable);
        const registration = await navigator.serviceWorker.register("/notification-worker.js", {scope:"/",updateViaCache:"none"});
        await guard();
        if (!await workerReadiness(registration, true, currentAction)) throw new Error(protectionUnavailable);
        await guard();
        menuAbsenceConfirmed = false;
        let subscription = await registration.pushManager.getSubscription();
        await guard(); localSubscription = subscription; menuAbsenceConfirmed = !subscription;
        const applicationServerKey = Uint8Array.from(atob(state.publicKey.replace(/-/g,"+").replace(/_/g,"/")), char=>char.charCodeAt(0));
        if (subscription?.options.applicationServerKey && [...new Uint8Array(subscription.options.applicationServerKey)].join() !== [...applicationServerKey].join()) {
          throw new Error("The server's push key changed. Disable this browser subscription, then enable it again.");
        }
        continuous = continuous && state.registered && gate.registrationId === registrationId && Boolean(subscription);
        if (!continuous) {
          if (gate?.enabled) {
            const nextToken = crypto.randomUUID();
            if (!await store.writeDeliveryGate({enabled:false,actionToken:nextToken,registrationId:""},token)) throw retiredAction();
            token = nextToken;
          }
          await guard();
          if (!await removeServerRegistration(subscription,guard) || !clearCleanup()) throw new Error(disablingIncomplete);
        }
        if (!subscription) {
          menuAbsenceConfirmed = false;
          subscription = await registration.pushManager.subscribe({userVisibleOnly:true,applicationServerKey});
          await guard(); localSubscription = subscription;
        }
        const value = subscription.toJSON();
        let saved;
        for (let attempt=0; attempt<2; attempt++) {
          await guard();
          try {
            saved = await api("/register",{subscription:{endpoint:value.endpoint,keys:value.keys},expirationTime:value.expirationTime??null,...(continuous?{}:{requireFresh:true})});
            break;
          } catch (error) {
            if (continuous || error.status !== 409 || attempt) throw error;
            // A lost prior request may have registered after earlier cleanup.
            if (!await removeServerRegistration(subscription,guard) || !clearCleanup()) throw new Error(disablingIncomplete);
          }
        }
        await guard();
        if (!digestPattern.test(saved?.registrationId) || continuous && saved.registrationId !== gate.registrationId) throw new Error(incomplete);
        // Storage and gate side effects remain inside the lock and token fence.
        const enabled = await store.writeDeliveryGate({enabled:true,actionToken:token,registrationId:saved.registrationId},token);
        if (!enabled) throw retiredAction();
        await guard();
        registrationId = saved.registrationId;
        localStorage.setItem(key,registrationId);
        if (localStorage.getItem(key) !== registrationId) throw new Error("Browser registration could not be saved. Try enabling notifications again.");
        deliveryUncertain = false; registered = true; menuValue = true; deliveryError = null;
        setStatus("Enabled on this browser. Send a test notification to check delivery.");
      });
    } catch (error) {
      if (currentAction()) {
        registered = false;
        menuValue = menuAbsenceConfirmed && !localSubscription && !registrationId && !pendingCleanup ? false : null;
        deliveryError = pendingCleanup || localSubscription || registrationId ? "setup" : deliveryError || "action";
        setStatus(error.message === protectionUnavailable ? protectionUnavailable : pendingCleanup ? disablingIncomplete : localSubscription ? incomplete : `Registration failed: ${error.message}`);
      }
    } finally {busy = false; buttons();}
  }
  async function disableNotifications() {
    if (soundBusy || busy) return;
    deliveryError = null; menuReading = false; invalidateRefresh(true); cancelTest();
    const generation = refreshGeneration, currentAction = () => generation === refreshGeneration && !pageClosed && !observationRetired;
    busy = true; registered = false; buttons();
    const token = crypto.randomUUID();
    let gateSaved = false;
    const guard = async () => {
      if (!currentAction()) throw retiredAction();
      if (gateSaved && (await store.readDeliveryGate())?.actionToken !== token) throw retiredAction();
      if (!currentAction()) throw retiredAction();
    };
    try {
      // Failures do not stop cleanup, but cannot certify durable Disable.
      try {gateSaved = Boolean(await store.writeDeliveryGate({enabled:false,actionToken:token,registrationId:""}));} catch (_) {}
      deliveryUncertain = !gateSaved;
      await deliveryLock(async () => {
        await guard();
        registrationId = localStorage.getItem(key) || registrationId; readCleanup();
        let registration, subscription = localSubscription;
        try {registration = await navigator.serviceWorker.getRegistration("/"); subscription = await registration?.pushManager.getSubscription() || subscription;} catch (_) {}
        await guard();
        const serverAbsent = await removeServerRegistration(subscription,guard);
        await guard();
        try {if (subscription) await subscription.unsubscribe();} catch (_) {}
        await guard(); localKnown = false;
        try {registration ||= await navigator.serviceWorker.getRegistration("/"); localSubscription = await registration?.pushManager.getSubscription() || null; localKnown = true;} catch (_) {}
        await guard();
        if (serverAbsent && localKnown && !localSubscription && !registrationId) clearCleanup();
        const complete = gateSaved && serverAbsent && localKnown && !localSubscription && !pendingCleanup && !registrationId;
        menuValue = complete ? false : null; deliveryError = complete ? null : "setup";
        setStatus(complete ? "Disabled on this browser. Site permission remains in your browser settings." : !gateSaved ?
          "Notification blocking could not be saved. Cleanup was attempted. Choose Disable again." : localKnown && !localSubscription && !serverAbsent ? serverPending : disablingIncomplete);
      });
    } catch (error) {if (currentAction()) {menuValue=null;deliveryError="setup";setStatus(error.message);}}
    finally {busy = false; buttons();}
  }
  enables.forEach(control => control.addEventListener("click", enableNotifications));
  disables.forEach(control => control.addEventListener("click", disableNotifications));
  deliveryControls.forEach(control => control.addEventListener("change", async () => {
    const selected = control.checked, allowed = !control.disabled && !menuReading && !busy && !soundBusy && !pageClosed && !observationRetired;
    // Native inputs toggle before change. Restore the last confirmed value
    // synchronously; only the existing action's outcome can certify a new one.
    buttons();
    if (!allowed || menuValue === null || selected === menuValue) return;
    if (selected) await enableNotifications();
    else await disableNotifications();
  }));
  tests.forEach(control => control.addEventListener("click", async () => {
    if (soundBusy || busy || !registered) return;
    invalidateRefresh(true);
    busy = true; buttons();
    const retirement = cancelTest(), generation = pollGeneration, identity = registrationId;
    setText(testStatus, "A matching test result is unavailable. Appearance on your screen is unconfirmed.");
    try {
      await localResult(retirement);
      if (generation !== pollGeneration || pageClosed || identity !== registrationId || !registered) return;
      const response = await api("/test", {registrationId});
      if (generation !== pollGeneration || pageClosed || identity !== registrationId || !registered) return;
      stopTest();
      let descriptor = store?.testDescriptor?.(response.test);
      if (descriptor && (descriptor.identity !== `test-${registrationId}` || descriptor.expiresAt > Date.now() + 70000 || descriptor.expiresAt <= Date.now())) descriptor = null;
      if (!descriptor) cancelTest();
      pendingTest = descriptor;
      setStatus("A test notification will be sent in 10 seconds. Switch to another tab or app to check background delivery.");
      setText(testStatus, pendingTest ? "Waiting for this test to reach the browser." : "A matching test result is unavailable. Appearance on your screen is unconfirmed.");
      if (pendingTest) {
        try { await localResult(store.saveTest(pendingTest)); } catch (_) { setText(testStatus, "This browser could not save the test check. Keep this page open to check the result."); }
        pollTimer = setTimeout(() => void pollTest(), 2000);
      }
    }
    catch (error) { setStatus(error.message); }
    finally { busy = false; buttons(); }
  }));
  globalThis.workspaceBrowserNotifications = {
    async presence(snapshot, focused, seen) {
      if (!focused || !registrationId || !registered) return;
      const eventId = snapshot.event && seen.includes(snapshot.event.id) ? snapshot.event.id : "";
      try { await fetch(`/api/sessions/${document.body.dataset.session}/attention`, {method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify({identity: snapshot.identity, registrationId, eventId})}); } catch (_) {}
    },
  };
  navigator.serviceWorker?.addEventListener?.("message", event => {
    if (event.data?.type === "notification-test-update" && event.data.identity === pendingTest?.identity) void pollTest();
  });
  navigator.serviceWorker?.addEventListener?.("controllerchange", () => void refresh());
  document.addEventListener?.("visibilitychange", () => { if (document.visibilityState === "visible") { void refreshSound(); void refresh(); if (pendingTest) void pollTest(); } });
  globalThis.addEventListener?.("pagehide", () => { pageClosed = true; soundGeneration++; invalidateRefresh(); stopTest(); });
  globalThis.addEventListener?.("pageshow", () => { pageClosed = false; void refreshSound(); void refresh(); if (pendingTest) void pollTest(); });
  globalThis.addEventListener?.("storage", event => {
    if ((event.key === key && event.newValue !== registrationId) || (event.key === cleanupKey && event.newValue)) {
      invalidateRefresh(); observationRetired = true;
      cancelTest(); registered = false; menuValue = null; menuReading = false; deliveryError = "setup"; setStatus(incomplete); buttons();
    }
  });
  document.addEventListener?.("session-section-change", event => { if (event.detail === "settings") void refreshSound(); });
  document.getElementById("sidebar-menu")?.addEventListener("toggle", event => {
    if (event.newState === "open") {void refreshSound(); void refresh();}
  });
  void refreshSound();
  void refresh();
})();
