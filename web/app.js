// Zyper Bot dashboard client.
const $ = (id) => document.getElementById(id);
let OFFLINE = false;
function setOffline(b) {
  if (b === OFFLINE) return;
  OFFLINE = b;
  const bar = $("offlineBar"); if (bar) bar.classList.toggle("hide", !b);
}
const api = async (path, opts = {}) => {
  const headers = { "Content-Type": "application/json", ...(opts.headers || {}) };
  // Remote (VPS) use only: send the shared token if one was stored. Loopback (desktop)
  // never needs it — the server lets localhost through without a token.
  try { const t = localStorage.getItem("zyperAuthToken"); if (t) headers["X-Auth-Token"] = t; } catch {}
  let res;
  try {
    res = await fetch("/api" + path, { ...opts, headers });
  } catch (e) {
    // Network-level failure (server down / opened via file:// / wrong host).
    setOffline(true);
    throw new Error("Cannot reach the server. Is zyper-bot running? Open http://<host>:8787 (not the .html file).");
  }
  setOffline(false);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.status);
  return body;
};
const short = (a) => (a && a.length > 12 ? a.slice(0, 6) + "…" + a.slice(-4) : a || "");
const fmtEth = (wei) => { if (!wei) return "0.0000"; try { return (Number(BigInt(wei)) / 1e18).toFixed(4); } catch { return "0"; } };
const copy = (t) => navigator.clipboard && navigator.clipboard.writeText(t);

// inline line-icons (no emoji)
const SVG = (p, fill) => `<svg class="i${fill ? " fill" : ""}" viewBox="0 0 24 24">${p}</svg>`;
const IC = {
  play: SVG('<polygon points="5 3 19 12 5 21 5 3"/>', true),
  stop: SVG('<rect x="6" y="6" width="12" height="12" rx="1"/>', true),
  boost: SVG('<polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2"/>'),
  trash: SVG('<polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>'),
  copy: SVG('<rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>'),
  send: SVG('<line x1="22" y1="2" x2="11" y2="13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/>'),
  box: SVG('<path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>'),
  edit: SVG('<path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z"/>'),
};

let TASKS = {}, CHAINS = [], CUR_GROUP = "Imported";
let WALLETS = [], CUR_WGROUP = ""; // wallet list + active group filter ("" = all)

// ---------- toast + confirm (replace alert/confirm) ----------
function toast(msg, type = "info", ms = 3800) {
  // Dedupe: if an identical toast is already visible, don't stack another.
  if ([...$("toasts").children].some((t) => t.dataset.msg === msg)) return;
  const el = document.createElement("div");
  el.className = "toast " + type;
  el.dataset.msg = msg;
  el.innerHTML = `<div class="tmsg"></div><span class="tx">×</span>`;
  el.querySelector(".tmsg").textContent = msg;
  el.querySelector(".tx").onclick = () => el.remove();
  $("toasts").appendChild(el);
  if (ms > 0) setTimeout(() => el.remove(), ms);
}
function confirmDialog(msg, okLabel = "Confirm") {
  return new Promise((resolve) => {
    const ov = document.createElement("div"); ov.className = "overlay show";
    ov.innerHTML = `<div class="modal confirm-box"><div class="mhead"><h3>Confirm</h3><span class="close">×</span></div><div class="mbody"></div><div class="mfoot"><button class="cancel">Cancel</button><button class="primary ok">${okLabel}</button></div></div>`;
    ov.querySelector(".mbody").textContent = msg;
    const done = (v) => { animateClose(ov, true); resolve(v); };
    ov.querySelector(".close").onclick = () => done(false);
    ov.querySelector(".cancel").onclick = () => done(false);
    ov.querySelector(".ok").onclick = () => done(true);
    ov.addEventListener("click", (e) => { if (e.target === ov) done(false); });
    document.body.appendChild(ov);
  });
}
// Styled text-input dialog (replaces native prompt). Resolves to the trimmed value or null.
function promptDialog(title, placeholder = "", okLabel = "Create", initial = "") {
  return new Promise((resolve) => {
    const ov = document.createElement("div"); ov.className = "overlay show";
    ov.innerHTML = `<div class="modal confirm-box"><div class="mhead"><h3></h3><span class="close">×</span></div><div class="mbody"><input class="pdin" style="width:100%" /></div><div class="mfoot"><button class="cancel">Cancel</button><button class="primary ok">${okLabel}</button></div></div>`;
    ov.querySelector("h3").textContent = title;
    const input = ov.querySelector(".pdin");
    input.placeholder = placeholder; input.value = initial;
    const done = (v) => { animateClose(ov, true); resolve(v); };
    ov.querySelector(".close").onclick = () => done(null);
    ov.querySelector(".cancel").onclick = () => done(null);
    ov.querySelector(".ok").onclick = () => done(input.value.trim() || null);
    ov.addEventListener("click", (e) => { if (e.target === ov) done(null); });
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") done(input.value.trim() || null); if (e.key === "Escape") done(null); });
    document.body.appendChild(ov);
    setTimeout(() => input.focus(), 40);
  });
}

// ---------- window controls (frameless desktop app) ----------
function wMin(e){ e&&e.stopPropagation(); window.winMinimize&&window.winMinimize(); }
function wMax(e){ e&&e.stopPropagation(); window.winMaximize&&window.winMaximize(); }
function wClose(e){ e&&e.stopPropagation(); window.winClose&&window.winClose(); }
function setupWindowDrag(){
  const bar=document.querySelector(".topbar");
  if(!bar) return;
  bar.addEventListener("mousedown",(e)=>{
    if(e.button!==0 || e.target.closest(".dots")) return;
    if(window.winDrag) window.winDrag(); // start native window move
  });
  bar.addEventListener("dblclick",(e)=>{ if(!e.target.closest(".dots")) window.winMaximize&&window.winMaximize(); });
}

// ---------- wallet selector (searchable, grouped, checkboxes) ----------
function makeWalletSelect(containerId) {
  const root = $(containerId);
  const state = { wallets: [], selected: new Set(), expanded: new Set(), q: "" };
  root.innerHTML = `
    <div class="wsel-box"><span class="wsel-summary">All wallets</span>
      <svg class="i chev" viewBox="0 0 24 24"><polyline points="6 9 12 15 18 9"/></svg></div>
    <div class="wsel-panel">
      <div class="wsel-search"><div class="wsel-search-in">
        <svg class="i" viewBox="0 0 24 24"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/></svg>
        <input placeholder="Search name or address…" /></div></div>
      <div class="wsel-groups"></div>
    </div>`;
  const box = root.querySelector(".wsel-box"), panel = root.querySelector(".wsel-panel"),
        search = root.querySelector("input"), groupsEl = root.querySelector(".wsel-groups"),
        summary = root.querySelector(".wsel-summary");
  box.onclick = (e) => { e.stopPropagation(); const open = panel.classList.toggle("show"); box.classList.toggle("open", open); if (open) render(); };
  search.oninput = () => { state.q = search.value.toLowerCase(); render(); };
  panel.onclick = (e) => e.stopPropagation();
  document.addEventListener("click", () => { panel.classList.remove("show"); box.classList.remove("open"); });

  const groups = () => { const g = {}; state.wallets.forEach((w) => (g[w.group] = g[w.group] || []).push(w)); return g; };
  function updateSummary() { const n = state.selected.size; summary.textContent = n === 0 ? "All wallets" : `${n} wallet${n > 1 ? "s" : ""} selected`; }
  function render() {
    const g = groups(); let html = "";
    for (const [name, ws] of Object.entries(g)) {
      const filtered = state.q ? ws.filter((w) => (w.label + "-" + w.id).toLowerCase().includes(state.q) || w.address.toLowerCase().includes(state.q)) : ws;
      if (state.q && !filtered.length) continue;
      const sel = ws.filter((w) => state.selected.has(w.id)).length;
      const exp = state.expanded.has(name) || !!state.q;
      html += `<div class="wsel-grp">
        <div class="wsel-grp-head ${exp ? "exp" : ""}" data-grp="${name}">
          <svg class="i gchev" viewBox="0 0 24 24"><polyline points="9 6 15 12 9 18"/></svg>
          <input type="checkbox" data-allgrp="${name}" ${sel === ws.length ? "checked" : ""}/>
          <span>${name}</span><span class="cnt">${sel}/${ws.length}</span>
        </div>
        <div class="wsel-items ${exp ? "show" : ""}">
          ${filtered.map((w) => `<label class="wsel-item"><input type="checkbox" data-wid="${w.id}" ${state.selected.has(w.id) ? "checked" : ""}/> ${w.label}-${w.id} <span class="mono">${short(w.address)}</span></label>`).join("")}
        </div></div>`;
    }
    groupsEl.innerHTML = html || `<div class="muted" style="padding:12px">No wallets</div>`;
    groupsEl.querySelectorAll(".wsel-grp-head").forEach((h) => {
      h.onclick = (e) => { if (e.target.matches("input")) return; const n = h.dataset.grp; state.expanded.has(n) ? state.expanded.delete(n) : state.expanded.add(n); render(); };
    });
    groupsEl.querySelectorAll("input[data-allgrp]").forEach((cb) => {
      cb.onclick = (e) => { e.stopPropagation(); const ws = groups()[cb.dataset.allgrp]; const all = ws.every((w) => state.selected.has(w.id));
        ws.forEach((w) => (all ? state.selected.delete(w.id) : state.selected.add(w.id))); updateSummary(); render(); };
    });
    groupsEl.querySelectorAll("input[data-wid]").forEach((cb) => {
      cb.onclick = (e) => { e.stopPropagation(); const id = Number(cb.dataset.wid); cb.checked ? state.selected.add(id) : state.selected.delete(id); updateSummary(); render(); };
    });
  }
  return {
    async reload() { state.wallets = await api("/wallets"); const ids = new Set(state.wallets.map((w) => w.id)); state.selected = new Set([...state.selected].filter((id) => ids.has(id))); updateSummary(); render(); },
    selected() { return [...state.selected]; },
    allIds() { return state.wallets.map((w) => w.id); },
    setSelected(ids) { state.selected = new Set(ids || []); updateSummary(); render(); },
    clear() { state.selected.clear(); updateSummary(); render(); },
  };
}
// RPC endpoint multi-select for the task form — mirrors the wallet selector but lists
// RPC endpoints grouped by chain; selected() returns the chosen URLs (empty = chain default).
function makeRpcSelect(containerId) {
  const root = $(containerId);
  const state = { rpcs: [], selected: new Set(), expanded: new Set(), q: "" };
  root.innerHTML = `
    <div class="wsel-box"><span class="wsel-summary">Chain default</span>
      <svg class="i chev" viewBox="0 0 24 24"><polyline points="6 9 12 15 18 9"/></svg></div>
    <div class="wsel-panel">
      <div class="wsel-search"><div class="wsel-search-in">
        <svg class="i" viewBox="0 0 24 24"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/></svg>
        <input placeholder="Search name or URL…" /></div></div>
      <div class="wsel-groups"></div>
    </div>`;
  const box = root.querySelector(".wsel-box"), panel = root.querySelector(".wsel-panel"),
        search = root.querySelector("input"), groupsEl = root.querySelector(".wsel-groups"),
        summary = root.querySelector(".wsel-summary");
  box.onclick = (e) => { e.stopPropagation(); const open = panel.classList.toggle("show"); box.classList.toggle("open", open); if (open) render(); };
  search.oninput = () => { state.q = search.value.toLowerCase(); render(); };
  panel.onclick = (e) => e.stopPropagation();
  document.addEventListener("click", () => { panel.classList.remove("show"); box.classList.remove("open"); });
  const chainName = (id) => { const list = (typeof CHAINS!=="undefined" && CHAINS) || []; const c = list.find(x=>x.id===id); return c ? c.name : ("chain "+id); };
  const groups = () => { const g = {}; state.rpcs.forEach((e) => (g[chainName(e.chainId)] = g[chainName(e.chainId)] || []).push(e)); return g; };
  function updateSummary() { const n = state.selected.size; summary.textContent = n === 0 ? "Chain default" : `${n} of ${state.rpcs.length} endpoints`; }
  function render() {
    const g = groups(); let html = "";
    for (const [name, es] of Object.entries(g)) {
      const filtered = state.q ? es.filter((e) => e.name.toLowerCase().includes(state.q) || e.url.toLowerCase().includes(state.q)) : es;
      if (state.q && !filtered.length) continue;
      const sel = es.filter((e) => state.selected.has(e.url)).length;
      const exp = state.expanded.has(name) || !!state.q;
      html += `<div class="wsel-grp">
        <div class="wsel-grp-head ${exp ? "exp" : ""}" data-grp="${name}">
          <svg class="i gchev" viewBox="0 0 24 24"><polyline points="9 6 15 12 9 18"/></svg>
          <input type="checkbox" data-allgrp="${name}" ${sel === es.length ? "checked" : ""}/>
          <span>${name}</span><span class="cnt">${sel}/${es.length}</span>
        </div>
        <div class="wsel-items ${exp ? "show" : ""}">
          ${filtered.map((e) => `<label class="wsel-item"><input type="checkbox" data-url="${encodeURIComponent(e.url)}" ${state.selected.has(e.url) ? "checked" : ""}/> ${e.name} <span class="mono">${shortURL2(e.url)}</span></label>`).join("")}
        </div></div>`;
    }
    groupsEl.innerHTML = html || `<div class="muted" style="padding:12px">No RPC endpoints — add some on the RPC page</div>`;
    groupsEl.querySelectorAll(".wsel-grp-head").forEach((h) => {
      h.onclick = (e) => { if (e.target.matches("input")) return; const n = h.dataset.grp; state.expanded.has(n) ? state.expanded.delete(n) : state.expanded.add(n); render(); };
    });
    groupsEl.querySelectorAll("input[data-allgrp]").forEach((cb) => {
      cb.onclick = (e) => { e.stopPropagation(); const es = groups()[cb.dataset.allgrp]; const all = es.every((x) => state.selected.has(x.url));
        es.forEach((x) => (all ? state.selected.delete(x.url) : state.selected.add(x.url))); updateSummary(); render(); };
    });
    groupsEl.querySelectorAll("input[data-url]").forEach((cb) => {
      cb.onclick = (e) => { e.stopPropagation(); const u = decodeURIComponent(cb.dataset.url); cb.checked ? state.selected.add(u) : state.selected.delete(u); updateSummary(); render(); };
    });
  }
  return {
    async reload() { try { state.rpcs = await api("/rpc"); } catch { state.rpcs = []; } updateSummary(); render(); },
    selected() { return [...state.selected]; },
    setSelected(urls) { state.selected = new Set(urls || []); updateSummary(); render(); },
    clear() { state.selected.clear(); updateSummary(); render(); },
  };
}
function shortURL2(u){ try{ const x=new URL(u); return x.hostname + (x.pathname.length>1?"/…":""); }catch{ return u.length>28?u.slice(0,28)+"…":u; } }
let taskWS = null, nftWS = null, taskRS = null;
function ensureSelectors() { if (!taskWS) taskWS = makeWalletSelect("taskWsel"); if (!nftWS) nftWS = makeWalletSelect("nftWsel"); if (!taskRS && $("taskRsel")) taskRS = makeRpcSelect("taskRsel"); }

