#!/usr/bin/env node
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const sdkPath = join(root, 'sdk', 'spot.js');
const cliPath = join(root, 'cli', 'spot');
const serverDir = join(root, 'server');

const timeoutMs = Number(process.env.SPOT_SDK_SMOKE_TIMEOUT_MS || 120000);
const startedAt = Date.now();
let tmp;
let apiProc;
let fakeAI;
let fakeSlack;

const fail = (message) => {
  throw new Error(message);
};

const assert = (condition, message) => {
  if (!condition) fail(message);
};

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

const withTimeout = async (promise, label, ms = timeoutMs) => {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error(`${label} timed out after ${ms}ms`)), ms);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
};

const waitFor = async (label, fn, { interval = 100, timeout = timeoutMs } = {}) => {
  const deadline = Date.now() + timeout;
  let last;
  while (Date.now() < deadline) {
    try {
      const value = await fn();
      if (value) return value;
    } catch (err) {
      last = err;
    }
    await wait(interval);
  }
  if (last) throw new Error(`${label} did not become ready: ${last.message}`);
  throw new Error(`${label} did not become ready`);
};

const readJSON = (req) =>
  new Promise((resolve, reject) => {
    let body = '';
    req.setEncoding('utf8');
    req.on('data', (chunk) => {
      body += chunk;
    });
    req.on('end', () => {
      try {
        resolve(body ? JSON.parse(body) : {});
      } catch (err) {
        reject(err);
      }
    });
    req.on('error', reject);
  });

const startFakeAI = async () => {
  const server = createServer(async (req, res) => {
    try {
      if (req.method !== 'POST') {
        res.writeHead(404).end();
        return;
      }
      const body = await readJSON(req);
      if (req.url === '/v1/chat/completions' && body.stream) {
        res.writeHead(200, {
          'Content-Type': 'text/event-stream',
          'Cache-Control': 'no-cache',
        });
        res.write(
          'data: {"model":"fake-chat","choices":[{"delta":{"content":"ok"},"finish_reason":""}]}\n\n',
        );
        res.write(
          'data: {"model":"fake-chat","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}\n\n',
        );
        res.end('data: [DONE]\n\n');
        return;
      }
      if (req.url === '/v1/chat/completions') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            model: 'fake-chat',
            choices: [{ message: { content: 'ok' }, finish_reason: 'stop' }],
            usage: { prompt_tokens: 2, completion_tokens: 1 },
          }),
        );
        return;
      }
      if (req.url === '/v1/images/generations') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(
          JSON.stringify({
            data: [
              {
                b64_json: 'iVBORw0KGgo=',
                revised_prompt: body.prompt || 'sdk smoke image',
              },
            ],
            usage: {},
          }),
        );
        return;
      }
      res.writeHead(404).end();
    } catch (err) {
      res.writeHead(500, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: String(err.message || err) }));
    }
  });

  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}`,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections?.();
        server.close(resolve);
      }),
  };
};

const startFakeSlack = async () => {
  let lastRequest;
  const server = createServer(async (req, res) => {
    try {
      if (req.method !== 'POST' || req.url !== '/chat.postMessage') {
        res.writeHead(404).end();
        return;
      }
      const body = await readJSON(req);
      lastRequest = {
        authorization: req.headers.authorization,
        contentType: req.headers['content-type'],
        body,
      };
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(
        JSON.stringify({
          ok: true,
          channel: 'CSDKSMOKE',
          ts: '1503435956.000247',
        }),
      );
    } catch (err) {
      res.writeHead(500, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: false, error: String(err.message || err) }));
    }
  });

  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}`,
    lastRequest: () => lastRequest,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections?.();
        server.close(resolve);
      }),
  };
};

const getFreePort = () =>
  new Promise((resolve, reject) => {
    const server = createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      server.close(() => resolve(port));
    });
  });

