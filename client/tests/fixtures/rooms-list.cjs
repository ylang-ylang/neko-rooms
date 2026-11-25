// Keep the unshortened pinned image from the reported layout regression.
const pinnedImage = 'ghcr.io/m1k1o/neko/chromium:3.2.0@sha256:270face8b8111e639744fb7bcdbbf0deed227900bb89d58b2f8adfc12af8c41e'
const rooms = [
  {
    id: 'fixture-running', name: 'native-single-port-demo',
    neko_image: pinnedImage, url: '/room/native-single-port-demo/',
    running: true, paused: false, is_outdated: false,
    status: 'Up 19 minutes (healthy)', created: '2026-11-24T12:00:00Z', max_connections: 0,
  },
  {
    id: 'fixture-stopped', name: 'native-single-port-demo-with-a-very-long-room-name-and-unbrokentext0123456789',
    neko_image: pinnedImage, url: '/room/long-room-name/',
    running: false, paused: false, is_outdated: true,
    status: 'Exited (0) 2 minutes ago', created: '2026-11-24T12:00:00Z', max_connections: 5,
  },
]

module.exports = { pinnedImage, rooms }