// ---------- nav ----------
const TABS = ["home","tasks","wallets","accounts","rpc","proxies","whitelists","nft","calculator","settings","logs"];
function go(tab) {
  document.querySelectorAll("#nav a").forEach((x) => x.classList.toggle("active", x.dataset.tab === tab));
  TABS.forEach((t) => $("tab-" + t).classList.toggle("hide", t !== tab));
  $("crumb").textContent = tab.toUpperCase();
  if (tab === "tasks") loadTasks();
  if (tab === "wallets") loadWallets();
  if (tab === "rpc") loadRPC();
  if (tab === "proxies") loadProxies();
  if (tab === "whitelists") loadWhitelistTab();
  if (tab === "home") loadHome();
  if (tab === "settings") { loadTelegram(); loadApiKeys(); loadSettingsPanel(); }
  if (tab === "calculator") renderCalc();
  if (tab === "logs") loadLogs();
  if (tab === "nft") { ensureSelectors(); nftWS.reload(); loadNftProxies(); }
}
document.querySelectorAll("#nav a").forEach((a) => a.addEventListener("click", () => go(a.dataset.tab)));

// ---------- modals ----------
function openModal(id) { $(id).classList.add("show"); }
// Animated close: play the out-animation, then hide (static) or remove (dynamic).
function animateClose(el, remove) {
  if (!el || el.classList.contains("closing")) return;
  el.classList.add("closing");
  setTimeout(() => { remove ? el.remove() : el.classList.remove("show", "closing"); }, 150);
}
function closeModal(id) { animateClose($(id)); }
document.querySelectorAll(".overlay").forEach((o) => o.addEventListener("click", (e) => { if (e.target === o) animateClose(o); }));

// ---------- status (vault is auto-managed; no password) ----------
let VER = "";
async function refreshStatus() { try { const s = await api("/status"); if (s && s.version) VER = s.version; return s; } catch { return {}; } }

// ---------- chains ----------
async function loadChains() {
  CHAINS = await api("/chains"); CHAINS.sort((a,b)=>a.id-b.id);
  const disabled=new Set((APP_CFG.chainsDisabled)||[]);   // hide chains turned off in Settings
  const opts = CHAINS.filter(c=>!disabled.has(c.id)).map((c)=>`<option value="${c.id}">${c.name} (${c.id})</option>`).join("");
  ["balChain","rpcChain","tChain","nftChain","walSendChain","meChain"].forEach((id)=>{ if($(id)) $(id).innerHTML = opts; });
}

// ---------- home / dashboard ----------
let HOME=null, PNL_UNIT="eth", HOME_WLABELS={};
function escHtml(s){ return String(s==null?"":s).replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c])); }
function shortAddr(a){ a=String(a||""); return a.length>12 ? a.slice(0,6)+"…"+a.slice(-4) : a; }
function weiToEthNum(wei){ try{ return Number(BigInt(String(wei||"0")))/1e18; }catch{ return 0; } }
function txExplorerUrl(chainId, hash){ if(!hash) return ""; const c=(CHAINS||[]).find(c=>c.id===chainId); return (c&&c.explorer)? c.explorer.replace(/\/+$/,"")+"/tx/"+hash : ""; }
function fmtPnlEth(eth){ const sign=eth>0?"+":(eth<0?"−":""); return `${sign}${Math.abs(eth).toLocaleString(undefined,{maximumFractionDigits:4})} Ξ`; }
function fmtPnlUsd(eth){ const u=(HOME&&HOME.ethUsd)||0; const v=eth*u; const sign=v>0?"+":(v<0?"−":""); return `${sign}$${Math.abs(v).toLocaleString(undefined,{maximumFractionDigits:2})}`; }
function fmtPnl(eth){ return PNL_UNIT==="usd" ? fmtPnlUsd(eth) : fmtPnlEth(eth); }
function pnlClass(eth){ return eth>0?"pos":(eth<0?"neg":""); }

async function loadHome() {
  const [home, ws, ts, rpc, tg] = await Promise.all([
    api("/home").catch(()=>null), api("/wallets"), api("/tasks"), api("/rpc"), api("/telegram").catch(()=>({running:false})),
  ]);
  HOME = home;
  HOME_WLABELS = {}; (ws||[]).forEach(w=>{ HOME_WLABELS[w.id]={label:w.label||("wallet-"+w.id), address:w.address}; });
  const card = (t,v,s)=>`<div class="card"><h2>${t}</h2><div style="font-size:26px;font-weight:700">${v}</div><div class="muted">${s||""}</div></div>`;
  $("homeCards").innerHTML =
    card("Wallets", ws.length, "stored") +
    card("Tasks", ts.length, ts.filter(t=>t.status==="running").length + " running") +
    card("RPC", rpc.length, "endpoints") +
    card("Telegram", tg.running ? "Live" : "Off", "remote control");
  renderHome();
}
function setPnlUnit(u){ PNL_UNIT=u; $("pnlEth").classList.toggle("seg-on",u==="eth"); $("pnlUsd").classList.toggle("seg-on",u==="usd"); renderPnl(); }
function renderPnl(){
  const eth = HOME ? weiToEthNum(HOME.pnlWei) : 0;
  const el = $("pnlValue"); if(el){ el.textContent = fmtPnl(eth); el.className = "pnl-value "+pnlClass(eth); }
  const plays = (HOME && HOME.topPlays) || [];
  const box = $("topPlays"); if(!box) return;
  box.innerHTML = plays.length ? plays.slice(0,8).map(p=>{
    const e = weiToEthNum(p.realizedWei);
    return `<div class="play-row"><span class="play-name">${escHtml(p.collection||p.contract)}</span><span class="${pnlClass(e)||'muted'}">${fmtPnl(e)}</span></div>`;
  }).join("") : `<div class="muted" style="padding:6px 0">No sales yet — realized PNL appears after you sell (Accept Offers).</div>`;
}
function renderHome(){
  renderPnl();
  $("mintedN").innerHTML = `&#10003; ${(HOME&&HOME.minted)||0} minted`;
  $("failedN").innerHTML = `&#10007; ${(HOME&&HOME.failed)||0} failed`;
  const rows = (HOME && HOME.activity) || [];
  $("actRows").innerHTML = rows.length ? rows.map(m=>{
    const t = new Date(m.ts).toLocaleTimeString([], {hour:"2-digit", minute:"2-digit"});
    const wl = HOME_WLABELS[m.walletId] || {label:"wallet", address:m.address};
    const tok = m.tokenId ? ("#"+escHtml(m.tokenId)) : "—";
    const url = txExplorerUrl(m.chainId, m.txHash);
    const act = url ? `<a href="${url}" target="_blank" rel="noopener" class="tx-link">View Transaction &#8599;</a>` : `<span class="muted">—</span>`;
    return `<tr><td class="act-time">${t}</td><td><b>${escHtml(wl.label)}</b><div class="act-addr">${shortAddr(m.address||wl.address)}</div></td><td class="act-tok">${tok}</td><td>${act}</td></tr>`;
  }).join("") : `<tr><td colspan="4" class="act-empty">No mints yet — run a task and successful mints show up here.</td></tr>`;
}
async function resetHome(){
  if(!await confirmDialog("Reset all PNL + activity history?\nThis clears every recorded mint and sale.","Reset")) return;
  try{ await api("/home/reset",{method:"POST"}); toast("Stats reset","success"); loadHome(); }catch(e){ toast(e.message,"error"); }
}
// Live-refresh the dashboard (throttled) when it's the open tab and a task/sale event lands.
let homeRefreshT=null;
function homeMaybeRefresh(){ const el=$("tab-home"); if(!el||el.classList.contains("hide"))return; clearTimeout(homeRefreshT); homeRefreshT=setTimeout(loadHome,1500); }

// ---------- wallets ----------
let WSEL = new Set(), SEND_WID = null;
// Persisted wallet groups so a freshly-created (empty) group survives until wallets are added.
let WALLET_GROUPS = new Set((()=>{ try { return JSON.parse(localStorage.getItem("walletGroups")||'["main"]'); } catch { return ["main"]; } })());
function saveWGroups(){ try { localStorage.setItem("walletGroups", JSON.stringify([...WALLET_GROUPS])); } catch {} }
async function newWGroup(){ const g=await promptDialog("New Wallet Group","Group name"); if(g){ WALLET_GROUPS.add(g); saveWGroups(); CUR_WGROUP=g; renderWallets(); toast(`Group "${g}" created — add wallets to it`,"success"); } }
async function loadWallets() { WALLETS = await api("/wallets"); renderWallets(); }
function pickWGroup(g){ CUR_WGROUP = (CUR_WGROUP === g) ? "" : g; renderWallets(); } // click again = show all
function shownWallets(){ return CUR_WGROUP ? WALLETS.filter(w=>w.group===CUR_WGROUP) : WALLETS; }
function renderWallets() {
  const groups = {}; WALLETS.forEach(w=>{ WALLET_GROUPS.add(w.group); groups[w.group]=(groups[w.group]||0)+1; });
  WALLET_GROUPS.forEach(g=>{ if(!(g in groups)) groups[g]=0; });   // show empty groups too
  $("walletGroups").innerHTML = Object.entries(groups).map(([g,n])=>
    `<div class="gcard ${g===CUR_WGROUP?'active':''}" onclick="pickWGroup('${g}')"><div class="gtitle">${IC.box} <span>${g}</span></div><div class="gcounts">${n} Wallets</div></div>`).join("")
    + `<div class="gcard add" onclick="newWGroup()">+ New Group</div>`;
  const shown = shownWallets();
  const idset=new Set(WALLETS.map(w=>w.id)); [...WSEL].forEach(id=>{ if(!idset.has(id)) WSEL.delete(id); }); // prune stale
  $("walletCount").textContent = CUR_WGROUP ? `· ${CUR_WGROUP} (${shown.length})` : `· ${WALLETS.length}`;
  $("walletRows").innerHTML = shown.map((w)=>{
    const sel=WSEL.has(w.id);
    return `
    <tr data-addr="${w.address}" class="${sel?'rowsel':''}">
      <td><input type="checkbox" ${sel?'checked':''} onchange="toggleWalletRow(${w.id},this.checked)" /></td>
      <td>${w.label}-${w.id}</td>
      <td class="mono">${short(w.address)} <span class="copyable" onclick="copy('${w.address}')">${IC.copy}</span></td>
      <td class="bal mono">—</td>
      <td class="acts" style="justify-content:flex-end">
        <button class="icon" title="Send funds" onclick="openWalSend(${w.id})">${IC.send}</button>
        <button class="icon danger" title="Delete" onclick="delWallet(${w.id})">${IC.trash}</button>
      </td></tr>`; }).join("") || `<tr><td colspan="5" class="muted" style="text-align:center;padding:24px">No wallets in this group</td></tr>`;
  const sa=$("walSelAll"); if(sa) sa.checked = shown.length>0 && shown.every(w=>WSEL.has(w.id));
  updateWalSel();
}
function toggleWalletRow(id,on){ on?WSEL.add(id):WSEL.delete(id); renderWallets(); }
function walSelectAll(on){ shownWallets().forEach(w=>{ on?WSEL.add(w.id):WSEL.delete(w.id); }); renderWallets(); }
function updateWalSel(){ const n=WSEL.size; const bar=$("walActbar"); if(bar) bar.style.display=n?"flex":"none"; const el=$("walSelInfo"); if(el) el.innerHTML=`<b>${n}</b> selected`; }
function selWallets(){ return WALLETS.filter(w=>WSEL.has(w.id)); }
function copyText(t){ try{ navigator.clipboard.writeText(t); }catch{ const ta=document.createElement("textarea"); ta.value=t; document.body.appendChild(ta); ta.select(); document.execCommand("copy"); ta.remove(); } }
function copySelAddrs(){ const a=selWallets().map(w=>w.address); if(!a.length)return; copyText(a.join("\n")); toast(`Copied ${a.length} address(es)`,"success"); }
async function copySelKeys(){
  const ids=[...WSEL]; if(!ids.length)return;
  if(!await confirmDialog(`Reveal & copy ${ids.length} PRIVATE KEY(S) to the clipboard? Anyone with these fully controls the wallets.`,"Copy keys")) return;
  try{
    const res=await api("/wallets/reveal",{method:"POST",body:JSON.stringify({ids,confirm:true})});
    if(!res.length) return toast("No keys returned","error");
    copyText(res.map(r=>r.privKey).join("\n")); toast(`Copied ${res.length} private key(s)`,"success");
  }catch(e){ toast(e.message,"error"); }
}
async function delSelWallets(){
  const ids=[...WSEL]; if(!ids.length)return;
  if(!await confirmDialog(`Delete ${ids.length} selected wallet(s)? This removes their encrypted keys.`,"Delete")) return;
  for(const id of ids){ await api("/wallets/"+id,{method:"DELETE"}).catch(()=>{}); }
  WSEL.clear(); loadWallets(); toast("Wallets deleted","info");
}
// ---- send funds (single wallet, native) ----
function openWalSend(id){
  const w=WALLETS.find(x=>x.id===id); if(!w) return;
  SEND_WID=id;
  $("walSendFrom").textContent=`From ${w.label}-${w.id} · ${short(w.address)}`;
  const bc=$("balChain"); if(bc && $("walSendChain")) $("walSendChain").value=bc.value;
  $("walSendTo").value=""; $("walSendAmt").value=""; $("walSendAmt").disabled=false; $("walSendMax").checked=false;
  openModal("walSendModal");
}
function onSendMaxToggle(){ const m=$("walSendMax").checked; $("walSendAmt").disabled=m; if(m) $("walSendAmt").value=""; }
async function doSendFunds(){
  if(SEND_WID==null) return;
  const to=$("walSendTo").value.trim(); if(!/^0x[0-9a-fA-F]{40}$/.test(to)) return toast("Invalid recipient address","error");
  const max=$("walSendMax").checked;
  const body={ chainId:Number($("walSendChain").value), to, max };
  if(!max){ const eth=$("walSendAmt").value.trim(); if(!eth||isNaN(+eth)||+eth<=0) return toast("Enter a valid amount","error"); body.amountWei=ethToWeiStr(+eth); }
  const btn=$("walSendBtn"); btn.disabled=true; btn.textContent="Sending…";
  try{
    const r=await api(`/wallets/${SEND_WID}/send`,{method:"POST",body:JSON.stringify(body)});
    closeModal("walSendModal"); toast(`Sent · tx ${r.txHash.slice(0,12)}…`,"success"); loadBalances();
  }catch(e){ toast(e.message,"error"); }
  finally{ btn.disabled=false; btn.textContent="Send"; }
}
function openGenModal(){ $("genGroup").value=CUR_WGROUP||"main"; openModal("genModal"); }
function openImpModal(){ $("impGroup").value=CUR_WGROUP||"main"; $("impKeys").value=""; openModal("impModal"); }
function rememberWGroup(g){ if(g){ WALLET_GROUPS.add(g); saveWGroups(); CUR_WGROUP=g; } }
async function genWallets(){ const group=$("genGroup").value.trim()||"main"; try{ const r=await api("/wallets/generate",{method:"POST",body:JSON.stringify({count:Number($("genCount").value),group})}); rememberWGroup(group); closeModal("genModal"); toast(`Created ${r.added} wallets`,"success"); loadWallets(); }catch(e){toast(e.message,"error");} }
async function importWallets(){ const keys=$("impKeys").value.split(/[\s,]+/).filter(Boolean); if(!keys.length)return toast("Paste at least one private key","info"); const group=$("impGroup").value.trim()||"main"; try{ const r=await api("/wallets/import",{method:"POST",body:JSON.stringify({privKeys:keys,group})}); rememberWGroup(group); closeModal("impModal"); toast(`Imported ${r.added} wallets`,"success"); $("impKeys").value=""; loadWallets(); }catch(e){toast(e.message,"error");} }
async function delWallet(id){ if(await confirmDialog("Delete this wallet?","Delete")){ await api("/wallets/"+id,{method:"DELETE"}); loadWallets(); toast("Wallet deleted","info"); } }
async function loadBalances(){
  try{ const res=await api("/wallets/balances",{method:"POST",body:JSON.stringify({chainId:Number($("balChain").value),group:CUR_WGROUP})});
    const map={}; res.forEach(r=>map[r.address.toLowerCase()]=r.err?"err":fmtEth(r.balanceWei));
    document.querySelectorAll("#walletRows tr").forEach(tr=>{ tr.querySelector(".bal").textContent=map[tr.dataset.addr.toLowerCase()]??"—"; });
  }catch(e){toast(e.message,"error");}
}
// ---------- manage funds (disperse / consolidate) ----------
let FUND_MODE="disperse", fundToSel=null, fundFromSel=null, FUND_RUN=null;
function pickFundMode(m){
  FUND_MODE=m;
  $("mDisperse").classList.toggle("active",m==="disperse");
  $("mConsolidate").classList.toggle("active",m==="consolidate");
  $("fDisperseFields").style.display = m==="disperse"?"":"none";
  $("fConsolidateFields").style.display = m==="consolidate"?"":"none";
  $("fundsBtn").textContent = m==="disperse"?"Disperse":"Consolidate";
}
function toggleConsAmount(){ $("fConsAmtRow").style.display = $("fMax").checked?"none":""; }
async function openFundsModal(){
  try{ WALLETS = await api("/wallets"); }catch{}
  const disabled=new Set((APP_CFG.chainsDisabled)||[]);
  $("fChain").innerHTML = (CHAINS||[]).filter(c=>!disabled.has(c.id)).map(c=>`<option value="${c.id}">${c.name} (${c.id})</option>`).join("");
  $("fFrom").innerHTML = `<option value="">— pick funder —</option>` + (WALLETS||[]).map(w=>`<option value="${w.id}">${w.label}-${w.id} · ${short(w.address)}</option>`).join("");
  if(!fundToSel) fundToSel = makeWalletSelect("fToSel");
  if(!fundFromSel) fundFromSel = makeWalletSelect("fFromSel");
  await fundToSel.reload(); await fundFromSel.reload();
  ["fToken","fAmount","fToAddr","fConsAmount"].forEach(id=>$(id)&&($(id).value=""));
  $("fMax").checked=true; toggleConsAmount();
  FUND_RUN=null; $("fundsBtn").disabled=false;
  $("fResults").style.display="none"; $("fResults").innerHTML="";
  pickFundMode("disperse");
  openModal("fundsModal");
}
const newRunId = ()=> "fn"+Date.now()+"-"+Math.floor(Math.random()*1e6); // unique per click/tab
const fmtAmt = (n)=> Number(n.toFixed(8)).toString();                    // strip JS float drift
const numOK = (v)=> v!=="" && !isNaN(+v) && +v>0;
async function doFunds(){
  if(FUND_RUN && !FUND_RUN.done) return toast("A transfer run is already in progress","info");
  const chainId=Number($("fChain").value)||1, token=($("fToken").value||"").trim();
  const unit=token?"tokens":"ETH";
  let req;
  if(FUND_MODE==="disperse"){
    const fromWalletId=Number($("fFrom").value)||0;
    // Explicit selection only — never disperse to "all wallets" by accident.
    let toWalletIds = (fundToSel ? fundToSel.selected() : []).filter(id=>id!==fromWalletId);
    const amountEth=($("fAmount").value||"").trim();
    if(!fromWalletId) return toast("Pick a funder wallet","info");
    if(!toWalletIds.length) return toast("Select recipient wallets (other than the funder)","info");
    if(!numOK(amountEth)) return toast("Enter a valid amount per wallet","info");
    const totalStr=fmtAmt(+amountEth*toWalletIds.length);
    if(!await confirmDialog(`Disperse ${amountEth} ${unit} to ${toWalletIds.length} wallet(s)?\n≈ ${totalStr} ${unit} total + gas (native ETH), from the funder.`,"Disperse")) return;
    req={ mode:"disperse", chainId, token, fromWalletId, toWalletIds, amountEth, runId:newRunId() };
  } else {
    // Explicit selection only — an empty selector must NOT sweep every wallet.
    let fromWalletIds = fundFromSel ? fundFromSel.selected() : [];
    const toAddress=($("fToAddr").value||"").trim();
    const max=$("fMax").checked, amountEth=($("fConsAmount").value||"").trim();
    if(!fromWalletIds.length) return toast("Select the source wallets to sweep","info");
    if(!/^0x[0-9a-fA-F]{40}$/.test(toAddress)) return toast("Enter a valid destination address","info");
    if(!max && !numOK(amountEth)) return toast("Enter a valid amount, or enable Sweep max","info");
    const msg = max ? `Sweep the FULL balance of ${fromWalletIds.length} wallet(s) to ${short(toAddress)}?`
                    : `Send ${amountEth} ${unit} from ${fromWalletIds.length} wallet(s) to ${short(toAddress)}?`;
    if(!await confirmDialog(msg,"Consolidate")) return;
    req={ mode:"consolidate", chainId, token, fromWalletIds, toAddress, max, amountEth, runId:newRunId() };
  }
  FUND_RUN={ id:req.runId, total:0, byIndex:{}, order:[], fatal:"", done:false };
  $("fResults").style.display="block"; renderFunds();
  $("fundsBtn").disabled=true;
  try{
    const r=await api("/funds/move",{method:"POST",body:JSON.stringify(req)});
    FUND_RUN.total=r.total||0; renderFunds();
  }catch(e){ toast(e.message,"error"); FUND_RUN.fatal=e.message; finishFundsRun(); renderFunds(); }
}
function stopFunds(){ if(FUND_RUN){ api("/funds/cancel",{method:"POST",body:JSON.stringify({runId:FUND_RUN.id})}).catch(()=>{}); toast("Stopping after the current transfer…","info"); } }
function finishFundsRun(){ if(FUND_RUN) FUND_RUN.done=true; $("fundsBtn").disabled=false; }
// WS push: one transfer finished.
function fundsOnResult(d){
  if(!FUND_RUN || d.runId!==FUND_RUN.id) return;
  if(d.total) FUND_RUN.total=d.total;
  if(d.fatal){ FUND_RUN.fatal=d.error||"failed"; finishFundsRun(); renderFunds(); toast(FUND_RUN.fatal,"error"); return; }
  if(!(d.index in FUND_RUN.byIndex)) FUND_RUN.order.push(d.index);
  FUND_RUN.byIndex[d.index]=d;
  if(FUND_RUN.total && FUND_RUN.order.length>=FUND_RUN.total) finishFundsRun();
  renderFunds();
}
function renderFunds(){
  if(!FUND_RUN) return;
  const rows=FUND_RUN.order.map(i=>FUND_RUN.byIndex[i]); let ok=0,fail=0;
  const body=rows.map(r=>{
    let stat;
    if(r.error){ fail++; stat=`<span style="color:var(--danger)">✗ ${r.error}</span>`; }
    else { ok++; stat=`<span style="color:var(--accent)">✓ ${r.txHash?short(r.txHash):"sent"}</span>`; }
    return `<div class="fundrow"><span class="mono">${short(r.from||"")} → ${short(r.to||"")}</span>${stat}</div>`;
  }).join("");
  const running = !FUND_RUN.done && !FUND_RUN.fatal;
  const stop = running ? ` · <span class="selclear" onclick="stopFunds()">Stop</span>` : "";
  const head = FUND_RUN.fatal
    ? `<div style="color:var(--danger);margin-bottom:6px">${FUND_RUN.fatal}</div>`
    : `<div class="muted" style="margin-bottom:6px">${rows.length}/${FUND_RUN.total||"?"} done · ${ok} ✓ · ${fail} ✗${stop}</div>`;
  $("fResults").innerHTML = head + (body || '<div class="muted">Starting…</div>');
}