const spawnLogged = (cmd, args, opts = {}) => {
  const proc = spawn(cmd, args, {
    cwd: opts.cwd || root,
    env: opts.env || process.env,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  proc.stdoutText = '';
  proc.stderrText = '';
  proc.stdout.on('data', (chunk) => {
    proc.stdoutText += chunk;
  });
  proc.stderr.on('data', (chunk) => {
    proc.stderrText += chunk;
  });
  return proc;
};

const run = (cmd, args, opts = {}) =>
  new Promise((resolve, reject) => {
    const proc = spawnLogged(cmd, args, opts);
    proc.on('error', reject);
    proc.on('close', (code) => {
      if (code === 0) {
        resolve({ stdout: proc.stdoutText, stderr: proc.stderrText });
        return;
      }
      reject(
        new Error(
          `${cmd} ${args.join(' ')} exited ${code}\nstdout:\n${proc.stdoutText}\nstderr:\n${proc.stderrText}`,
        ),
      );
    });
  });

const startSpot = async (port, aiURL, slackURL) => {
  const dataDir = join(tmp, 'data');
  await mkdir(dataDir, { recursive: true });
  const binary = join(tmp, 'spot-api');
  await run('go', ['build', '-o', binary, '.'], { cwd: serverDir });
  apiProc = spawnLogged(binary, [], {
    env: {
      ...process.env,
      PORT: String(port),
      SPOT_DOMAIN: 'spot.localhost',
      SPOT_STORAGE_MODE: 'local',
      SPOT_DATA_DIR: dataDir,
      SPOT_SQLITE_PATH: join(dataDir, 'spot.db'),
      SPOT_SITES_DIR: join(dataDir, 'sites'),
      SPOT_AUTH_MODE: '',
      NETBIRD_API_URL: '',
      NETBIRD_API_TOKEN: '',
      TAILSCALE_API_URL: '',
      TAILSCALE_API_TOKEN: '',
      TAILSCALE_OAUTH_CLIENT_ID: '',
      TAILSCALE_OAUTH_CLIENT_SECRET: '',
      OPENAI_API_KEY: 'sdk-smoke-key',
      OPENAI_BASE_URL: aiURL,
      SPOT_AI_MODEL: 'fake-chat',
      SPOT_AI_ALLOWED_MODELS: 'fake-chat',
      SPOT_AI_IMAGE_MODEL: 'fake-image',
      SPOT_AI_ALLOWED_IMAGE_MODELS: 'fake-image',
      SLACK_BOT_TOKEN: 'sdk-smoke-slack-token',
      SLACK_BASE_URL: slackURL,
      SPOT_SLACK_ACCESS: 'visitors',
      SPOT_DEV_IDENTITY_EMAIL: 'sdk-smoke@localhost',
      SPOT_DEV_IDENTITY_NAME: 'SDK Smoke',
      SPOT_DEV_IDENTITY_GROUPS: 'sdk-testers',
    },
  });
  apiProc.on('exit', (code) => {
    if (code !== null && code !== 0) {
      console.error(apiProc.stderrText || apiProc.stdoutText);
    }
  });
  await waitFor('spot-api', async () => {
    if (apiProc.exitCode !== null) {
      fail(`spot-api exited early with ${apiProc.exitCode}`);
    }
    const res = await fetch(`http://spot.localhost:${port}/health`);
    if (!res.ok) return false;
    const body = await res.json().catch(() => null);
    return body && body.status === 'ok';
  });
};

const deploySite = async (port, name, files) => {
  const siteDir = join(tmp, name);
  await mkdir(siteDir, { recursive: true });
  for (const [path, content] of Object.entries(files)) {
    const target = join(siteDir, path);
    await mkdir(dirname(target), { recursive: true });
    await writeFile(target, content);
  }
  await run(cliPath, ['deploy', name, siteDir], {
    env: {
      ...process.env,
      SPOT_URL: `http://spot.localhost:${port}`,
      XDG_CONFIG_HOME: join(tmp, 'xdg'),
    },
  });
};

const makeSpot = async (origin, fetchImpl = fetch) => {
  const source = await readFile(sdkPath, 'utf8');
  const window = {};
  const relativeFetch = (input, init = {}) => {
    let url = input instanceof URL ? input.toString() : String(input);
    if (url.startsWith('/')) url = origin + url;
    return fetchImpl(url, {
      ...init,
      headers: {
        Origin: origin,
        ...(init.headers || {}),
      },
    });
  };
  const context = {
    window,
    console,
    fetch: relativeFetch,
    WebSocket,
    location: new URL(`${origin}/`),
    URLSearchParams,
    FormData,
    File,
    Blob,
    TextDecoder,
    AbortController,
    setTimeout,
    clearTimeout,
    Math,
  };
  vm.createContext(context);
  vm.runInContext(source, context, { filename: sdkPath });
  return window.spot;
};

const expectSpotError = async (label, fn, expected) => {
  try {
    await fn();
  } catch (err) {
    assert(err.name === 'SpotError', `${label}: got ${err.name}, want SpotError`);
    for (const [key, value] of Object.entries(expected)) {
      assert(err[key] === value, `${label}: err.${key}=${err[key]}, want ${value}`);
    }
    return err;
  }
  fail(`${label}: expected SpotError`);
};

const expectEventually = (label, fn) => waitFor(label, fn, { interval: 50, timeout: 10000 });

const testRetryPolicy = async () => {
  const okResponse = (body) =>
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });

  let calls = 0;
  const getSpot = await makeSpot('http://unit.test', async () => {
    calls += 1;
    if (calls === 1) throw new TypeError('temporary network failure');
    return okResponse({ id: 'abc', data: {}, created_at: 'now', updated_at: 'now' });
  });
  await getSpot.db.collection('items').get('abc', { retry: 1 });
  assert(calls === 2, `GET retry calls=${calls}, want 2`);

  calls = 0;
  const putSpot = await makeSpot('http://unit.test', async () => {
    calls += 1;
    throw new TypeError('ambiguous update failure');
  });
  await expectSpotError(
    'PUT network failures are not replayed',
    () => putSpot.db.collection('items').update('abc', { stale: true }, { retry: 3 }),
    { code: 'network' },
  );
  assert(calls === 1, `PUT retry calls=${calls}, want 1`);
};

