// Service worker da portaria: cacheia o app shell para abrir offline. As chamadas de
// API não são cacheadas (a validação do QR é feita localmente com a chave pública, e os
// check-ins ficam numa fila em localStorage até sincronizar).
const CACHE = 'timbre-gate-v1';
const SHELL = ['./', 'index.html', 'app.js', 'manifest.webmanifest'];

self.addEventListener('install', e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll(SHELL)).then(() => self.skipWaiting()));
});
self.addEventListener('activate', e => {
  e.waitUntil(caches.keys().then(ks => Promise.all(ks.filter(k => k !== CACHE).map(k => caches.delete(k)))).then(() => self.clients.claim()));
});
self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if (url.pathname.includes('/api/')) return; // deixa a rede/tratamento offline com a app
  e.respondWith(caches.match(e.request).then(r => r || fetch(e.request)));
});