// ---------- rpc ----------
async function loadRPC(){
  const es=await api("/rpc");
  $("rpcCount").textContent=`· ${es.length} Endpoints`;
  $("rpcRows").innerHTML=es.map(e=>{
    const c=CHAINS.find(x=>x.id===e.chainId);
    return `<tr><td><input type="checkbox"/></td><td>${e.name}</td><td>${c?c.name:e.chainId}</td>
      <td class="mono">${e.url}</td><td class="lat" data-url="${e.url}"><span class="muted">—</span></td>
      <td class="acts" style="justify-content:flex-end"><button class="icon danger" title="Delete" onclick="delRPC(${e.id})">${IC.trash}</button></td></tr>`;
  }).join("");
}
function openRpcModal(){ openModal("rpcModal"); }
async function addRPC(){ try{ await api("/rpc",{method:"POST",body:JSON.stringify({name:$("rpcName").value,chainId:Number($("rpcChain").value),url:$("rpcUrl").value})}); closeModal("rpcModal"); $("rpcUrl").value=""; loadRPC(); toast("RPC added","success"); }catch(e){toast(e.message,"error");} }
async function delRPC(id){ await api("/rpc/"+id,{method:"DELETE"}); loadRPC(); }
async function testRPC(){
  const urls=[...document.querySelectorAll("#rpcRows .lat")].map(td=>td.dataset.url); if(!urls.length)return;
  const res=await api("/rpc/test",{method:"POST",body:JSON.stringify({urls})});
  const map={}; res.forEach(p=>map[p.url]=p);
  document.querySelectorAll("#rpcRows .lat").forEach(td=>{ const p=map[td.dataset.url]; if(!p){td.innerHTML="—";return;}
    td.innerHTML = p.ok ? `<span style="color:var(--accent)">${p.latencyMs} ms</span>` : `<span style="color:var(--danger)">fail</span>`; });
}

// ---------- proxies ----------
let PROXIES = [], CUR_PGROUP = "";
let PROXY_GROUPS = new Set((()=>{ try { return JSON.parse(localStorage.getItem("proxyGroups")||'["default"]'); } catch { return ["default"]; } })());
function savePGroups(){ try { localStorage.setItem("proxyGroups", JSON.stringify([...PROXY_GROUPS])); } catch {} }
async function newPGroup(){ const g=await promptDialog("New Proxy Group","Group name"); if(g){ PROXY_GROUPS.add(g); savePGroups(); CUR_PGROUP=g; renderProxies(); } }
function pickPGroup(g){ CUR_PGROUP = (CUR_PGROUP===g)?"":g; renderProxies(); }
async function loadProxies(){ try{ PROXIES = await api("/proxies"); }catch{ PROXIES=[]; } renderProxies(); }
function shortProxy(u){ try{ const x=new URL(u); return (x.username?x.username+"@":"")+x.host; }catch{ return u; } }
function renderProxies(){
  const groups={}; PROXIES.forEach(p=>{ PROXY_GROUPS.add(p.group); groups[p.group]=(groups[p.group]||0)+1; });
  PROXY_GROUPS.forEach(g=>{ if(!(g in groups)) groups[g]=0; });
  $("proxyGroups").innerHTML = Object.entries(groups).map(([g,n])=>
    `<div class="gcard ${g===CUR_PGROUP?'active':''}" onclick="pickPGroup('${g}')"><div class="gtitle">${IC.box} <span>${g}</span></div><div class="gcounts">${n} Proxies</div></div>`).join("")
    + `<div class="gcard add" onclick="newPGroup()">+ New Group</div>`;
  const shown = CUR_PGROUP ? PROXIES.filter(p=>p.group===CUR_PGROUP) : PROXIES;
  $("proxyCount").textContent = CUR_PGROUP ? `· ${CUR_PGROUP} (${shown.length})` : `· ${PROXIES.length}`;
  $("proxyRows").innerHTML = shown.map(p=>`
    <tr data-url="${encodeURIComponent(p.url)}">
      <td class="mono">${shortProxy(p.url)}</td>
      <td>${p.group}</td>
      <td class="pstat muted">—</td>
      <td class="acts" style="justify-content:flex-end"><button class="icon danger" title="Delete" onclick="delProxy(${p.id})">${IC.trash}</button></td>
    </tr>`).join("") || `<tr><td colspan="4" class="muted" style="text-align:center;padding:24px">Group is empty — click Add Proxies</td></tr>`;
}
function parseProxyLine(s){ s=(s||"").trim(); if(!s||s.startsWith("#")) return null; if(s.includes("://")) return s; const p=s.split(":"); return (p.length===2||p.length===4)?s:null; }
function updateProxyCount(){ const n=($("proxyLines").value||"").split("\n").filter(l=>parseProxyLine(l)).length; $("proxyParseCount").textContent=`${n} parseable line${n!==1?'s':''}`; $("proxyAddBtn").textContent=`Add ${n}`; }
function openProxyModal(){ $("proxyGroupName").value=CUR_PGROUP||"default"; $("proxyLines").value=""; updateProxyCount(); openModal("proxyModal"); }
async function addProxies(){ const lines=$("proxyLines").value, group=$("proxyGroupName").value.trim()||"default"; try{ const r=await api("/proxies",{method:"POST",body:JSON.stringify({lines,group})}); PROXY_GROUPS.add(group); savePGroups(); CUR_PGROUP=group; closeModal("proxyModal"); toast(`Added ${r.added} prox${r.added===1?'y':'ies'}`,"success"); loadProxies(); }catch(e){ toast(e.message,"error"); } }
async function delProxy(id){ if(await confirmDialog("Delete this proxy?","Delete")){ await api("/proxies/"+id,{method:"DELETE"}); loadProxies(); } }
async function testProxies(){
  const urls=[...document.querySelectorAll("#proxyRows tr")].map(tr=>tr.dataset.url).filter(Boolean).map(u=>decodeURIComponent(u));
  if(!urls.length) return;
  document.querySelectorAll("#proxyRows .pstat").forEach(td=>{ td.textContent="testing…"; td.className="pstat muted"; td.style.color=""; });
  try{ const res=await api("/proxies/test",{method:"POST",body:JSON.stringify({urls})});
    const map={}; res.forEach(r=>map[r.url]=r);
    document.querySelectorAll("#proxyRows tr").forEach(tr=>{ const r=map[decodeURIComponent(tr.dataset.url)]; const td=tr.querySelector(".pstat"); if(!r||!td) return;
      if(r.ok){ td.textContent=`${r.ms}ms · ${r.ip||"ok"}`; td.className="pstat"; td.style.color="var(--accent)"; }
      else { td.textContent=r.error||"failed"; td.className="pstat"; td.style.color="var(--danger)"; } });
  }catch(e){ toast(e.message,"error"); }
}
function fillProxyGroups(){ const sel=$("tProxyGroup"); if(!sel) return; const cur=sel.value; const gs=new Set(["",...PROXY_GROUPS]); PROXIES.forEach(p=>gs.add(p.group)); sel.innerHTML=[...gs].map(g=>`<option value="${g}">${g||"none"}</option>`).join(""); sel.value=cur; }