const testSiteSDK = async (port) => {
  const origin = `http://sdksmoke.spot.localhost:${port}`;
  const spot = await makeSpot(origin);
  const config = spot.configure({ retry: false, maxRetries: 1, retryBaseMs: 1, retryCapMs: 5 });
  assert(config.retry === false, 'configure() did not return updated retry setting');

  const me = await spot.me();
  assert(me.email === 'sdk-smoke@localhost', `me.email=${me.email}`);
  assert(me.ai_allowed === true, 'me.ai_allowed should be true for the site owner');
  assert(me.slack_allowed === true, 'me.slack_allowed should be true for visitors-mode Slack');

  const suffix = String(Date.now());
  const notes = spot.db.collection(`sdk-smoke-notes-${suffix}`);
  const created = await notes.create({ title: 'alpha', status: 'open', priority: 2, views: 1 });
  assert(created.id && created.data.title === 'alpha', 'create() did not return the document');
  const got = await notes.get(created.id);
  assert(got.id === created.id, 'get() returned the wrong document');
  const updated = await notes.update(created.id, {
    title: 'alpha-updated',
    status: 'open',
    priority: 3,
    views: 1,
  });
  assert(updated.data.title === 'alpha-updated', 'update() did not replace data');
  const mineUpdated = await notes.updateMine(created.id, {
    title: 'alpha-mine',
    status: 'open',
    priority: 4,
    views: 1,
  });
  assert(mineUpdated.data.title === 'alpha-mine', 'updateMine() did not update owner doc');
  const incremented = await notes.increment(created.id, 'views', 2);
  assert(incremented.data.views === 3, 'increment() did not add 2');
  const incrementedMine = await notes.incrementMine(created.id, 'views', 4);
  assert(incrementedMine.data.views === 7, 'incrementMine() did not add 4');
  const other = await notes.create({ title: 'beta', status: 'done', priority: 1, views: 0 });
  const count = await notes.count({ where: { status: 'open' } });
  assert(count === 1, `count(where)=${count}, want 1`);
  const listed = await notes.list({
    where: { priority: { gte: 1 } },
    sort: 'priority',
    order: 'asc',
  });
  assert(listed.length >= 2 && listed[0].data.priority === 1, 'list(where/sort/order) failed');
  const many = await notes.getMany([created.id, null, '', other.id]);
  assert(many.some((doc) => doc.id === created.id), 'getMany() missing created doc');
  assert(many.some((doc) => doc.id === other.id), 'getMany() missing second doc');
  const iterated = [];
  for await (const doc of notes.iterate({ pageSize: 1 })) {
    iterated.push(doc.id);
    if (iterated.length >= 2) break;
  }
  assert(iterated.length >= 2, 'iterate() did not yield multiple pages');

  await expectSpotError('get missing document', () => notes.get('00000000-0000-0000-0000-000000000000'), {
    status: 404,
    code: 'not_found',
  });

  const mineDelete = await notes.create({ title: 'delete-mine' });
  await notes.deleteMine(mineDelete.id);
  await expectSpotError('deleteMine removed document', () => notes.get(mineDelete.id), {
    status: 404,
    code: 'not_found',
  });
  await notes.delete(other.id);
  await expectSpotError('delete removed document', () => notes.get(other.id), {
    status: 404,
    code: 'not_found',
  });

  const live = spot.db.collection(`sdk-smoke-live-${suffix}`);
  const beforeSub = await live.create({ step: 'before-subscribe' });
  const events = [];
  const unsubscribe = live.subscribe(
    {
      onCreate: (doc) => events.push(['create', doc.id, doc.data.step]),
      onUpdate: (doc) => events.push(['update', doc.id, doc.data.step]),
      onDelete: (id) => events.push(['delete', id]),
      onError: (err) => events.push(['error', err.message]),
    },
    { replay: true, retry: false },
  );
  await expectEventually('subscribe replay create', () =>
    events.some((event) => event[0] === 'create' && event[1] === beforeSub.id),
  );
  const duringSub = await live.create({ step: 'created-live' });
  await live.update(duringSub.id, { step: 'updated-live' });
  await live.delete(duringSub.id);
  await expectEventually('subscribe live create/update/delete', () => {
    const createdLive = events.some((event) => event[0] === 'create' && event[1] === duringSub.id);
    const updatedLive = events.some((event) => event[0] === 'update' && event[1] === duringSub.id);
    const deletedLive = events.some((event) => event[0] === 'delete' && event[1] === duringSub.id);
    return createdLive && updatedLive && deletedLive;
  });
  unsubscribe();

  const roomA = spot.realtime.room(`sdk-smoke-room-${suffix}`);
  const roomB = spot.realtime.room(`sdk-smoke-room-${suffix}`);
  const statuses = [];
  const roomMessages = [];
  let presenceUsers = [];
  roomA.onStatus((status) => statuses.push(['a', status]));
  roomB.onStatus((status) => statuses.push(['b', status]));
  roomB.on('ping', (msg) => roomMessages.push(msg));
  roomB.onPresence((users) => {
    presenceUsers = users;
  });
  await expectEventually('rooms open', () =>
    statuses.some((event) => event[0] === 'a' && event[1] === 'open') &&
    statuses.some((event) => event[0] === 'b' && event[1] === 'open'),
  );
  roomA.setPresence({ role: 'sender' });
  roomA.send('ping', { n: 1 });
  await expectEventually('room message delivered', () =>
    roomMessages.some((msg) => msg.data && msg.data.n === 1 && msg.from.email === 'sdk-smoke@localhost'),
  );
  await expectEventually('room presence delivered', () =>
    presenceUsers.some((user) => user.email === 'sdk-smoke@localhost'),
  );
  roomA.close();
  roomB.close();
  assert(statuses.some((event) => event[1] === 'closed'), 'room close did not emit closed status');

  const chat = await spot.ai.chat(
    [{ role: 'user', content: 'Reply with ok' }],
    { model: 'fake-chat', system: 'test', max_tokens: 4 },
  );
  assert(chat.text === 'ok' && chat.model === 'fake-chat', 'ai.chat() failed');
  const tokens = [];
  const stream = await spot.ai.stream(
    [{ role: 'user', content: 'Stream ok' }],
    { model: 'fake-chat', max_tokens: 4, onToken: (delta) => tokens.push(delta) },
  );
  assert(stream.text === 'ok' && tokens.join('') === 'ok', 'ai.stream() failed');
  const image = await spot.ai.image('A tiny blue square', {
    model: 'fake-image',
    output_format: 'png',
  });
  assert(image.images.length === 1 && image.images[0].data_url.startsWith('data:image/png;base64,'), 'ai.image() failed');

  const slack = await spot.slack.send({ channel: '#sdk-smoke', text: 'hello from smoke' });
  assert(slack.ok === true && slack.channel === 'CSDKSMOKE' && slack.ts, 'slack.send() failed');
  const slackRequest = fakeSlack.lastRequest();
  assert(slackRequest, 'fake Slack did not receive a request');
  assert(slackRequest.authorization === 'Bearer sdk-smoke-slack-token', 'Slack Authorization header missing');
  assert(slackRequest.body.channel === '#sdk-smoke', 'Slack gateway received wrong channel');
  assert(slackRequest.body.text === 'hello from smoke', 'Slack gateway received wrong text');

  const firstFile = await spot.files.upload(new File(['hello sdk'], 'hello.txt', { type: 'text/plain' }));
  assert(firstFile.url && firstFile.name === 'hello.txt', 'files.upload() failed');
  const files = await spot.files.list();
  assert(files.some((file) => file.id === firstFile.id), 'files.list() missing upload');
  const downloaded = await (await fetch(origin + firstFile.url)).text();
  assert(downloaded === 'hello sdk', 'uploaded file download mismatch');
  await spot.files.delete(firstFile);
  const secondFile = await spot.files.upload(new Blob(['bye sdk'], { type: 'text/plain' }), {
    name: 'bye.txt',
  });
  await spot.files.delete(secondFile.id, secondFile.name);
};

