"use strict";
(function () {
  const $ = (id) => document.getElementById(id);
  const state = {
    status: null, auth: { logged_in: false }, chats: [], selection: [], loadedSelection: false,
    byChat: {}, errors: {}, warnings: new Set(), loading: new Set(),
    topics: [], active: null, thread: null, preview: null, search: "",
    listTab: localStorage.getItem("feishu_list_tab") || "my",
    myTopics: [], myLoading: false, myLoaded: false, myError: null,
    failed: new Set(), drafts: {}, mentions: [], sending: false, composerThread: null,
    viewGen: 0,
    tab: "files", dlSig: "", dlMap: new Map(),
    dl: { items: [], dir: "", opener: false, docker: false, enabled: true },
    notify: { feishu_enabled: true, browser_enabled: true, sound_enabled: true },
    readState: { initialized: false, threads: {} },
    csrf: "",
  };
  try { state.drafts = JSON.parse(sessionStorage.getItem("drafts") || "{}"); } catch (_) { state.drafts = {}; }

  // ---------- helpers ----------
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  async function api(path, opts) {
    const r = await fetch(path, opts);
    let body = null;
    try { body = await r.json(); } catch (_) { /* ignore */ }
    if (!r.ok) {
      const err = new Error((body && body.error) || ("HTTP " + r.status));
      err.hint = body && body.hint;
      err.kind = body && body.kind;
      err.code = body && body.code;
      err.scopes = body && body.scopes;
      if (err.kind === "login_required") onLoggedOut();
      throw err;
    }
    return body;
  }
  const pad = (n) => String(n).padStart(2, "0");
  function fmtTime(ms, full) {
    const d = new Date(ms), now = new Date();
    const hm = pad(d.getHours()) + ":" + pad(d.getMinutes());
    const full_ = d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + hm + ":" + pad(d.getSeconds());
    if (full) return full_;
    const sameDay = (a, b) => a.toDateString() === b.toDateString();
    if (sameDay(d, now)) return hm;
    const y = new Date(now); y.setDate(now.getDate() - 1);
    if (sameDay(d, y)) return "昨天 " + hm;
    if (d.getFullYear() === now.getFullYear()) return pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + hm;
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
  }
  function dayLabel(ms) {
    const d = new Date(ms), now = new Date();
    if (d.toDateString() === now.toDateString()) return "今天";
    const y = new Date(now); y.setDate(now.getDate() - 1);
    if (d.toDateString() === y.toDateString()) return "昨天";
    return d.getFullYear() + "年" + (d.getMonth() + 1) + "月" + d.getDate() + "日";
  }
  function hue(s) { let h = 0; for (const c of s || "?") h = (h * 31 + c.codePointAt(0)) % 360; return h; }
  function avatar(name, url, small, bot) {
    const a = el("div", "avatar" + (small ? " sm" : ""));
    const initial = bot ? "🤖" : (Array.from((name || "?").replace(/^用户\s*/, "")).slice(-1)[0] || "?");
    const fallback = () => { a.textContent = initial; a.style.background = "hsl(" + hue(name) + ",45%,50%)"; };
    if (url && /^(https?:\/\/|\/api\/)/.test(url)) {
      const img = new Image();
      img.referrerPolicy = "no-referrer";
      img.alt = "";
      img.onerror = () => { img.remove(); fallback(); };
      img.src = url;
      a.appendChild(img);
    } else fallback();
    return a;
  }
  const safeHref = (h) => /^https?:\/\//i.test(h || "") ? h : null;
  function ext(name) { const m = /\.([A-Za-z0-9]+)$/.exec(name || ""); return m ? m[1].toLowerCase() : ""; }
  function resUrl(a, dl) {
    if (a.local_id) return "/api/files/" + a.local_id + "/content" + (dl ? "?dl=1" : "");
    const it = dlOf(a);
    if (it && it.status === "done") return "/api/files/" + it.id + "/content" + (dl ? "?dl=1" : "");
    const q = new URLSearchParams({ message_id: a.message_id, file_key: a.file_key, type: a.res_type });
    if (a.name) q.set("name", a.name);
    if (dl) q.set("dl", "1");
    return "/api/resource?" + q;
  }
  function resUrlRaw(messageId, key, type) {
    const it = dlItem(messageId, key);
    if (it && it.status === "done") return "/api/files/" + it.id + "/content";
    return "/api/resource?" + new URLSearchParams({ message_id: messageId, file_key: key, type });
  }
  const FILE_ICONS = { pdf: "📕", doc: "📘", docx: "📘", xls: "📗", xlsx: "📗", csv: "📗", ppt: "📙", pptx: "📙", zip: "🗜️", rar: "🗜️", "7z": "🗜️", txt: "📄", md: "📄", log: "📄", mp4: "🎞️", mov: "🎞️", mp3: "🎵", opus: "🎵" };
  const fileIcon = (a) => a.kind === "image" ? "🖼️" : a.kind === "video" ? "🎞️" : a.kind === "audio" ? "🎵" : a.kind === "doc" ? "📄" : a.kind === "link" ? "🔗" : (FILE_ICONS[ext(a.name)] || "📎");

  // ---------- local downloads ----------
  const DRIVE_RE = /^https?:\/\/[^/]+\/file\/([A-Za-z0-9]{8,64})(?:[/?#]|$)/;
  function attKey(a) {
    const k = a.file_key || a.resource_token || (DRIVE_RE.exec(a.url || "") || [])[1];
    return k ? (a.message_id || "") + "|" + k : "";
  }
  const dlItem = (msgId, key) => state.dlMap.get(msgId + "|" + key) || state.dlMap.get("|" + key);
  const dlOf = (a) => { const k = attKey(a); const fk = a.file_key || a.resource_token; return (k && state.dlMap.get(k)) || (fk && state.dlMap.get("|" + fk)) || state.dl.items.find((i) => i.file_key === fk && (i.res_type === a.res_type || i.res_type === "export:" + a.resource_type)); };
  const isOnlineAtt = (a) => (a.kind === "doc" || a.kind === "unresolved") && (!dlOf(a) || isOnlineDl(dlOf(a)));
  const topicKeyOf = (t) => (t && t.topic && (t.topic.root_message_id || t.topic.thread_id)) || "";
  const isOnlineDl = (i) => i.res_type.startsWith("export:") && i.resource_type !== "file";
  const dlDisplayName = (i) => i && i.cloud_name ? i.cloud_name : i && i.name && !/^https?:\/\//i.test(i.name) ? i.name : (i && (i.name_hint || i.name)) || "（未下载）";
  const isPendingDl = (i) => i.status === "waiting" || i.status === "downloading";

  function toast(msg, isErr) {
    let t = document.getElementById("toast");
    if (t) t.remove();
    t = el("div", "toast" + (isErr ? " err" : ""), msg);
    t.id = "toast"; t.setAttribute("role", "status");
    document.body.appendChild(t);
    clearTimeout(toast.t);
    toast.t = setTimeout(() => t.remove(), isErr ? 5000 : 2200);
  }
  async function csrfToken(fresh) {
    if (!state.csrf || fresh) state.csrf = (await api("/api/csrf")).token;
    return state.csrf;
  }
  async function postFile(path, id) {
    const go = async (fresh) => api(path, { method: "POST", headers: { "Content-Type": "application/json", "X-FT-CSRF": await csrfToken(fresh) }, body: JSON.stringify({ id }) });
    try { return await go(false); } catch (e) {
      if (/CSRF/.test(e.message || "")) return go(true); // server restarted: token changed
      throw e;
    }
  }

  // ---------- task completion notifications ----------
  function playChime() {
    if (state.notify && state.notify.sound_enabled === false) return;
    try {
      const AudioCtx = window.AudioContext || window.webkitAudioContext;
      if (!AudioCtx) return;
      const ctx = new AudioCtx();
      if (ctx.state === "suspended") ctx.resume();
      const now = ctx.currentTime;

      const o1 = ctx.createOscillator();
      const g1 = ctx.createGain();
      o1.type = "sine";
      o1.frequency.setValueAtTime(659.25, now);
      o1.frequency.exponentialRampToValueAtTime(880, now + 0.12);
      g1.gain.setValueAtTime(0.001, now);
      g1.gain.exponentialRampToValueAtTime(0.2, now + 0.04);
      g1.gain.exponentialRampToValueAtTime(0.001, now + 0.55);
      o1.connect(g1); g1.connect(ctx.destination);
      o1.start(now); o1.stop(now + 0.55);

      const o2 = ctx.createOscillator();
      const g2 = ctx.createGain();
      o2.type = "sine";
      o2.frequency.setValueAtTime(1318.5, now + 0.12);
      g2.gain.setValueAtTime(0.001, now + 0.12);
      g2.gain.exponentialRampToValueAtTime(0.25, now + 0.16);
      g2.gain.exponentialRampToValueAtTime(0.001, now + 0.85);
      o2.connect(g2); g2.connect(ctx.destination);
      o2.start(now + 0.12); o2.stop(now + 0.85);
    } catch (_) {}
  }

  function showDesktopNotice(title, body, threadID) {
    if (state.notify && state.notify.browser_enabled === false) return;
    if (!("Notification" in window) || Notification.permission !== "granted") return;
    try {
      const n = new Notification(title, {
        body: body || "呆呆鸟已完成本轮任务，请前往查看。",
        icon: "/favicon.svg",
        tag: "task-" + (threadID || "done"),
        renotify: true,
      });
      n.onclick = () => {
        window.focus();
        if (threadID && state.active !== threadID) {
          selectTopic(threadID);
        }
        n.close();
      };
    } catch (_) {}
  }

  let titleAlertTimer = null;
  const originalTitle = document.title || "网页呆呆鸟";
  function flashTitle(alertText) {
    if (!document.hidden) return;
    clearInterval(titleAlertTimer);
    let toggle = false;
    titleAlertTimer = setInterval(() => {
      if (!document.hidden) {
        clearInterval(titleAlertTimer);
        document.title = originalTitle;
        return;
      }
      toggle = !toggle;
      document.title = toggle ? (alertText + " " + originalTitle) : originalTitle;
    }, 900);
  }
  window.addEventListener("focus", () => {
    clearInterval(titleAlertTimer);
    document.title = originalTitle;
  });

  async function onTaskFinished(t, id) {
    const topicTitle = (t.topic && (t.topic.title || t.topic.snippet)) || "当前话题";
    const chatName = (t.topic && t.topic.chat_name) || "呆呆鸟群";
    let snippet = "";
    if (t.messages && t.messages.length) {
      for (let i = t.messages.length - 1; i >= 0; i--) {
        const m = t.messages[i];
        if (m.sender_type === "app" && !m.deleted && m.plain) {
          snippet = m.plain.slice(0, 160).trim();
          break;
        }
      }
    }
    if (!snippet) snippet = "本轮任务已执行结束，请前往查看详细内容与附件。";

    playChime();
    showDesktopNotice("🔔 呆呆鸟任务已完成", "话题：「" + topicTitle + "」\n" + snippet, id);
    flashTitle("【🔔 任务完成!】");
    toast("🎉「" + topicTitle + "」呆呆鸟任务已完成！", false);

    try {
      await api("/api/notifications/complete", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-FT-CSRF": await csrfToken() },
        body: JSON.stringify({
          thread_id: id,
          chat_name: chatName,
          topic_title: topicTitle,
          snippet: snippet,
        }),
      });
    } catch (_) {}
  }

  function updateBrowserPermBadge() {
    const b = $("browserPermStatus");
    if (!b) return;
    if (!("Notification" in window)) {
      b.textContent = "浏览器不支持桌面通知";
      b.className = "perm-status warn";
      return;
    }
    const p = Notification.permission;
    if (p === "granted") {
      b.textContent = "已授权桌面通知 ✅";
      b.className = "perm-status ok";
    } else if (p === "denied") {
      b.textContent = "已在浏览器中禁止通知 🚫";
      b.className = "perm-status err";
    } else {
      b.textContent = "未授权（点击按钮开启）";
      b.className = "perm-status warn";
    }
  }

  async function testBrowserNotification() {
    if (!("Notification" in window)) {
      toast("当前浏览器不支持桌面通知", true);
      return;
    }
    if (Notification.permission !== "granted") {
      const res = await Notification.requestPermission();
      updateBrowserPermBadge();
      if (res !== "granted") {
        toast("桌面通知授权未通过，请检查浏览器通知权限设置", true);
        return;
      }
    }
    updateBrowserPermBadge();
    showDesktopNotice("🔔 网页呆呆鸟 - 桌面通知测试", "这是一条测试桌面通知。当任务完成时，你会收到类似提醒！");
    toast("已触发桌面通知，请查看屏幕右上角/右下角", false);
  }

  async function testFeishuNotification() {
    const btn = $("btnTestFeishu");
    btn.disabled = true;
    btn.textContent = "发送中…";
    try {
      await api("/api/notifications/test", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-FT-CSRF": await csrfToken() },
        body: "{}",
      });
      toast("📱 测试卡片已发送！请查看手机或电脑飞书单聊消息", false);
    } catch (e) {
      toast("发送失败：" + (e.hint || e.message), true);
    } finally {
      btn.disabled = false;
      btn.textContent = "发送测试卡片";
    }
  }

  async function updateNotifySetting(key, val) {
    if (!state.notify) state.notify = { feishu_enabled: true, browser_enabled: true, sound_enabled: true };
    state.notify[key] = val;
    try {
      await api("/api/notifications/settings", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-FT-CSRF": await csrfToken() },
        body: JSON.stringify(state.notify),
      });
    } catch (_) {}
  }

  function openNotifyModal() {
    if (state.notify) {
      $("chkNotifyFeishu").checked = state.notify.feishu_enabled !== false;
      $("chkNotifyBrowser").checked = state.notify.browser_enabled !== false;
      $("chkNotifySound").checked = state.notify.sound_enabled !== false;
    }
    updateBrowserPermBadge();
    $("notifyModal").hidden = false;
  }

  function closeNotifyModal() {
    $("notifyModal").hidden = true;
  }

  // Download state timestamps and filenames must not change a material's order.
  function dlTime(i) { return i.msg_time || 0; }
  function sortAndDedupeDl(list) {
    const sorted = (list || []).slice().sort((a, b) => {
      const ta = dlTime(a), tb = dlTime(b);
      if (ta !== tb) return tb - ta;
      const ia = a.id || "", ib = b.id || "";
      return ia === ib ? 0 : ia < ib ? -1 : 1;
    });
    const seen = new Set();
    const deduped = [];
    for (const i of sorted) {
      const k = (i.rel || i.path || "").toLowerCase();
      if (k) {
        if (seen.has(k)) continue;
        seen.add(k);
      }
      deduped.push(i);
    }
    return { sorted, deduped };
  }
  function dlSig(items) { return items.map((i) => i.id + ":" + (i.cloud_name || "") + ":" + (i.name || "") + ":" + (i.name_hint || "") + ":" + i.status + ":" + i.size + ":" + (i.reason || "") + ":" + (i.sync_status || "") + ":" + (i.sync_error || "") + ":" + (i.last_checked || 0) + ":" + (i.downloaded_bytes || 0) + ":" + (i.total_bytes || 0)).join(","); }
  // apply a new list of download states; re-render what depends on it
  function setDownloads(list) {
    const key = topicKeyOf(state.thread);
    const { sorted, deduped } = sortAndDedupeDl(key ? (list || []).filter((i) => i.topic_key === key) : []);
    let chat = sorted.some((i) => i.status === "done" && (state.dlMap.get(i.message_id + "|" + i.file_key) || {}).status !== "done");
    state.dlMap.clear();
    for (const i of sorted) {
      state.dlMap.set(i.message_id + "|" + i.file_key, i);
      if (i.file_key && !state.dlMap.has("|" + i.file_key)) {
        state.dlMap.set("|" + i.file_key, i);
      }
    }
    const sig = dlSig(deduped);
    if (sig === state.dlSig) { scheduleDlPoll(); return false; }
    state.dlSig = sig;
    state.dl.items = deduped;
    for (const i of sorted) if (i.status === "done" && state.failed.delete(failKey(i.message_id, i.file_key))) chat = true;
    updateDlBadges();
    if (chat && state.thread) { const box = $("messages"); renderThread(nearBottom(box) ? "bottom" : "keep", box.scrollTop); }
    renderFiles();
    scheduleDlPoll();
    return true;
  }
  function renderTabs() {
    const done = state.dl.items.filter((i) => i.status === "done").length;
    const pend = state.dl.items.some(isPendingDl);
    const fileCount = state.dl.items.filter((i) => !isOnlineDl(i)).length;
    $("dlCount").textContent = fileCount ? String(fileCount) : "";
    const docCount = ((state.thread && state.thread.attachments) || []).filter(isOnlineAtt).length;
    $("filesCount").textContent = docCount ? String(docCount) : "";
    $("tabFiles").classList.toggle("on", state.tab === "files");
    $("tabDl").classList.toggle("on", state.tab === "dl");
    $("tabFiles").setAttribute("aria-selected", state.tab === "files");
    $("tabDl").setAttribute("aria-selected", state.tab === "dl");
    $("filesTabs").hidden = !!state.preview;
    $("filesTitle").hidden = !state.preview;
  }
  function setTab(tab) {
    state.tab = tab; state.preview = null;
    renderFiles();
    if (tab === "dl") refreshDl();
  }
  $("tabFiles").onclick = () => setTab("files");
  $("tabDl").onclick = () => setTab("dl");

  let dlTimer = null, dlBusy = false;
  function dlNeedsPoll() { return (state.thread && state.thread.file_sync && (state.thread.file_sync.status === "checking" || state.thread.file_sync.names_loading)) || state.dl.items.some(isPendingDl); }
  function scheduleDlPoll() {
    clearTimeout(dlTimer);
    if (!state.auth.logged_in || !state.thread || !dlNeedsPoll()) return;
    dlTimer = setTimeout(refreshDl, 1500);
  }
  async function refreshDl() {
    clearTimeout(dlTimer);
    const t = state.thread;
    if (!t || !state.auth.logged_in || dlBusy) return;
    const key = topicKeyOf(t);
    if (!key) return;
    dlBusy = true;
    try {
      const r = await api("/api/downloads?topic=" + encodeURIComponent(key));
      if (topicKeyOf(state.thread) === key) {
        Object.assign(state.dl, { dir: r.dir, opener: r.opener, docker: r.docker, enabled: r.enabled });
        if (r.file_sync) state.thread.file_sync = r.file_sync;
        if (r.attachments) state.thread.attachments = r.attachments;
        setDownloads(r.items);
        if (!state.preview) renderFiles();
      }
    } catch (_) { /* keep the current state */ }
    dlBusy = false;
    scheduleDlPoll();
  }
  // status label next to file chips in the conversation
  function downloadButton(i) {
    const b = el("button", "retry download-btn", isPendingDl(i) ? "下载中…" : "下载");
    b.type = "button"; b.disabled = isPendingDl(i) || !state.dl.enabled;
    b.title = i.status === "done" ? "重新下载云端文件，覆盖前会备份本地修改" : "下载到默认保存位置";
    b.onclick = (ev) => { ev.preventDefault(); ev.stopPropagation(); b.disabled = true; retryDl(i).finally(() => { b.disabled = isPendingDl(state.dl.items.find((x) => x.id === i.id) || i); }); };
    return b;
  }
  function updateDlBadges() {
    document.querySelectorAll("[data-dlk]").forEach((n) => {
      const k = n.dataset.dlk || "";
      const sep = k.indexOf("|");
      const i = state.dlMap.get(k) || (sep >= 0 && state.dlMap.get(k.slice(sep)));
      n.textContent = ""; n.className = "dlst";
      if (!i) return;
      n.appendChild(dlStatusNode(i));
      n.appendChild(downloadButton(i));
    });
  }
  async function retryDl(i) {
    try {
      const item = await postFile("/api/files/download", i.id);
      state.failed.delete(failKey(i.message_id, i.file_key));
      setDownloads(state.dl.items.map((x) => x.id === item.id ? item : x));
      await refreshDl();
    } catch (e) { toast("下载失败：" + e.message, true); }
  }
  async function openLocal(i) {
    if (i.status !== "done") return;
    if (!state.dl.opener) { window.open("/api/files/" + i.id + "/content", "_blank", "noopener"); return; }
    try { await postFile("/api/files/open", i.id); } catch (e) { toast("无法打开：" + e.message, true); }
  }
  async function revealLocal(i) {
    try { await postFile("/api/files/reveal", i.id); } catch (e) { toast("无法显示文件夹：" + e.message, true); }
  }
  async function copyText(txt) {
    try { await navigator.clipboard.writeText(txt); return true; } catch (_) { /* insecure context etc. */ }
    const ta = el("textarea"); ta.value = txt; ta.style.cssText = "position:fixed;opacity:0;left:-1000px";
    document.body.appendChild(ta); ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
    ta.remove();
    return ok;
  }
  async function copyPath(i) {
    if (!i.path) { toast("该文件还没有本地路径", true); return; }
    toast((await copyText(i.path)) ? "已复制文件路径" : "复制失败，请手动复制：" + i.path, false);
  }

  // ---------- context menu ----------
  let menuEl = null;
  function closeMenu() { if (menuEl) { menuEl.remove(); menuEl = null; } }
  function showMenu(x, y, entries) {
    closeMenu();
    const m = el("div", "ctxmenu"); m.setAttribute("role", "menu");
    for (const it of entries) {
      if (it === "-") { m.appendChild(el("hr")); continue; }
      const b = el("button", null, it.label); b.type = "button"; b.setAttribute("role", "menuitem");
      if (it.disabled) { b.disabled = true; if (it.title) b.title = it.title; }
      b.onclick = (ev) => { ev.stopPropagation(); closeMenu(); it.run(); };
      m.appendChild(b);
    }
    m.style.left = "0px"; m.style.top = "0px"; m.style.visibility = "hidden";
    document.body.appendChild(m);
    const r = m.getBoundingClientRect();
    m.style.left = Math.max(4, Math.min(x, innerWidth - r.width - 4)) + "px";
    m.style.top = Math.max(4, Math.min(y, innerHeight - r.height - 4)) + "px";
    m.style.visibility = "";
    menuEl = m;
    const first = m.querySelector("button:not(:disabled)"); if (first) first.focus({ preventScroll: true });
    m.addEventListener("keydown", (ev) => {
      const bs = Array.from(m.querySelectorAll("button:not(:disabled)")); const i = bs.indexOf(document.activeElement);
      if (ev.key === "ArrowDown") { ev.preventDefault(); (bs[i + 1] || bs[0]).focus(); }
      else if (ev.key === "ArrowUp") { ev.preventDefault(); (bs[i - 1] || bs[bs.length - 1]).focus(); }
    });
  }
  document.addEventListener("mousedown", (e) => { if (menuEl && !menuEl.contains(e.target)) closeMenu(); }, true);
  document.addEventListener("contextmenu", (e) => { if (menuEl && !menuEl.contains(e.target)) closeMenu(); }, true);
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeMenu(); });
  addEventListener("blur", closeMenu);
  addEventListener("resize", closeMenu);
  document.addEventListener("scroll", closeMenu, true);
  function fileMenu(i) {
    const done = i.status === "done";
    const noOpener = !state.dl.opener;
    const items = [
      { label: noOpener ? "在浏览器中打开" : "用默认应用打开", disabled: !done, title: done ? "" : "文件尚未下载", run: () => openLocal(i) },
      { label: "在文件夹中显示", disabled: !done || noOpener, title: noOpener ? "当前运行环境无法打开宿主机的文件管理器" : (done ? "" : "文件尚未下载"), run: () => revealLocal(i) },
      { label: "复制文件路径", disabled: !i.path, run: () => copyPath(i) },
    ];
    if (done && dlPreviewable(i)) {
      items.push({ label: "右侧内联预览", run: () => openPreview(dlAsAtt(i)) });
    }
    items.push("-", { label: "重新下载", disabled: isPendingDl(i), run: () => retryDl(i) });
    return items;
  }
  function withFileMenu(node, getItem) {
    node.addEventListener("contextmenu", (ev) => {
      const i = getItem();
      if (!i) return;
      ev.preventDefault(); ev.stopPropagation();
      showMenu(ev.clientX, ev.clientY, fileMenu(i));
    });
    return node;
  }

  // ---------- theme ----------
  function applyTheme(t) {
    document.documentElement.dataset.theme = t; localStorage.setItem("theme", t);
    const m = document.getElementById("themeColor"); // title-bar colour of the installed app window
    if (m) m.content = getComputedStyle(document.documentElement).getPropertyValue("--panel").trim() || "#ffffff";
  }
  applyTheme(localStorage.getItem("theme") || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"));
  $("btnTheme").onclick = () => applyTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark");

  // ---------- splitters ----------
  for (const v of ["--left-w", "--right-w"]) {
    const saved = localStorage.getItem(v);
    if (saved) document.documentElement.style.setProperty(v, saved);
  }
  document.querySelectorAll(".splitter").forEach((sp) => {
    sp.addEventListener("mousedown", (ev) => {
      ev.preventDefault();
      const v = sp.dataset.var, dir = Number(sp.dataset.dir);
      const startX = ev.clientX;
      const start = parseInt(getComputedStyle(document.documentElement).getPropertyValue(v), 10);
      sp.classList.add("drag");
      const move = (e) => {
        const w = Math.min(640, Math.max(200, start + dir * (e.clientX - startX)));
        document.documentElement.style.setProperty(v, w + "px");
      };
      const up = () => {
        sp.classList.remove("drag");
        localStorage.setItem(v, getComputedStyle(document.documentElement).getPropertyValue(v).trim());
        removeEventListener("mousemove", move); removeEventListener("mouseup", up);
      };
      addEventListener("mousemove", move); addEventListener("mouseup", up);
    });
  });

  // ---------- auth ----------
  function renderAuth() {
    const a = state.auth, ok = !!a.logged_in;
    $("btnLogin").hidden = ok;
    $("userBox").hidden = !ok;
    $("btnGroups").hidden = !ok;
    $("btnRefresh").hidden = !ok;
    $("btnMarkAllRead").hidden = !ok;
    $("btnNewTopic").hidden = !(ok && a.reply_enabled !== false && state.selection.length);
    renderComposer();
    if (ok && a.user) {
      $("userAvatar").textContent = "";
      $("userAvatar").appendChild(avatar(a.user.name, a.user.avatar_url, true));
      $("userName").textContent = a.user.name || a.user.open_id;
      $("userName").title = (a.can_refresh ? "已授权（可自动续期）" : "未授予 offline_access，token 过期后需重新登录") + (a.expires_at ? "\nuser_access_token 有效期至 " + fmtTime(Date.parse(a.expires_at), true) : "");
    }
  }
  function onLoggedOut() {
    if (!state.auth.logged_in) return;
    state.auth = { logged_in: false };
    state.thread = null; state.active = null; state.preview = null; state.topics = []; state.byChat = {}; state.composerThread = null;
    stopAutoRefresh();
    clearTimeout(dlTimer); closeMenu(); state.dl = { items: [], dir: "", opener: false, docker: false, enabled: true }; state.dlMap = new Map(); state.dlSig = ""; state.csrf = "";
    replyTray.reset(true); ntTray.reset(true); $("ntModal").hidden = true; $("btnMarkAllRead").hidden = true;
    renderAuth(); renderList(); renderThread(); renderFiles();
  }
  function loginPrompt(parent, text) {
    const e = el("div", "empty");
    e.append(el("div", "empty-ico", "🔐"), el("div", null, text || "请先登录飞书"));
    e.append(el("div", "sub muted", "使用你自己的飞书账号授权后，可读取你已加入的所有群聊（无需把机器人拉进群）。"));
    const b = el("a", "btn primary", "用飞书登录");
    b.href = "/api/auth/login";
    e.appendChild(b);
    parent.appendChild(e);
  }
  $("btnLogout").onclick = async () => {
    try { await api("/api/auth/logout", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }); } catch (_) { /* ignore */ }
    state.auth = { logged_in: true }; onLoggedOut();
  };

  // ---------- topic list ----------
  const isScopeErr = (e) => e && (e.kind === "scope_missing" || e.kind === "app_scope_missing");
  // "authorization is missing a scope" -> explain + re-login button
  function scopeActions(e) {
    const w = el("div", "scope-actions");
    if (e && e.scopes && e.scopes.length) w.appendChild(el("div", "hint", "所需权限：" + e.scopes.join("、")));
    if (e && e.kind === "app_scope_missing") w.appendChild(el("div", "hint", "应用尚未开通该权限：请先在开发者后台「权限管理 → 用户身份权限」开通并发布版本，再重新登录授权。"));
    const b = el("a", "btn primary", "重新登录授权");
    b.href = "/api/auth/login";
    w.appendChild(b);
    return w;
  }
  function banner(kind, msg, hint, err) {
    const b = el("div", "banner " + kind);
    b.appendChild(el("div", null, isScopeErr(err) ? "需要重新登录授权：" + msg : msg));
    if (hint) b.appendChild(el("div", "hint", hint));
    if (isScopeErr(err)) b.appendChild(scopeActions(err));
    return b;
  }
  function selectedChatName(id) {
    const c = state.chats.find((x) => x.chat_id === id);
    if (c && c.name) return c.name;
    const tp = (state.byChat[id] || []).find((t) => t.chat_name);
    return (tp && tp.chat_name) || "";
  }
  function updateListTabUI() {
    const isMy = state.listTab === "my";
    const tabMy = $("tabMyTopics"), tabGrp = $("tabGroupTopics");
    if (tabMy) {
      tabMy.classList.toggle("on", isMy);
      tabMy.setAttribute("aria-selected", isMy ? "true" : "false");
    }
    if (tabGrp) {
      tabGrp.classList.toggle("on", !isMy);
      tabGrp.setAttribute("aria-selected", !isMy ? "true" : "false");
    }
  }
  function setListTab(tab) {
    if (state.listTab === tab && (tab !== "my" || state.myLoaded)) return;
    state.listTab = tab;
    try { localStorage.setItem("feishu_list_tab", tab); } catch (_) {}
    updateListTabUI();
    if (tab === "group" && state.thread && state.thread.topic && !state.selection.includes(state.thread.topic.chat_id)) {
      state.thread = null;
      state.active = null;
      setDownloads([]);
      renderThread();
      renderFiles();
    }
    if (tab === "my" && !state.myLoaded && state.auth.logged_in) {
      loadMyTopics(false);
    } else {
      renderList();
      refreshTopicListSilently();
    }
  }
  function renderGroupButton() {
    if (state.listTab === "my") {
      $("groupLabel").textContent = "所有呆呆鸟群";
      $("groupCount").textContent = "";
      $("btnGroups").title = "当前在「我的会话」模式，已自动汇聚所有呆呆鸟群中与你相关的话题";
      return;
    }
    const n = state.selection.length;
    if (n === 0) {
      $("groupLabel").textContent = "选择群聊";
      $("groupCount").textContent = "";
      $("btnGroups").title = "选择要包含的群聊";
    } else if (n === 1) {
      const name = selectedChatName(state.selection[0]) || "已选 1 个群";
      $("groupLabel").textContent = name;
      $("groupCount").textContent = "";
      $("btnGroups").title = name + " · 点击更换群聊";
    } else {
      const firstName = selectedChatName(state.selection[0]) || "群聊";
      $("groupLabel").textContent = firstName + " 等";
      $("groupCount").textContent = String(n);
      $("btnGroups").title = "已选择 " + n + " 个群聊 · 点击更换";
    }
  }
  function rebuildTopics() {
    const all = [];
    for (const id of state.selection) if (state.byChat[id]) all.push(...state.byChat[id]);
    all.sort((a, b) => b.last_time - a.last_time);
    state.topics = all;
  }
  function checkInitReadState() {
    if (!state.readState || state.readState.initialized) return;
    const all = [...state.myTopics, ...state.topics];
    if (!all.length) return;
    const threads = {};
    for (const t of all) {
      if (t.thread_id) {
        threads[t.thread_id] = t.last_time || t.create_time || Date.now();
      }
    }
    state.readState.initialized = true;
    state.readState.threads = threads;
    api("/api/read_state", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ initialized: true, threads: threads })
    }).catch(() => {});
  }
  function markTopicRead(id, timeMs) {
    if (!id || !state.readState || !state.readState.threads) return;
    const cur = state.readState.threads[id] || 0;
    const t = state.myTopics.find((x) => x.thread_id === id) || state.topics.find((x) => x.thread_id === id);
    const ms = Math.max(timeMs || 0, (t && t.last_time) || 0, Date.now());
    if (ms > cur) {
      state.readState.threads[id] = ms;
      api("/api/read_state", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ thread_id: id, read_time: ms })
      }).catch(() => {});
    }
  }
  function renderTopicRow(list, t, showChatBadge) {
    const row = el("div", "topic" + (state.active === t.thread_id ? " active" : ""));
    row.dataset.thread = t.thread_id;
    row.appendChild(avatar(t.sender_name, t.sender_avatar, false, t.sender_type === "app"));
    const main = el("div", "topic-main");
    const trow = el("div", "topic-row");

    const readTime = (state.readState && state.readState.threads && state.readState.threads[t.thread_id]) || 0;
    const isUnread = !!(state.readState && state.readState.initialized && t.last_time && t.last_time > readTime && state.active !== t.thread_id);
    if (isUnread) {
      row.classList.add("unread");
      const dot = el("span", "unread-dot");
      dot.title = "有新未读内容";
      trow.appendChild(dot);
    }

    trow.appendChild(el("div", "topic-title", t.title || t.snippet || "无标题话题"));
    const tm = el("span", "topic-time", fmtTime(t.last_time));
    tm.title = "最近活动 " + fmtTime(t.last_time, true) + "\n发起 " + fmtTime(t.create_time, true);
    trow.appendChild(tm);
    main.appendChild(trow);
    const meta = el("div", "topic-meta");
    if (isUnread) {
      meta.appendChild(el("span", "pill unread", "新回复"));
    }
    meta.appendChild(el("span", "who", t.sender_name));
    if (t.pending) meta.appendChild(el("span", "pill new", "新建 · 待回复"));
    else meta.appendChild(el("span", "pill", t.reply_count >= 0 ? t.reply_count + " 条回复" : "回复数未知"));
    if (t.attachment_count > 0) meta.appendChild(el("span", "pill", "📎 " + t.attachment_count));
    main.appendChild(meta);
    if (showChatBadge && t.chat_name) {
      const chatRow = el("div", "topic-chat-row");
      const badge = el("span", "pill chat", t.chat_name);
      badge.title = t.chat_name;
      chatRow.appendChild(badge);
      main.appendChild(chatRow);
    }
    row.appendChild(main);
    row.onclick = () => selectTopic(t.thread_id);
    list.appendChild(row);
  }
  function renderList() {
    const list = $("topicList"), bn = $("banner");
    list.textContent = ""; bn.textContent = "";
    updateListTabUI();
    renderGroupButton();
    const myCountEl = $("myCount"), grpCountEl = $("groupCountTab");
    if (myCountEl) myCountEl.textContent = state.myTopics.length ? String(state.myTopics.length) : "";
    if (grpCountEl) grpCountEl.textContent = state.topics.length ? String(state.topics.length) : "";
    const canReply = state.auth.logged_in && state.auth.reply_enabled !== false;
    $("btnNewTopic").hidden = !canReply;
    if (state.status && !state.status.configured) {
      bn.appendChild(banner("err", "尚未配置飞书凭证", "请配置 config.json 或环境变量 FEISHU_APP_ID / FEISHU_APP_SECRET，或使用 --mock 运行（详见 README）。"));
    }
    if (state.listTab === "group") {
      for (const [id, e] of Object.entries(state.errors)) {
        const c = state.chats.find((x) => x.chat_id === id);
        bn.appendChild(banner("err", (c ? c.name : id) + "：" + e.message, e.hint, e));
      }
    } else if (state.myError) {
      bn.appendChild(banner("err", "拉取我的会话失败：" + state.myError.message, state.myError.hint, state.myError));
    }
    for (const w of state.warnings) bn.appendChild(banner("warn", w));
    if (state.status && state.status.configured && !state.auth.logged_in) {
      if (state.loadedSelection) loginPrompt(list);
      return;
    }

    const q = state.search.trim().toLowerCase();
    if (state.listTab === "my") {
      if (state.myLoading) list.appendChild(el("div", "loading", "正在加载所有呆呆鸟群中与你相关的话题…"));
      const items = state.myTopics.filter((t) => !q || (t.title + " " + t.sender_name + " " + t.chat_name + " " + (t.snippet || "")).toLowerCase().includes(q));
      if (!items.length && !state.myLoading) {
        list.appendChild(el("div", "empty", q ? "没有匹配的话题" : "暂无与你相关的呆呆鸟群话题"));
      }
      for (const t of items) renderTopicRow(list, t, true);
    } else {
      if (!state.selection.length) {
        const e = el("div", "empty");
        e.append(el("div", "empty-ico", "👥"), el("div", null, state.loadedSelection ? "尚未选择群聊" : "加载中…"));
        if (state.loadedSelection) {
          const b = el("button", "btn primary", "选择群聊");
          b.onclick = openModal; e.appendChild(b);
        }
        list.appendChild(e); return;
      }
      if (state.loading.size) list.appendChild(el("div", "loading", "正在加载 " + state.loading.size + " 个群聊的话题…"));
      const items = state.topics.filter((t) => !q || (t.title + " " + t.sender_name + " " + t.chat_name + " " + (t.snippet || "")).toLowerCase().includes(q));
      if (!items.length && !state.loading.size) {
        list.appendChild(el("div", "empty", q ? "没有匹配的话题" : "所选群聊中暂无话题（话题 = 带回复的消息串）"));
      }
      const multi = state.selection.length > 1;
      for (const t of items) renderTopicRow(list, t, multi);
    }
  }

  function renderListKeepScroll() {
    const list = $("topicList");
    const top = list.scrollTop;
    renderList();
    list.scrollTop = top;
  }
  async function loadMyTopics(refresh) {
    if (!state.auth.logged_in) return;
    state.myLoading = true; state.myError = null;
    renderList();
    $("btnRefresh").classList.add("spin");
    try {
      const r = await api("/api/topics/my" + (refresh ? "?refresh=1" : ""));
      state.myTopics = r.topics || [];
      state.myLoaded = true;
      (r.warnings || []).forEach((w) => state.warnings.add(w));
    } catch (e) {
      state.myError = e;
    } finally {
      state.myLoading = false;
      $("btnRefresh").classList.remove("spin");
      checkInitReadState();
      renderList();
    }
  }
  async function loadTopics(refresh) {
    state.errors = {}; state.warnings = new Set(); state.byChat = refresh ? {} : state.byChat;
    const ids = state.selection.slice();
    for (const k of Object.keys(state.byChat)) if (!ids.includes(k)) delete state.byChat[k];
    ids.forEach((id) => state.loading.add(id));
    rebuildTopics(); renderList();
    $("btnRefresh").classList.add("spin");
    try {
      await Promise.all(ids.map(async (id) => {
        try {
          const r = await api("/api/topics?chat_id=" + encodeURIComponent(id) + (refresh ? "&refresh=1" : ""));
          if (!state.auth.logged_in) return;
          state.byChat[id] = r.topics || [];
          (r.warnings || []).forEach((w) => state.warnings.add(w));
        } catch (e) { state.errors[id] = e; }
        state.loading.delete(id);
        rebuildTopics(); renderList();
      }));
    } finally {
      $("btnRefresh").classList.remove("spin");
      checkInitReadState();
      renderList();
    }
    if (state.active && !state.topics.some((t) => t.thread_id === state.active) && !state.myTopics.some((t) => t.thread_id === state.active) && !state.thread) state.active = null;
  }

  // ---------- chat ----------
  const failKey = (msgId, key) => msgId + ":" + key;
  let failTimer = null;
  function markFailed(key) {
    if (state.failed.has(key)) return;
    state.failed.add(key);
    clearTimeout(failTimer);
    failTimer = setTimeout(() => { if (!state.preview) renderFiles(); failNotice(); }, 150);
  }
  const NODL_TIP = "卡片 / 合并转发中的图片和文件：飞书接口不支持下载";
  const DL_TIP = "飞书「获取消息资源」接口仅支持应用身份（tenant_access_token）且要求机器人在群内，用户身份无法下载。解决：把应用机器人加入该群，或在飞书中查看。";
  function openLink(text) {
    const u = state.thread && state.thread.open_url;
    if (!u) return null;
    const a = el("a", "openfs", text || "在飞书中打开");
    a.href = u; a.target = "_blank"; a.rel = "noopener noreferrer";
    a.title = "在飞书客户端打开该群聊（飞书未提供指向单条消息/话题的公开链接）";
    a.onclick = (ev) => ev.stopPropagation();
    return a;
  }
  // compact inline placeholder for a resource that cannot be downloaded
  function failChip(label, noDl, item) {
    const c = el("span", "failchip");
    c.title = noDl ? NODL_TIP : DL_TIP;
    const why = noDl ? "（卡片/转发内，飞书不支持下载）" : (item && item.reason ? " · " + item.reason : "");
    c.appendChild(el("span", null, (label === "图片" ? "🖼️ " : "📎 ") + label + "无法下载" + why));
    if (item && !noDl) { const b = el("button", "retry", "重试"); b.type = "button"; b.onclick = (ev) => { ev.stopPropagation(); retryDl(item); }; c.append(" ", b); }
    return c;
  }
  // one compact notice per thread instead of a big warning per image
  function failNotice() {
    const t = state.thread, w = $("chatWarn");
    const old = document.getElementById("failNote"); if (old) old.remove();
    if (!t) return;
    const n = (t.attachments || []).filter((a) => a.res_type && (a.no_download || state.failed.has(failKey(a.message_id, a.file_key)))).length;
    if (!n) return;
    const b = el("div", "banner warn compact", "有 " + n + " 个图片/文件无法下载（卡片、转发内容或机器人不在群内等）；原因见各项提示，也可在右侧「已下载」中重试。");
    b.id = "failNote"; b.title = DL_TIP;
    w.appendChild(b);
  }
  const TXT_STYLE = { bold: "b", italic: "i", underline: "u", lineThrough: "s", muted: "muted", code: "ic", tag: "ttag" };
  function styleClasses(e, styles, linkOnly) {
    for (const st of styles || []) {
      let c = TXT_STYLE[st];
      if (linkOnly && (st === "muted" || st === "code" || st === "tag")) c = null;
      if (!c && /^c-[a-z]+$/.test(st)) c = st;
      if (c) e.classList.add(c);
    }
  }
  function renderNodes(parent, nodes, msg, opts) {
    const fwd = !!(opts && opts.noDL);
    for (const n of nodes) {
      switch (n.type) {
        case "text": {
          const s = el("span", null, n.text);
          styleClasses(s, n.style);
          parent.appendChild(s); break;
        }
        case "link": {
          const href = safeHref(n.href);
          if (!href) { parent.appendChild(el("span", null, n.text)); break; }
          const a = el("a", n.is_doc ? "doc" : "", n.text);
          a.href = href; a.target = "_blank"; a.rel = "noopener noreferrer";
          styleClasses(a, n.style, true);
          parent.appendChild(a); break;
        }
        case "at": { const s = el("span", "mention", n.text); styleClasses(s, n.style, true); parent.appendChild(s); break; }
        case "emoji": parent.appendChild(el("span", null, n.text)); break;
        case "image": {
          const key = failKey(msg.id, n.key);
          const it0 = dlItem(msg.id, n.key);
          if (n.no_dl || fwd || state.failed.has(key) || (it0 && it0.status === "failed")) { parent.appendChild(failChip("图片", n.no_dl || fwd, it0)); break; }
          if (!it0 || it0.status !== "done") {
            const c = el("div", "chip"); c.appendChild(el("span", "nm", "🖼️ 图片"));
            const st = el("span", "dlst"); st.dataset.dlk = msg.id + "|" + n.key; c.appendChild(st);
            c.onclick = () => openPreviewFor(msg.id, n.key, "image", "图片", "image");
            parent.appendChild(c); queueMicrotask(updateDlBadges); break;
          }
          const img = el("img", "inline");
          img.loading = "lazy"; img.alt = "图片";
          img.src = resUrlRaw(msg.id, n.key, "image");
          img.onerror = () => { img.replaceWith(failChip("图片", false, dlItem(msg.id, n.key))); markFailed(key); };
          img.onclick = () => openPreviewFor(msg.id, n.key, "image", "图片", "image");
          withFileMenu(img, () => dlItem(msg.id, n.key));
          parent.appendChild(img); break;
        }
        case "media": {
          const itv = dlItem(msg.id, n.key);
          if (n.no_dl || fwd || state.failed.has(failKey(msg.id, n.key)) || (itv && itv.status === "failed")) { parent.appendChild(failChip(n.text || "视频", n.no_dl || fwd, itv)); break; }
          const c = el("div", "video-card");

          c.append(el("div", "play", "▶"), el("div", "cap", n.text || "视频"));
          c.onclick = () => openPreviewFor(msg.id, n.key, "file", n.text || "视频", "video", n.cover);
          withFileMenu(c, () => dlItem(msg.id, n.key));
          parent.appendChild(c); break;
        }
        case "file": {
          const kind = (n.style || []).includes("audio") ? "audio" : "file";
          const c = el("div", "chip");
          c.append(el("span", "ico", fileIcon({ kind, name: n.text })), el("span", "nm", n.text));
          if (n.no_dl || fwd) {
            c.classList.add("nodl"); c.title = NODL_TIP;
            c.append(el("span", "dlst st-err", "⚠ 卡片/转发内，飞书不支持下载"));
          } else {
            c.title = "点击用默认应用打开 · 右键在文件夹中显示或更多操作";
            c.onclick = () => {
              const it = dlItem(msg.id, n.key);
              if (it && it.status === "done" && state.dl.opener) {
                state.preview = null; state.tab = "dl"; renderFiles();
                openLocal(it);
                return;
              }
              openPreviewFor(msg.id, n.key, "file", n.text, kind);
            };
            const st = el("span", "dlst"); st.dataset.dlk = msg.id + "|" + n.key; c.appendChild(st);
            withFileMenu(c, () => dlItem(msg.id, n.key));
            queueMicrotask(updateDlBadges);
          }
          parent.appendChild(c); break;
        }
        case "code": {
          const wrap = el("div", "rt-codewrap");
          if (n.lang) wrap.appendChild(el("div", "rt-codelang", n.lang));
          const pre = el("pre"); const code = el("code", null, n.text);
          pre.appendChild(code); wrap.appendChild(pre); parent.appendChild(wrap); break;
        }
        case "table": {
          const cells = n.cells || [];
          if (!cells.length) break;
          const wrap = el("div", "cb-tablewrap rt-tablewrap"), t = el("table", "cb-table rt-table");
          const thead = el("thead"), hr = el("tr");
          for (const cNodes of cells[0] || []) {
            const th = el("th");
            renderNodes(th, cNodes || [], msg, opts);
            hr.appendChild(th);
          }
          thead.appendChild(hr); t.appendChild(thead);
          if (cells.length > 1) {
            const tbody = el("tbody");
            for (let r = 1; r < cells.length; r++) {
              const tr = el("tr");
              for (const cNodes of cells[r] || []) {
                const td = el("td");
                renderNodes(td, cNodes || [], msg, opts);
                tr.appendChild(td);
              }
              tbody.appendChild(tr);
            }
            t.appendChild(tbody);
          }
          wrap.appendChild(t); parent.appendChild(wrap); break;
        }
        case "hr": parent.appendChild(el("hr")); break;
      }
    }
  }
  const BLOCK_NODES = ["code", "image", "media", "file", "hr", "table"];
  const UL_BULLETS = ["•", "◦", "▪", "▪"];
  function stripListPrefix(para) {
    if (!para || !para.length) return para;
    const first = Object.assign({}, para[0]);
    first.text = (first.text || "").replace(/^\s*(?:(?:[-*+]|•)|\d+[.)])\s+/, "");
    return [first, ...para.slice(1)];
  }
  function renderParas(parent, paras, m, opts) {
    for (const para of paras || []) {
      if (!para.length) { parent.appendChild(el("p", "blank")); continue; }
      if (para.some((n) => BLOCK_NODES.includes(n.type))) { renderNodes(parent, para, m, opts); continue; }
      const n0 = para[0];
      let blk = n0.block || "";
      let indent = n0.indent || 0;
      let marker = n0.marker || "";
      if (!blk && n0.type === "text" && n0.text) {
        const ulM = /^(\s*)(?:[-*+]|•)\s+/.exec(n0.text);
        const olM = !ulM && /^(\s*)(\d+[.)])\s+/.exec(n0.text);
        if (ulM) { blk = "ul"; indent = ulM[1].length >= 2 ? 1 : 0; marker = "•"; }
        else if (olM) { blk = "ol"; indent = olM[1].length >= 2 ? 1 : 0; marker = olM[2]; }
        else if (para.length === 1 && (n0.style || []).includes("bold") && n0.text.length <= 60) { blk = "sec"; }
      }
      if (blk === "ul" || blk === "ol") {
        const lvl = Math.min(Math.max(indent, 0), 3);
        const li = el("div", "rt-li " + blk + " lv-" + lvl);
        const mk = blk === "ul" ? UL_BULLETS[lvl] : (marker || "1.");
        li.appendChild(el("span", "rt-marker", mk));
        const lic = el("div", "rt-lic");
        renderNodes(lic, stripListPrefix(para), m, opts);
        li.appendChild(lic);
        parent.appendChild(li);
        continue;
      }
      if (blk === "quote") {
        let q = parent.lastElementChild;
        if (!q || !q.classList || !q.classList.contains("rt-quote")) {
          q = el("blockquote", "rt-quote");
          parent.appendChild(q);
        }
        const p = el("p");
        const cleanPara = para.map((n, idx) => idx === 0 ? Object.assign({}, n, { style: (n.style || []).filter((s) => s !== "muted") }) : n);
        renderNodes(p, cleanPara, m, opts);
        q.appendChild(p);
        continue;
      }
      if (/^h[1-6]$/.test(blk)) {
        const h = el("div", "rt-heading rt-" + blk);
        renderNodes(h, para, m, opts);
        parent.appendChild(h);
        continue;
      }
      const p = el("p", blk === "sec" ? "rt-sec" : null);
      renderNodes(p, para, m, opts);
      parent.appendChild(p);
    }
  }

  // ----- interactive cards -----
  function renderBlock(parent, b, m, opts) {
    switch (b.type) {
      case "text": { const d = el("div", "cb-text"); renderParas(d, b.paras, m, opts); parent.appendChild(d); break; }
      case "hr": parent.appendChild(el("hr", "cb-hr")); break;
      case "note": { const d = el("div", "cb-note"); renderParas(d, b.paras, m, opts); parent.appendChild(d); break; }
      case "image": {
        const d = el("div", "cb-img");
        const chip = failChip("图片", true);
        if (b.title) chip.firstChild.textContent = "🖼️ " + b.title + " · 图片无法加载";
        d.appendChild(chip); parent.appendChild(d); break;
      }
      case "actions": {
        const d = el("div", "cb-actions");
        for (const bt of b.buttons || []) {
          const href = safeHref(bt.url);
          const x = href ? el("a", "cbtn " + (bt.kind || ""), bt.text) : el("span", "cbtn disabled " + (bt.kind || ""), bt.text);
          if (href) { x.href = href; x.target = "_blank"; x.rel = "noopener noreferrer"; }
          else x.title = "该按钮需要在飞书中操作";
          d.appendChild(x);
        }
        parent.appendChild(d); break;
      }
      case "columns": {
        const d = el("div", "cb-cols");
        d.style.gridTemplateColumns = (b.weights || []).map((w) => (w > 0 ? w + "fr" : "auto")).join(" ");
        for (const col of b.cols || []) { const c = el("div", "cb-col"); for (const x of col) renderBlock(c, x, m, opts); d.appendChild(c); }
        parent.appendChild(d); break;
      }
      case "collapse": {
        const d = el("details", "cb-collapse"); if (b.open) d.open = true;
        d.appendChild(el("summary", null, b.title || "详情"));
        const inner = el("div", "cb-inner"); for (const x of b.children || []) renderBlock(inner, x, m, opts);
        d.appendChild(inner); parent.appendChild(d); break;
      }
      case "group": { const d = el("div", "cb-group"); for (const x of b.children || []) renderBlock(d, x, m, opts); parent.appendChild(d); break; }
      case "fields": {
        const d = el("div", "cb-fields");
        for (const x of b.children || []) { const f = el("div", "cb-field" + (x.short ? " short" : "")); renderParas(f, x.paras, m, opts); d.appendChild(f); }
        parent.appendChild(d); break;
      }
      case "table": {
        const wrap = el("div", "cb-tablewrap"), t = el("table", "cb-table");
        const hdr = b.header || [], rows = b.rows || [];
        // Hide columns that are completely empty across all rows when table has >4 columns
        const keepCols = hdr.map((_, ci) => hdr.length <= 4 || rows.some((r) => r && r[ci] && String(r[ci]).trim() !== ""));
        const thead = el("thead"), hr = el("tr");
        hdr.forEach((h, ci) => { if (keepCols[ci]) hr.appendChild(el("th", null, h)); });
        thead.appendChild(hr); t.appendChild(thead);
        const tbody = el("tbody");
        for (const r of rows) {
          const tr = el("tr");
          (r || []).forEach((c, ci) => { if (keepCols[ci]) tr.appendChild(el("td", null, c)); });
          tbody.appendChild(tr);
        }
        t.appendChild(tbody);
        wrap.appendChild(t); parent.appendChild(wrap); break;
      }
    }
  }
  function renderCard(card, m, opts) {
    const c = el("div", "card t-" + (/^[a-z]+$/.test(card.template || "") ? card.template : "default"));
    if (card.title || card.subtitle || (card.tags || []).length) {
      const h = el("div", "card-head");
      const tl = el("div", "card-title");
      if (card.title) tl.appendChild(el("span", null, card.title));
      for (const tg of card.tags || []) tl.appendChild(el("span", "ttag", tg));
      h.appendChild(tl);
      if (card.subtitle) h.appendChild(el("div", "card-sub", card.subtitle));
      c.appendChild(h);
    }
    if ((card.blocks || []).length) {
      const body = el("div", "card-body");
      for (const b of card.blocks) renderBlock(body, b, m, opts);
      c.appendChild(body);
    }
    return c;
  }

  // body of a message: title, text, card, merged-forward, notices
  function renderBody(bubble, m, opts) {
    if (m.title) bubble.appendChild(el("div", "ptitle", m.title));
    if (m.card) {
      bubble.appendChild(renderCard(m.card, m, opts));
    } else if (!(m.forwarded || []).length) {
      renderParas(bubble, m.paragraphs, m, opts);
    }
    if (m.notice === "card_unavailable") {
      const d = el("div", "cardmiss", "该卡片的内容飞书接口没有返回（模板/卡片实体发送的卡片常见），无法在此显示。");
      const l = openLink(); if (l) d.append(" ", l);
      bubble.appendChild(d);
    } else if (m.notice === "forward_failed") {
      const d = el("div", "cardmiss", "无法展开合并转发的内容（接口读取失败）。");
      const l = openLink(); if (l) d.append(" ", l);
      bubble.appendChild(d);
    }
    if ((m.forwarded || []).length) bubble.appendChild(renderForward(m, opts));
  }
  function renderForward(m, opts) {
    const q = el("div", "fwd");
    q.appendChild(el("div", "fwd-head", "合并转发的聊天记录 · " + m.forwarded.length + " 条"));
    for (const f of m.forwarded) {
      const it = el("div", "fwd-item");
      const h = el("div", "fwd-meta");
      h.appendChild(el("span", "name", f.sender_name));
      h.appendChild(el("span", null, fmtTime(f.time, true)));
      it.appendChild(h);
      const body = el("div", "fwd-body");
      renderBody(body, f, { noDL: true });
      it.appendChild(body);
      q.appendChild(it);
    }
    return q;
  }
  function renderMessage(m) {
    if (m.notice === "system") {
      const s = el("div", "sysmsg");
      s.id = "m-" + m.id;
      s.textContent = (m.plain || "系统消息");
      s.title = fmtTime(m.time, true);
      return s;
    }
    const row = el("div", "msg" + (m.is_root ? " root" : ""));
    row.id = "m-" + m.id;
    row.appendChild(avatar(m.sender_name, m.sender_avatar, false, m.sender_type === "app"));
    const body = el("div", "msg-body");
    const head = el("div", "msg-head");
    head.appendChild(el("span", "name", m.sender_name));
    if (m.sender_type === "app") head.appendChild(el("span", "tag", "BOT"));
    if (m.is_root) head.appendChild(el("span", "tag root", "话题发起"));
    const tm = el("span", null, fmtTime(m.time)); tm.title = fmtTime(m.time, true);
    head.appendChild(tm);
    if (m.edited) head.appendChild(el("span", null, "已编辑"));
    if (m.pending) head.appendChild(el("span", "pendtag", "发送中…"));
    body.appendChild(head);
    const bubble = el("div", "bubble" + (m.card && !(m.paragraphs || []).length ? " cardbubble" : "") + ((m.forwarded || []).length ? " fwdbubble" : ""));
    renderBody(bubble, m, null);
    body.appendChild(bubble);
    row.appendChild(body);
    return row;
  }
  function appendMessages(box, msgs) {
    let lastDay = box.dataset.lastDay || "";
    for (const m of msgs) {
      const d = new Date(m.time).toDateString();
      if (d !== lastDay) { lastDay = d; const s = el("div", "day"); s.appendChild(el("span", null, dayLabel(m.time))); box.appendChild(s); }
      box.appendChild(renderMessage(m));
    }
    box.dataset.lastDay = lastDay;
  }
  function renderQueuedMessage(qItem, idx, t) {
    const me = state.auth.user || {};
    const row = el("div", "msg queued-msg");
    row.id = "q-" + qItem.id;
    row.appendChild(avatar(me.name || "我", me.avatar_url || "", false, false));
    const body = el("div", "msg-body");
    const head = el("div", "msg-head");
    head.appendChild(el("span", "name", me.name || "我"));
    if (qItem.status === "failed") {
      head.appendChild(el("span", "tag qtag err", "⚠ 发送失败"));
    } else if (qItem.status === "sending") {
      head.appendChild(el("span", "tag qtag run", "🚀 正在发出…"));
    } else {
      head.appendChild(el("span", "tag qtag", "⏳ 排队中 #" + (idx + 1)));
    }
    const tm = el("span", null, fmtTime(qItem.created_at));
    tm.title = fmtTime(qItem.created_at, true);
    head.appendChild(tm);
    body.appendChild(head);

    const bubble = el("div", "bubble qbubble" + (qItem.status === "failed" ? " err" : ""));
    if (qItem.text) {
      renderParas(bubble, localParas(qItem.text, qItem.mentions || []), qItem, null);
    }
    for (const a of qItem.attachments || []) {
      bubble.appendChild(el("div", "lfile", (a.kind === "image" ? "🖼️ " : fileIcon({ name: a.name }) + " ") + a.name + " (" + fmtSize(a.size) + ")"));
    }
    if ((qItem.attachments || []).length) {
      bubble.appendChild(el("div", "lupl", qItem.encrypt === false ? "附件不加密，发出后当前群可阅读" : "附件加密，仅你和呆呆鸟可阅读"));
    }

    const foot = el("div", "qfoot");
    const st = el("div", "qstatus");
    if (qItem.status === "failed") {
      st.textContent = "自动发送失败：" + (qItem.error || "未知错误");
    } else if (qItem.status === "sending") {
      st.append(el("span", "spin-dot"), "呆呆鸟任务已结束，正在自动发送…");
    } else {
      st.append(el("span", "spin-dot"), "等待呆呆鸟当前任务结束后自动发送" + (idx > 0 ? "（前方还有 " + idx + " 条排队）" : ""));
    }
    foot.appendChild(st);

    const acts = el("div", "qacts");
    const btnNow = el("button", "qbtn primary", qItem.status === "failed" ? "重试发送" : "立即发送");
    btnNow.type = "button";
    btnNow.disabled = qItem.status === "sending";
    btnNow.title = "不等待呆呆鸟当前任务结束，立即发送到话题";
    btnNow.onclick = () => sendQueuedNow(t.topic.thread_id, qItem.id, btnNow);

    const btnEdit = el("button", "qbtn", "撤回编辑");
    btnEdit.type = "button";
    btnEdit.disabled = qItem.status === "sending";
    btnEdit.title = "从队列撤回并放回底部输入框重新编辑";
    btnEdit.onclick = () => reeditQueued(t.topic.thread_id, qItem, btnEdit);

    const btnCancel = el("button", "qbtn danger", "取消");
    btnCancel.type = "button";
    btnCancel.disabled = qItem.status === "sending";
    btnCancel.title = "取消并删除这条排队消息";
    btnCancel.onclick = () => cancelQueued(t.topic.thread_id, qItem.id, btnCancel);

    acts.append(btnNow, btnEdit, btnCancel);
    foot.appendChild(acts);
    bubble.appendChild(foot);
    body.appendChild(bubble);
    row.appendChild(body);
    return row;
  }
  function appendQueuedMessages(box, queue, t) {
    if (!queue || !queue.length) return;
    const sep = el("div", "day qday");
    sep.appendChild(el("span", null, "⏳ 排队待发送 (" + queue.length + ")"));
    box.appendChild(sep);
    queue.forEach((qItem, idx) => {
      box.appendChild(renderQueuedMessage(qItem, idx, t));
    });
  }
  async function sendQueuedNow(threadId, queueId, btn) {
    if (btn) { btn.disabled = true; btn.textContent = "发送中…"; }
    try {
      const r = await api("/api/topics/" + encodeURIComponent(threadId) + "/queue/" + encodeURIComponent(queueId) + "/send", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: "{}",
      });
      const msgs = r.messages && r.messages.length ? r.messages : (r.message ? [r.message] : []);
      if (state.thread && state.thread.topic.thread_id === threadId) {
        state.thread.queue = r.queue || [];
        for (const m of msgs) if (m && !state.thread.messages.some((x) => x.id === m.id)) state.thread.messages.push(m);
        state.thread.topic.reply_count = Math.max(0, state.thread.topic.reply_count) + msgs.length;
        renderThread("bottom");
      }
      if (msgs.length) bumpTopic(threadId, msgs.length, msgs[msgs.length - 1].time);
      toast("已立即发送", false);
      if (state.thread) refreshAfterReply(r.thread_id || threadId, msgs, state.thread.topic.chat_id);
    } catch (e) {
      if (btn) { btn.disabled = false; btn.textContent = "立即发送"; }
      toast("立即发送失败：" + (e.message || e), true);
    }
  }
  async function reeditQueued(threadId, qItem, btn) {
    if (btn) btn.disabled = true;
    try {
      const r = await api("/api/topics/" + encodeURIComponent(threadId) + "/queue/" + encodeURIComponent(qItem.id) + "?reedit=1", {
        method: "DELETE", headers: { "Content-Type": "application/json" }, body: "{}",
      });
      if (state.thread && state.thread.topic.thread_id === threadId) {
        state.thread.queue = r.queue || [];
        const input = $("replyInput");
        const cur = input.value.trim();
        input.value = cur && qItem.text ? (qItem.text + "\n" + cur) : (qItem.text || cur);
        state.drafts[threadId] = input.value;
        saveDrafts();
        for (const m of qItem.mentions || []) {
          if (!state.mentions.some((x) => x.id === m.id)) state.mentions.push(m);
        }
        if (qItem.attachments && qItem.attachments.length) {
          replyTray.restoreStaged(qItem.attachments);
        }
        renderThread("keep", $("messages").scrollTop);
        autosize();
        input.focus();
      }
      toast("已撤回至输入框，可修改后重新发送", false);
    } catch (e) {
      if (btn) btn.disabled = false;
      toast("撤回失败：" + (e.message || e), true);
    }
  }
  async function cancelQueued(threadId, queueId, btn) {
    if (btn) btn.disabled = true;
    try {
      const r = await api("/api/topics/" + encodeURIComponent(threadId) + "/queue/" + encodeURIComponent(queueId), {
        method: "DELETE", headers: { "Content-Type": "application/json" }, body: "{}",
      });
      if (state.thread && state.thread.topic.thread_id === threadId) {
        state.thread.queue = r.queue || [];
        renderThread("keep", $("messages").scrollTop);
      }
      toast("已取消排队消息", false);
    } catch (e) {
      if (btn) btn.disabled = false;
      toast("取消失败：" + (e.message || e), true);
    }
  }
  function renderThread(scroll, keepTop) {
    const box = $("messages"), t = state.thread;
    box.textContent = ""; box.dataset.lastDay = ""; $("chatWarn").textContent = "";
    renderComposer();
    if (!t) {
      $("chatHead").hidden = true;
      renderGroupButton();
      if (state.status && !state.auth.logged_in && state.loadedSelection) { loginPrompt(box, "登录后查看话题对话"); return; }
      const e = el("div", "empty"); e.append(el("div", "empty-ico", "💬"), el("div", null, "从左侧选择一个话题查看对话"));
      box.appendChild(e); return;
    }
    $("chatHead").hidden = false;
    $("chatTitle").textContent = t.topic.title;
    $("chatTitle").title = t.topic.title;
    const qCount = (t.queue || []).length;
    $("chatSub").textContent = t.topic.chat_name + " · " + t.messages.length + " 条消息" + (qCount ? " · " + qCount + " 条排队中" : "") + " · 发起人 " + t.topic.sender_name;
    renderGroupButton();
    (t.warnings || []).forEach((w) => $("chatWarn").appendChild(banner("warn", w)));
    failNotice();
    if (t.topic.pending) $("chatWarn").appendChild(el("div", "pendnote", "这是你刚发出的一条消息：在飞书里它还不是话题，发送第一条回复后才会创建话题。"));
    appendMessages(box, t.messages);
    appendQueuedMessages(box, t.queue, t);
    if (scroll === "keep") {
      box.scrollTop = keepTop || 0;
    } else if (scroll === "top") {
      box.scrollTop = 0;
    } else {
      box.scrollTop = box.scrollHeight;
      requestAnimationFrame(() => { box.scrollTop = box.scrollHeight; });
    }
  }
  function nearBottom(box) {
    return box.scrollHeight - box.scrollTop - box.clientHeight < 48;
  }
  function threadSig(t) {
    if (!t || !t.topic) return "";
    const msgs = (t.messages || []).map((m) => [m.id, m.edited ? 1 : 0, m.deleted ? 1 : 0, m.time, m.plain || "", m.notice || "", (m.forwarded || []).length].join("\u0002")).join("\u0001");
    const att = (t.attachments || []).map((a) => a.id + ":" + (a.name || "")).join(",");
    const qSig = (t.queue || []).map((q) => [q.id, q.status, q.error || ""].join(":")).join(",");
    const fs = t.file_sync || {};
    return [t.topic.thread_id, t.topic.title, t.topic.pending ? 1 : 0, t.topic.reply_count, (t.warnings || []).join("\u0001"), msgs, att, t.bot_busy ? 1 : 0, t.bot_status || "", qSig, fs.status || "", fs.names_loading ? 1 : 0, fs.phase || "", fs.last_checked || 0, (fs.warnings || []).join("\u0001")].join("\u0000");
  }

  // ---------- attachments (shared by reply composer and new-topic dialog) ----------
  const fmtSize = (n) => n >= 1048576 ? (n / 1048576).toFixed(1) + " MB" : Math.max(1, Math.round(n / 1024)) + " KB";
  const uploadsAllowed = () => state.auth.upload_enabled !== false && state.auth.reply_enabled !== false;
  const isImageFile = (f) => /^image\//.test(f.type) && f.type !== "image/svg+xml" || /\.(png|jpe?g|gif|webp|bmp|ico|tiff?|heic)$/i.test(f.name || "");
  function limits() { return Object.assign({ image: 10485760, file: 31457280, drive: 20971520, max_attachments: 9 }, state.auth.limits || {}); }
  function uploadScopeErr(e) {
    return isScopeErr(e) && ((e.scopes || []).length ? e.scopes.every((x) => /^(im:resource|drive:|docs:)/.test(x)) : /^附件|上传/.test(e.message || ""));
  }
  // createTray: chips with thumbnail / progress; files are staged on the local
  // server as soon as they are added and sent with the message.
  function createTray(box, fileInput, attachBtn, dropEl, textarea, onChange) {
    const tray = { items: [], note: null };
    let seq = 0;
    const L = limits;
    function render() {
      box.textContent = "";
      for (const it of tray.items) {
        const c = el("div", "chip" + (it.status === "error" ? " err" : ""));
        if (it.url) { const im = el("img"); im.src = it.url; im.alt = ""; c.appendChild(im); }
        else c.appendChild(el("div", "ico", it.kind === "image" ? "🖼️" : fileIcon({ name: it.name })));
        const cn = el("div", "cn");
        const n = el("div", "n", it.name); n.title = it.name;
        cn.appendChild(n);
        cn.appendChild(el("div", "s", it.status === "error" ? it.err : it.status === "uploading" ? "读取中 " + Math.round(it.progress * 100) + "%" : fmtSize(it.size) + (it.note ? " · " + it.note : "")));
        c.appendChild(cn);
        const x = el("button", "x", "✕"); x.type = "button"; x.title = "移除";
        x.onclick = () => tray.remove(it);
        c.appendChild(x);
        if (it.status === "uploading") { const bar = el("div", "bar"); bar.style.width = Math.round(it.progress * 100) + "%"; c.appendChild(bar); }
        box.appendChild(c);
      }
      if (onChange) onChange();
    }
    function fail(it, msg) { it.status = "error"; it.err = msg; render(); }
    function start(it) {
      const xhr = new XMLHttpRequest();
      it.xhr = xhr;
      xhr.open("POST", "/api/upload");
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      xhr.setRequestHeader("X-File-Name", encodeURIComponent(it.name));
      xhr.upload.onprogress = (ev) => { if (ev.lengthComputable) { it.progress = ev.loaded / ev.total; render(); } };
      xhr.onerror = () => fail(it, "上传到本地服务失败");
      xhr.onload = () => {
        let body = null; try { body = JSON.parse(xhr.responseText); } catch (_) { /* ignore */ }
        if (xhr.status === 200 && body && body.upload_id) {
          it.status = "ready"; it.uploadId = body.upload_id; it.kind = body.kind; it.note = body.note || "";
          if (it.kind !== "image" && it.url) { URL.revokeObjectURL(it.url); it.url = null; }
          render();
        } else {
          if (body && body.kind === "login_required") onLoggedOut();
          fail(it, (body && body.error) || ("HTTP " + xhr.status));
        }
      };
      xhr.send(it.file);
    }
    tray.add = (files) => {
      if (!uploadsAllowed()) return;
      for (const f of Array.from(files)) {
        if (tray.items.length >= L().max_attachments) { setNote("一次最多 " + L().max_attachments + " 个附件"); break; }
        let name = f.name || "";
        if (!name || /^image\.(png|jpe?g|gif|webp)$/i.test(name)) {
          const d = new Date();
          name = (f.type.startsWith("image/") ? "图片-" : "文件-") + d.getFullYear() + pad(d.getMonth() + 1) + pad(d.getDate()) + "-" + pad(d.getHours()) + pad(d.getMinutes()) + pad(d.getSeconds()) + (++seq) + "." + ((f.type.split("/")[1] || "bin").replace("jpeg", "jpg").replace(/[^a-z0-9]/gi, ""));
        }
        const it = { file: f, name, size: f.size, kind: isImageFile(f) && f.size <= L().image ? "image" : "file", status: "uploading", progress: 0, url: null, note: "" };
        tray.items.push(it);
        if (f.size === 0) { fail(it, "空文件无法发送"); continue; }
        if (f.size > L().file) { fail(it, "超过 30 MB（飞书限制）"); continue; }
        if ((state.auth.upload_mode === "drive") && f.size > L().drive) { fail(it, "云空间上传最大 " + fmtSize(L().drive)); continue; }
        if (it.kind === "image") it.url = URL.createObjectURL(f);
        else if (isImageFile(f)) it.note = "超过 10 MB，作为文件发送";
        start(it);
      }
      render();
    };
    function setNote(t) { tray.note = t; if (onChange) onChange(); }
    tray.remove = (it) => {
      if (it.xhr && it.status === "uploading") it.xhr.abort();
      if (it.uploadId) fetch("/api/upload/" + it.uploadId, { method: "DELETE", headers: { "Content-Type": "application/json" }, body: "{}" }).catch(() => {});
      if (it.url) URL.revokeObjectURL(it.url);
      tray.items = tray.items.filter((x) => x !== it);
      render();
    };
    tray.reset = (deleteStaged) => { for (const it of tray.items.slice()) { if (deleteStaged) tray.remove(it); } if (!deleteStaged) { for (const it of tray.items) if (it.url) URL.revokeObjectURL(it.url); tray.items = []; } tray.note = null; render(); };
    tray.restoreStaged = (atts) => {
      for (const a of atts || []) {
        if (!a || !a.upload_id || tray.items.some((x) => x.uploadId === a.upload_id)) continue;
        tray.items.push({
          name: a.name || "附件",
          size: a.size || 0,
          kind: a.kind || "file",
          status: "ready",
          progress: 1,
          url: null,
          note: "已从排队撤回",
          uploadId: a.upload_id,
        });
      }
      render();
    };
    tray.uploading = () => tray.items.some((i) => i.status === "uploading");
    tray.ready = () => tray.items.filter((i) => i.status === "ready");
    tray.ids = () => tray.ready().map((i) => i.uploadId);
    attachBtn.onclick = () => {
      if (state.auth.can_upload === false) { setNote("当前登录授权不含上传权限，请重新登录授权。"); if (onChange) onChange(); return; }
      fileInput.click();
    };
    fileInput.onchange = () => { tray.add(fileInput.files); fileInput.value = ""; };
    textarea.addEventListener("paste", (ev) => {
      const fs = ev.clipboardData && ev.clipboardData.files;
      if (fs && fs.length) { ev.preventDefault(); tray.add(fs); }
    });
    let depth = 0;
    const over = (ev) => { if (ev.dataTransfer && Array.from(ev.dataTransfer.types || []).includes("Files")) { ev.preventDefault(); dropEl.classList.add("drop"); } };
    dropEl.addEventListener("dragenter", (ev) => { depth++; over(ev); });
    dropEl.addEventListener("dragover", over);
    dropEl.addEventListener("dragleave", () => { if (--depth <= 0) { depth = 0; dropEl.classList.remove("drop"); } });
    dropEl.addEventListener("drop", (ev) => {
      depth = 0; dropEl.classList.remove("drop");
      if (ev.dataTransfer && ev.dataTransfer.files && ev.dataTransfer.files.length) { ev.preventDefault(); tray.add(ev.dataTransfer.files); }
    });
    return tray;
  }
  // @ picker shared by both composers
  function createMentionPicker(pop, input, getList, mentions) {
    const api_ = {
      close() { pop.hidden = true; },
      open(typedAt) {
        const list = getList();
        pop.textContent = "";
        if (!list.length) pop.appendChild(el("div", "empty small", "这里还没有其他成员发过言"));
        for (const p of list) {
          const b = el("button", "mi", p.name); b.type = "button";
          b.onmousedown = (ev) => ev.preventDefault();
          b.onclick = () => api_.insert(p, typedAt);
          pop.appendChild(b);
        }
        pop.hidden = false;
      },
      toggle() { if (pop.hidden) api_.open(false); else api_.close(); },
      insert(p, typedAt) {
        const pos = input.selectionStart == null ? input.value.length : input.selectionStart;
        let before = input.value.slice(0, pos);
        if (typedAt && before.endsWith("@")) before = before.slice(0, -1);
        const ins = "@" + p.name + " ";
        input.value = before + ins + input.value.slice(pos);
        input.focus(); input.selectionStart = input.selectionEnd = before.length + ins.length;
        if (!mentions.some((m) => m.id === p.id)) mentions.push(p);
        api_.close(); input.dispatchEvent(new Event("input"));
      },
    };
    input.addEventListener("input", (ev) => { if (ev.inputType === "insertText" && ev.data === "@") api_.open(true); });
    return api_;
  }

  // ---------- reply composer ----------
  function saveDrafts() { try { sessionStorage.setItem("drafts", JSON.stringify(state.drafts)); } catch (_) { /* ignore */ } }
  function autosize() {
    const i = $("replyInput");
    i.style.height = "auto";
    i.style.height = Math.min(i.scrollHeight, 180) + "px";
  }
  function setReplyErr(e, kind) {
    const box = $("replyErr"); box.textContent = "";
    if (!e) return;
    if (typeof e === "string") { box.appendChild(banner(kind || "warn", e)); return; }
    box.appendChild(banner("err", "发送失败：" + e.message, e.hint, e));
  }
  function isTopicQueuedOrBusy(t) {
    return !!(t && (t.bot_busy || (t.queue && t.queue.length)));
  }
  function updateReplyHint() {
    const t = state.thread, up = uploadsAllowed(), qBusy = isTopicQueuedOrBusy(t);
    $("btnAttach").hidden = !up;
    $("replyHint").textContent = (t && t.topic.pending ? "发送第一条回复后飞书才会创建话题 · " : "") +
      (qBusy ? "呆呆鸟正在运行任务，现在发送会自动排队等待上一任务结束 · " : "以你本人的身份回复到该话题 · ") +
      (up ? "可粘贴/拖拽图片与文件" : "") +
      (replyTray && replyTray.note ? " · ⚠ " + replyTray.note : "");
  }
  function renderComposer() {
    const f = $("composer"), t = state.thread;
    const ok = !!(t && state.auth.logged_in && state.auth.reply_enabled !== false);
    f.hidden = !ok;
    if (!ok) { replyMention.close(); return; }
    const can = state.auth.can_reply !== false;
    const note = $("composerNotice"); note.textContent = "";
    if (!can) {
      const e = state.scopeErr;
      const b = el("div", "banner err");
      b.appendChild(el("div", null, "需要重新登录授权：当前登录授权不包含「发送消息」权限（im:message、im:message.send_as_user），重新登录后才能回复。"));
      if (e && e.hint) b.appendChild(el("div", "hint", e.hint));
      b.appendChild(scopeActions(e || null));
      note.appendChild(b);
    } else if (isTopicQueuedOrBusy(t)) {
      const qLen = (t.queue && t.queue.length) || 0;
      const bar = el("div", "bot-busy-bar");
      bar.appendChild(el("span", "spin-dot"));
      const txt = el("span", "bb-txt");
      txt.textContent = (t.bot_status || "🤖 呆呆鸟正在运行任务中") + (qLen ? " · 已有 " + qLen + " 条消息在排队" : " · 当前发送将先排队，任务结束后自动发出");
      bar.appendChild(txt);
      note.appendChild(bar);
    }
    const i = $("replyInput");
    if (state.composerThread !== t.topic.thread_id) {
      state.composerThread = t.topic.thread_id;
      i.value = state.drafts[t.topic.thread_id] || "";
      state.mentions.length = 0; setReplyErr(null); replyMention.close();
      replyTray.reset(true);
    }
    i.disabled = !can;
    const qBusy = isTopicQueuedOrBusy(t);
    const sendBtn = $("btnSend");
    sendBtn.disabled = !can || state.sending || replyTray.uploading();
    sendBtn.textContent = qBusy ? "排队发送" : "发送";
    sendBtn.classList.toggle("qmode", qBusy);
    sendBtn.title = qBusy ? "呆呆鸟正在执行任务，点击将先进入排队；按住 Option/Alt 点击可立即发送" : "发送到飞书话题";
    $("btnMention").disabled = !can;
    $("btnAttach").disabled = !can || state.sending;
    i.placeholder = (t.topic.pending ? "回复以创建话题…" : (qBusy ? "输入下一条指令（将自动排队，待当前任务结束后发出）…" : "回复话题…")) + "（Enter 发送，Shift+Enter 换行）";
    updateReplyHint();
    autosize();
  }
  // optimistic rendering of what the user typed
  function localParas(text, mentions) {
    const names = mentions.map((m) => m.name).filter(Boolean).sort((a, b) => b.length - a.length);
    const esc = (x) => x.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const re = names.length ? new RegExp("(@(?:" + names.map(esc).join("|") + "))", "g") : null;
    return text.split("\n").map((ln) => {
      if (!ln.trim()) return [];
      if (!re) return [{ type: "text", text: ln }];
      return ln.split(re).filter(Boolean).map((p) => (p.startsWith("@") && names.includes(p.slice(1)) ? { type: "at", text: p } : { type: "text", text: p }));
    });
  }
  // local preview of attachments inside an optimistic bubble
  function localExtras(bubble, items) {
    for (const it of items) {
      if (it.url) { const im = el("img", "lthumb"); im.src = it.url; im.alt = it.name; bubble.appendChild(im); }
      else bubble.appendChild(el("div", "lfile", fileIcon({ name: it.name }) + " " + it.name + " (" + fmtSize(it.size) + ")"));
    }
    if (items.length) bubble.appendChild(el("div", "lupl", "正在上传并发送…"));
  }
  function bumpTopic(threadId, n, time) {
    for (const list of Object.values(state.byChat)) {
      const tp = list.find((x) => x.thread_id === threadId);
      if (tp) { tp.reply_count = Math.max(0, tp.reply_count) + n; tp.last_time = Math.max(tp.last_time, time); }
    }
    rebuildTopics(); renderList();
  }
  async function reloadChat(chatId) {
    if (chatId && state.selection.includes(chatId)) {
      try {
        const r = await api("/api/topics?chat_id=" + encodeURIComponent(chatId));
        state.byChat[chatId] = r.topics || []; rebuildTopics(); renderList();
      } catch (_) { /* ignore */ }
    }
  }
  async function refreshAfterReply(threadId, msgs, chatId) {
    try {
      const t = await api("/api/topics/" + encodeURIComponent(threadId) + "/messages?refresh=1");
      for (const msg of msgs) if (!t.messages.some((x) => x.id === msg.id)) t.messages.push(msg);
      t.messages.sort((a, b) => a.time - b.time);
      if (state.active === threadId && !state.sending) { state.thread = t; setDownloads(t.downloads); renderThread("bottom"); renderFiles(); }
    } catch (_) { /* keep the locally reconciled view */ }
    await reloadChat(chatId);
  }
  // ---------- encrypt confirmation dialog ----------
  let encryptResolve = null;
  let encryptKeyHandler = null;
  function askEncrypt() {
    return new Promise((resolve) => {
      encryptResolve = resolve;
      $("encryptYes").classList.add("selected");
      $("encryptNo").classList.remove("selected");
      $("encryptModal").hidden = false;
      // Delay attaching key handler to prevent the Enter key that triggered the send
      // from immediately confirming this dialog in the same bubbling cycle.
      setTimeout(() => {
        if ($("encryptModal").hidden) return;
        encryptKeyHandler = (e) => {
          if ($("encryptModal").hidden) return;
          if (e.key === "Escape") {
            e.preventDefault();
            e.stopPropagation();
            closeEncryptModal(null);
          } else if (e.key === "Enter" && !e.isComposing) {
            e.preventDefault();
            e.stopPropagation();
            closeEncryptModal($("encryptYes").classList.contains("selected"));
          } else if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
            e.preventDefault();
            $("encryptYes").classList.add("selected");
            $("encryptNo").classList.remove("selected");
          } else if (e.key === "ArrowRight" || e.key === "ArrowDown") {
            e.preventDefault();
            $("encryptNo").classList.add("selected");
            $("encryptYes").classList.remove("selected");
          }
        };
        window.addEventListener("keydown", encryptKeyHandler, true);
      }, 50);
    });
  }
  function closeEncryptModal(result) {
    $("encryptModal").hidden = true;
    if (encryptKeyHandler) {
      window.removeEventListener("keydown", encryptKeyHandler, true);
      encryptKeyHandler = null;
    }
    if (encryptResolve) { const r = encryptResolve; encryptResolve = null; r(result); }
  }
  $("encryptYes").onclick = () => { $("encryptYes").classList.add("selected"); $("encryptNo").classList.remove("selected"); };
  $("encryptNo").onclick = () => { $("encryptNo").classList.add("selected"); $("encryptYes").classList.remove("selected"); };
  $("encryptConfirm").onclick = () => closeEncryptModal($("encryptYes").classList.contains("selected"));
  $("encryptCancel").onclick = () => closeEncryptModal(null);
  $("encryptModal").addEventListener("click", (e) => { if (e.target === $("encryptModal")) closeEncryptModal(null); });

  const newUUID = () => (crypto.randomUUID ? crypto.randomUUID() : Date.now() + "-" + Math.random().toString(16).slice(2)).slice(0, 50);
  async function sendReply(forceImmediate) {
    const t = state.thread;
    if (!t || state.sending) return;
    const input = $("replyInput");
    const text = input.value.replace(/\s+$/, "");
    const items = replyTray.ready();
    if (replyTray.uploading()) { updateReplyHint(); return; }
    if (!text.trim() && !items.length) return;
    if (state.auth.can_reply === false) { renderComposer(); return; }
    let encrypt = true;
    if (items.length) {
      const choice = await askEncrypt();
      if (choice === null) return; // cancelled
      encrypt = choice;
    }
    let threadId = t.topic.thread_id;
    const chatId = t.topic.chat_id;
    const mentions = state.mentions.filter((m) => text.includes("@" + m.name));
    const uuid = newUUID();
    state.viewGen++;
    state.sending = true; $("btnSend").disabled = true; setReplyErr(null); replyMention.close();
    const me = state.auth.user || {};
    const pending = {
      id: "local-" + uuid, sender_id: me.open_id || "", sender_name: me.name || "我", sender_avatar: me.avatar_url || "", sender_type: "user",
      time: Date.now(), msg_type: "text", paragraphs: localParas(text, mentions), plain: text, pending: true,
    };
    const box = $("messages");
    appendMessages(box, [pending]);
    const pendRow = box.lastElementChild;
    pendRow.classList.add("pending");
    const pb = pendRow.querySelector(".bubble"); if (pb) localExtras(pb, items);
    box.scrollTop = box.scrollHeight;
    const draftKey = threadId;
    input.value = ""; delete state.drafts[threadId]; saveDrafts(); autosize();
    try {
      const r = await api("/api/topics/" + encodeURIComponent(threadId) + "/reply", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text, mentions, uuid, attachments: items.map((i) => i.uploadId), force: !!forceImmediate, encrypt }),
      });
      state.mentions.length = 0;
      replyTray.reset(false);
      markTopicRead(threadId, Date.now());
      if (r.queued) {
        if (pendRow && pendRow.isConnected) pendRow.remove();
        state.sending = false;
        if (state.active === threadId && state.thread) {
          state.thread.queue = r.queue || (r.queue_item ? [r.queue_item] : []);
          state.thread.bot_busy = true;
          if (r.bot_status) state.thread.bot_status = r.bot_status;
          renderThread("bottom");
        } else {
          renderComposer();
        }
        toast("⏳ 呆呆鸟正在运行任务，消息已加入排队，任务结束后将自动发出", false);
        scheduleAutoRefresh(2500);
        return;
      }
      const msgs = r.messages && r.messages.length ? r.messages : [r.message];
      // a pending topic became a real thread with this reply
      if (r.thread_id && r.thread_id !== threadId && !r.thread_id.startsWith("new_")) {
        for (const list of Object.values(state.byChat)) for (const tp of list) if (tp.thread_id === threadId) { tp.thread_id = r.thread_id; tp.pending = false; }
        if (state.active === threadId && state.thread) {
          state.active = r.thread_id; state.thread.topic.thread_id = r.thread_id; state.thread.topic.pending = false;
          state.composerThread = r.thread_id;
          history.replaceState(null, "", "#t=" + encodeURIComponent(r.thread_id));
          $("chatWarn").querySelectorAll(".pendnote").forEach((n) => n.remove());
        }
        threadId = r.thread_id;
      }
      const last = msgs[msgs.length - 1];
      if (state.active === threadId && state.thread && state.thread.topic.thread_id === threadId) {
        pendRow.classList.remove("pending");
        const frag = document.createDocumentFragment();
        for (const m of msgs) frag.appendChild(renderMessage(m));
        pendRow.replaceWith(frag);
        for (const m of msgs) if (!state.thread.messages.some((x) => x.id === m.id)) state.thread.messages.push(m);
        state.thread.topic.reply_count = Math.max(0, state.thread.topic.reply_count) + msgs.length;
        $("chatSub").textContent = state.thread.topic.chat_name + " · " + state.thread.messages.length + " 条消息" + " · 发起人 " + state.thread.topic.sender_name;
        box.scrollTop = box.scrollHeight;
      }
      bumpTopic(threadId, msgs.length, last.time);
      state.sending = false; renderComposer();
      if ((r.warnings || []).length) setReplyErr(r.warnings.join("\n"), "warn");
      refreshAfterReply(threadId, msgs, chatId);
    } catch (e) {
      if (pendRow && pendRow.isConnected) pendRow.remove();
      // keep what was typed (and the attachments) so it can be sent again
      state.drafts[draftKey] = text; saveDrafts();
      state.sending = false;
      if (uploadScopeErr(e)) { state.auth.can_upload = false; state.uploadScopeErr = e; }
      else if (isScopeErr(e)) { state.auth.can_reply = false; state.scopeErr = e; }
      if (state.active === draftKey) {
        input.value = text; autosize(); renderComposer();
        if (!isScopeErr(e) || uploadScopeErr(e)) setReplyErr(e); // missing-send-scope is explained by the re-login notice
      }
    }
  }
  // @ picker (reply)
  function participants() {
    const me = (state.auth.user && state.auth.user.open_id) || "", seen = new Map();
    for (const m of (state.thread && state.thread.messages) || []) {
      if (m.sender_type === "user" && m.sender_id && m.sender_id !== me && !seen.has(m.sender_id)) seen.set(m.sender_id, m.sender_name);
    }
    return Array.from(seen, ([id, name]) => ({ id, name }));
  }
  const replyMention = createMentionPicker($("mentionPop"), $("replyInput"), participants, state.mentions);
  const replyTray = createTray($("replyTray"), $("replyFile"), $("btnAttach"), $("composer"), $("replyInput"), () => { if (!$("composer").hidden) { $("btnSend").disabled = state.auth.can_reply === false || state.sending || replyTray.uploading(); updateReplyHint(); } });
  $("replyInput").addEventListener("input", () => {
    const t = state.thread; if (t) { state.drafts[t.topic.thread_id] = $("replyInput").value; saveDrafts(); }
    autosize();
  });
  $("replyInput").addEventListener("keydown", (e) => {
    if (e.key === "Escape") { replyMention.close(); return; }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      e.stopPropagation();
      sendReply(e.altKey);
    }
  });
  $("btnSend").addEventListener("click", (e) => {
    if (e.altKey) { e.preventDefault(); sendReply(true); }
  });
  $("composer").addEventListener("submit", (e) => { e.preventDefault(); sendReply(false); });
  $("btnMention").onclick = () => replyMention.toggle();
  document.addEventListener("mousedown", (e) => { if (!e.target.closest("#mentionPop, #btnMention")) replyMention.close(); });

  // ---------- new topic ----------
  const nt = { mentions: [], sending: false, replyTo: null, recentFor: null };
  function ntChats() {
    let chats = state.chats;
    const hasDaidainiao = chats.some((c) => c.name && c.name.includes("呆呆鸟"));
    if (hasDaidainiao) {
      chats = chats.filter((c) => c.name && c.name.includes("呆呆鸟"));
    }
    const noGeneral = chats.filter((c) => !c.name.includes("总群"));
    if (state.listTab === "my") {
      if (noGeneral.length) {
        return noGeneral.map((c) => ({ id: c.chat_id, name: c.name }));
      }
    }
    if (state.selection.length) {
      const selChats = state.selection.map((id) => {
        const c = state.chats.find((x) => x.chat_id === id);
        const t = (state.byChat[id] || [])[0];
        return { id, name: (c && c.name) || (t && t.chat_name) || id };
      }).filter((c) => !c.name.includes("总群") && (!hasDaidainiao || c.name.includes("呆呆鸟")));
      if (selChats.length) return selChats;
    }
    return (noGeneral.length ? noGeneral : chats).map((c) => ({ id: c.chat_id, name: c.name }));
  }
  function ntMentionList() {
    const chat = $("ntChat").value, me = (state.auth.user && state.auth.user.open_id) || "", seen = new Map();
    for (const tp of state.byChat[chat] || []) if (tp.sender_type === "user" && tp.sender_id && tp.sender_id !== me && !seen.has(tp.sender_id)) seen.set(tp.sender_id, tp.sender_name);
    if (state.thread && state.thread.topic.chat_id === chat) for (const p of participants()) if (!seen.has(p.id)) seen.set(p.id, p.name);
    return Array.from(seen, ([id, name]) => ({ id, name }));
  }
  const ntMention = createMentionPicker($("ntMentionPop"), $("ntInput"), ntMentionList, nt.mentions);
  const ntTray = createTray($("ntTray"), $("ntFile"), $("ntAttach"), $("ntForm"), $("ntInput"), () => { $("ntSend").disabled = nt.sending || ntTray.uploading(); ntHint(); });
  function ntHint() {
    const up = uploadsAllowed();
    $("ntAttach").hidden = !up;
    const lim = limits();
    $("ntHint").textContent = up ? ("图片 ≤ " + fmtSize(lim.image) + "，文件 ≤ " + fmtSize(lim.file) + (ntTray.note ? " · ⚠ " + ntTray.note : "")) : "";
  }
  const ntMode = () => document.querySelector('input[name="ntMode"]:checked').value;
  function ntSetErr(e, kind) {
    const box = $("ntErr"); box.textContent = "";
    if (!e) return;
    if (typeof e === "string") box.appendChild(banner(kind || "warn", e));
    else box.appendChild(banner("err", "发送失败：" + e.message, e.hint, e));
  }
  async function ntLoadRecent() {
    const box = $("ntRecent"), chat = $("ntChat").value;
    nt.replyTo = null;
    box.hidden = ntMode() !== "reply";
    $("ntExplain").textContent = ntMode() === "reply"
      ? "你的内容会以话题形式回复到所选消息下（reply_in_thread），飞书会立即为该消息创建话题。仅列出最近的、尚未成为话题的群消息。"
      : "飞书没有「创建话题」接口：这里以你的身份向群里发送一条消息，它会出现在左侧列表中；第一次以话题形式回复它之后，飞书才会真正创建话题。";
    if (ntMode() !== "reply") return;
    box.textContent = ""; box.appendChild(el("div", "loading", "加载最近的群消息…"));
    try {
      const r = await api("/api/chats/" + encodeURIComponent(chat) + "/recent");
      if ($("ntChat").value !== chat || ntMode() !== "reply") return;
      box.textContent = "";
      if (!r.messages.length) box.appendChild(el("div", "empty small", "最近没有可用的群消息"));
      for (const m of r.messages) {
        const l = el("label"); const rb = el("input"); rb.type = "radio"; rb.name = "ntReply"; rb.value = m.id;
        rb.onchange = () => { nt.replyTo = m.id; };
        l.append(rb, el("span", "who", m.sender_name), el("span", "tx", m.text), el("span", "tm", fmtTime(m.time)));
        box.appendChild(l);
      }
    } catch (e) { box.textContent = ""; box.appendChild(banner("err", "加载失败：" + e.message, e.hint, e)); }
  }
  async function openNewTopic() {
    if (!state.auth.logged_in) return;
    if (!state.chats.length) {
      try {
        const r = await api("/api/chats");
        state.chats = r.chats || [];
      } catch (_) {}
    }
    const available = ntChats();
    if (!available.length) {
      toast("没有可用的群聊发起新话题", true);
      return;
    }
    const sel = $("ntChat"); sel.textContent = "";
    for (const c of available) { const o = el("option", null, c.name); o.value = c.id; sel.appendChild(o); }
    const cur = state.thread && state.thread.topic && state.thread.topic.chat_id;
    if (cur && available.some((c) => c.id === cur)) {
      sel.value = cur;
    } else if (state.selection.length && available.some((c) => c.id === state.selection[0])) {
      sel.value = state.selection[0];
    } else {
      sel.value = available[0].id;
    }
    sel.parentElement.hidden = false;
    const chosen = available.find((c) => c.id === sel.value) || available[0];
    $("ntTitle").textContent = "发起新话题 · " + chosen.name;
    document.querySelector('input[name="ntMode"][value="root"]').checked = true;
    $("ntInput").value = ""; nt.mentions.length = 0; ntSetErr(null); ntMention.close(); ntTray.reset(true);
    $("ntSend").disabled = false; nt.sending = false;
    if (state.auth.can_reply === false) {
      const e = state.scopeErr;
      ntSetErr({ message: "当前登录授权不包含「发送消息」权限，重新登录后才能发起话题。", hint: e && e.hint, kind: "scope_missing", scopes: (e && e.scopes) || ["im:message", "im:message.send_as_user"] });
    }
    ntHint(); ntLoadRecent();
    $("ntModal").hidden = false;
    setTimeout(() => $("ntInput").focus(), 0);
  }
  function closeNewTopic() { if (nt.sending) return; $("ntModal").hidden = true; ntTray.reset(true); ntMention.close(); }
  async function sendNewTopic() {
    if (nt.sending) return;
    const input = $("ntInput"), text = input.value.replace(/\s+$/, ""), items = ntTray.ready();
    if (ntTray.uploading()) return;
    if (!text.trim() && !items.length) { ntSetErr("请输入话题内容或添加附件", "warn"); return; }
    const chatId = $("ntChat").value, mode = ntMode();
    if (mode === "reply" && !nt.replyTo) { ntSetErr("请选择要在其下开启话题的消息", "warn"); return; }
    let encrypt = true;
    if (items.length) {
      const choice = await askEncrypt();
      if (choice === null) return;
      encrypt = choice;
    }
    const mentions = nt.mentions.filter((m) => text.includes("@" + m.name));
    nt.sending = true; $("ntSend").disabled = true; $("ntSend").textContent = "发送中…"; ntSetErr(null);
    try {
      const r = await api("/api/topics/new", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ chat_id: chatId, text, mentions, uuid: newUUID(), attachments: items.map((i) => i.uploadId), reply_to: mode === "reply" ? nt.replyTo : "", encrypt }),
      });
      nt.sending = false; $("ntSend").textContent = "发送";
      ntTray.reset(false); $("ntModal").hidden = true;
      const topic = r.topic;
      if (topic) {
        const list = state.byChat[chatId] || (state.byChat[chatId] = []);
        const i = list.findIndex((x) => x.thread_id === topic.thread_id || x.root_message_id === topic.root_message_id);
        if (i >= 0) list[i] = topic; else list.unshift(topic);
        rebuildTopics();
        const myI = state.myTopics.findIndex((x) => x.thread_id === topic.thread_id || x.root_message_id === topic.root_message_id);
        if (myI >= 0) state.myTopics[myI] = topic; else state.myTopics.unshift(topic);
        state.myTopics.sort((a, b) => b.last_time - a.last_time);
        renderList();
        await selectTopic(topic.thread_id, true);
      }
      if ((r.warnings || []).length) for (const w of r.warnings) $("chatWarn").appendChild(banner("warn", w));
      reloadChat(chatId);
    } catch (e) {
      nt.sending = false; $("ntSend").textContent = "发送"; $("ntSend").disabled = ntTray.uploading();
      if (uploadScopeErr(e)) { state.auth.can_upload = false; state.uploadScopeErr = e; }
      else if (isScopeErr(e)) { state.auth.can_reply = false; state.scopeErr = e; }
      ntSetErr(e);
    }
  }
  $("btnNewTopic").onclick = openNewTopic;
  $("ntClose").onclick = closeNewTopic;
  $("ntCancel").onclick = closeNewTopic;
  $("ntForm").addEventListener("submit", (e) => { e.preventDefault(); sendNewTopic(); });
  $("ntInput").addEventListener("keydown", (e) => {
    if (e.key === "Escape") { if (!$("ntMentionPop").hidden) ntMention.close(); else closeNewTopic(); return; }
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey) && !e.isComposing) {
      e.preventDefault();
      e.stopPropagation();
      sendNewTopic();
    }
  });
  $("ntChat").onchange = () => {
    nt.mentions.length = 0;
    const available = ntChats();
    const chosen = available.find((c) => c.id === $("ntChat").value);
    if (chosen) {
      $("ntTitle").textContent = "发起新话题 · " + chosen.name;
    }
    ntLoadRecent();
  };
  document.querySelectorAll('input[name="ntMode"]').forEach((r) => { r.onchange = ntLoadRecent; });
  $("ntMention").onclick = () => ntMention.toggle();
  $("ntModal").addEventListener("mousedown", (e) => { if (e.target === $("ntModal")) closeNewTopic(); if (!e.target.closest("#ntMentionPop, #ntMention")) ntMention.close(); });

  async function selectTopic(id, refresh) {
    const generation = ++state.viewGen;
    state.active = id; state.preview = null; state.thread = null;
    markTopicRead(id);
    setDownloads([]);
    renderFiles();
    history.replaceState(null, "", "#t=" + encodeURIComponent(id));
    renderList();
    $("messages").textContent = ""; $("messages").appendChild(el("div", "loading", "加载对话中…"));
    try {
      const thread = await api("/api/topics/" + encodeURIComponent(id) + "/messages" + (refresh ? "?refresh=1" : ""));
      if (state.active !== id || state.viewGen !== generation) return;
      state.thread = thread;
    } catch (e) {
      if (state.active !== id || state.viewGen !== generation) return;
      state.thread = null;
      $("chatHead").hidden = true;
      $("messages").textContent = "";
      $("messages").appendChild(banner("err", "加载话题失败：" + e.message, e.hint, e));
      renderFiles(); return;
    }
    if (state.active !== id) return;
    const real = state.thread.topic.thread_id;
    if (real && real !== id) { // a pending topic turned into a real thread meanwhile
      for (const list of Object.values(state.byChat)) for (const tp of list) if (tp.thread_id === id) { tp.thread_id = real; tp.pending = false; }
      state.active = real; history.replaceState(null, "", "#t=" + encodeURIComponent(real)); rebuildTopics(); renderList();
    }
    state.dlSig = null;
    if (state.thread && state.thread.topic) markTopicRead(state.thread.topic.thread_id, state.thread.topic.last_time);
    setDownloads(state.thread.downloads);
    renderThread(); renderFiles();
    scheduleDlPoll();
  }

  // Name tooltips appear synchronously, without the browser title delay.
  let fileNameTip = null, fileNameTarget = null;
  function hideFileNameTip() {
    if (fileNameTip) fileNameTip.hidden = true;
    if (fileNameTarget) fileNameTarget.removeAttribute("aria-describedby");
    fileNameTarget = null;
  }
  function showFileNameTip(name) {
    const text = name && name.textContent.trim();
    if (!text) { hideFileNameTip(); return; }
    if (fileNameTarget === name && fileNameTip && !fileNameTip.hidden) return;
    hideFileNameTip();
    if (!fileNameTip) {
      fileNameTip = el("div", "file-name-tooltip");
      fileNameTip.id = "fileNameTooltip"; fileNameTip.setAttribute("role", "tooltip");
      document.body.appendChild(fileNameTip);
    }
    fileNameTip.textContent = text;
    fileNameTip.style.left = "0px"; fileNameTip.style.top = "0px";
    fileNameTip.hidden = false;
    fileNameTarget = name; name.setAttribute("aria-describedby", fileNameTip.id);
    const anchor = name.getBoundingClientRect(), box = fileNameTip.getBoundingClientRect();
    const margin = 12, gap = 6;
    const left = Math.max(margin, Math.min(anchor.left, window.innerWidth - box.width - margin));
    let top = anchor.bottom + gap;
    if (top + box.height > window.innerHeight - margin) top = anchor.top - box.height - gap;
    top = Math.max(margin, Math.min(top, window.innerHeight - box.height - margin));
    fileNameTip.style.left = left + "px"; fileNameTip.style.top = top + "px";
  }
  $("filesBody").addEventListener("pointerover", (ev) => {
    if (ev.pointerType === "touch") return;
    const name = ev.target.closest(".file .fname");
    if (name) showFileNameTip(name);
  });
  $("filesBody").addEventListener("pointerout", (ev) => {
    if (fileNameTarget && !fileNameTarget.contains(ev.relatedTarget)) hideFileNameTip();
  });
  $("filesBody").addEventListener("focusin", (ev) => {
    const name = ev.target.closest(".fname") || (ev.target.matches(".file") && ev.target.querySelector(".fname"));
    if (name) showFileNameTip(name);
  });
  $("filesBody").addEventListener("focusout", hideFileNameTip);
  document.addEventListener("keydown", (ev) => { if (ev.key === "Escape") hideFileNameTip(); });
  window.addEventListener("scroll", hideFileNameTip, true);
  window.addEventListener("resize", hideFileNameTip);
  window.addEventListener("blur", hideFileNameTip);

  // ---------- files / preview ----------
  function renderFiles() {
    hideFileNameTip();
    const body = $("filesBody"), t = state.thread;
    const keepTop = body.scrollTop;
    body.textContent = ""; body.classList.remove("previewing");
    renderTabs();
    if (state.preview) { renderPreview(body); return; }
    if (state.tab === "dl") { renderDownloaded(body); body.scrollTop = keepTop; return; }
    if (!t) { $("filesCount").textContent = ""; body.appendChild(el("div", "empty small", "选择话题后在此显示附件")); return; }
    renderFileSync(body);
    const atts = (t.attachments || []).filter(isOnlineAtt).sort((a,b) => (b.time || 0) - (a.time || 0) || (a.id || "").localeCompare(b.id || ""));
    $("filesCount").textContent = atts.length ? String(atts.length) : "";
    if (!atts.length) body.appendChild(el("div", "empty small", "该话题暂无飞书在线文档"));
    for (const a of atts) body.appendChild(fileRow(a));
    body.scrollTop = keepTop;
  }
  function renderFileSync(body) {
    const bar = el("div", "sync-status", "按需下载 · 不自动检查或下载资料");
    const fs = state.thread && state.thread.file_sync;
    const names = el("button", "retry", fs && fs.names_loading ? "正在刷新名称…" : "刷新名称");
    names.type = "button"; names.disabled = !!(fs && fs.names_loading);
    names.title = "仅刷新当前话题的文件名称，不下载文件或检查内容版本";
    names.onclick = async () => {
      const id = state.active; if (!id) return;
      names.disabled = true;
      try { const t = await api("/api/topics/" + encodeURIComponent(id) + "/messages?refresh_names=1"); if (state.active === id) applyPolledThread(t,id); }
      catch (e) { toast(e.message || "刷新名称失败",true); }
      finally { names.disabled = false; }
    };
    bar.appendChild(names);
    for (const w of ((fs && fs.warnings) || [])) bar.appendChild(el("div", "fsub st-err", w));
    body.appendChild(bar);
  }
  async function expandFolder(a, button) {
    const id = state.active; if (!id) return;
    button.disabled = true; button.textContent = "展开中…";
    try {
      const t = await api("/api/topics/" + encodeURIComponent(id) + "/materials/expand", {
        method: "POST", headers: { "Content-Type": "application/json", "X-FT-CSRF": await csrfToken() }, body: JSON.stringify({token: a.resource_token})
      });
      if (state.active === id) applyPolledThread(t, id);
    } catch (e) { toast(e.message || "展开失败", true); }
    finally { button.disabled = false; button.textContent = "展开"; }
  }

  const isFailed = (a) => {
    if (!a.res_type) return false;
    const it = dlOf(a);
    return !!(a.no_download || state.failed.has(failKey(a.message_id, a.file_key)) || (it && it.status === "failed"));
  };
  // one-line download status for a file row
  function dlStatusNode(it) {
    const sp = el("span", "download-status");
    if (!it) { sp.textContent = "未下载"; return sp; }
    if (it.status === "done") { sp.classList.add("st-ok"); sp.textContent = "已下载 · " + fmtSize(it.size); }
    else if (isPendingDl(it)) { sp.classList.add("st-run"); sp.textContent = it.status === "waiting" ? "等待下载" : "下载中…"; }
    else { sp.textContent = "未下载"; }
    if (it.status === "failed" || it.status === "missing" || it.sync_status === "failed") {
      sp.appendChild(el("span", "st-err", " · " + (it.reason || it.sync_error || "上次下载失败")));
    }
    return sp;
  }
  function downloadProgressNode(i) {
    const wrap = el("div", "dl-transfer");
    const received = Math.max(0, i.downloaded_bytes || 0), total = Math.max(0, i.total_bytes || 0);
    const bytes = (n) => n < 1024 ? n + " B" : fmtSize(n);
    const progress = el("progress", "dl-progress");
    progress.max = total || 1;
    progress.setAttribute("aria-label", (i.name || i.name_hint || "文件") + "下载进度");
    let label = i.status === "waiting" ? "等待下载" : i.res_type.startsWith("export:") ? "正在导出文档…" : "正在连接飞书…";
    if (i.status === "downloading" && total > 0) {
      progress.value = Math.min(received, total);
      label = received >= total ? "接收完成，正在校验并保存" : "下载中 " + Math.floor(received / total * 100) + "% · " + bytes(received) + " / " + bytes(total);
    } else if (i.status === "downloading" && received > 0) label = "已接收 " + bytes(received) + " · 总大小未知";
    wrap.append(progress, el("div", "fsub", label));
    return wrap;
  }
  function fileRow(a) {
    const isLink = a.kind === "doc" || a.kind === "link" || a.kind === "unresolved";
    const it = dlOf(a);
    const failed = isFailed(a);
    const doneLocal = !!(it && it.status === "done" && state.dl.opener);
    const row = el("div", "file" + (failed ? " failed" : ""));
    if (doneLocal && !isLink) {
      row.onclick = () => openLocal(it);
    } else if (isLink) {
      const h = safeHref(a.url);
      if (h) row.onclick = () => window.open(h, "_blank", "noopener,noreferrer");
    } else row.onclick = () => openPreview(a);
    const th = el("div", "thumb");
    const key = failKey(a.message_id, a.file_key);
    if (!failed && it && it.status === "done" && a.kind === "image") { const i = el("img"); i.loading = "lazy"; i.alt = ""; i.src = resUrl(a); i.onerror = () => { i.remove(); th.textContent = "🖼️"; markFailed(key); }; th.appendChild(i); }
    else th.textContent = fileIcon(a);
    row.appendChild(th);
    const info = el("div", "info");
    const name = el(isLink ? "a" : "div", "fname", (isLink && it && dlDisplayName(it)) || a.name || a.url);
    name.title = "";
    if (isLink && safeHref(a.url)) { name.href = safeHref(a.url); name.target = "_blank"; name.rel = "noopener noreferrer"; name.onclick = (ev) => ev.stopPropagation(); }
    info.appendChild(name);
    const sub = el("div", "fsub");
    if (isLink && !it) sub.textContent = a.kind === "doc" ? "未下载 · 该类型暂不支持导出，可在线打开" : a.url;
    else if (isLink) { sub.append("飞书在线文档 · "); sub.appendChild(dlStatusNode(it)); }
    else if (failed) {
      sub.append(a.no_download ? "卡片/转发内 · 飞书不支持下载" : "");
      if (it && !a.no_download) sub.appendChild(dlStatusNode(it)); else if (!a.no_download) sub.append("无法下载");
      row.title = a.no_download ? NODL_TIP : (it && it.reason) || DL_TIP;
      const l = !a.no_download && openLink("在飞书中查看"); if (l) sub.append(" · ", l);
    } else {
      sub.append((a.sender_name || "") + (a.time ? " · " + fmtTime(a.time) : ""));
      if (it) { sub.append(" · "); sub.appendChild(dlStatusNode(it)); }
    }
    info.appendChild(sub);
    if (it && isPendingDl(it)) info.appendChild(downloadProgressNode(it));
    if (!failed) row.title = (a.name || a.url || "") + (doneLocal ? "\n点击用默认应用打开 · 右键在文件夹中显示" : "");
    row.appendChild(info);
    if (it) { const acts = el("div", "acts"); acts.appendChild(downloadButton(it)); row.appendChild(acts); }
    if (a.resource_type === "folder") {
      const b = el("button", "retry", "展开"); b.type = "button";
      b.onclick = (ev) => { ev.preventDefault(); ev.stopPropagation(); expandFolder(a, b); };
      row.appendChild(b);
    }
    withFileMenu(row, () => dlOf(a));
    return row;
  }
  // ---------- "已下载" tab ----------
  const isImageName = (n) => /\.(png|jpe?g|gif|webp|bmp|svg|ico|heic)$/i.test(n || "");
  function dlPreviewable(i) {
    return /^image\//.test(i.mime || "") && i.mime !== "image/svg+xml" || ["pdf", "txt", "md", "csv", "json", "log", "mp4", "webm", "mp3", "wav", "ogg", "opus", "m4a"].includes(ext(i.name)) || isImageName(i.name);
  }
  function dlAsAtt(i) {
    const kind = (/^image\//.test(i.mime || "") || isImageName(i.name)) ? "image" : (i.kind === "drive" ? "file" : i.kind);
    return { id: "local:" + i.id, kind, name: i.name, local_id: i.id };
  }
  function dlRow(i) {
    const done = i.status === "done";
    const row = el("div", "file dl");
    row.tabIndex = 0;
    const th = el("div", "thumb");
    if (done && (/^image\//.test(i.mime || "") || isImageName(i.name)) && i.mime !== "image/svg+xml") {
      const im = el("img"); im.loading = "lazy"; im.alt = ""; im.src = "/api/files/" + i.id + "/content"; im.onerror = () => { im.remove(); th.textContent = "🖼️"; }; th.appendChild(im);
    } else th.textContent = fileIcon({ kind: i.kind === "drive" ? "file" : i.kind, name: i.name });
    row.appendChild(th);
    const info = el("div", "info");
    const name = el("div", "fname", dlDisplayName(i));
    name.title = ""; info.appendChild(name);
    const sub = el("div", "fsub");
    sub.appendChild(dlStatusNode(i));

    info.appendChild(sub);
    if (isPendingDl(i)) info.appendChild(downloadProgressNode(i));
    if (done && i.cloud_name && i.name && i.cloud_name !== i.name) info.appendChild(el("div", "fsub", "本地副本：" + i.name));
    if (i.source_path) info.appendChild(el("div", "fsub", "来源：" + i.source_path));
    if (i.backup_rel) info.appendChild(el("div", "fsub", "已保留本地修改备份：" + i.backup_rel.split("/").pop()));
    row.appendChild(info);
    const acts = el("div", "acts");
    acts.appendChild(downloadButton(i));
    const bOpen = el("button", null, state.dl.opener ? "打开" : "查看"); bOpen.type = "button"; bOpen.disabled = !done;
    bOpen.title = state.dl.opener ? "用本地默认应用打开" : "在浏览器中查看";
    bOpen.onclick = (ev) => { ev.stopPropagation(); openLocal(i); };
    acts.appendChild(bOpen);
    if (state.dl.opener) { const bf = el("button", null, "📂"); bf.type = "button"; bf.title = "在文件夹中显示"; bf.disabled = !done; bf.onclick = (ev) => { ev.stopPropagation(); revealLocal(i); }; acts.appendChild(bf); }
    row.appendChild(acts);
    row.title = (i.path || i.name || "") + (done ? "\n点击用默认应用打开 · 右键在文件夹中显示或更多操作" : "");
    row.onclick = () => {
      document.querySelectorAll(".file.dl.sel").forEach((n) => n.classList.remove("sel"));
      row.classList.add("sel");
      if (done) openLocal(i);
    };
    row.onkeydown = (ev) => { if (ev.key === "Enter" && done) openLocal(i); };
    withFileMenu(row, () => i);
    return row;
  }
  function renderDownloaded(body) {
    if (!state.thread) { body.appendChild(el("div", "empty small", "选择话题后，可按需下载其中的文件")); return; }
    renderFileSync(body);
    const items = state.dl.items.filter((i) => !isOnlineDl(i));
    const bar = el("div", "dl-bar");
    const complete = (i) => i.status === "done";
    const done = items.filter(complete).length, run = items.filter(isPendingDl).length, bad = items.filter((i) => i.status === "failed" || i.status === "missing" || i.sync_status === "failed" || i.remote_missing).length;
    bar.appendChild(el("span", null, "已下载 " + done + " · 未下载 " + (items.length - done - run)  + (run ? " · 下载 / 等待 " + run : "") + (bad ? " · 失败 " + bad : "")));
    if (run) {
      const progress = el("progress", "dl-progress"); progress.max = items.length || 1;
      progress.setAttribute("aria-label", "当前话题文件下载总进度");
      if ((state.thread.file_sync || {}).phase !== "scanning") progress.value = done;
      bar.appendChild(progress);
    }
    if (state.dl.dir) {
      const d = el("div", "dl-dir", "📁 " + state.dl.dir); d.title = "点击复制下载目录路径";
      d.onclick = async () => toast((await copyText(state.dl.dir)) ? "已复制下载目录" : "复制失败", false);
      bar.appendChild(d);
    }
    body.appendChild(bar);
    if (!state.dl.enabled) { body.appendChild(el("div", "empty small", "已关闭文件下载（disable_download）")); return; }
    for (const i of items) body.appendChild(dlRow(i));
    if (!items.length) body.appendChild(el("div", "empty small", "该话题没有直接附件；文件夹内的资料可在下方点击展开"));
    const sources = ((state.thread && state.thread.attachments) || []).filter((a) => a.kind === "folder");
    if (sources.length) {
      const folders = el("details", "source-folders");
      folders.appendChild(el("summary", "sec", "来源文件夹（" + sources.length + "）"));
      for (const a of sources) { const row = fileRow(Object.assign({}, a, {kind:"link"})); if (a.resolve_error) row.appendChild(el("div", "fsub st-err", a.resolve_error)); folders.appendChild(row); }
      body.appendChild(folders);
    }
    const unavailable = ((state.thread && state.thread.attachments) || []).filter((a) => a.no_download && ["file", "image", "audio", "video"].includes(a.kind));
    if (unavailable.length) {
      body.appendChild(el("div", "sec", "受限附件"));
      for (const a of unavailable) body.appendChild(fileRow(a));
    }
  }
  function openPreviewFor(messageId, key, resType, name, kind, cover) {
    const found = state.thread && (state.thread.attachments || []).find((a) => a.message_id === messageId && a.file_key === key);
    openPreview(found || { id: resType + ":" + messageId + ":" + key, kind, name, message_id: messageId, file_key: key, res_type: resType, cover_key: cover });
  }
  function openPreview(a) { state.preview = a; renderFiles(); }
  const TEXT_EXT = new Set(["txt", "md", "csv", "json", "log", "xml", "yaml", "yml", "ini", "sql", "go", "py", "js", "ts", "java", "sh", "html", "css"]);
  function renderPreview(body) {
    const a = state.preview;
    body.classList.add("previewing");
    $("filesTitle").textContent = "预览";
    const bar = el("div", "preview-bar");
    const back = el("button", "btn", "← 列表"); back.onclick = () => { state.preview = null; renderFiles(); };
    bar.appendChild(back);
    bar.appendChild(el("div", "nm", a.name || "附件"));
    const it = dlOf(a) || (a.local_id && state.dl.items.find((x) => x.id === a.local_id));
    if (it && it.status !== "done") {
      bar.appendChild(downloadButton(it)); body.appendChild(bar);
      const pv = el("div", "preview-body"); pv.appendChild(el("div", "empty", "未下载，请点击下载后查看"));
      if (isPendingDl(it)) pv.appendChild(downloadProgressNode(it));
      body.appendChild(pv); return;
    }
    const url = it && it.status === "done" ? "/api/files/" + it.id + "/content" : resUrl(a);
    if (it && it.status === "done" && state.dl.opener) {
      const bOpen = el("button", "btn", "打开"); bOpen.type = "button"; bOpen.title = "用本地默认应用打开";
      bOpen.onclick = () => openLocal(it);
      const bDir = el("button", "btn", "📂 文件夹"); bDir.type = "button"; bDir.title = "在文件夹中显示";
      bDir.onclick = () => revealLocal(it);
      bar.append(bOpen, bDir);
    } else {
      const dl = el("a", "btn", "下载");
      dl.href = resUrl(a, true); dl.download = a.name || "";
      const open = el("a", "btn", "↗"); open.href = url; open.target = "_blank"; open.rel = "noopener"; open.title = "新标签页打开";
      bar.append(dl, open);
    }
    body.appendChild(bar);
    const pv = el("div", "preview-body");
    const e = ext(a.name);
    const nopreview = (msg, tip) => {
      pv.textContent = "";
      const d = el("div", "nopreview"); d.append(el("div", "empty-ico", fileIcon(a)), el("div", null, msg));
      if (tip) d.title = tip;
      const l = openLink(); if (l && a.res_type) { l.classList.add("btn"); d.appendChild(el("div", "nopreview-act")); d.lastChild.appendChild(l); }
      pv.appendChild(d);
    };
    body.appendChild(pv);
    if (isFailed(a)) { nopreview(a.no_download ? "卡片 / 合并转发中的资源无法通过接口获取" : "无法通过飞书接口下载此文件", a.no_download ? NODL_TIP : DL_TIP); return; }
    pv.appendChild(el("div", "loading", "加载中…"));
    // probe first so API refusals (e.g. resource API is app-identity only) are explained
    fetch(url, { method: "HEAD" }).then(async (r) => {
      if (state.preview !== a) return;
      if (!r.ok) {
        let tip = DL_TIP;
        try { const b = await (await fetch(url)).json(); tip = (b.hint || b.error || tip); } catch (_) { /* ignore */ }
        if (a.res_type) markFailed(failKey(a.message_id, a.file_key));
        nopreview("无法通过飞书接口下载此文件", tip); return;
      }
      pv.textContent = ""; build();
    }).catch(() => { pv.textContent = ""; build(); });
    function build() {
    if (a.kind === "image" || ["png", "jpg", "jpeg", "gif", "webp", "bmp"].includes(e)) {
      const i = el("img"); i.alt = a.name || ""; i.src = url; i.onerror = () => nopreview("图片加载失败"); pv.appendChild(i);
    } else if (e === "pdf") {
      const f = el("iframe"); f.src = url; f.title = a.name || "PDF"; pv.appendChild(f);
    } else if (a.kind === "video" || ["mp4", "webm"].includes(e)) {
      const v = el("video"); v.controls = true; v.src = url; v.onerror = () => nopreview("浏览器无法播放该视频，请下载后查看"); pv.appendChild(v);
    } else if (a.kind === "audio" || ["mp3", "wav", "opus", "ogg", "m4a"].includes(e)) {
      const v = el("audio"); v.controls = true; v.src = url; v.onerror = () => nopreview("浏览器无法播放该音频，请下载后查看"); pv.appendChild(v);
    } else if (TEXT_EXT.has(e)) {
      pv.appendChild(el("div", "loading", "加载中…"));
      fetch(url).then((r) => { if (!r.ok) throw new Error("HTTP " + r.status); return r.text(); })
        .then((txt) => { pv.textContent = ""; pv.appendChild(el("pre", null, txt.length > 200000 ? txt.slice(0, 200000) + "\n…（已截断）" : txt)); })
        .catch(() => nopreview("预览失败，请尝试下载"));
    } else nopreview("该类型暂不支持预览，请点击「下载」");
    }
  }

  // ---------- group selector ----------
  let draft = new Set();
  function renderGroups() {
    const box = $("groupList"); box.textContent = "";
    const q = $("groupSearch").value.trim().toLowerCase();
    let chats = state.chats;
    const hasDaidainiao = chats.some((c) => c.name && c.name.includes("呆呆鸟"));
    if (hasDaidainiao) {
      chats = chats.filter((c) => c.name && c.name.includes("呆呆鸟"));
    }
    chats = chats.filter((c) => !q || c.name.toLowerCase().includes(q));
    if (!chats.length) box.appendChild(el("div", "empty small", state.chats.length ? "没有匹配的群" : "暂无可用的呆呆鸟群聊"));
    for (const c of chats) {
      const row = el("label", "group-item");
      const cb = el("input"); cb.type = "checkbox"; cb.checked = draft.has(c.chat_id);
      cb.onchange = () => { cb.checked ? draft.add(c.chat_id) : draft.delete(c.chat_id); };
      row.appendChild(cb);
      row.appendChild(avatar(c.name, c.avatar, true));
      const info = el("div");
      info.appendChild(el("div", "gname", c.name + (c.external ? "（外部群）" : "")));
      if (c.description) info.appendChild(el("div", "gdesc", c.description));
      row.appendChild(info);
      box.appendChild(row);
    }
  }
  async function openModal() {
    draft = new Set(state.selection);
    $("modal").hidden = false; $("groupSearch").value = "";
    $("groupList").textContent = ""; $("groupList").appendChild(el("div", "loading", "加载群聊列表…"));
    try {
      const r = await api("/api/chats");
      state.chats = r.chats || [];
      renderGroups();
    } catch (e) {
      $("groupList").textContent = "";
      $("groupList").appendChild(banner("err", "获取群聊列表失败：" + e.message, e.hint));
    }
  }
  const closeModal = () => { $("modal").hidden = true; };
  $("btnGroups").onclick = openModal;
  $("modalClose").onclick = closeModal;
  $("modal").addEventListener("mousedown", (e) => { if (e.target === $("modal")) closeModal(); });
  addEventListener("keydown", (e) => { if (e.key === "Escape") closeModal(); });
  $("groupSearch").oninput = renderGroups;
  $("groupAll").onclick = () => {
    let chats = state.chats;
    if (chats.some((c) => c.name && c.name.includes("呆呆鸟"))) {
      chats = chats.filter((c) => c.name && c.name.includes("呆呆鸟"));
    }
    chats.forEach((c) => draft.add(c.chat_id));
    renderGroups();
  };
  $("groupNone").onclick = () => { draft.clear(); renderGroups(); };
  $("groupSave").onclick = async () => {
    const ids = state.chats.map((c) => c.chat_id).filter((id) => draft.has(id));
    try {
      const r = await api("/api/selection", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ chat_ids: ids }) });
      state.selection = r.chat_ids;
    } catch (e) { alert("保存失败：" + e.message); return; }
    closeModal();
    if (state.thread && state.thread.topic && !ids.includes(state.thread.topic.chat_id)) {
      state.thread = null;
      state.active = null;
      setDownloads([]);
      renderThread();
      renderFiles();
    }
    await loadTopics(false);
    startAutoRefresh();
  };

  // ---------- auto refresh ----------
  // Active conversation thread polls every 5s (2.5s when queued/busy).
  // Left-side topic list polls silently in background every 10s (and immediately on focus/visibility),
  // so new topics created in Feishu appear automatically without full-page reloads.
  const THREAD_POLL_MS = 5000;
  const TOPIC_LIST_POLL_MS = 10000;
  let pollTimer = null;
  let polling = false;
  let lastTopicPoll = 0;

  function sameTopics(a, b) {
    if (!a && !b) return true;
    if (!a || !b) return false;
    if (a.length !== b.length) return false;
    for (let i = 0; i < a.length; i++) {
      const x = a[i], y = b[i];
      if (!x || !y) return false;
      if (x.thread_id !== y.thread_id ||
          x.root_message_id !== y.root_message_id ||
          x.last_time !== y.last_time ||
          x.reply_count !== y.reply_count ||
          x.title !== y.title ||
          x.snippet !== y.snippet ||
          !!x.pending !== !!y.pending ||
          x.attachment_count !== y.attachment_count ||
          x.sender_id !== y.sender_id ||
          x.sender_name !== y.sender_name ||
          x.chat_id !== y.chat_id ||
          x.chat_name !== y.chat_name) {
        return false;
      }
    }
    return true;
  }

  let refreshingTopics = false;
  async function refreshTopicListSilently() {
    if (refreshingTopics || !state.auth.logged_in) return;
    refreshingTopics = true;
    try {
      if (state.listTab === "my") {
        const r = await api("/api/topics/my");
        if (!state.auth.logged_in) return;
        const newTopics = r.topics || [];
        if (!sameTopics(state.myTopics, newTopics)) {
          state.myTopics = newTopics;
          state.myLoaded = true;
          if (state.active && document.hasFocus && document.hasFocus()) {
            markTopicRead(state.active);
          }
          renderListKeepScroll();
        }
      } else {
        if (!state.selection.length) return;
        let anyChanged = false;
        await Promise.all(state.selection.map(async (id) => {
          try {
            const r = await api("/api/topics?chat_id=" + encodeURIComponent(id));
            if (!state.auth.logged_in) return;
            const newTopics = r.topics || [];
            if (!sameTopics(state.byChat[id], newTopics)) {
              state.byChat[id] = newTopics;
              anyChanged = true;
            }
          } catch (_) {}
        }));
        if (anyChanged) {
          rebuildTopics();
          if (state.active && document.hasFocus && document.hasFocus()) {
            markTopicRead(state.active);
          }
          renderListKeepScroll();
        }
      }
    } catch (_) {
      // background silent polling: ignore temporary network errors
    } finally {
      refreshingTopics = false;
    }
  }

  function syncTopicFromThread(t) {
    const tp = t.topic;
    if (!tp) return;
    let changed = false;
    for (const list of Object.values(state.byChat)) {
      const x = list.find((i) => i.thread_id === tp.thread_id || (tp.root_message_id && i.root_message_id === tp.root_message_id));
      if (!x) continue;
      if (x.thread_id !== tp.thread_id || x.reply_count !== tp.reply_count || x.last_time !== tp.last_time || x.title !== tp.title || x.snippet !== tp.snippet || !!x.pending !== !!tp.pending || x.attachment_count !== tp.attachment_count || x.sender_id !== tp.sender_id || x.sender_name !== tp.sender_name || x.sender_type !== tp.sender_type || x.root_message_id !== tp.root_message_id) {
        changed = true;
        x.thread_id = tp.thread_id;
        x.reply_count = tp.reply_count;
        x.last_time = tp.last_time;
        x.title = tp.title;
        x.snippet = tp.snippet;
        x.pending = tp.pending;
        x.attachment_count = tp.attachment_count;
        x.sender_id = tp.sender_id;
        x.sender_name = tp.sender_name;
        x.sender_avatar = tp.sender_avatar;
        x.sender_type = tp.sender_type;
        x.root_message_id = tp.root_message_id;
        x.create_time = tp.create_time;
      }
    }
    let myX = state.myTopics.find((i) => i.thread_id === tp.thread_id || (tp.root_message_id && i.root_message_id === tp.root_message_id));
    if (myX) {
      Object.assign(myX, tp);
      state.myTopics.sort((a, b) => b.last_time - a.last_time);
      changed = true;
    } else if (tp.user_involved || (state.auth.user && tp.sender_id === state.auth.user.open_id)) {
      state.myTopics.unshift(Object.assign({}, tp));
      state.myTopics.sort((a, b) => b.last_time - a.last_time);
      changed = true;
    }
    if (!changed) return;
    rebuildTopics();
    renderListKeepScroll();
  }
  function applyPolledThread(t, id) {
    const real = t.topic && t.topic.thread_id;
    if (real && real !== id) {
      for (const list of Object.values(state.byChat)) {
        for (const tp of list) if (tp.thread_id === id) { tp.thread_id = real; tp.pending = false; }
      }
      for (const tp of state.myTopics) {
        if (tp.thread_id === id) { tp.thread_id = real; tp.pending = false; }
      }
      if (state.active === id) {
        state.active = real;
        history.replaceState(null, "", "#t=" + encodeURIComponent(real));
      }
    }
    const prevQ = (state.thread && state.thread.queue && state.thread.queue.length) || 0;
    const nextQ = (t.queue && t.queue.length) || 0;
    const prevMsgs = (state.thread && state.thread.messages && state.thread.messages.length) || 0;
    const nextMsgs = (t.messages && t.messages.length) || 0;
    const prevBusy = !!(state.thread && state.thread.bot_busy);
    const nextBusy = !!t.bot_busy;
    const box = $("messages");
    const stick = nearBottom(box);
    const top = box.scrollTop;
    const changed = threadSig(state.thread) !== threadSig(t);
    state.thread = t;
    if (changed) {
      renderThread(stick ? "bottom" : "keep", top);
      if (!state.preview) renderFiles();
    }
    if (prevQ > nextQ && nextMsgs > prevMsgs) {
      toast("✅ 呆呆鸟上一任务已结束，排队消息已自动发出", false);
    }
    if (prevBusy && !nextBusy) {
      onTaskFinished(t, id);
    }
    setDownloads(t.downloads);
    syncTopicFromThread(t);
  }
  async function refreshOpenThread(gen) {
    const id = state.active;
    if (!id || state.sending || gen !== state.viewGen) return;
    const t = await api("/api/topics/" + encodeURIComponent(id) + "/messages?refresh=1");
    if (!state.auth.logged_in || state.active !== id || state.sending || gen !== state.viewGen) return;
    applyPolledThread(t, id);
  }
  function stopAutoRefresh() {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
  function scheduleAutoRefresh(ms) {
    clearTimeout(pollTimer);
    pollTimer = setTimeout(runAutoRefresh, ms);
  }
  function startAutoRefresh() {
    stopAutoRefresh();
    if (state.auth.logged_in) scheduleAutoRefresh(THREAD_POLL_MS);
  }
  async function runAutoRefresh() {
    if (polling) return;
    if (!state.auth.logged_in) return;
    if (document.hidden) { scheduleAutoRefresh(THREAD_POLL_MS); return; }
    polling = true;
    const gen = state.viewGen;
    try {
      if (state.active && !state.sending) {
        await refreshOpenThread(gen);
      }
      const now = Date.now();
      if (now - lastTopicPoll >= TOPIC_LIST_POLL_MS) {
        lastTopicPoll = now;
        await refreshTopicListSilently();
      }
    } catch (e) {
      if (!(e && e.kind === "login_required")) { /* keep the current view */ }
    } finally {
      polling = false;
      const nextMs = isTopicQueuedOrBusy(state.thread) ? 2500 : THREAD_POLL_MS;
      if (state.auth.logged_in) scheduleAutoRefresh(nextMs);
    }
  }
  document.addEventListener("visibilitychange", () => {
    if (document.hidden || !state.auth.logged_in) return;
    lastTopicPoll = 0;
    stopAutoRefresh();
    runAutoRefresh();
  });
  window.addEventListener("focus", () => {
    if (document.hidden || !state.auth.logged_in) return;
    const now = Date.now();
    if (now - lastTopicPoll >= 3000) {
      lastTopicPoll = 0;
      stopAutoRefresh();
      runAutoRefresh();
    }
  });

  // ---------- wiring ----------
  $("btnNotify").onclick = () => openNotifyModal();
  $("notifyClose").onclick = () => closeNotifyModal();
  $("btnNotifyDone").onclick = () => closeNotifyModal();
  $("chkNotifyFeishu").onchange = (e) => updateNotifySetting("feishu_enabled", e.target.checked);
  $("chkNotifyBrowser").onchange = (e) => updateNotifySetting("browser_enabled", e.target.checked);
  $("chkNotifySound").onchange = (e) => updateNotifySetting("sound_enabled", e.target.checked);
  $("btnTestFeishu").onclick = testFeishuNotification;
  $("btnTestBrowser").onclick = testBrowserNotification;
  $("btnTestSound").onclick = () => playChime();

  $("search").oninput = (e) => { state.search = e.target.value; renderList(); };
  $("tabMyTopics").onclick = () => setListTab("my");
  $("tabGroupTopics").onclick = () => setListTab("group");
  $("btnMarkAllRead").onclick = async () => {
    if (!state.readState || !state.readState.threads) {
      state.readState = { initialized: true, threads: {} };
    }
    const all = [...state.myTopics, ...state.topics];
    const threads = { ...state.readState.threads };
    const now = Date.now();
    for (const t of all) {
      if (t.thread_id) {
        threads[t.thread_id] = Math.max(t.last_time || 0, now);
      }
    }
    state.readState.initialized = true;
    state.readState.threads = threads;
    renderListKeepScroll();
    toast("已将所有话题标记为已读");
    try {
      await api("/api/read_state", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ all: true, threads: threads })
      });
    } catch (_) {}
  };
  $("btnRefresh").onclick = async () => {
    state.viewGen++;
    lastTopicPoll = Date.now();
    if (state.listTab === "my") {
      await loadMyTopics(true);
      if (state.selection.length) loadTopics(true);
    } else {
      await loadTopics(true);
      loadMyTopics(true);
    }
    if (state.active) await selectTopic(state.active, true);
  };

  async function init() {
    renderList();
    try {
      state.status = await api("/api/status");
      state.dl.opener = !!state.status.opener;
      state.dl.docker = !!state.status.docker;
      state.dl.enabled = state.status.downloads !== false;
      if (state.status.download_dir) state.dl.dir = state.status.download_dir;
      $("modeBadge").hidden = !state.status.mock;
      state.auth = await api("/api/auth/status");
      renderAuth();
      const sel = await api("/api/selection");
      state.selection = sel.chat_ids || [];
      if (state.auth.logged_in) {
        try {
          const cr = await api("/api/chats");
          state.chats = cr.chats || [];
        } catch (_) {}
      }
      try {
        state.notify = await api("/api/notifications/settings");
      } catch (_) {}
      try {
        state.readState = await api("/api/read_state");
        if (!state.readState || !state.readState.threads) {
          state.readState = { initialized: false, threads: {} };
        }
      } catch (_) {}
    } catch (e) { $("banner").appendChild(banner("err", "无法连接后端：" + e.message)); }
    state.loadedSelection = true;
    renderList(); renderThread();
    if (!state.auth.logged_in) return;
    if (state.listTab === "my") {
      await loadMyTopics(false);
      if (state.selection.length) loadTopics(false);
    } else {
      if (!state.selection.length && state.status && (state.status.configured)) { openModal(); return; }
      if (state.selection.length) await loadTopics(false);
      loadMyTopics(false);
    }
    const m = /#t=([^&]+)/.exec(location.hash);
    if (m) await selectTopic(decodeURIComponent(m[1]));
    startAutoRefresh();
  }
  init();
})();