// ---------- whitelist checker ----------
let wlWS = null;
async function loadWhitelistTab(){
  if(!wlWS && $("wlWsel")) wlWS = makeWalletSelect("wlWsel");
  if(wlWS) await wlWS.reload();
  try{ PROXIES = await api("/proxies"); }catch{}
  const sel=$("wlProxy"); if(sel){ const cur=sel.value; const gs=new Set(["",...PROXY_GROUPS]); PROXIES.forEach(p=>gs.add(p.group)); sel.innerHTML=[...gs].map(g=>`<option value="${g}">${g||"none"}</option>`).join(""); sel.value=cur; }
}
let WL_RUN = null; // { id, total, slug, byId:{}, order:[] } — current/last check
async function wlCheck(){
  const link=($("wlLink").value||"").trim(); if(!link) return toast("Paste a drop link or contract address","info");
  let walletIds = wlWS ? wlWS.selected() : [];
  if(!walletIds.length && wlWS) walletIds = wlWS.allIds(); // empty selection = all wallets
  if(!walletIds.length) return toast("No wallets — add some first","info");
  const proxyGroup = ($("wlProxy")||{}).value || "";
  const threads = Number($("wlThreads").value)||5;
  const runId = "wl"+Date.now();
  WL_RUN = { id: runId, total: walletIds.length, slug:"", byId:{}, order:[] };
  $("wlResultCard").style.display="block";
  $("wlThead").innerHTML=""; $("wlRows").innerHTML="";
  renderWLLive(); // shows "0/N checked" while results stream in over the WS
  const btn=$("wlCheckBtn"); btn.disabled=true; btn.textContent="Checking…";
  try{
    const r=await api("/whitelist/check",{method:"POST",body:JSON.stringify({link, walletIds, proxyGroup, threads, runId})});
    // Authoritative final pass (covers any WS messages dropped by a slow client).
    if(WL_RUN && WL_RUN.id===runId){
      WL_RUN.slug=r.slug||WL_RUN.slug;
      (r.wallets||[]).forEach(w=>{ if(!(w.walletId in WL_RUN.byId)) WL_RUN.order.push(w.walletId); WL_RUN.byId[w.walletId]=w; });
      renderWLLive(true);
    }
  }catch(e){ toast(e.message,"error"); }
  finally{ btn.disabled=false; btn.textContent="Check"; }
}
// WS push: one wallet finished checking — drop its row in immediately.
function wlOnResult(d){
  if(!WL_RUN || d.runId!==WL_RUN.id) return;
  if(d.total) WL_RUN.total=d.total; // backend's filtered count is authoritative
  if(d.slug) WL_RUN.slug=d.slug;
  if(!(d.walletId in WL_RUN.byId)) WL_RUN.order.push(d.walletId);
  WL_RUN.byId[d.walletId]=d;
  renderWLLive();
}
function wlStageLabel(c, nonPubCount){ return c.stageType==="PUBLIC_SALE" ? "Public stage" : (nonPubCount>1 ? "WL #"+c.stageIndex : "WL"); }
function renderWLLive(done){
  if(!WL_RUN) return;
  const results = WL_RUN.order.map(id=>WL_RUN.byId[id]);
  const byIdx={}; results.forEach(w=>(w.stages||[]).forEach(s=>byIdx[s.stageIndex]={stageType:s.stageType, stageIndex:s.stageIndex}));
  const cols=Object.values(byIdx).sort((a,b)=>{ const ap=a.stageType==="PUBLIC_SALE"?1:0, bp=b.stageType==="PUBLIC_SALE"?1:0; return ap-bp || a.stageIndex-b.stageIndex; });
  const nonPub=cols.filter(c=>c.stageType!=="PUBLIC_SALE").length;
  $("wlThead").innerHTML = `<tr><th>Wallet</th>${cols.map(c=>`<th>${wlStageLabel(c,nonPub)}</th>`).join("")}</tr>`;
  let eligibleAny=0;
  let rowsHtml = results.map(w=>{
    const nameCell=`<td><div class="two"><span>${w.label||"wallet"}-${w.walletId}</span><span class="sm2 mono">${short(w.address)}</span></div></td>`;
    if(w.error) return `<tr>${nameCell}<td colspan="${cols.length||1}" class="mono" style="color:var(--danger)">${w.error}</td></tr>`;
    const sByIdx={}; (w.stages||[]).forEach(s=>sByIdx[s.stageIndex]=s);
    let cum=0, anyElig=false;
    const cells=cols.map(c=>{ const s=sByIdx[c.stageIndex];
      if(s && s.isEligible){ cum += (s.eligibleMaxTotalMintableByWallet||s.maxTotalMintableByWallet||0); anyElig=true; return `<td><span style="color:var(--accent)">✓ ${cum}</span></td>`; }
      return `<td class="muted">—</td>`;
    }).join("");
    if(anyElig) eligibleAny++;
    return `<tr>${nameCell}${cells}</tr>`;
  }).join("");
  const checked=results.length, total=WL_RUN.total;
  if(checked<total) rowsHtml += `<tr><td colspan="${(cols.length||0)+1}" class="muted" style="padding:14px;text-align:center">Checking ${checked}/${total}… <span class="spin">◠</span></td></tr>`;
  $("wlRows").innerHTML = rowsHtml || `<tr><td class="muted" style="padding:24px;text-align:center">No wallets</td></tr>`;
  $("wlSummary").textContent = `· ${WL_RUN.slug||""} · ${checked}/${total} checked · ${eligibleAny} eligible`;
}

// ---------- tasks ----------
let TASK_SEL = new Set(), LAST_TASK_KEYS = [], MASS_IDS = [];
let TASK_GROUPS = new Set((()=>{ try { return JSON.parse(localStorage.getItem("taskGroups")||'["Imported"]'); } catch { return ["Imported"]; } })());
function saveGroups(){ try { localStorage.setItem("taskGroups", JSON.stringify([...TASK_GROUPS])); } catch {} }
async function loadTasks(){ const ts=await api("/tasks"); TASKS={}; ts.forEach(t=>TASKS[t.id]=t); if(!WALLETS.length){ try{ WALLETS=await api("/wallets"); }catch{} } renderTasks(); }
function taskStatusHTML(r){
  if(r.status==="running") return `<span class="st s-running"><svg class="i spin" viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-6.2-8.5"/></svg>${(r.detail||"Running").slice(0,28)}</span>`;
  const label = r.status==="idle" ? "Ready" : (r.detail ? r.detail.slice(0,28) : r.status);
  return `<span class="st s-${r.status}"><span class="dot"></span>${label}</span>`;
}
function renderTasks(){
  const all=Object.values(TASKS).sort((a,b)=>a.id-b.id);
  // Each wallet-row counts as a "task" (one wallet doing one action), matching the
  // Zyper model — so a config targeting 5 wallets shows as 5 tasks, not 1.
  const evmCount=WALLETS.filter(w=>w.network==="evm").length;
  const rowCount=t=>(t.walletIds&&t.walletIds.length)?t.walletIds.length:(evmCount||(t.wallets||[]).length);
  const groups={};
  all.forEach(t=>{ TASK_GROUPS.add(t.group); const g=groups[t.group]=groups[t.group]||{tasks:0,run:0,fail:0};
    g.tasks+=rowCount(t); const w=t.wallets||[]; g.run+=w.filter(x=>x.status==="running").length; g.fail+=w.filter(x=>x.status==="failed").length; });
  $("taskGroups").innerHTML = [...TASK_GROUPS].map(g=>{ const s=groups[g]||{tasks:0,run:0,fail:0};
    return `<div class="gcard ${g===CUR_GROUP?'active':''}" onclick="pickGroup('${g}')">
       <div class="gtitle">${IC.box} <span>${g}</span><span class="gdel" title="Delete group" onclick="delGroup('${g}',event)">${IC.trash}</span></div>
       <div class="gcounts"><span>${s.tasks} tasks</span><span class="ok">${s.run} running</span><span style="color:var(--danger)">${s.fail} failed</span></div></div>`; }).join("")
    + `<div class="gcard add" onclick="newGroup()">+ New Group</div>`;
  // per-wallet rows for the active group
  const groupTasks=all.filter(t=>t.group===CUR_GROUP);
  const rows=[];
  groupTasks.forEach(t=>{
    const live={}; (t.wallets||[]).forEach(w=>live[w.walletId]=w);
    let wlist = (t.walletIds && t.walletIds.length) ? WALLETS.filter(w=>t.walletIds.includes(w.id)) : WALLETS.filter(w=>w.network==="evm");
    if(!wlist.length && (t.wallets||[]).length) wlist=t.wallets.map(w=>({id:w.walletId,label:"wallet",address:w.address}));
    const N=wlist.length || (t.wallets||[]).length;
    wlist.forEach(w=>{ const lv=live[w.id]||{};
      rows.push({ key:t.id+":"+w.id, taskId:t.id, walletId:w.id, name:`${N} / ${w.label}-${w.id}`, address:lv.address||w.address||"",
        mode:t.mode, seadrop:t.seadrop, status:lv.status||"idle", detail:lv.detail||"", gasFee:lv.gasFee||"", value:t.valueWei||"0" }); });
  });
  // Order rows by wallet (stable), not by task id — so a wallet edited/split into a new
  // (higher-id) task keeps its position instead of jumping to the bottom of the list.
  rows.sort((a,b)=> a.walletId - b.walletId || a.taskId - b.taskId);
  LAST_TASK_KEYS=rows.map(r=>r.key);
  const keys=new Set(LAST_TASK_KEYS); [...TASK_SEL].forEach(k=>{ if(!keys.has(k)) TASK_SEL.delete(k); });
  $("curGroup").textContent=CUR_GROUP; $("taskCount").textContent=`· ${rows.length} Tasks`;
  $("taskRows").innerHTML = rows.map(r=>{
    const sel=TASK_SEL.has(r.key), modeCls=r.mode==="spam"?"blue":"green";
    return `<tr class="${sel?'rowsel':''}">
      <td><input type="checkbox" ${sel?'checked':''} onchange="toggleTaskRow('${r.key}',this.checked)" /></td>
      <td><div class="two"><span>${r.name}</span><span class="sm2 mono">${short(r.address)}</span></div></td>
      <td class="mono">${r.gasFee||"auto / auto"}</td>
      <td>${r.value}</td>
      <td><span class="pill ${modeCls}">${r.mode}${r.seadrop?" · seadrop":""}</span></td>
      <td>${taskStatusHTML(r)}</td>
      <td class="acts" style="justify-content:flex-end">
        <button class="icon" title="Run this wallet" onclick="taskAction(${r.taskId},'start',${r.walletId})">${IC.play}</button>
        <button class="icon" title="Stop this wallet" onclick="taskAction(${r.taskId},'stop',${r.walletId})">${IC.stop}</button>
        <button class="icon" title="Boost (whole task)" onclick="taskAction(${r.taskId},'boost')">${IC.boost}</button>
        <button class="icon" title="Edit this wallet" onclick="openTaskEdit(${r.taskId},${r.walletId})">${IC.edit}</button>
        <button class="icon danger" title="Delete task" onclick="delTask(${r.taskId})">${IC.trash}</button>
      </td></tr>`;
  }).join("") || `<tr><td colspan="7" class="muted" style="text-align:center;padding:30px">No wallets — add wallets or create a task with New Task</td></tr>`;
  $("taskGroupInfo").textContent=`${rows.length} items shown · live`;
  const sa=$("taskSelAll"); if(sa) sa.checked = rows.length>0 && TASK_SEL.size===rows.length;
  updateTaskSel();
}
function distinctTaskIds(){ return [...new Set([...TASK_SEL].map(k=>Number(k.split(":")[0])))]; }
function updateTaskSel(){
  const el=$("taskSelInfo"); if(el) el.innerHTML=`<b>${TASK_SEL.size}</b> selected`;
  const bar=$("taskSelbar"); if(bar) bar.style.display=TASK_SEL.size?"flex":"none";
  const n=$("taskSelN"); if(n) n.textContent=TASK_SEL.size;   // each selected row = a task
}
function toggleTaskRow(key,on){ on?TASK_SEL.add(key):TASK_SEL.delete(key); renderTasks(); }
function taskSelectAll(on){ TASK_SEL.clear(); if(on) LAST_TASK_KEYS.forEach(k=>TASK_SEL.add(k)); renderTasks(); }
// Bulk actions on the DISTINCT tasks behind the selected wallet rows (red-box toolbar).
async function bulkTask(action){
  const keys=[...TASK_SEL]; if(!keys.length) return toast("Select task rows first","info");
  const n=keys.length;
  let ok=0;
  if(action==="boost"){ // boost rebroadcasts pending tx — whole task
    for(const id of distinctTaskIds()){ try{ await api(`/tasks/${id}/boost`,{method:"POST"}); ok++; }catch{} }
  }else{ // start/stop only the selected wallets (not the whole task)
    for(const k of keys){ const [tid,wid]=k.split(":"); try{ await api(`/tasks/${tid}/${action}?wallet=${wid}`,{method:"POST"}); ok++; }catch{} }
  }
  toast(ok?`${action} → ${n} wallet(s)`:`${action} failed`, ok?"success":"error");
}
async function bulkDeleteTasks(){
  const ids=distinctTaskIds(); if(!ids.length) return;
  if(!await confirmDialog(`Delete ${ids.length} selected task(s)?`,"Delete")) return;
  for(const id of ids){ await api("/tasks/"+id,{method:"DELETE"}).catch(()=>{}); }
  TASK_SEL.clear(); loadTasks(); toast("Tasks deleted","info");
}
async function delGroup(g, ev){ if(ev) ev.stopPropagation();
  const ids=Object.values(TASKS).filter(t=>t.group===g).map(t=>t.id);
  if(ids.length && !await confirmDialog(`Delete group "${g}" and its ${ids.length} task(s)?`,"Delete")) return;
  for(const id of ids){ await api("/tasks/"+id,{method:"DELETE"}).catch(()=>{}); }
  TASK_GROUPS.delete(g); saveGroups(); if(CUR_GROUP===g) CUR_GROUP=[...TASK_GROUPS][0]||"Imported"; TASK_SEL.clear(); loadTasks();
}
// Mass edit: edit the distinct tasks of the selected wallet rows.
let MASS_SEL=0;
function editSelected(){
  MASS_IDS=[...new Set([...TASK_SEL].map(k=>Number(k.split(":")[0])))];
  if(!MASS_IDS.length) return toast("Select rows first — their tasks will be edited","info");
  MASS_SEL=TASK_SEL.size;  // report by selected rows (tasks), not config count
  ["meContract","meChain","meFn","meParams","meValue","meMode","meStart","meGroup","meGasMode","meMaxFee","mePrio","meDelay","meMulti","meFlash"]
    .forEach(id=>{ const cb=$(id+"On"); if(cb)cb.checked=false; });
  $("massCount").textContent=`(${MASS_SEL} task${MASS_SEL>1?'s':''})`;
  $("massApplyBtn").textContent=`Apply to ${MASS_SEL}`;
  openModal("massEditModal");
}
async function applyMassEdit(){
  if(!MASS_IDS.length) return;
  let ok=0, fail=0;
  for(const id of MASS_IDS){
    let cfg; try{ cfg=await api("/tasks/"+id); }catch{ fail++; continue; }
    cfg.gas=cfg.gas||{mode:"auto"};
    if($("meContractOn").checked) cfg.contractAddress=$("meContract").value.trim();
    if($("meChainOn").checked) cfg.chainId=Number($("meChain").value);
    if($("meFnOn").checked){ cfg.functionSig=$("meFn").value.trim(); cfg.hexMode=false; }
    if($("meParamsOn").checked) cfg.params=$("meParams").value.split(";").map(s=>s.trim()).filter(s=>s!=="");
    if($("meValueOn").checked) cfg.valueWei=$("meValue").value.trim()||"0";
    if($("meModeOn").checked) cfg.mode=$("meMode").value;
    if($("meStartOn").checked) cfg.startAt=Number($("meStart").value)||0;
    if($("meGroupOn").checked) cfg.group=$("meGroup").value.trim()||cfg.group;
    if($("meGasModeOn").checked) cfg.gas.mode=$("meGasMode").value;
    if($("meMaxFeeOn").checked) cfg.gas.maxFeeGwei=Number($("meMaxFee").value)||0;
    if($("mePrioOn").checked) cfg.gas.priorityFeeGwei=Number($("mePrio").value)||0;
    if($("meDelayOn").checked) cfg.delayMs=Number($("meDelay").value)||0;
    if($("meMultiOn").checked) cfg.multiRpc=$("meMulti").value==="true";
    if($("meFlashOn").checked) cfg.flashbots=$("meFlash").value==="true";
    try{ await api("/tasks/"+id,{method:"PUT",body:JSON.stringify(cfg)}); ok++; }catch{ fail++; }
  }
  closeModal("massEditModal"); loadTasks(); toast(`Edited ${MASS_SEL} task(s)${fail?` · ${fail} config(s) skipped (running?)`:""}`, fail?"info":"success");
}
async function deleteGroupTasks(){ const ids=Object.values(TASKS).filter(t=>t.group===CUR_GROUP).map(t=>t.id); if(!ids.length) return; if(!await confirmDialog(`Delete all ${ids.length} task(s) in ${CUR_GROUP}?`,"Delete")) return; for(const id of ids){ await api("/tasks/"+id,{method:"DELETE"}).catch(()=>{}); } loadTasks(); toast("Tasks deleted","info"); }
function pickGroup(g){ CUR_GROUP=g; TASK_SEL.clear(); renderTasks(); }
async function newGroup(){ const g=await promptDialog("New Group","Group name"); if(g){ TASK_GROUPS.add(g); saveGroups(); CUR_GROUP=g; TASK_SEL.clear(); renderTasks(); } }
async function taskAction(id,action,walletId){ const q=walletId?`?wallet=${walletId}`:""; try{ await api(`/tasks/${id}/${action}${q}`,{method:"POST"}); if(action==="boost") toast("Boost — rebroadcasting pending tx with higher gas (same nonce)","info"); }catch(e){toast(e.message,"error");} }
async function delTask(id){ if(await confirmDialog("Delete task?","Delete")){ await api("/tasks/"+id,{method:"DELETE"}); loadTasks(); toast("Task deleted","info"); } }
async function startGroup(){ try{ await api(`/tasks/group/${encodeURIComponent(CUR_GROUP)}/start`,{method:"POST"}); }catch(e){toast(e.message,"error");} }
async function stopGroup(){ try{ await api(`/tasks/group/${encodeURIComponent(CUR_GROUP)}/stop`,{method:"POST"}); }catch(e){toast(e.message,"error");} }
async function boostGroup(){ Object.values(TASKS).filter(t=>t.group===CUR_GROUP).forEach(t=>api(`/tasks/${t.id}/boost`,{method:"POST"}).catch(()=>{})); }

