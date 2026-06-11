// Quick browser SDK. Every site loads this from its own origin:
//
//   <script src="/quick.js"></script>
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
      throw new Error((body && body.error) || `quick: HTTP ${res.status}`);
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
  });

  window.quick = {
    // Who is visiting, according to the NetBird mesh.
    me: () => api('/api/me'),
    db: { collection },
  };
})();