const testApexSDK = async (port) => {
  const spot = await makeSpot(`http://spot.localhost:${port}`);
  const mine = await spot.sites.mine();
  assert(mine.some((site) => site.name === 'sdksmoke'), 'sites.mine() missing sdksmoke');
  assert(mine.some((site) => site.name === 'sdksmokedelete'), 'sites.mine() missing sdksmokedelete');
  const publicSites = await spot.sites.public();
  assert(publicSites.some((site) => site.name === 'sdksmoke'), 'sites.public() missing sdksmoke');
  const deleted = await spot.sites.delete('sdksmokedelete');
  assert(deleted.site === 'sdksmokedelete', 'sites.delete() returned wrong site');
  await expectSpotError('delete missing site', () => spot.sites.delete('sdksmokedelete'), {
    status: 404,
    code: 'not_found',
  });
};

const main = async () => {
  tmp = await mkdtemp(join(tmpdir(), 'spot-sdk-smoke-'));
  fakeAI = await startFakeAI();
  fakeSlack = await startFakeSlack();
  const port = process.env.SPOT_SDK_SMOKE_PORT
    ? Number(process.env.SPOT_SDK_SMOKE_PORT)
    : await getFreePort();
  console.log(`==> starting fake AI at ${fakeAI.url}`);
  console.log(`==> starting fake Slack at ${fakeSlack.url}`);
  console.log(`==> starting spot-api on http://spot.localhost:${port}`);
  await startSpot(port, fakeAI.url, fakeSlack.url);

  console.log('==> deploying SDK smoke sites');
  await deploySite(port, 'sdksmoke', {
    'index.html': '<!doctype html><title>SDK Smoke</title><h1>SDK Smoke</h1><script src="/spot.js"></script>',
  });
  await deploySite(port, 'sdksmokedelete', {
    'index.html': '<!doctype html><title>Delete Me</title>',
  });

  console.log('==> SDK retry policy');
  await testRetryPolicy();
  console.log('==> SDK site methods');
  await testSiteSDK(port);
  console.log('==> SDK apex site methods');
  await testApexSDK(port);
  console.log(`SDK SMOKE PASS in ${Math.round((Date.now() - startedAt) / 1000)}s`);
};

const cleanup = async () => {
  if (apiProc && apiProc.exitCode === null) {
    apiProc.kill('SIGTERM');
    await Promise.race([
      new Promise((resolve) => apiProc.once('exit', resolve)),
      wait(3000).then(() => apiProc.kill('SIGKILL')),
    ]);
  }
  if (fakeAI) await fakeAI.close();
  if (fakeSlack) await fakeSlack.close();
  if (tmp) await rm(tmp, { recursive: true, force: true });
};

try {
  await withTimeout(main(), 'SDK smoke');
} catch (err) {
  console.error('');
  console.error(`SDK SMOKE FAIL: ${err.message}`);
  if (apiProc) {
    console.error('\n--- spot-api stdout ---');
    console.error(apiProc.stdoutText.trim());
    console.error('\n--- spot-api stderr ---');
    console.error(apiProc.stderrText.trim());
  }
  process.exitCode = 1;
} finally {
  await cleanup();
}
