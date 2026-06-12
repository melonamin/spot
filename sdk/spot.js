// Spot browser SDK. Every site loads this from its own origin:
//
//   <script src="/spot.js"></script>
//
// All calls are same-origin: Caddy routes /api/* on every site host to
// the shared backend, so there is no CORS and no configuration.
(() => {
  const api = async (path, opts = {}) => {
    const res = await fetch(path, {
      headers: { 'Content-Type': 'application/json' },
      ...opts,
    });
    if (res.status === 204) return null;
    const body = await res.json().catch(() => null);
    if (!res.ok) {
      throw new Error((body && body.error) || `spot: HTTP ${res.status}`);
    }
    return body;
  };

  const collection = (name) => ({
    list: async ({ limit = 100 } = {}) =>
      (await api(`/api/db/${name}?limit=${limit}`)).documents,
    create: (data) =>
      api(`/api/db/${name}`, { method: 'POST', body: JSON.stringify(data) }),
    get: (id) => api(`/api/db/${name}/${id}`),
    update: (id, data) =>
      api(`/api/db/${name}/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
    delete: (id) => api(`/api/db/${name}/${id}`, { method: 'DELETE' }),
    // Live changes from all visitors. Returns an unsubscribe function;
    // reconnects automatically until unsubscribed.
    subscribe: (handlers = {}) => {
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      let ws;
      let closed = false;
      const connect = () => {
        ws = new WebSocket(`${proto}//${location.host}/api/ws`);
        ws.addEventListener('open', () => {
          ws.send(JSON.stringify({ type: 'subscribe', collection: name }));
        });
        ws.addEventListener('message', (e) => {
          const msg = JSON.parse(e.data);
          if (msg.collection !== name) return;
          if (msg.type === 'create') handlers.onCreate?.(msg.doc);
          if (msg.type === 'update') handlers.onUpdate?.(msg.doc);
          if (msg.type === 'delete') handlers.onDelete?.(msg.id);
        });
        ws.addEventListener('close', () => {
          if (!closed) setTimeout(connect, 1000);
        });
      };
      connect();
      return () => {
        closed = true;
        ws.close();
      };
    },
  });

  window.spot = {
    // Who is visiting, according to the NetBird mesh.
    me: () => api('/api/me'),
    db: { collection },
    ai: {
      // chat([{role, content}, ...], {model, system, max_tokens})
      // -> { text, model, stop_reason, usage }
      chat: (messages, opts = {}) =>
        api('/api/ai/chat', {
          method: 'POST',
          body: JSON.stringify({ messages, ...opts }),
        }),
    },
    files: {
      // upload(File|Blob) -> { id, name, size, content_type, url }
      upload: async (file, { name } = {}) => {
        const form = new FormData();
        form.append('file', file, name || file.name || 'file');
        const res = await fetch('/api/files', { method: 'POST', body: form });
        const body = await res.json().catch(() => null);
        if (!res.ok) {
          throw new Error((body && body.error) || `spot: HTTP ${res.status}`);
        }
        return body;
      },
    },
  };
})();
