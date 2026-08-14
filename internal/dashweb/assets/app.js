// Painel do produtor: login, seleciona evento e faz polling do overview para
// acompanhar a venda em tempo real. Simples de propósito (mobile-first).
'use strict';
const API = location.origin;
const TK = 'timbre_dash_token';
const $ = id => document.getElementById(id);
let timer = null, currentEvent = '';

const brl = c => (c / 100).toLocaleString('pt-BR', { style: 'currency', currency: 'BRL' });
const pct = (a, b) => b > 0 ? Math.round(a * 100 / b) : 0;

async function api(path) {
  const res = await fetch(API + path, { headers: { 'Authorization': 'Bearer ' + localStorage.getItem(TK) } });
  if (res.status === 401) { logout(); throw new Error('401'); }
  return res.json();
}

async function login() {
  $('loginErr').textContent = '';
  try {
    const res = await fetch(API + '/api/v1/auth/login', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: $('email').value, password: $('password').value }),
    });
    const j = await res.json();
    if (!res.ok || !j.token) { $('loginErr').textContent = 'Credenciais inválidas'; return; }
    localStorage.setItem(TK, j.token); boot();
  } catch { $('loginErr').textContent = 'Falha de rede'; }
}
function logout() { localStorage.removeItem(TK); clearInterval(timer); $('dashView').classList.add('hide'); $('loginView').classList.remove('hide'); }

async function loadEvents() {
  const r = await api('/api/v1/events').catch(() => null);
  const sel = $('eventSel'); sel.length = 1;
  if (r && r.events) for (const e of r.events) { const o = document.createElement('option'); o.value = e.id; o.textContent = e.title; sel.appendChild(o); }
}

async function refresh() {
  if (!currentEvent) return;
  const d = await api('/api/v1/dash/events/' + currentEvent).catch(() => null);
  if (!d) return;
  $('live').textContent = 'atualizado ' + new Date().toLocaleTimeString('pt-BR');
  const f = d.finance || {};
  $('repasse').textContent = brl(f.repasse_cents || 0);
  $('gross').textContent = brl(f.gross_cents || 0);
  $('taxa').textContent = brl(f.taxa_cents || 0);
  const chk = d.checkin || {}, occ = d.occupancy || {};
  $('tickets').textContent = chk.tickets_total || 0;
  $('occv').textContent = (occ.seats_sold || 0) + ' / ' + (occ.seats_total || 0);
  $('occbar').style.width = pct(occ.seats_sold || 0, occ.seats_total || 0) + '%';
  $('chkv').textContent = (chk.admitted || 0) + ' / ' + (chk.tickets_total || 0);
  $('chkbar').style.width = pct(chk.admitted || 0, chk.tickets_total || 0) + '%';
  const tb = $('lots'); tb.innerHTML = '';
  for (const l of (d.sales || [])) {
    const tr = document.createElement('tr');
    tr.innerHTML = `<td>${l.name}</td><td>${l.sold_count}/${l.quantity}</td><td>${brl(l.revenue_cents)}</td>`;
    tb.appendChild(tr);
  }
}

function selectEvent(id) {
  currentEvent = id; clearInterval(timer);
  const panel = $('panel');
  if (!id) { panel.classList.add('hide'); return; }
  panel.classList.remove('hide');
  refresh(); timer = setInterval(refresh, 4000);
}

// Download do CSV com o header de auth (um <a> simples não enviaria o Bearer).
async function downloadCSV(e) {
  e.preventDefault();
  if (!currentEvent) return;
  const res = await fetch(API + '/api/v1/dash/events/' + currentEvent + '/export.csv', {
    headers: { 'Authorization': 'Bearer ' + localStorage.getItem(TK) },
  });
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url; a.download = 'ingressos.csv'; a.click();
  URL.revokeObjectURL(url);
}

function boot() {
  if (!localStorage.getItem(TK)) { $('loginView').classList.remove('hide'); return; }
  $('loginView').classList.add('hide'); $('dashView').classList.remove('hide');
  loadEvents();
}

$('loginBtn').onclick = login;
$('logout').onclick = e => { e.preventDefault(); logout(); };
$('csv').onclick = downloadCSV;
$('eventSel').onchange = e => selectEvent(e.target.value);
boot();
