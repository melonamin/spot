// Spot live reload helper. Add this to a deployed site during development:
//
//   <script src="/spot-live.js" defer></script>
//
// When the site is redeployed, Spot publishes a site-scoped realtime event and
// this page reloads. Set data-cache-bust="true" to add ?spot_live=<version>
// instead of using location.reload().
(() => {
  const script = document.currentScript;
  const quiet = script?.dataset.quiet === 'true';
  const cacheBust = script?.dataset.cacheBust === 'true';
  const reloadDelay = Number(script?.dataset.delay || 100);
  const collection = '_spot_deploys';
  let socket;
  let reloadTimer;
  let reconnectTimer;
  let attempts = 0;

  const log = (...args) => {
    if (!quiet) console.info('[spot-live]', ...args);
  };

  const wsURL = () => {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    return `${proto}//${location.host}/api/ws`;
  };

  const reload = (version) => {
    if (reloadTimer) return;
    reloadTimer = setTimeout(() => {
      log('redeploy detected; reloading');
      if (cacheBust && version) {
        const next = new URL(location.href);
        next.searchParams.set('spot_live', version);
        location.replace(next.toString());
        return;
      }
      location.reload();
    }, Number.isFinite(reloadDelay) && reloadDelay >= 0 ? reloadDelay : 100);
  };

  const scheduleReconnect = () => {
    clearTimeout(reconnectTimer);
    const delay = Math.min(1000 * 2 ** attempts, 10000);
    attempts += 1;
    reconnectTimer = setTimeout(connect, delay);
  };

  const connect = () => {
    try {
      socket = new WebSocket(wsURL());
    } catch (err) {
      log('websocket failed:', err.message || err);
      scheduleReconnect();
      return;
    }

    socket.addEventListener('open', () => {
      attempts = 0;
      log('watching for deploys');
      socket.send(JSON.stringify({ type: 'subscribe', collection }));
    });

    socket.addEventListener('message', (event) => {
      let msg;
      try {
        msg = JSON.parse(event.data);
      } catch {
        return;
      }
      if (msg.type === 'deploy' && msg.collection === collection) {
        reload(msg.version || msg.id);
      } else if (msg.type === 'error') {
        log('server error:', msg.error || msg.message || msg);
      }
    });

    socket.addEventListener('close', () => {
      if (!reloadTimer) scheduleReconnect();
    });

    socket.addEventListener('error', () => {
      // The close event will schedule reconnect; logging every transient error
      // gets noisy during server restarts.
    });
  };

  if (location.hostname.split('.').length < 2) {
    log('not on a Spot site host; live reload disabled');
    return;
  }
  connect();
})();
