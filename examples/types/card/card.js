// Card viewer. The whole viewer loads at startup; data is fetched from the
// instance root and refreshed on deploy events or by bounded polling.
(() => {
  const VERSION = 'card@1.0.0';
  const root = document.getElementById('card');
  let pollDelay = 3000;
  let pollTimer;

  const el = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  };

  const render = (card) => {
    const nodes = [el('h1', '', typeof card.title === 'string' ? card.title : 'Untitled')];
    if (typeof card.subtitle === 'string') nodes.push(el('p', 'subtitle', card.subtitle));
    // Images live under the media/ data path; only plain relative names are used.
    if (typeof card.image === 'string' && /^[\w.-]+$/.test(card.image)) {
      const img = el('img');
      img.src = '/media/' + card.image;
      img.alt = typeof card.imageAlt === 'string' ? card.imageAlt : '';
      nodes.push(img);
    }
    if (typeof card.body === 'string') nodes.push(el('p', '', card.body));
    if (Array.isArray(card.points) && card.points.length) {
      const list = el('ul');
      for (const point of card.points) list.append(el('li', '', String(point)));
      nodes.push(list);
    }
    if (typeof card.title === 'string') document.title = card.title;
    root.replaceChildren(...nodes);
  };

  const schedulePoll = () => {
    clearTimeout(pollTimer);
    pollTimer = setTimeout(load, pollDelay);
    pollDelay = Math.min(pollDelay * 2, 60000);
  };

  async function load() {
    try {
      const res = await fetch('/card.json', { cache: 'no-store' });
      if (res.status === 404) {
        root.replaceChildren(el('p', 'placeholder', 'Waiting for card.json…'));
        schedulePoll();
        return;
      }
      if (!res.ok) throw new Error('HTTP ' + res.status);
      render(await res.json());
      pollDelay = 3000;
    } catch (err) {
      root.replaceChildren(el('p', 'problem', 'Could not load card.json: ' + err.message));
      schedulePoll();
    }
  }

  // Deploy events are best effort and not every visitor may open the socket;
  // polling above covers the rest.
  const watch = () => {
    let socket;
    try {
      socket = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/ws');
    } catch {
      return;
    }
    socket.addEventListener('open', () => socket.send(JSON.stringify({ type: 'subscribe', collection: '_spot_deploys' })));
    socket.addEventListener('message', (event) => {
      let msg;
      try { msg = JSON.parse(event.data); } catch { return; }
      if (msg.type !== 'deploy') return;
      if (msg.site_type && msg.site_type !== VERSION) {
        location.reload();
        return;
      }
      load();
    });
    socket.addEventListener('close', () => setTimeout(watch, 10000));
  };

  load();
  watch();
})();