// create / edit task modal
let TASK_MODE="simulate", EDIT_ID=null, EDIT_CFG=null, EDIT_WALLET=null, SEADROP_ON=false, _linkTimer=null, PHASES=[];
function toggleHex(){ const h=$("tHex").checked; $("hexFld").classList.toggle("hide",!h); }
// ---- ABI helper: paste or fetch a contract ABI, pick a function from a dropdown ----
let ABI_FNS=[];
function toggleAbi(){ const b=$("abiBlock"); if(b){ b.classList.toggle("hide"); if(!b.classList.contains("hide")) setTimeout(()=>$("tAbi")&&$("tAbi").focus(),0); } }
function onAbiInput(){
  const raw=($("tAbi").value||"").trim(), fld=$("abiFnFld"), sel=$("tAbiFn"); ABI_FNS=[];
  let abi=null; if(raw){ try{ abi=JSON.parse(raw); }catch{} }
  if(abi && !Array.isArray(abi) && Array.isArray(abi.abi)) abi=abi.abi;   // some explorers wrap {abi:[...]}
  if(!Array.isArray(abi)){ if(fld)fld.classList.add("hide"); return; }
  // Only state-changing functions can be sent as a task (skip view/pure).
  ABI_FNS = abi.filter(x=>x.type==="function" && (x.stateMutability==="payable"||x.stateMutability==="nonpayable"||x.stateMutability===undefined));
  if(!ABI_FNS.length){ if(fld)fld.classList.add("hide"); return; }
  sel.innerHTML = `<option value="">— pick a function (${ABI_FNS.length}) —</option>` + ABI_FNS.map((f,i)=>{
    const args=(f.inputs||[]).map(p=>p.type+(p.name?` ${p.name}`:"")).join(", ");
    const pay=f.stateMutability==="payable"?" · payable":"";
    return `<option value="${i}">${f.name}(${args})${pay}</option>`;
  }).join("");
  fld.classList.remove("hide");
}
function onAbiFnPick(){
  const i=$("tAbiFn").value; if(i==="") return;
  const f=ABI_FNS[Number(i)]; if(!f) return;
  $("tFn").value = `${f.name}(${(f.inputs||[]).map(p=>p.type).join(",")})`;   // canonical signature
  $("tHex").checked=false; toggleHex();
  const names=(f.inputs||[]).map(p=>p.name||p.type);
  $("tParams").placeholder = names.length ? names.join(";")+"  ({address}=wallet)" : "(no params)";
}
async function fetchAbi(ev){
  const addr=($("tContract").value||"").trim();
  if(!/^0x[0-9a-fA-F]{40}$/.test(addr)) return toast("Enter a contract address first","info");
  const btn=ev&&ev.target; if(btn){ btn.disabled=true; btn.textContent="Fetching…"; }
  try{
    const r=await api("/contract/abi",{method:"POST",body:JSON.stringify({chainId:Number($("tChain").value),address:addr})});
    if(r.abi){ const b=$("abiBlock"); if(b) b.classList.remove("hide"); $("tAbi").value=r.abi; onAbiInput(); toast(ABI_FNS.length?`ABI loaded — ${ABI_FNS.length} function(s)`:"ABI loaded","success"); }
  }catch(e){ toast(e.message,"error"); }
  finally{ if(btn){ btn.disabled=false; btn.textContent="Fetch ABI"; } }
}
// ---- mint helpers (price <-> wei, time formatting) ----
function weiToEthStr(wei){ try{ return (Number(BigInt(wei||"0"))/1e18).toString(); }catch{ return "0"; } }
function ethToWeiStr(eth){
  let s=String(eth); if(s.indexOf("e")>=0) s=Number(eth).toFixed(18);
  if(s.startsWith("-")) s=s.slice(1);
  let [i,f=""]=s.split("."); f=(f+"000000000000000000").slice(0,18);
  return (i+f).replace(/^0+/,"")||"0";
}
function fmtLocal(unix){ try{ return new Date(unix*1000).toLocaleString(); }catch{ return ""; } }
function relTime(unix){ const d=unix-Math.floor(Date.now()/1000); if(d<=0) return "live now"; const D=Math.floor(d/86400),H=Math.floor(d%86400/3600),M=Math.floor(d%3600/60); let s="in "; if(D)s+=D+"d "; if(H||D)s+=H+"h "; return s+M+"m"; }
// General Start Time field (any task can be scheduled to wait until this unix time).
function updateStartHint(){ const el=$("tStartHint"); if(!el) return; const ts=Number(($("tStartAt")||{}).value||0); el.textContent = ts ? (fmtLocal(ts)+" · "+relTime(ts)) : "fires immediately"; }
function setStartNow(){ if($("tStartAt")){ $("tStartAt").value=Math.floor(Date.now()/1000); updateStartHint(); } }
function onPhaseChange(){
  if(!PHASES.length) return;
  const i=Number(($("tMintPhase")||{}).value||0), p=PHASES[i]||PHASES[0];
  if($("tMintPrice")) $("tMintPrice").value=(+p.priceEth||0);
  if($("tStartAt")){ $("tStartAt").value=p.startUnix||""; updateStartHint(); }       // phase start -> general Start Time
  const ph=$("tPhaseStart"); if(ph) ph.textContent = p.startUnix ? ("opens "+fmtLocal(p.startUnix)+" · "+relTime(p.startUnix)) : "";
}
// Paste into the contract field -> auto-detect what it is:
//   - a tx hash / explorer /tx/ link  -> replay that transaction (contract+fn+params+value)
//   - an OpenSea link/slug or a bare contract address -> resolve + detect SeaDrop
const EXPLORER_CHAINS={ "etherscan.io":1, "basescan.org":8453, "optimistic.etherscan.io":10, "bscscan.com":56, "polygonscan.com":137, "lineascan.build":59144, "abscan.org":2741, "apescan.io":33139, "sepolia.etherscan.io":11155111 };
function inferChainFromLink(s){
  let host=""; try{ host=new URL(s).hostname.toLowerCase(); }catch{ host=(s||"").toLowerCase(); }
  if(EXPLORER_CHAINS[host]!==undefined) return EXPLORER_CHAINS[host];
  // suffix match, MOST-SPECIFIC (longest) first so "optimistic.etherscan.io" beats "etherscan.io"
  for(const d of Object.keys(EXPLORER_CHAINS).sort((a,b)=>b.length-a.length)){ if(host.endsWith(d)) return EXPLORER_CHAINS[d]; }
  return 0;
}
function onContractInput(){
  const v=$("tContract").value.trim();
  const isTxHash=/^0x[0-9a-fA-F]{64}$/.test(v);
  const isTxLink=/\/tx\/0x[0-9a-fA-F]{64}/i.test(v);
  if(isTxHash || isTxLink){ clearTimeout(_linkTimer); _linkTimer=setTimeout(resolveTxReplay, 500); return; }
  const isAddr=/^0x[0-9a-fA-F]{40}$/.test(v), isLink=/opensea\.io/i.test(v);
  if(!isAddr && !isLink) return;
  clearTimeout(_linkTimer); _linkTimer=setTimeout(resolveTaskLink, 500);
}
// Replay a pasted tx: fill contract, value, function + params (or raw Hex calldata).
async function resolveTxReplay(){
  const raw=$("tContract").value.trim(); const m=raw.match(/0x[0-9a-fA-F]{64}/); if(!m) return;
  const chainId=inferChainFromLink(raw)||Number($("tChain").value)||1;
  try{
    const r=await api("/contract/tx",{method:"POST",body:JSON.stringify({hash:m[0],chainId})});
    if(!r.contractAddress) return;
    SEADROP_ON=false; PHASES=[];
    const hint=$("taskNftHint"); if(hint){ hint.style.display="none"; hint.innerHTML=""; }
    const fn=$("fnRow"), pr=$("paramsRow"); if(fn) fn.style.display=""; if(pr) pr.style.display="";
    $("tContract").value=r.contractAddress;
    if(r.chainId) $("tChain").value=r.chainId;
    $("tValue").value=r.valueWei||"0";
    if(r.functionSig){
      $("tHex").checked=false; toggleHex();
      $("tFn").value=r.functionSig;
      $("tParams").value=(r.params||[]).join(";");
      toast(`Replayed tx → ${r.functionSig.split("(")[0]}() · ${(r.params||[]).length} param(s)`,"success");
    } else if(r.rawInput && r.rawInput!=="0x" && r.rawInput.length>2){
      $("tHex").checked=true; toggleHex();
      $("tRawHex").value=r.rawInput;
      toast("Replayed tx → raw Hex calldata (contract not verified — set ETHERSCAN_API_KEY to decode)","info");
    } else {
      $("tHex").checked=false; toggleHex(); $("tFn").value=""; $("tParams").value="";
      toast("Replayed tx → plain value transfer","success");
    }
  }catch(e){ toast(e.message,"error"); }
}
async function resolveTaskLink(){
  const v=$("tContract").value.trim(); if(!v) return;
  try{
    const r=await api("/nft/resolve-link",{method:"POST",body:JSON.stringify({link:v,chainId:Number($("tChain").value)})});
    if(!r.contractAddress) return;
    $("tContract").value=r.contractAddress;
    if(r.chainId) $("tChain").value=r.chainId;
    const hint=$("taskNftHint"), fn=$("fnRow"), pr=$("paramsRow");
    if(r.seadrop){
      SEADROP_ON=true;
      if(fn) fn.style.display="none";
      if(pr) pr.style.display="none";
      if($("abiBlock")) $("abiBlock").classList.add("hide");
      const max=r.maxPerWallet||1;
      // Use the GraphQL phases (public + allowlist) if present, else a single public phase.
      PHASES = (Array.isArray(r.phases) && r.phases.length) ? r.phases
        : [{index:0,kind:"public",priceEth:weiToEthStr(r.priceWei),priceWei:r.priceWei||"0",startUnix:0,endUnix:0}];
      const opts = PHASES.map((p,i)=>{
        const name=(p.kind==="public"?"Public Mint":"Allowlist");
        const when=p.startUnix?` · ${fmtLocal(p.startUnix)}`:"";
        return `<option value="${i}">${name} · ${(+p.priceEth||0)} ETH${when}</option>`;
      }).join("");
      hint.style.display="block";
      hint.innerHTML=`
        <div style="display:flex;align-items:center;gap:8px;margin-bottom:11px"><span class="badge on">SEADROP</span><b>${r.name||"collection"}</b></div>
        <div style="display:grid;grid-template-columns:2fr 1fr 1fr;gap:12px;align-items:end">
          <label class="fld">Mint Phase<select id="tMintPhase" onchange="onPhaseChange()">${opts}</select></label>
          <label class="fld">NFT Amount (max ${max})<input id="tQty" value="1" /></label>
          <label class="fld">Price / NFT (ETH)<input id="tMintPrice" value="" /></label>
        </div>
        <div class="muted" id="tPhaseStart" style="margin-top:9px;text-transform:none"></div>`;
      onPhaseChange();   // fill price + Start Time from the first phase
      // Keep the current mode (Simulate by default) — don't force Action on link paste.
    } else {
      SEADROP_ON=false; PHASES=[]; if(fn) fn.style.display=""; if(pr) pr.style.display="";
      hint.style.display="none"; hint.innerHTML="";
      if($("tFn")) $("tFn").placeholder="mint(uint256)  ← not a SeaDrop, set the mint function";
    }
    toast(r.seadrop ? `SeaDrop: ${r.name||"collection"}` : `Resolved ${r.name||"contract"} — not a SeaDrop, enter the mint Function (or use Hex)`, r.seadrop?"success":"info");
  }catch(e){ toast(e.message,"error"); }
}
function pickMode(m){ TASK_MODE=m; ["Simulate","Spam","Action"].forEach(x=>$("tg"+x).classList.toggle("on",x.toLowerCase()===m)); const pr=$("postActionRow"); if(pr) pr.style.display = m==="action" ? "grid" : "none"; }
function onPostActionChange(){ const t=($("tPostAction")||{}).value; const df=$("postDestFld"); if(df) df.style.visibility=(t==="transfer"||t==="drain")?"visible":"hidden"; }
function gasParams(){ const mode=$("tGasMode").value; const g={mode};
  if(mode==="manual"){ if($("tMaxFee").value)g.maxFeeGwei=Number($("tMaxFee").value); if($("tPrio").value)g.priorityFeeGwei=Number($("tPrio").value); }
  if($("tGasLimit").value)g.gasLimit=Number($("tGasLimit").value); return g; }
