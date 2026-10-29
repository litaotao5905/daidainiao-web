"use strict";
// Registers the service worker (needed for "install app") and wires an
// optional "安装应用" button to Chrome's beforeinstallprompt event.
(function () {
  if ("serviceWorker" in navigator && window.isSecureContext) {
    addEventListener("load", () => {
      navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch(() => { /* not installable here (e.g. plain-http LAN host) */ });
    });
  }
  const btn = document.getElementById("btnInstall");
  const standalone = matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
  let deferred = null;
  addEventListener("beforeinstallprompt", (e) => {
    e.preventDefault();
    deferred = e;
    if (btn && !standalone) btn.hidden = false;
  });
  addEventListener("appinstalled", () => { deferred = null; if (btn) btn.hidden = true; });
  if (btn) {
    btn.onclick = async () => {
      if (!deferred) return;
      deferred.prompt();
      try { await deferred.userChoice; } catch (_) { /* ignore */ }
      deferred = null; btn.hidden = true;
    };
  }
})();
