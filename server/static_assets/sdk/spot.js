// Spot browser SDK. Every site loads this from its own origin:
//
//   <script src="/spot.js"></script>
//
// All calls are same-origin: Spot routes /api/* on every site host, so
// there is no CORS and no configuration.
(() => {
  // Tunable request behavior. spot.configure({ retry }) sets the default for
  // every call; any call also takes a per-call { retry } override (false to
  // disable, a number to cap the retries).
  const config = { retry: true, maxRetries: 4, retryBaseMs: 200, retryCapMs: 10000 };

  const configure = (opts = {}) => {
    Object.assign(config, opts);
    return { ...config };
  };

  // SpotError carries the HTTP status, a coarse machine-readable code, and (on
  // a 429) the server's Retry-After hint in seconds. Sites can branch on
  // err.code or `err instanceof spot.SpotError`.
  class SpotError extends Error {
    constructor(message, { status = 0, code = 'error', retryAfter } = {}) {
      super(message);
      this.name = 'SpotError';
      this.status = status;
      this.code = code;
      if (retryAfter !== undefined) this.retryAfter = retryAfter;
    }
  }

  const codeForStatus = (status) => {
    if (status === 429) return 'rate_limited';
    if (status === 401) return 'unauthorized';
    if (status === 403) return 'forbidden';
    if (status === 404) return 'not_found';
    if (status === 409) return 'conflict';
    if (status >= 400 && status < 500) return 'bad_request';
    if (status >= 500) return 'server';
    return 'error';
  };

  // Methods safe to replay after a network or 5xx failure that may have been
  // applied server-side. A 429 is always safe to retry regardless of method:
  // the rate limiter rejects before the handler runs, so the request never
  // took effect.
  const replayable = new Set(['GET', 'HEAD', 'OPTIONS']);

  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

  const retriesFor = (retry) => {
    const setting = retry === undefined ? config.retry : retry;
    if (setting === false) return 0;
    if (setting === true) return config.maxRetries;
    if (typeof setting === 'number' && setting >= 0) return Math.floor(setting);
    return config.maxRetries;
  };

  const retryAfterSeconds = (res) => {
    const header = res.headers.get('Retry-After');
    if (!header) return undefined;
    // Retry-After is either delta-seconds or an HTTP-date; support both.
    const seconds = Number(header);
    if (Number.isFinite(seconds)) return seconds >= 0 ? seconds : undefined;
    const when = Date.parse(header);
    if (Number.isNaN(when)) return undefined;
    return Math.max(0, (when - Date.now()) / 1000);
  };

  // Full jitter for backoff. For an explicit Retry-After, wait at least the
  // hinted seconds (plus a little jitter) so we don't retry into the same
  // still-closed bucket.
  const backoffDelay = (attempt, retryAfter) => {
    if (retryAfter !== undefined) return retryAfter * 1000 + Math.random() * 250;
    const ceiling = Math.min(config.retryBaseMs * 2 ** attempt, config.retryCapMs);
    return Math.random() * ceiling;
  };

  // Single fetch path for every JSON and upload call: builds SpotError
  // consistently and applies the retry policy. Streaming (ai.stream) does not
  // go through here — a streamed body cannot be safely replayed.
  const request = async (path, opts = {}) => {
    const { retry, ...init } = opts;
    const method = (init.method || 'GET').toUpperCase();
    const maxRetries = retriesFor(retry);
    const headers =
      init.headers ||
      (typeof init.body === 'string' ? { 'Content-Type': 'application/json' } : undefined);
    for (let attempt = 0; ; attempt++) {
      let res;
      try {
        res = await fetch(path, { ...init, headers });
      } catch (err) {
        if (err && err.name === 'AbortError') throw err;
        if (attempt < maxRetries && replayable.has(method)) {
          await sleep(backoffDelay(attempt));
          continue;
        }
        throw new SpotError((err && err.message) || 'spot: network error', { code: 'network' });
      }
      if (res.status === 204) return null;
      const body = await res.json().catch(() => null);
      if (res.ok) return body;
      const retryAfter = retryAfterSeconds(res);
      const retryable =
        res.status === 429 ? true : res.status === 503 ? replayable.has(method) : false;
      if (retryable && attempt < maxRetries) {
        await sleep(backoffDelay(attempt, retryAfter));
        continue;
      }
      throw new SpotError((body && body.error) || `spot: HTTP ${res.status}`, {
        status: res.status,
        code: codeForStatus(res.status),
        retryAfter,
      });
    }
  };

  const api = (path, opts) => request(path, opts);

  const query = (params) => {
    const qs = new URLSearchParams();
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== false && value !== null) qs.set(key, String(value));
    }
    const encoded = qs.toString();
    return encoded ? `?${encoded}` : '';
  };

  const collection = (name) => {
    const c = {
      // list({ limit, mine }). Each document carries an `owner` (the mesh
      // identity that created it). Pass mine:true to return only documents
      // owned by the current visitor. Pass after:<doc id> for cursor paging.
      // list({ limit, mine, after, where, sort, order }). `where` is an object
      // of field -> value (equality) or field -> { op: value } where op is one
      // of eq, ne, gt, gte, lt, lte, in (in takes an array). `sort` names a
      // field; `order` is 'asc' or 'desc'. The `after` cursor applies to the
      // default created-at order in either direction.
      list: async ({ limit = 100, mine = false, after, where, sort, order, retry } = {}) => {
        const params = { limit, mine, after, sort, order };
        if (where) params.where = JSON.stringify(where);
        return (await api(`/api/db/${name}${query(params)}`, { retry })).documents;
      },
      // count({ where, mine }) -> number of matching documents.
      count: async ({ where, mine = false, retry } = {}) => {
        const params = { mine };
        if (where) params.where = JSON.stringify(where);
        return (await api(`/api/db/${name}/count${query(params)}`, { retry })).count;
      },
      // getMany([id, ...]) -> the documents that exist, newest first. Missing
      // ids are omitted, and null/undefined ids are dropped before the request
      // so a trailing absent id does not turn the whole batch into a 400.
      getMany: async (ids = [], { retry } = {}) => {
        const valid = ids.filter((id) => id !== undefined && id !== null && id !== '');
        if (!valid.length) return [];
        return (await api(`/api/db/${name}${query({ ids: valid.join(',') })}`, { retry })).documents;
      },
      // Async iterator over the whole collection, newest first, paging through
      // the `after` cursor so a site never has to manage it by hand:
      //   for await (const doc of posts.iterate({ pageSize })) { … }
      iterate: async function* ({ pageSize = 100, mine = false, where, retry } = {}) {
        let after;
        for (;;) {
          const params = { limit: pageSize, mine, after };
          if (where) params.where = JSON.stringify(where);
          const page = (await api(`/api/db/${name}${query(params)}`, { retry })).documents;
          for (const doc of page) yield doc;
          if (page.length < pageSize) return;
          after = page[page.length - 1].id;
        }
      },
      create: (data, { retry } = {}) =>
        api(`/api/db/${name}`, { method: 'POST', body: JSON.stringify(data), retry }),
      get: (id, { retry } = {}) => api(`/api/db/${name}/${id}`, { retry }),
      update: (id, data, { mine = false, retry } = {}) =>
        api(`/api/db/${name}/${id}${query({ mine })}`, {
          method: 'PUT',
          body: JSON.stringify(data),
          retry,
        }),
      updateMine: (id, data, opts = {}) => c.update(id, data, { ...opts, mine: true }),
      delete: (id, { mine = false, retry } = {}) =>
        api(`/api/db/${name}/${id}${query({ mine })}`, { method: 'DELETE', retry }),
      deleteMine: (id, opts = {}) => c.delete(id, { ...opts, mine: true }),
      // Atomically add `by` (default 1) to a numeric field, server-side, so
      // concurrent counters never lose updates. Resolves with the updated doc.
      increment: (id, field, by = 1, { mine = false, retry } = {}) =>
        api(`/api/db/${name}/${id}/increment${query({ mine })}`, {
          method: 'POST',
          body: JSON.stringify({ field, by }),
          retry,
        }),
      incrementMine: (id, field, by = 1, opts = {}) =>
        c.increment(id, field, by, { ...opts, mine: true }),
      // Live changes from all visitors. Returns an unsubscribe function;
      // reconnects automatically until unsubscribed. With { replay: true } the
      // current documents are emitted as onCreate first (in creation order),
      // then live changes. The snapshot also runs after every reconnect and
      // reconciles against what was already delivered, so creates, updates, and
      // deletes that happened while the socket was down are surfaced exactly
      // once rather than missed.
      subscribe: (handlers = {}, { replay = false, retry } = {}) => {
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
        let ws;
        let closed = false;
        let reconnectTimer;
        let replaying = false;
        let replayAgain = false;
        // Last updated_at delivered per id, so a replay after a reconnect can
        // tell a new doc (onCreate) from a changed one (onUpdate) from one that
        // vanished while we were offline (onDelete).
        const seen = new Map();
        const pending = [];

        const emit = (type, doc, id) => {
          if (type === 'create') handlers.onCreate?.(doc);
          else if (type === 'update') handlers.onUpdate?.(doc);
          else if (type === 'delete') handlers.onDelete?.(id);
        };

        // In replay mode, reconcile each event against the docs already
        // delivered so each surfaces as a single onCreate and later changes as
        // onUpdate; outside replay mode events pass through unchanged.
        const dispatch = (type, doc, id) => {
          if (!replay) {
            emit(type, doc, id);
            return;
          }
          if (type === 'delete') {
            if (seen.delete(id)) emit('delete', null, id);
            return;
          }
          if (seen.has(doc.id)) {
            seen.set(doc.id, doc.updated_at);
            if (type === 'create') return;
            emit('update', doc);
            return;
          }
          seen.set(doc.id, doc.updated_at);
          emit('create', doc);
        };

        const handleEvent = (type, doc, id) => {
          if (replaying) {
            pending.push({ type, doc, id });
            return;
          }
          dispatch(type, doc, id);
        };

        // Snapshot the collection oldest-first and reconcile it against what we
        // have already delivered. Pages stream as they arrive (only ids are
        // retained, not whole documents), and the ascending cursor lets us emit
        // in creation order without buffering the whole collection.
        const runReplay = async () => {
          if (replaying) {
            replayAgain = true;
            return;
          }
          replaying = true;
          replayAgain = false;
          try {
            const pageSize = 1000;
            const present = new Set();
            let after;
            for (;;) {
              const docs = (
                await api(`/api/db/${name}${query({ limit: pageSize, after, order: 'asc' })}`, {
                  retry,
                })
              ).documents;
              for (const doc of docs) {
                present.add(doc.id);
                const prev = seen.get(doc.id);
                if (prev === undefined) {
                  seen.set(doc.id, doc.updated_at);
                  emit('create', doc);
                } else if (prev !== doc.updated_at) {
                  seen.set(doc.id, doc.updated_at);
                  emit('update', doc);
                }
              }
              if (docs.length < pageSize) break;
              after = docs[docs.length - 1].id;
            }
            // Anything we knew about that is no longer present was deleted while
            // we were offline.
            for (const id of [...seen.keys()]) {
              if (!present.has(id)) {
                seen.delete(id);
                emit('delete', null, id);
              }
            }
          } catch (err) {
            if (handlers.onError) handlers.onError(err);
            else console.error('spot:', err.message || err);
          } finally {
            replaying = false;
            while (pending.length) {
              const ev = pending.shift();
              dispatch(ev.type, ev.doc, ev.id);
            }
            if (replayAgain && !closed) runReplay();
          }
        };

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
            if (msg.type === 'subscribed') {
              // The server sends this only after the subscription is
              // registered, so the snapshot cannot miss later changes; socket
              // events that arrive during it are buffered by handleEvent. Run
              // on every (re)subscribe so a reconnect catches up on what
              // changed while we were offline.
              if (replay) runReplay();
              return;
            }
            if (msg.type === 'create') handleEvent('create', msg.doc);
            if (msg.type === 'update') handleEvent('update', msg.doc);
            if (msg.type === 'delete') handleEvent('delete', null, msg.id);
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
    };
    return c;
  };

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
    // Typed error thrown by every call: err.status, err.code
    // ('rate_limited' | 'forbidden' | 'unauthorized' | 'not_found' |
    // 'conflict' | 'bad_request' | 'server' | 'network' | 'stream'), and
    // err.retryAfter (seconds) on a 429.
    SpotError,
    // configure({ retry }) sets the default retry behavior. retry is true
    // (smart auto-retry, the default), false (never), or a max-retry count.
    configure,
    // Who is visiting, according to the mesh, plus per-site capabilities:
    // { email, name, peer_name, peer_ip, groups, ai_allowed }. ai_allowed
    // reports whether this visitor may call spot.ai on this site, so a page
    // can show or hide AI features without provoking a 403.
    me: ({ retry } = {}) => api('/api/me', { retry }),
    db: { collection },
    realtime: {
      // Ephemeral room for multiplayer/control-panel traffic.
      // room.send(type, data), room.on(type, handler), room.onPresence(handler)
      room: realtimeRoom,
    },
    ai: {
      // chat([{role, content}, ...], {model, system, max_tokens})
      // -> { text, model, stop_reason, usage }
      chat: (messages, opts = {}) => {
        const { retry, ...rest } = opts;
        return api('/api/ai/chat', {
          method: 'POST',
          body: JSON.stringify({ messages, ...rest }),
          retry,
        });
      },
      // stream([{role, content}, ...], {onToken, model, system, max_tokens, signal})
      // Calls onToken(delta, text) as tokens arrive and resolves with the
      // final { text, model, stop_reason, usage }.
      stream: async (messages, opts = {}) => {
        const { onToken, signal, retry, ...rest } = opts;
        let res;
        try {
          res = await fetch('/api/ai/chat/stream', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ messages, ...rest }),
            signal,
          });
        } catch (err) {
          if (err && err.name === 'AbortError') throw err;
          throw new SpotError((err && err.message) || 'spot: network error', { code: 'network' });
        }
        if (!res.ok || !res.body) {
          const body = await res.json().catch(() => null);
          throw new SpotError((body && body.error) || `spot: HTTP ${res.status}`, {
            status: res.status,
            code: codeForStatus(res.status),
            retryAfter: retryAfterSeconds(res),
          });
        }
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        const result = { text: '', model: undefined, stop_reason: undefined, usage: undefined };
        let buffer = '';
        let doneFrame = false;
        // SSE frames are separated by a blank line; tolerate CRLF in case an
        // intermediary rewrites the stream's line endings.
        const frameSep = /\r?\n\r?\n/;
        try {
          for (;;) {
            const { value, done } = await reader.read();
            if (done) break;
            buffer += decoder.decode(value, { stream: true });
            let match;
            while ((match = frameSep.exec(buffer))) {
              const frame = buffer.slice(0, match.index);
              buffer = buffer.slice(match.index + match[0].length);
              if (!frame.startsWith('data:')) continue;
              const payload = frame.slice(5).trim();
              if (!payload) continue;
              let msg;
              try {
                msg = JSON.parse(payload);
              } catch {
                continue;
              }
              if (msg.error) throw new SpotError(msg.error, { code: 'stream' });
              if (msg.delta) {
                result.text += msg.delta;
                onToken?.(msg.delta, result.text);
              }
              if (msg.done) {
                doneFrame = true;
                result.model = msg.model;
                result.stop_reason = msg.stop_reason;
                result.usage = msg.usage;
              }
            }
          }
        } catch (err) {
          // Re-throw our own typed stream errors and aborts as-is; wrap anything
          // else (a mid-stream connection drop rejects reader.read() with a raw
          // TypeError) so callers always see a SpotError.
          if (err instanceof SpotError) throw err;
          if (err && err.name === 'AbortError') throw err;
          throw new SpotError((err && err.message) || 'spot: network error', { code: 'network' });
        }
        if (!doneFrame) throw new SpotError('AI stream ended before completion', { code: 'stream' });
        return result;
      },
      // image('prompt', {model, size, aspect_ratio, image_size, quality, output_format})
      // -> { provider, model, images: [{ b64, mime_type, data_url }] }
      image: (prompt, opts = {}) => {
        const { retry, ...rest } = opts;
        return api('/api/ai/image', {
          method: 'POST',
          body: JSON.stringify({ prompt, ...rest }),
          retry,
        });
      },
    },
    files: {
      // upload(File|Blob) -> { id, name, size, content_type, url }
      upload: (file, { name, retry } = {}) => {
        const form = new FormData();
        form.append('file', file, name || file.name || 'file');
        return request('/api/files', { method: 'POST', body: form, retry });
      },
      // list() -> [{ id, name, size, url }, ...]
      list: async ({ retry } = {}) => (await api('/api/files', { retry })).files,
      // delete(file) where file is a stored descriptor or (id, name).
      delete: (file, nameOrOpts, opts = {}) => {
        const id = typeof file === 'object' ? file.id : file;
        const fileName = typeof file === 'object' ? file.name : nameOrOpts;
        const { retry } =
          typeof file === 'object' && nameOrOpts && typeof nameOrOpts === 'object' ? nameOrOpts : opts;
        return api(`/api/files/${encodeURIComponent(id)}/${encodeURIComponent(fileName)}`, {
          method: 'DELETE',
          retry,
        });
      },
    },
    sites: {
      // Apex-only platform APIs: these work from the Spot root, not site subdomains.
      mine: async ({ retry } = {}) => (await api('/api/sites/mine', { retry })).sites,
      public: async ({ retry } = {}) => (await api('/api/sites/public', { retry })).sites,
      delete: (name, { retry } = {}) =>
        api(`/api/sites/${encodeURIComponent(name)}`, { method: 'DELETE', retry }),
    },
  };
})();