function resetTaskForm(){
  ["tContract","tFn","tRawHex","tParams","tMaxFee","tPrio","tGasLimit","tNonce","tStartAt","tAbi"].forEach(id=>$(id)&&($(id).value=""));
  ABI_FNS=[]; if($("abiBlock"))$("abiBlock").classList.add("hide"); if($("abiFnFld"))$("abiFnFld").classList.add("hide"); if($("tAbiFn"))$("tAbiFn").innerHTML=""; if($("tParams"))$("tParams").placeholder="param1;param2  ({address}=wallet · {rand:1-10000}=random)";
  $("tValue").value="0"; $("tDelay").value=String(APP_CFG.defaultDelayMs||0);
  $("tHex").checked=false; toggleHex(); $("tMulti").value=APP_CFG.defaultMultiRpc?"true":"false"; $("tGasMode").value="auto"; $("tFlashbots").checked=false; if($("tProxyGroup")) $("tProxyGroup").value="";
  if($("tPostAction")) $("tPostAction").value="none"; if($("tPostDest")) $("tPostDest").value=""; if($("tPostDrain")) $("tPostDrain").checked=false; onPostActionChange();
  pickMode("simulate"); $("tGroup").value=CUR_GROUP; updateStartHint();
  SEADROP_ON=false; PHASES=[]; const h=$("taskNftHint"); if(h){ h.style.display="none"; h.innerHTML=""; }
  const fr=$("fnRow"); if(fr) fr.style.display=""; const pr=$("paramsRow"); if(pr) pr.style.display="";
}
async function openTaskModal(){
  EDIT_ID=null; EDIT_CFG=null; EDIT_WALLET=null; ensureSelectors(); resetTaskForm();
  await taskWS.reload(); taskWS.clear();
  if(taskRS){ await taskRS.reload(); taskRS.clear(); }
  try{ PROXIES = await api("/proxies"); }catch{} fillProxyGroups(); if($("tProxyGroup")) $("tProxyGroup").value="";
  $("taskModalTitle").textContent="Create Task"; $("taskSubmitBtn").textContent="Create"; openModal("taskModal");
}
async function openTaskEdit(id, walletId){
  let base; try{ base=await api("/tasks/"+id); }catch(e){ return toast(e.message,"error"); }
  EDIT_ID=id; EDIT_CFG=base; ensureSelectors(); resetTaskForm(); EDIT_WALLET=walletId||null;
  // Fill the form from this wallet's EFFECTIVE config (its override if it has one), but keep
  // EDIT_CFG = the base task so saving writes the override back into the SAME task.
  const cfg=(walletId && base.walletOverrides && base.walletOverrides[walletId]) ? {...base, ...base.walletOverrides[walletId]} : base;
  $("tGroup").value=cfg.group||CUR_GROUP; if(cfg.chainId)$("tChain").value=cfg.chainId; $("tContract").value=cfg.contractAddress||"";
  $("tHex").checked=!!cfg.hexMode; toggleHex(); $("tFn").value=cfg.functionSig||""; $("tRawHex").value=cfg.rawHex||"";
  $("tParams").value=(cfg.params||[]).join(";"); $("tValue").value=cfg.valueWei||"0";
  $("tMulti").value=String(!!cfg.multiRpc); $("tDelay").value=cfg.delayMs||0;
  $("tFlashbots").checked=!!cfg.flashbots; if(cfg.nonceOverride!=null)$("tNonce").value=cfg.nonceOverride;
  if(cfg.postAction){ if($("tPostAction"))$("tPostAction").value=cfg.postAction.type||"none"; if($("tPostDest"))$("tPostDest").value=cfg.postAction.destination||""; if($("tPostDrain"))$("tPostDrain").checked=!!cfg.postAction.drainEth; onPostActionChange(); }
  const g=cfg.gas||{}; $("tGasMode").value=g.mode||"auto"; if(g.maxFeeGwei!=null)$("tMaxFee").value=g.maxFeeGwei; if(g.priorityFeeGwei!=null)$("tPrio").value=g.priorityFeeGwei; if(g.gasLimit!=null)$("tGasLimit").value=g.gasLimit;
  pickMode(cfg.mode||"simulate");
  if(cfg.startAt && $("tStartAt")) $("tStartAt").value=cfg.startAt; updateStartHint();
  await taskWS.reload(); taskWS.setSelected(EDIT_WALLET?[EDIT_WALLET]:(cfg.walletIds||[]));
  if(taskRS){ await taskRS.reload(); taskRS.setSelected(cfg.rpcUrls||[]); }
  try{ PROXIES = await api("/proxies"); }catch{} fillProxyGroups(); if($("tProxyGroup")) $("tProxyGroup").value=cfg.proxyGroup||"";
  if(cfg.seadrop){
    await resolveTaskLink();   // rebuild the SeaDrop mint block from the contract
    if($("tQty")) $("tQty").value=cfg.quantity||1;
    if($("tMintPrice")&&cfg.mintPriceWei) $("tMintPrice").value=weiToEthStr(cfg.mintPriceWei);
    if($("tStartAt")&&cfg.startAt){ $("tStartAt").value=cfg.startAt; updateStartHint(); }   // keep saved time over phase default
    pickMode(cfg.mode||"simulate");
  }
  const multi = ((EDIT_CFG.walletIds||[]).length||999) > 1; // imported tasks have many wallets
  $("taskModalTitle").textContent = (EDIT_WALLET && multi) ? "Edit Wallet (this wallet only · same task)" : "Edit Task";
  $("taskSubmitBtn").textContent="Save"; openModal("taskModal");
}
function buildTaskConfig(){
  const hex=$("tHex").checked;
  const cfg={ group:$("tGroup").value||"Imported", chainId:Number($("tChain").value), contractAddress:$("tContract").value.trim(),
    mode:TASK_MODE, hexMode:hex, functionSig:hex?"":$("tFn").value.trim(), rawHex:hex?$("tRawHex").value.trim():"",
    params:hex?[]:$("tParams").value.split(";").map(s=>s.trim()).filter(s=>s!==""), valueWei:$("tValue").value.trim()||"0",
    multiRpc:$("tMulti").value==="true", delayMs:Number($("tDelay").value)||0,
    flashbots:$("tFlashbots").checked, gas:gasParams() };
  // Always emit these keys (null/0 when cleared) so editing overrides the merge in createTask.
  if(TASK_MODE==="action" && $("tPostAction") && $("tPostAction").value!=="none"){
    const pa={ type:$("tPostAction").value, drainEth:$("tPostDrain").checked };
    const dest=($("tPostDest").value||"").trim(); if(dest) pa.destination=dest;
    cfg.postAction=pa;
  } else { cfg.postAction=null; }
  cfg.nonceOverride = $("tNonce").value!=="" ? Number($("tNonce").value) : null;
  // Always emit startAt (0 = unscheduled) so clearing it on edit overrides the merge below.
  cfg.startAt = Number(($("tStartAt")||{}).value)||0;
  if(SEADROP_ON){
    cfg.seadrop=true;
    cfg.quantity=Number(($("tQty")||{}).value)||1;
    const pe=($("tMintPrice")||{}).value; if(pe!=="" && pe!=null && !isNaN(+pe)) cfg.mintPriceWei=ethToWeiStr(+pe);
  }
  const ids = taskWS ? taskWS.selected() : [];
  if(ids.length) cfg.walletIds=ids;
  cfg.rpcUrls = taskRS ? taskRS.selected() : [];   // empty = chain default
  cfg.proxyGroup = ($("tProxyGroup")||{}).value || "";   // empty = no proxy (direct)
  if(APP_CFG.spamGuardrailS) cfg.spamGuardrailMs = Number(APP_CFG.spamGuardrailS)*1000;
  return cfg;
}
async function createTask(){
  const cfg=buildTaskConfig();
  if(!cfg.contractAddress && !cfg.hexMode) return toast("Contract address required","error");
  // A non-hex, non-SeaDrop task must specify a Function — otherwise calldata can't be
  // built and every wallet fails with "bad function signature".
  const effSeadrop = cfg.seadrop || (EDIT_ID && EDIT_CFG && EDIT_CFG.seadrop);
  if(!cfg.hexMode && !effSeadrop && !cfg.functionSig)
    return toast("Set a Function (e.g. mint(uint256)), enable Hex, or paste a SeaDrop link/address","error");
  try{
    if(EDIT_ID){
      const orig=EDIT_CFG||{};
      let origIds=(orig.walletIds&&orig.walletIds.length)?orig.walletIds.slice():WALLETS.filter(w=>w.network==="evm").map(w=>w.id);
      if(EDIT_WALLET && origIds.length>1){
        // Editing ONE wallet of a multi-wallet task: store its settings as a per-wallet
        // override in the SAME task (task id unchanged); the others keep the base config.
        const ov={...cfg}; delete ov.walletOverrides; delete ov.walletIds; delete ov.group; delete ov.id;
        const merged={...orig, walletOverrides:{...(orig.walletOverrides||{}), [EDIT_WALLET]:ov}};
        await api("/tasks/"+EDIT_ID,{method:"PUT",body:JSON.stringify(merged)});
        toast("Saved — only this wallet changed (same task id)","success");
      } else {
        const merged={...orig, ...cfg}; // preserve seadrop/quantity/etc not in the form
        await api("/tasks/"+EDIT_ID,{method:"PUT",body:JSON.stringify(merged)}); toast("Task saved","success");
      }
    } else {
      await api("/tasks",{method:"POST",body:JSON.stringify(cfg)}); toast("Task created","success");
    }
    TASK_GROUPS.add(cfg.group); saveGroups(); CUR_GROUP=cfg.group; closeModal("taskModal"); loadTasks();
  }catch(e){ toast(e.message,"error"); }
}

// ---------- NFT manager (OpenSea) ----------
let NFT_ITEMS = [], NFT_SEL = new Set(), NFT_VIEW = "card", NFT_SLUG = "", NFT_FLOOR = null;
const nftKey = (it) => it.walletId + ":" + it.tokenId;
const ethToWei = (s) => { s = String(s || "").trim(); if (!s) return "0"; const [i, f = ""] = s.split("."); const frac = (f + "0".repeat(18)).slice(0, 18); try { return (BigInt(i || "0") * (10n ** 18n) + BigInt(frac || "0")).toString(); } catch { return "0"; } };

