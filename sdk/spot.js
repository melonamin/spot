// Spot browser SDK. Every site loads this from its own origin:
//
//   <script src="/spot.js"></script>
//
// All calls are same-origin: Spot routes /api/* on every site host, so
// there is no CORS and no configuration.
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
      let reconnectTimer;
      const connect = () => {
        if (closed) return;
        ws = new WebSocket(`${proto}//${location.host}/api/ws`);
        ws.addEventListener('open', () => {
          ws.send(JSON.stringify({ type: 'subscribe', collection: name }));
        });
        ws.addEventListener('message', (e) => {
          let msg;
          try {
            msg = JSON.parse(e.data);
          } catch {
            return;
          }
          if (msg.type === 'error') {
            const err = new Error(msg.error || 'spot subscribe error');
            if (handlers.onError) handlers.onError(err);
            else console.error('spot:', err.message);
            return;
          }
          if (msg.collection !== name) return;
          if (msg.type === 'create') handlers.onCreate?.(msg.doc);
          if (msg.type === 'update') handlers.onUpdate?.(msg.doc);
          if (msg.type === 'delete') handlers.onDelete?.(msg.id);
        });
        ws.addEventListener('close', () => {
          if (!closed) reconnectTimer = setTimeout(connect, 1000);
        });
      };
      connect();
      return () => {
        closed = true;
        if (reconnectTimer) clearTimeout(reconnectTimer);
        if (ws) ws.close();
      };
    },
  });

  const realtimeRoom = (name) => {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const handlers = new Map();
    const presenceHandlers = new Set();
    const errorHandlers = new Set();
    const queue = [];
    const maxQueue = 100;
    let ws;
    let closed = false;
    let presenceData;
    let reconnectTimer;

    const sendWire = (msg) => {
      if (closed) return;
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(msg));
        return;
      }
      if (msg.type === 'room_presence') return;
      if (queue.length >= maxQueue) queue.shift();
      queue.push(msg);
    };

    const flush = () => {
      while (queue.length && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(queue.shift()));
      }
    };

    const emitError = (err) => {
      if (errorHandlers.size === 0) {
        console.warn('spot realtime:', err.message || err);
        return;
      }
      for (const handler of errorHandlers) handler(err);
    };

    const connect = () => {
      if (closed) return;
      ws = new WebSocket(`${proto}//${location.host}/api/ws`);
      ws.addEventListener('open', () => {
        ws.send(JSON.stringify({ type: 'room_join', room: name }));
        if (presenceData !== undefined) {
          ws.send(JSON.stringify({ type: 'room_presence', room: name, data: presenceData }));
        }
        flush();
      });
      ws.addEventListener('message', (e) => {
        let msg;
        try {
          msg = JSON.parse(e.data);
        } catch {
          return;
        }
        if (msg.type === 'error') {
          emitError(new Error(msg.error || 'realtime error'));
          return;
        }
        if (msg.room !== name) return;
        if (msg.type === 'room_message') {
          const set = handlers.get(msg.event);
          if (!set) return;
          for (const handler of set) {
            handler({
              event: msg.event,
              room: msg.room,
              from: msg.from,
              data: msg.data,
              sent_at: msg.sent_at,
            });
          }
        }
        if (msg.type === 'room_presence') {
          for (const handler of presenceHandlers) handler(msg.users || []);
        }
      });
      ws.addEventListener('close', () => {
        if (closed) return;
        reconnectTimer = setTimeout(() => {
          reconnectTimer = undefined;
          connect();
        }, 1000);
      });
      ws.addEventListener('error', () => {
        emitError(new Error('realtime connection error'));
      });
    };
    connect();

    return {
      send: (event, data = null) => {
        sendWire({ type: 'room_send', room: name, event, data });
      },
      setPresence: (data = null) => {
        presenceData = data;
        sendWire({ type: 'room_presence', room: name, data });
      },
      on: (event, handler) => {
        if (!handlers.has(event)) handlers.set(event, new Set());
        handlers.get(event).add(handler);
        return () => handlers.get(event)?.delete(handler);
      },
      onPresence: (handler) => {
        presenceHandlers.add(handler);
        return () => presenceHandlers.delete(handler);
      },
      onError: (handler) => {
        errorHandlers.add(handler);
        return () => errorHandlers.delete(handler);
      },
      close: () => {
        closed = true;
        if (reconnectTimer) {
          clearTimeout(reconnectTimer);
          reconnectTimer = undefined;
        }
        if (ws && ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: 'room_leave', room: name }));
        }
        if (ws) ws.close();
      },
    };
  };

  window.spot = {
    // Who is visiting, according to the mesh.
    me: () => api('/api/me'),
    db: { collection },
    realtime: {
      // Ephemeral room for multiplayer/control-panel traffic.
      // room.send(type, data), room.on(type, handler), room.onPresence(handler)
      room: realtimeRoom,
    },
    ai: {
      // chat([{role, content}, ...], {model, system, max_tokens})
      // -> { text, model, stop_reason, usage }
      chat: (messages, opts = {}) =>
        api('/api/ai/chat', {
          method: 'POST',
          body: JSON.stringify({ messages, ...opts }),
        }),
      // image('prompt', {model, size, aspect_ratio, image_size, quality, output_format})
      // -> { provider, model, images: [{ b64, mime_type, data_url }] }
      image: (prompt, opts = {}) =>
        api('/api/ai/image', {
          method: 'POST',
          body: JSON.stringify({ prompt, ...opts }),
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
