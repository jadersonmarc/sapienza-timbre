// PWA da portaria. Valida o QR OFFLINE com a chave pública (WebCrypto Ed25519),
// mantém uma fila local de check-ins e sincroniza ao reconectar. A prevenção de
// duplicidade final é do servidor (reconciliação); localmente evitamos o óbvio.
'use strict';

const API = location.origin;
const K = {
  token: 'timbre_gate_token', pub: 'timbre_gate_pub',
  admitted: 'timbre_gate_admitted', queue: 'timbre_gate_queue',
  seatmap: id => 'timbre_gate_seatmap_' + id,
};
const $ = id => document.getElementById(id);
const getJSON = (k, d) => { try { return JSON.parse(localStorage.getItem(k)) ?? d; } catch { return d; } };
const setJSON = (k, v) => localStorage.setItem(k, JSON.stringify(v));

let pubKey = null;         // CryptoKey Ed25519
let admitted = new Set(getJSON(K.admitted, []));
let queue = getJSON(K.queue, []);
let seatmap = {};          // seat_id -> {sector,row,number}
let scanning = false;

// ── base64url ────────────────────────────────────────────────────────────────
function b64urlToBytes(s) {
  s = s.replace(/-/g, '+').replace(/_/g, '/'); while (s.length % 4) s += '=';
  const bin = atob(s); const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

async function importPub(b64) {
  const raw = b64urlToBytes(b64.replace(/-/g, '+').replace(/_/g, '/'));
  try {
    return await crypto.subtle.importKey('raw', raw, { name: 'Ed25519' }, false, ['verify']);
  } catch (e) { console.warn('Ed25519 indisponível neste navegador', e); return null; }
}

// Verifica o token OFFLINE. Devolve o payload ou null.
async function verifyToken(token) {
  const dot = token.indexOf('.'); if (dot < 0) return null;
  const msg = b64urlToBytes(token.slice(0, dot));
  const sig = b64urlToBytes(token.slice(dot + 1));
  if (!pubKey) return null;
  const ok = await crypto.subtle.verify({ name: 'Ed25519' }, pubKey, sig, msg);
  if (!ok) return null;
  try { return JSON.parse(new TextDecoder().decode(msg)); } catch { return null; }
}

// ── check-in ─────────────────────────────────────────────────────────────────
function uid() { return (crypto.randomUUID ? crypto.randomUUID() : Date.now() + '-' + Math.random()); }

async function handleToken(token) {
  token = (token || '').trim(); if (!token) return;
  const p = await verifyToken(token);
  if (!p) return showVerdict('invalid', null);
  if (admitted.has(p.tid)) return showVerdict('duplicate', p.sid);
  admitted.add(p.tid); setJSON(K.admitted, [...admitted]);
  queue.push({ token, gate: $('gateName').value || 'G1', device_id: deviceId(), client_uid: uid(),
    reentry: false, entered_at: new Date().toISOString() });
  setJSON(K.queue, queue); renderQueue();
  showVerdict('admitted', p.sid);
  if (navigator.onLine) sync();
}

function showVerdict(kind, seatId) {
  const el = $('verdict'); el.className = ''; el.classList.add(kind); el.style.display = 'block';
  const labels = { admitted: 'ENTRAR', reentry: 'REENTRADA', duplicate: 'JÁ ENTROU', invalid: 'INVÁLIDO', unknown: 'DESCONHECIDO' };
  el.querySelector('.v').textContent = labels[kind] || kind.toUpperCase();
  const s = seatId && seatmap[seatId];
  el.querySelector('.seat').textContent = s ? `${s.sector} · fila ${s.row} · assento ${s.number}` : '';
  if (navigator.vibrate) navigator.vibrate(kind === 'admitted' ? 60 : [40, 40, 40]);
}

function deviceId() {
  let d = localStorage.getItem('timbre_gate_device');
  if (!d) { d = uid(); localStorage.setItem('timbre_gate_device', d); }
  return d;
}

// ── sync ─────────────────────────────────────────────────────────────────────
async function sync() {
  // Sincroniza mesmo com a fila vazia: é assim que o aparelho se anuncia ao painel e diz
  // com qual chave está, ANTES do dia do evento. Aparelho que só aparece quando tem
  // check-in para entregar só é descoberto desatualizado na fila da porta.
  if (!navigator.onLine) { renderQueue(); return; }
  try {
    const r = await api('POST', '/api/v1/gate/sync', {
      checkins: queue, device_id: deviceId(), gate: $('gateName').value || 'G1',
      key_fingerprint: await keyFingerprint(),
    });
    if (r && Array.isArray(r.results)) { queue = []; setJSON(K.queue, queue); renderQueue(); }
  } catch (e) { console.warn('sync falhou', e); }
}
// Impressão da chave que ESTE aparelho tem embarcada. Vai junto do sync para o painel
// conseguir apontar o aparelho que ficou com a chave velha — ele recusa ingresso legítimo
// com a mesma cara de quem recusa um falso.
async function keyFingerprint() {
  const pub = localStorage.getItem(K.pub);
  if (!pub || !crypto.subtle) return '';
  const buf = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(pub));
  return [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, '0')).join('').slice(0, 12);
}

