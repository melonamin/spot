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

  const query = (params) => {
    const qs = new URLSearchParams();
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== false && value !== null) qs.set(key, String(value));
    }
    const encoded = qs.toString();
    return encoded ? `?${encoded}` : '';
  };

  const collection = (name) => ({
    // list({ limit, mine }). Each document carries an `owner` (the mesh
    // identity that created it). Pass mine:true to return only documents
    // owned by the current visitor. Pass after:<doc id> for cursor paging.
    list: async ({ limit = 100, mine = false, after } = {}) => {
      return (await api(`/api/db/${name}${query({ limit, mine, after })}`)).documents;
    },
    create: (data) =>
      api(`/api/db/${name}`, { method: 'POST', body: JSON.stringify(data) }),
    get: (id) => api(`/api/db/${name}/${id}`),
    update: (id, data, { mine = false } = {}) =>
      api(`/api/db/${name}/${id}${query({ mine })}`, { method: 'PUT', body: JSON.stringify(data) }),
    updateMine: (id, data) =>
      api(`/api/db/${name}/${id}?mine=true`, { method: 'PUT', body: JSON.stringify(data) }),
    delete: (id, { mine = false } = {}) =>
      api(`/api/db/${name}/${id}${query({ mine })}`, { method: 'DELETE' }),
    deleteMine: (id) => api(`/api/db/${name}/${id}?mine=true`, { method: 'DELETE' }),
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
    const statusHandlers = new Set();
    const queue = [];
    const maxQueue = 100;
    let ws;
    let closed = false;
    let presenceData;
    let reconnectTimer;
    let status = 'connecting';

    const emitStatus = (next) => {
      if (status === next) return;
      status = next;
      for (const handler of statusHandlers) handler(status);
    };

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
      emitStatus(ws ? 'reconnecting' : 'connecting');
      ws = new WebSocket(`${proto}//${location.host}/api/ws`);
      ws.addEventListener('open', () => {
        emitStatus('open');
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
        emitStatus('reconnecting');
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
      onStatus: (handler) => {
        statusHandlers.add(handler);
        handler(status);
        return () => statusHandlers.delete(handler);
      },
      close: () => {
        closed = true;
        emitStatus('closed');
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
      // stream([{role, content}, ...], {onToken, model, system, max_tokens, signal})
      // Calls onToken(delta, text) as tokens arrive and resolves with the
      // final { text, model, stop_reason, usage }.
      stream: async (messages, opts = {}) => {
        const { onToken, signal, ...rest } = opts;
        const res = await fetch('/api/ai/chat/stream', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ messages, ...rest }),
          signal,
        });
        if (!res.ok || !res.body) {
          const body = await res.json().catch(() => null);
          throw new Error((body && body.error) || `spot: HTTP ${res.status}`);
        }
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        const result = { text: '', model: undefined, stop_reason: undefined, usage: undefined };
        let buffer = '';
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let sep;
          while ((sep = buffer.indexOf('\n\n')) >= 0) {
            const frame = buffer.slice(0, sep);
            buffer = buffer.slice(sep + 2);
            const line = frame.split('\n').find((l) => l.startsWith('data:'));
            if (!line) continue;
            const payload = line.slice(5).trim();
            if (!payload) continue;
            let msg;
            try {
              msg = JSON.parse(payload);
            } catch {
              continue;
            }
            if (msg.error) throw new Error(msg.error);
            if (msg.delta) {
              result.text += msg.delta;
              onToken?.(msg.delta, result.text);
            }
            if (msg.done) {
              result.model = msg.model;
              result.stop_reason = msg.stop_reason;
              result.usage = msg.usage;
            }
          }
        }
        return result;
      },
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
      // list() -> [{ id, name, size, url }, ...]
      list: async () => (await api('/api/files')).files,
      // delete(file) where file is a stored descriptor or (id, name).
      delete: (file, name) => {
        const id = typeof file === 'object' ? file.id : file;
        const fileName = typeof file === 'object' ? file.name : name;
        return api(`/api/files/${encodeURIComponent(id)}/${encodeURIComponent(fileName)}`, {
          method: 'DELETE',
        });
      },
    },
    sites: {
      // Apex-only platform APIs: these work from the Spot root, not site subdomains.
      mine: async () => (await api('/api/sites/mine')).sites,
      public: async () => (await api('/api/sites/public')).sites,
      delete: (name) => api(`/api/sites/${encodeURIComponent(name)}`, { method: 'DELETE' }),
    },
  };
})();