async function loadNftProxies(){
  try{ PROXIES = await api("/proxies"); }catch{}
  const sel=$("nftProxy"); if(sel){ const cur=sel.value; const gs=new Set(["",...(PROXY_GROUPS||[])]); (PROXIES||[]).forEach(p=>gs.add(p.group)); sel.innerHTML=[...gs].map(g=>`<option value="${g}">${g||"none"}</option>`).join(""); sel.value=cur; }
}
let NFT_RUN = null, _nftRenderT = null;
async function nftLoad(){
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  if(!contract) return toast("Enter a contract address","error");
  const ids = nftWS ? nftWS.selected() : [];
  const proxyGroup=($("nftProxy")||{}).value||""; const threads=Number($("nftThreads").value)||5;
  const runId="nft"+Date.now()+"-"+Math.floor(Math.random()*1e6);
  NFT_ITEMS=[]; NFT_SEL.clear(); NFT_FLOOR=null; NFT_SLUG=""; $("nftFloor").textContent="";
  NFT_RUN={ id:runId, total:0, doneWallets:0, failed:0, finished:false };
  $("nftManagerCard").style.display="block";
  $("nftGrid").innerHTML=`<div class="muted" style="padding:20px">Fetching NFTs from OpenSea…</div>`;
  $("nftCount").textContent="0 NFTs"; $("nftWalletInfo").textContent="";
  try{
    const r=await api("/nft/items",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract,walletIds:ids,proxyGroup,threads,runId})});
    NFT_RUN.total=r.total||0;
    if(!NFT_RUN.total){ $("nftGrid").innerHTML=`<div class="muted" style="padding:20px">No wallets selected.</div>`; NFT_RUN=null; return; }
    updateNftProgress();
  }catch(e){ $("nftGrid").innerHTML=`<div class="warnbox">${e.message}</div>`; NFT_RUN=null; }
}
// WS push: one wallet's NFTs arrived (or the run finished).
function nftOnResult(d){
  if(!NFT_RUN || d.runId!==NFT_RUN.id) return;
  if(d.total) NFT_RUN.total=d.total;
  if(d.slug) NFT_SLUG=d.slug;
  if(d.done){
    NFT_RUN.failed=d.failed||0; NFT_RUN.finished=true; updateNftProgress(); nftRender();
    if(!NFT_ITEMS.length) toast("No NFTs found for the selected wallet(s)","info");
    else if(d.failed) toast(`${d.failed} wallet(s) still failed after retries — add a proxy group / lower threads, then reload`,"info");
    return;
  }
  NFT_RUN.doneWallets++;
  if(d.items && d.items.length) NFT_ITEMS.push(...d.items);
  updateNftProgress();
  scheduleNftRender(); // debounced so 100s of wallets don't thrash the DOM
}
function scheduleNftRender(){ if(_nftRenderT) return; _nftRenderT=setTimeout(()=>{ _nftRenderT=null; nftRender(); }, 120); }
function updateNftProgress(){
  if(!NFT_RUN){ return; }
  const {doneWallets:done,total,failed,finished}=NFT_RUN;
  let s = (!finished && total) ? `· fetching ${done}/${total} wallet(s)…` : (total?`· ${total} wallet(s)`:"");
  if(failed) s += ` · ${failed} failed`;
  $("nftWalletInfo").textContent=s;
}
function nftToggleView(){ NFT_VIEW = NFT_VIEW==="card"?"list":"card"; $("nftViewBtn").textContent = NFT_VIEW==="card"?"List view":"Card view"; nftRender(); }
function nftRender(){
  $("nftCount").textContent=`${NFT_ITEMS.length} NFTs`;
  const g=$("nftGrid"); const sc=g.scrollTop; // preserve scroll across streaming re-renders
  g.classList.toggle("cardview", NFT_VIEW==="card");
  g.classList.toggle("listview", NFT_VIEW==="list");
  if(!NFT_ITEMS.length){
    const streaming = NFT_RUN && !NFT_RUN.finished;
    g.innerHTML=`<div class="muted" style="padding:20px">${streaming?"Fetching NFTs from OpenSea…":"No NFTs found."}</div>`;
    nftUpdateBar(); return;
  }
  if(NFT_VIEW==="list"){
    g.innerHTML=NFT_ITEMS.map(it=>{
      const k=nftKey(it), sel=NFT_SEL.has(k);
      const img=it.image?`style="background-image:url('${it.image.replace(/'/g,"")}')"`:"";
      return `<div class="nft-row ${sel?'sel':''} ${it.listed?'listed':''}" onclick="nftToggle('${k}')">
        <span class="nr-check">${sel?'✓':''}</span>
        <span class="nr-thumb" ${img}></span>
        <span class="nr-name">${it.name||('#'+it.tokenId)} <span class="muted">#${it.tokenId}</span></span>
        <span class="nr-owner mono">${short(it.owner)}</span>
        <span class="nr-stat">${it.listed?'<span class="badge listed">LISTED</span>':''}</span>
      </div>`;
    }).join("");
  } else {
    g.innerHTML=NFT_ITEMS.map(it=>{
      const k=nftKey(it), sel=NFT_SEL.has(k);
      const img=it.image?`style="background-image:url('${it.image.replace(/'/g,"")}')"`:"";
      return `<div class="nft-cell ${sel?'sel':''} ${it.listed?'listed':''}" onclick="nftToggle('${k}')">
        <div class="nft-img" ${img}></div>
        <div class="nft-body">
          <div class="nft-name"><span class="nm">${it.name||('#'+it.tokenId)}</span><span class="id">#${it.tokenId}</span></div>
          <div class="nft-foot"><span class="vault">${short(it.owner)}</span>${it.listed?'<span class="badge listed">LISTED</span>':''}</div>
        </div></div>`;
    }).join("");
  }
  g.scrollTop=sc;
  nftUpdateBar();
}
function nftToggle(k){ NFT_SEL.has(k)?NFT_SEL.delete(k):NFT_SEL.add(k); nftRender(); }
function nftClearSel(){ NFT_SEL.clear(); nftRender(); }
function nftSelectAll(){ NFT_ITEMS.forEach(it=>NFT_SEL.add(nftKey(it))); nftRender(); }
function nftSelectN(){ const n=Math.max(0,Math.floor(Number($("nftSelNum").value)||0)); NFT_SEL.clear(); NFT_ITEMS.slice(0,n).forEach(it=>NFT_SEL.add(nftKey(it))); nftRender(); if(n>NFT_ITEMS.length) toast(`Only ${NFT_ITEMS.length} NFTs loaded`,"info"); }
async function nftFetchFloor(){
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  if(!contract) return toast("Enter a contract address","info");
  $("nftFloor").textContent="· floor …";
  try{
    const r=await api("/nft/floor",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract,slug:NFT_SLUG})});
    NFT_FLOOR=Number(r.floor)||0; if(r.slug) NFT_SLUG=r.slug;
    $("nftFloor").textContent = NFT_FLOOR ? `· floor ${NFT_FLOOR} ETH` : "· no floor listed";
  }catch(e){ $("nftFloor").textContent=""; toast(e.message,"error"); }
}
async function nftUseFloor(){ if(NFT_FLOOR==null) await nftFetchFloor(); if(NFT_FLOOR) $("listPrice").value=String(NFT_FLOOR); else toast("No floor price available","info"); }
function nftSelected(){ return NFT_ITEMS.filter(it=>NFT_SEL.has(nftKey(it))); }
function nftUpdateBar(){
  const sel=nftSelected(); const wallets=new Set(sel.map(it=>it.walletId)).size; const listed=sel.filter(it=>it.listed).length;
  $("nftActbar").style.display=sel.length?"flex":"none";
  $("nftSelInfo").textContent=`${sel.length} selected · ${wallets} wallet(s)`;
  $("nftCancelN").textContent=listed;
}
// Send Selected → transfer tasks
function nftSend(){ const sel=nftSelected(); if(!sel.length)return; $("sendSummary").textContent=`${sel.length} NFT(s) from ${new Set(sel.map(s=>s.walletId)).size} wallet(s)`; openModal("sendModal"); }
async function nftSendConfirm(){
  const dest=$("sendDest").value.trim(); if(!/^0x[0-9a-fA-F]{40}$/.test(dest)) return toast("Invalid destination address","error");
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  let n=0;
  for(const it of nftSelected()){
    const cfg={ group:"NFT-Send", chainId, contractAddress:contract, mode:"action",
      functionSig:"safeTransferFrom(address,address,uint256)", params:["{address}",dest,it.tokenId],
      gas:{mode:"auto"}, walletIds:[it.walletId] };
    try{ await api("/tasks",{method:"POST",body:JSON.stringify(cfg)}); n++; }catch(e){ toast(e.message,"error"); }
  }
  closeModal("sendModal"); toast(`Created ${n} transfer task(s) in group 'NFT-Send'`,"success");
}
// List Selected → OpenSea-style Create-listings modal (per-item pricing)
let LIST_ITEMS = [], LIST_FEE_BPS = 0;
function nftList(){
  const sel=nftSelected(); if(!sel.length)return;
  LIST_ITEMS = sel.map(it=>({ walletId:it.walletId, tokenId:it.tokenId, name:it.name||('#'+it.tokenId), image:it.image||"", floor:null, price:"" }));
  LIST_FEE_BPS=0; $("listCount").textContent=`${LIST_ITEMS.length} item(s)`; $("listSetAll").value="";
  $("listPlatFee").textContent="—"; $("listCreatorFee").textContent="—";
  renderListRows();
  openModal("listModal");
  loadListFloorFees();
}
async function loadListFloorFees(){
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  try{ const f=await api("/nft/floor",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract,slug:NFT_SLUG})}); NFT_FLOOR=Number(f.floor)||0; if(f.slug)NFT_SLUG=f.slug; LIST_ITEMS.forEach(it=>it.floor=NFT_FLOOR); }catch{}
  try{ const fe=await api("/nft/fees",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract,slug:NFT_SLUG})});
    LIST_FEE_BPS=Number(fe.feeBps)||0;
    $("listPlatFee").textContent=((Number(fe.platformBps)||0)/100).toFixed(1)+"%";
    $("listCreatorFee").textContent=((Number(fe.creatorBps)||0)/100).toFixed(1)+"%";
  }catch{}
  renderListRows();
}
function renderListRows(){
  $("listRows").innerHTML=LIST_ITEMS.map((it,i)=>{
    const img=it.image?`style="background-image:url('${it.image.replace(/'/g,"")}')"`:"";
    const proc=procOf(it.price);
    return `<div class="ltr">
      <span class="c-item"><span class="lt-thumb" ${img}></span><span>${it.name}</span></span>
      <span class="c-qty">1</span>
      <span class="c-floor">${it.floor!=null?it.floor:"—"}</span>
      <span class="c-proc">${proc}</span>
      <span class="c-price"><input class="lprice" value="${it.price}" placeholder="0.00" oninput="listPriceInput(${i},this.value)" /></span>
    </div>`;
  }).join("");
  recomputeList();
}
const procOf=(p)=>{ const n=Number(p); return (p!==""&&!isNaN(n)&&n>0) ? Number((n*(1-LIST_FEE_BPS/10000)).toFixed(5)) : "—"; };
function listPriceInput(i,v){
  LIST_ITEMS[i].price=String(v).trim();
  const row=$("listRows").children[i]; if(row){ const pc=row.querySelector(".c-proc"); if(pc) pc.textContent=procOf(LIST_ITEMS[i].price); }
  recomputeList();
}
function recomputeList(){
  let total=0; LIST_ITEMS.forEach(it=>{ const p=Number(it.price); if(!isNaN(p)&&p>0) total+=p; });
  $("listTotal").textContent=`${Number(total.toFixed(6))} ETH`;
  $("listProceeds").textContent=`${Number((total*(1-LIST_FEE_BPS/10000)).toFixed(6))} ETH`;
}
function listApplyAll(){ const v=$("listSetAll").value.trim(); if(v==="") return; LIST_ITEMS.forEach(it=>it.price=v); renderListRows(); }
async function listSetFloor(){ if(!NFT_FLOOR) await loadListFloorFees(); if(NFT_FLOOR){ LIST_ITEMS.forEach(it=>it.price=String(NFT_FLOOR)); renderListRows(); } else toast("No floor price available","info"); }
async function listLadder(){
  const v=await promptDialog("Ladder pricing","from,to (ETH) e.g. 0.05,0.1"); if(!v) return;
  const [a,b]=v.split(",").map(x=>Number(x.trim()));
  if(isNaN(a)||isNaN(b)) return toast("Enter as: from,to","error");
  const n=LIST_ITEMS.length;
  LIST_ITEMS.forEach((it,i)=>{ const p = n<=1 ? a : a+(b-a)*i/(n-1); it.price=String(Number(p.toFixed(6))); });
  renderListRows();
}
function listAdjustFees(){ const f=1-LIST_FEE_BPS/10000; if(f<=0) return toast("Fees not loaded yet","info");
  LIST_ITEMS.forEach(it=>{ const p=Number(it.price); if(!isNaN(p)&&p>0) it.price=String(Number((p/f).toFixed(6))); }); renderListRows();
  toast("Prices grossed up so proceeds ≈ the price you set","info");
}
async function nftListConfirm(){
  const items=LIST_ITEMS.filter(it=>Number(it.price)>0).map(it=>({walletId:it.walletId,tokenId:it.tokenId,priceWei:ethToWei(it.price)}));
  if(!items.length) return toast("Set a unit price first","error");
  const days=Number($("listDuration").value)||7;
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  try{
    const r=await api("/nft/list",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract,durationSec:days*86400,items})});
    closeModal("listModal");
    if(r.needApproval){ // first listing per wallet needs a one-time on-chain approval (now auto-running)
      toast(`Approving ${r.needApproval} wallet(s) on-chain — wait ~30s for it to mine, then click List again.`,"info");
    } else if(r.failed){
      toast(`Listed ${r.listed||0}/${items.length} · ${r.failed} failed${r.error?": "+r.error:""}`,"error");
    } else {
      toast(`Listed ${r.listed||0}/${items.length}`,"success");
    }
    setTimeout(nftLoad,2200);
  }catch(e){ toast(e.message,"error"); }
}
async function nftCancel(){
  const sel=nftSelected().filter(it=>it.listed); if(!sel.length) return toast("No listed NFTs selected","info");
  const wallets=new Set(sel.map(it=>it.walletId)).size;
  if(!await confirmDialog(`Cancel listings on ${wallets} wallet(s)?\nThis cancels ALL open Seaport listings for those wallets (one incrementCounter tx each).`,"Cancel listings")) return;
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  try{
    const r=await api("/nft/cancel",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract,items:sel.map(it=>({walletId:it.walletId,tokenId:it.tokenId}))})});
    toast(`Cancelling ${r.cancelled||0} wallet(s)' listings on-chain — refresh in a moment`,"info"); setTimeout(nftLoad,3000);
  }catch(e){ toast(e.message,"error"); }
}
// Accept Offers → sells each selected NFT into the best active collection offer (paid in
// WETH). Each accept is eth_call-simulated server-side before broadcast, so a doomed tx
// never burns gas. Results stream over the WS ("accept" event).
let ACCEPT_RUN=null;
async function nftAccept(){
  const sel=nftSelected(); if(!sel.length) return toast("Select NFTs first","info");
  const wallets=new Set(sel.map(it=>it.walletId)).size;
  const minEth=await promptDialog(
    `Accept the BEST offer on ${sel.length} NFT(s) across ${wallets} wallet(s).\nThis SELLS them for WETH. Minimum price in ETH (0 = accept any):`,
    "0","Accept offers","0");
  if(minEth===null) return; // cancelled
  const e=Number(minEth); if(isNaN(e)||e<0) return toast("Invalid minimum price","error");
  const minPriceWei = e>0 ? ethToWeiStr(String(minEth)) : "";
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  const runId=newRunId();
  ACCEPT_RUN={ id:runId, total:sel.length, done:0, ok:0, fail:0, approve:0, skip:0 };
  try{
    await api("/nft/accept",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract,runId,minPriceWei,
      items:sel.map(it=>({walletId:it.walletId,tokenId:it.tokenId}))})});
    toast(`Accepting offers on ${sel.length} NFT(s)… each is simulated first (safe).`,"info");
  }catch(err){ ACCEPT_RUN=null; toast(err.message,"error"); }
}
function nftOnAccept(d){
  const r=ACCEPT_RUN; if(!r||d.runId!==r.id) return;
  r.done++;
  if(d.txHash && !d.error) r.ok++;
  else if(d.needApproval) r.approve++;
  else if(d.skipped) r.skip++;
  else if(d.error) r.fail++;
  if(r.done<r.total){ toast(`Accepting… ${r.done}/${r.total} · ${r.ok} sold`,"info"); return; }
  let msg=`Accepted ${r.ok}/${r.total}`;
  if(r.approve) msg+=` · ${r.approve} need approval (auto-sent — retry in ~30s)`;
  if(r.skip) msg+=` · ${r.skip} below min`;
  if(r.fail) msg+=` · ${r.fail} failed`;
  toast(msg, (r.ok? "success":"error"));
  ACCEPT_RUN=null; setTimeout(nftLoad,3000);
}
// SeaDrop mint via the NFT tab was removed — create mint tasks from the Tasks tab
// (paste an OpenSea link) instead. nftResolve/nftCreateMint kept as internal helpers.
async function nftResolve(){
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  if(!contract) return toast("Enter a contract address","error");
  $("nftDrop").innerHTML=`<div class="muted" style="margin-top:12px">Resolving…</div>`;
  try{
    const r=await api("/nft/resolve",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract})});
    if(!r.seadrop){ $("nftDrop").innerHTML=`<div class="warnbox" style="margin-top:12px">${r.name?r.name+": ":""}Not a SeaDrop public mint. Use a Task with a Function signature or Hex calldata to mint this contract.</div>`; return; }
    const priceEth=(Number(BigInt(r.priceWei))/1e18).toFixed(5);
    $("nftDrop").innerHTML=`
      <div class="dropinfo">
        <div class="di"><div class="k">Collection</div><div class="v">${r.name||"—"}</div></div>
        <div class="di"><div class="k">Price</div><div class="v">${priceEth} ETH</div></div>
        <div class="di"><div class="k">Max / Wallet</div><div class="v">${r.maxPerWallet}</div></div>
        <div class="di"><div class="k">Fee</div><div class="v">${(r.feeBps/100).toFixed(2)}%</div></div>
        <div class="di"><div class="k">Status</div><div class="v"><span class="badge ${r.active?'on':'off'}">${r.active?'ACTIVE':'not active'}</span></div></div>
      </div>
      <div class="row" style="margin-top:14px;align-items:flex-end">
        <label class="fld" style="width:100px">Quantity<input id="nftQty" value="1" /></label>
        <label class="fld" style="width:130px">Mode<select id="nftMode"><option value="simulate">Simulate</option><option value="action">Action</option></select></label>
        <button class="primary sm" onclick="nftCreateMint()">Create Mint Task</button>
        <span class="muted">uses the wallets selected above</span>
      </div>`;
  }catch(e){ $("nftDrop").innerHTML=`<div class="warnbox" style="margin-top:12px">${e.message}</div>`; }
}
async function nftScan(){
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  if(!contract) return toast("Enter a contract address","error");
  const ids = nftWS ? nftWS.selected() : [];
  try{
    const r=await api("/nft/holdings",{method:"POST",body:JSON.stringify({chainId,contractAddress:contract,walletIds:ids})});
    $("nftHoldCard").style.display="block"; $("nftHoldName").textContent=r.name?("· "+r.name):"";
    $("nftHoldRows").innerHTML=(r.holdings||[]).map(h=>`<tr><td>${h.label}</td><td class="mono">${short(h.address)}</td><td style="text-align:right">${h.err?'<span class="bad">err</span>':h.balance}</td></tr>`).join("")||`<tr><td colspan="3" class="muted">No wallets</td></tr>`;
  }catch(e){ toast(e.message,"error"); }
}
async function nftCreateMint(){
  const contract=$("nftContract").value.trim(); const chainId=Number($("nftChain").value);
  const ids = nftWS ? nftWS.selected() : [];
  const cfg={ group:"NFT", chainId, contractAddress:contract, mode:$("nftMode").value, seadrop:true, quantity:Number($("nftQty").value)||1, gas:{mode:"auto"} };
  if(ids.length) cfg.walletIds=ids;
  try{ await api("/tasks",{method:"POST",body:JSON.stringify(cfg)}); toast("Mint task created in group 'NFT'","success"); }
  catch(e){ toast(e.message,"error"); }
}

// ---------- calculator (port calc.ts rz) ----------
const GWEI_ROWS=[1,3,5,10,15,20,25,30,40,50,75,100,125,150,200,250,300,350,400,450,500,600,700,800,900,1000];
function rz(gwei,gasUsage,failUsage,nftAmount,nftPrice){
  const p=gwei*gasUsage/1e9, c=gwei*failUsage/1e9, m=nftAmount*nftPrice+p, h=m+p*0.25, y=nftAmount>0?m/nftAmount:NaN;
  return {gwei, avgUsage:m, avgFail:c, balanceNeeded:h, perNft:y};
}
const ki=(n)=>Number.isFinite(n)?n.toFixed(4)+" ":"— ";
function renderCalc(){
  if(!$("unixTs").value) setNow();
  updateUnix();
  const gu=parseFloat($("gcUsage").value)||0, gf=parseFloat($("gcFail").value)||0, na=parseFloat($("gcAmt").value)||0, np=parseFloat($("gcPrice").value)||0;
  $("gcRows").innerHTML=GWEI_ROWS.map(g=>{const r=rz(g,gu,gf,na,np);
    return `<tr><td>${g}</td><td>${ki(r.avgUsage)}ETH</td><td style="color:var(--warn)">${ki(r.avgFail)}ETH</td><td>${ki(r.balanceNeeded)}ETH</td><td>${ki(r.perNft)}ETH</td></tr>`;}).join("");
}
function setNow(){ $("unixTs").value=Math.floor(Date.now()/1000); }
function updateUnix(){ const ts=Number($("unixTs").value); if(!ts){return;} const d=new Date(ts*1000);
  $("localDt").value=d.toLocaleString(); $("unixOut").textContent=d.toUTCString(); }