function renderQueue() { $('queue').textContent = queue.length ? `${queue.length} check-in(s) na fila` : 'fila vazia · tudo sincronizado'; }

// ── API ──────────────────────────────────────────────────────────────────────
async function api(method, path, body) {
  const res = await fetch(API + path, {
    method, headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + localStorage.getItem(K.token) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) { logout(); throw new Error('não autenticado'); }
  return res.json().catch(() => ({}));
}

// ── câmera ───────────────────────────────────────────────────────────────────
async function startScan() {
  if (!('BarcodeDetector' in window)) { alert('Câmera/leitor indisponível — use "colar código".'); return; }
  const cam = $('cam');
  try {
    const stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: 'environment' } });
    cam.srcObject = stream; cam.style.display = 'block'; await cam.play();
    const det = new BarcodeDetector({ formats: ['qr_code'] });
    scanning = true; let last = '', lastAt = 0;
    const loop = async () => {
      if (!scanning) return;
      try {
        const codes = await det.detect(cam);
        if (codes[0]) {
          const v = codes[0].rawValue, now = Date.now();
          if (v !== last || now - lastAt > 2500) { last = v; lastAt = now; handleToken(v); }
        }
      } catch {}
      requestAnimationFrame(loop);
    };
    loop();
  } catch (e) { alert('Não foi possível abrir a câmera: ' + e.message); }
}

// ── eventos / mapa de assentos ───────────────────────────────────────────────
async function loadEvents() {
  const r = await api('GET', '/api/v1/events').catch(() => null);
  if (!r || !r.events) return;
  const sel = $('eventSel');
  for (const e of r.events) { const o = document.createElement('option'); o.value = e.id; o.textContent = e.title; sel.appendChild(o); }
}
async function loadSeatmap(eventId) {
  if (!eventId) { seatmap = {}; return; }
  const cached = getJSON(K.seatmap(eventId), null);
  if (cached) seatmap = cached;
  if (navigator.onLine) {
    const r = await api('GET', `/api/v1/gate/events/${eventId}/seatmap`).catch(() => null);
    if (r && r.seats) { seatmap = {}; for (const s of r.seats) seatmap[s.seat_id] = s; setJSON(K.seatmap(eventId), seatmap); }
  }
}

// ── auth / boot ──────────────────────────────────────────────────────────────
async function login() {
  $('loginErr').textContent = '';
  try {
    const res = await fetch(API + '/api/v1/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: $('email').value, password: $('password').value }) });
    const j = await res.json();
    if (!res.ok || !j.token) { $('loginErr').textContent = 'Credenciais inválidas'; return; }
    localStorage.setItem(K.token, j.token); boot();
  } catch { $('loginErr').textContent = 'Falha de rede'; }
}
function logout() { localStorage.removeItem(K.token); $('gateView').classList.add('hide'); $('loginView').classList.remove('hide'); scanning = false; }

async function boot() {
  if (!localStorage.getItem(K.token)) { $('loginView').classList.remove('hide'); return; }
  $('loginView').classList.add('hide'); $('gateView').classList.remove('hide');
  // chave pública: cacheada para operar offline.
  let pub = localStorage.getItem(K.pub);
  if (navigator.onLine) { const c = await api('GET', '/api/v1/gate/config').catch(() => null); if (c && c.public_key) { pub = c.public_key; localStorage.setItem(K.pub, pub); } }
  if (pub) pubKey = await importPub(pub);
  if (navigator.onLine) loadEvents();
  renderQueue(); sync();
}

// ── wiring ───────────────────────────────────────────────────────────────────
function setNet() { $('net').textContent = navigator.onLine ? 'online' : 'offline'; if (navigator.onLine) sync(); }
addEventListener('online', setNet); addEventListener('offline', setNet);

$('loginBtn').onclick = login;
$('logoutBtn').onclick = logout;
$('scanBtn').onclick = startScan;
$('syncBtn').onclick = sync;
$('manualBtn').onclick = () => handleToken($('manual').value);
$('eventSel').onchange = e => loadSeatmap(e.target.value);

if ('serviceWorker' in navigator) navigator.serviceWorker.register('sw.js').catch(() => {});
setNet(); boot();
