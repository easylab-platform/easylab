const CACHE = 'zergx-static-v1'

self.addEventListener('install', event => {
  self.skipWaiting()
})

self.addEventListener('activate', event => {
  event.waitUntil(
    caches
      .keys()
      .then(keys =>
        Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))),
      )
      .then(() => self.clients.claim()),
  )
})

self.addEventListener('fetch', event => {
  const url = new URL(event.request.url)
  if (event.request.method !== 'GET') return
  const isStatic =
    url.pathname.startsWith('/assets/') ||
    url.pathname === '/' ||
    url.pathname === '/manifest.webmanifest' ||
    url.pathname.startsWith('/icons/')
  if (!isStatic) return

  event.respondWith(
    fetch(event.request)
      .then(resp => {
        const copy = resp.clone()
        caches.open(CACHE).then(cache =>
          cache.put(event.request, copy).catch(() => {}),
        )
        return resp
      })
      .catch(() => caches.match(event.request)),
  )
})