["gcUsage","gcFail","gcAmt","gcPrice"].forEach(id=>{ const el=$(id); if(el) el.addEventListener("input",renderCalc); });
$("unixTs") && $("unixTs").addEventListener("input",updateUnix);

// ---------- telegram settings ----------
// ---- API keys (OpenSea / Etherscan) configured in-app instead of the .env file ----
function toggleReveal(id){ const el=$(id); if(el) el.type = el.type==="password" ? "text" : "password"; }
async function loadApiKeys(){
  try{ const r=await api("/settings");
    if($("setOpensea")) $("setOpensea").value=r.OPENSEA_API_KEY||"";
    if($("setEtherscan")) $("setEtherscan").value=r.ETHERSCAN_API_KEY||"";
    if($("setKeysStatus")) $("setKeysStatus").textContent="";
  }catch(e){ if($("setKeysStatus")) $("setKeysStatus").textContent="(unable to load)"; }
}
async function saveApiKeys(){
  const body={ OPENSEA_API_KEY:($("setOpensea").value||"").trim(), ETHERSCAN_API_KEY:($("setEtherscan").value||"").trim() };
  try{ await api("/settings",{method:"POST",body:JSON.stringify(body)}); toast("API keys saved & applied","success"); if($("setKeysStatus")) $("setKeysStatus").textContent="saved"; }
  catch(e){ toast(e.message,"error"); }
}
// ---- Settings sub-tabs + app config (Appearance / Discord / Task defaults) ----
let APP_CFG = {};
function goSub(sub){
  ["app","setup","social"].forEach(s=>{ const p=$("sub-"+s); if(p) p.classList.toggle("hide", s!==sub); });
  document.querySelectorAll("#tab-settings .subnav-item").forEach(a=>a.classList.toggle("active", a.dataset.sub===sub));
}
// Appearance scale (per-machine, localStorage; applied instantly + on boot)
function applyScale(v){ v=Math.max(80,Math.min(150,Number(v)||100)); document.documentElement.style.zoom = (v/100); const lbl=$("setScaleVal"); if(lbl) lbl.textContent=v+"%"; const sl=$("setScale"); if(sl) sl.value=v; }
function onScaleInput(){ const v=$("setScale").value; applyScale(v); try{ localStorage.setItem("uiScale", String(v)); }catch{} }
function resetScale(){ try{ localStorage.setItem("uiScale","100"); }catch{} applyScale(100); }
async function loadAppCfg(){ try{ APP_CFG = await api("/appsettings") || {}; }catch{ APP_CFG = {}; } return APP_CFG; }
async function loadSettingsPanel(){
  await loadAppCfg();
  if($("setDiscord")) $("setDiscord").value = APP_CFG.discordWebhook || "";
  if($("setDefDelay")) $("setDefDelay").value = APP_CFG.defaultDelayMs!=null ? APP_CFG.defaultDelayMs : "";
  if($("setDefMulti")) $("setDefMulti").value = APP_CFG.defaultMultiRpc ? "true" : "false";
  if($("setSpamGuard")) $("setSpamGuard").value = APP_CFG.spamGuardrailS!=null ? APP_CFG.spamGuardrailS : "";
  if($("setFbWindow")) $("setFbWindow").value = APP_CFG.fbWindowBlocks!=null ? APP_CFG.fbWindowBlocks : "";
  if($("setFbPrio")) $("setFbPrio").value = APP_CFG.fbPriorityGwei!=null ? APP_CFG.fbPriorityGwei : "";
  if($("setFbMax")) $("setFbMax").value = APP_CFG.fbMaxFeeGwei!=null ? APP_CFG.fbMaxFeeGwei : "";
  if($("setUpdRepo")) $("setUpdRepo").value = APP_CFG.updateRepo || "";
  if($("updVer") && VER) $("updVer").textContent = "v"+VER;
  renderChainsCard();
  applyScale(localStorage.getItem("uiScale") || 100);
}
async function saveDiscord(){
  const url=($("setDiscord").value||"").trim();
  try{ await api("/appsettings",{method:"POST",body:JSON.stringify({discordWebhook:url})}); await loadAppCfg(); toast("Discord webhook saved","success"); if($("setDiscordStatus"))$("setDiscordStatus").textContent="saved"; }
  catch(e){ toast(e.message,"error"); }
}
async function testDiscord(){
  const url=($("setDiscord").value||"").trim(); if(!url) return toast("Paste a webhook URL first","info");
  try{ await api("/discord/test",{method:"POST",body:JSON.stringify({url})}); toast("Sent a test message to Discord","success"); }
  catch(e){ toast(e.message,"error"); }
}
async function saveTaskDefaults(){
  const body={ defaultDelayMs:Number($("setDefDelay").value)||0, defaultMultiRpc:$("setDefMulti").value==="true", spamGuardrailS:Number($("setSpamGuard").value)||0 };
  try{ await api("/appsettings",{method:"POST",body:JSON.stringify(body)}); await loadAppCfg(); toast("Task defaults saved","success"); if($("setDefStatus"))$("setDefStatus").textContent="saved"; }
  catch(e){ toast(e.message,"error"); }
}
async function saveFlashbots(){
  const body={ fbWindowBlocks:Number($("setFbWindow").value)||0, fbPriorityGwei:Number($("setFbPrio").value)||0, fbMaxFeeGwei:Number($("setFbMax").value)||0 };
  try{ await api("/appsettings",{method:"POST",body:JSON.stringify(body)}); await loadAppCfg(); toast("Flashbots tuning saved","success"); if($("setFbStatus"))$("setFbStatus").textContent="saved"; }
  catch(e){ toast(e.message,"error"); }
}
function renderChainsCard(){
  const grid=$("chainsGrid"); if(!grid) return;
  const disabled=new Set((APP_CFG.chainsDisabled)||[]); const ov=APP_CFG.chainRPCOverrides||{};
  const custom=new Set(((APP_CFG.customChains)||[]).map(c=>c.id));
  grid.innerHTML = (CHAINS||[]).map(c=>{
    const on=!disabled.has(c.id);
    return `<div class="chain-row ${on?'':'off'}" data-cid="${c.id}">
      <div class="ch-head"><div><span class="ch-name">${c.name}</span> <span class="ch-id">${c.symbol||'ETH'} · ${c.id}${custom.has(c.id)?' · custom':''}</span></div>
        <div class="row" style="margin:0;gap:9px">${custom.has(c.id)?`<span class="selclear" title="Remove custom chain" onclick="removeCustomChain(${c.id})">✕</span>`:''}<input type="checkbox" class="ch-on" ${on?'checked':''} onchange="this.closest('.chain-row').classList.toggle('off',!this.checked)" /></div></div>
      <input class="ch-rpc" placeholder="built-in default" value="${(ov[c.id]||'').replace(/"/g,'&quot;')}" /></div>`;
  }).join("");
}
async function saveChains(){
  const disabled=[], overrides={};
  document.querySelectorAll("#chainsGrid .chain-row").forEach(row=>{
    const cid=Number(row.dataset.cid);
    if(!row.querySelector(".ch-on").checked) disabled.push(cid);
    const u=(row.querySelector(".ch-rpc").value||"").trim(); if(u) overrides[cid]=u;
  });
  try{ await api("/appsettings",{method:"POST",body:JSON.stringify({chainsDisabled:disabled, chainRPCOverrides:overrides})}); await loadAppCfg(); await loadChains(); toast("Chains saved","success"); if($("setChainsStatus"))$("setChainsStatus").textContent="saved"; }
  catch(e){ toast(e.message,"error"); }
}
async function addCustomChain(){
  const id=Number($("ccId").value), name=($("ccName").value||"").trim(), sym=($("ccSym").value||"").trim()||"ETH", rpc=($("ccRpc").value||"").trim();
  if(!id||!name) return toast("Chain ID and Name are required","info");
  if((CHAINS||[]).some(c=>c.id===id)) return toast("That chain ID already exists","info");
  const list=((APP_CFG.customChains)||[]).slice(); list.push({id,name,symbol:sym,rpc});
  const ov=Object.assign({}, APP_CFG.chainRPCOverrides||{}); if(rpc) ov[id]=rpc;   // engine resolves via override
  try{ await api("/appsettings",{method:"POST",body:JSON.stringify({customChains:list, chainRPCOverrides:ov})}); await loadAppCfg(); await loadChains(); renderChainsCard();
    ["ccId","ccName","ccSym","ccRpc"].forEach(i=>$(i)&&($(i).value="")); toast(`Added chain ${name} (${id})`,"success"); }
  catch(e){ toast(e.message,"error"); }
}
async function removeCustomChain(id){
  if(!await confirmDialog("Remove this custom chain?","Remove")) return;
  const list=((APP_CFG.customChains)||[]).filter(c=>c.id!==id);
  const ov=Object.assign({}, APP_CFG.chainRPCOverrides||{}); delete ov[id];
  try{ await api("/appsettings",{method:"POST",body:JSON.stringify({customChains:list, chainRPCOverrides:ov})}); await loadAppCfg(); await loadChains(); renderChainsCard(); toast("Custom chain removed","info"); }
  catch(e){ toast(e.message,"error"); }
}
async function saveUpdRepo(){
  const repo=($("setUpdRepo").value||"").trim();
  try{ await api("/appsettings",{method:"POST",body:JSON.stringify({updateRepo:repo})}); await loadAppCfg(); toast("Update source saved","success"); }
  catch(e){ toast(e.message,"error"); }
}
async function checkUpdate(){
  const st=$("updStatus"); if(st) st.textContent="checking…";
  try{
    const r=await api("/update/check");
    if($("updVer")) $("updVer").textContent="v"+(r.current||"?");
    if(!r.configured){ if(st) st.textContent="set an update source first"; toast("Set a GitHub owner/repo, then Check","info"); return; }
    if(r.hasUpdate){ if(st) st.innerHTML=`<span style="color:var(--accent-text)">v${r.latest} available</span> · <a href="${r.url}" target="_blank">release</a>`; toast(`Update available: v${r.latest}`,"success"); }
    else { if(st) st.textContent="up to date ✓"; toast("You're on the latest version","info"); }
  }catch(e){ if(st) st.textContent=""; toast(e.message,"error"); }
}
async function loadTelegram(){
  try{ const r=await api("/telegram"); const c=r.config||{};
    $("tgToken").value=""; $("tgToken").placeholder=c.token?`(saved: ${c.token})`:"123456:ABC-...";
    $("tgChats").value=(c.allowedChats||[]).join(", ");
    $("tgNotify").value=c.notify||"summary"; $("tgEnabled").value=String(!!c.enabled);
    $("tgStatus").textContent=r.running?"running":"off";
  }catch(e){ $("tgStatus").textContent=""; }
}
async function saveTelegram(){
  const cfg={ enabled:$("tgEnabled").value==="true", token:$("tgToken").value.trim(),
    allowedChats:$("tgChats").value.split(",").map(s=>parseInt(s.trim(),10)).filter(n=>!isNaN(n)),
    notify:$("tgNotify").value };
  try{ const r=await api("/telegram",{method:"POST",body:JSON.stringify(cfg)}); $("tgStatus").textContent=r.running?"running":"off"; $("tgToken").value=""; loadTelegram(); toast("Telegram config saved","success"); }
  catch(e){ toast(e.message,"error"); }
}

// ---------- logs ----------
function logMatches(e){ const cat=$("logCat").value, lvl=$("logLevel").value; return (!cat||e.category===cat)&&(!lvl||e.level===lvl); }
function fmtLogFields(f){
  if(!f||typeof f!=="object") return "";
  const parts=Object.entries(f).map(([k,v])=>`${k}=${(v&&typeof v==="object")?JSON.stringify(v):v}`);
  return parts.length?" · "+parts.join(" "):"";
}
function appendLog(e){ if(!logMatches(e))return;
  const div=document.createElement("div"); div.className="logline";
  div.style.color=(e.level==="ERROR"||e.level==="WARN")?"var(--danger)":"var(--muted)";
  const t=new Date(e.time).toLocaleTimeString();
  div.textContent=`${t} [${e.level}] ${e.category}${e.taskId?" #"+e.taskId:""} ${e.wallet?e.wallet+" ":""}${e.msg}${fmtLogFields(e.fields)}`;
  const box=$("logstream"); box.appendChild(div); while(box.childElementCount>2000)box.removeChild(box.firstChild);
  if($("logAutoscroll").checked)box.scrollTop=box.scrollHeight;
}
function clearLogView(){ $("logstream").innerHTML=""; }
async function loadLogs(){ const es=await api("/logs"); clearLogView(); es.forEach(appendLog); }
["logCat","logLevel"].forEach(id=>$(id)&&$(id).addEventListener("change",loadLogs));
$("tContract") && $("tContract").addEventListener("input", onContractInput);

// ---------- websocket ----------
function connectWS(){
  if(location.protocol==="file:"){ setOffline(true); return; } // opened as a file, no server
  const proto=location.protocol==="https:"?"wss":"ws";
  let ws; try{ ws=new WebSocket(`${proto}://${location.host}/api/ws`); }catch{ setTimeout(connectWS,2000); return; }
  ws.onopen=()=>{ $("wsState").textContent="live"; $("wsState").style.color="var(--accent)"; setOffline(false); if(!CHAINS.length) bootData().catch(()=>{}); };
  ws.onclose=()=>{ $("wsState").textContent="offline"; $("wsState").style.color="var(--danger)"; setTimeout(connectWS,2000); };
  ws.onmessage=(ev)=>{ let m; try{m=JSON.parse(ev.data);}catch{return;}
    if(m.type==="task"){ TASKS[m.data.id]=m.data; renderTasks(); homeMaybeRefresh(); }
    else if(m.type==="log"){ appendLog(m.data); }
    else if(m.type==="accept"){ nftOnAccept(m.data); homeMaybeRefresh(); }
    else if(m.type==="whitelist"){ wlOnResult(m.data); }
    else if(m.type==="funds"){ fundsOnResult(m.data); }
    else if(m.type==="nft"){ nftOnResult(m.data); }
  };
}

// ---------- clock + gwei ----------
function tickClock(){ const d=new Date(); const p=(n)=>String(n).padStart(2,"0"); const el=$("clock"); if(el) el.textContent=`${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`; }
tickClock(); setInterval(tickClock,1000);
async function tickGwei(){ try{ const g=await api("/gas?chainId=1"); $("gwei").textContent=g.gwei; }catch{} }

// ---------- boot ----------
function loadAll(){ loadTasks(); loadWallets(); loadRPC(); }
async function bootData(){
  await loadAppCfg();   // load config first so chains can be filtered + task defaults applied
  await loadChains();
  await refreshStatus();
  go("tasks");
  await loadLogs();
}
(async()=>{
  try{ applyScale(localStorage.getItem("uiScale") || 100); }catch{}   // apply saved UI scale ASAP
  setupWindowDrag();
  connectWS(); // starts the reconnect loop; recovers boot data on reopen
  try { await bootData(); }
  catch(e){ setOffline(true); } // banner explains; WS reconnect will retry
  tickGwei(); setInterval(tickGwei, 15000);
})();
