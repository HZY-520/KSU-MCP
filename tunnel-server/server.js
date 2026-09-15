#!/usr/bin/env node
/**
 * KSU MCP Tunnel Server v1.2.0
 * 内网穿透服务端：设备（模块端 mcpd tunnel）主动连接本服务，远端 MCP 客户端
 * 通过 https://<域名>/mcp/<device> 访问设备上的 MCP 服务。
 *
 * v1.2.0 变更：
 *  - 心跳 30s → 10s，连续 3 次未收到 pong（≈35s）即判定连接死亡并回收，
 *    与设备端 10s 心跳 / 35s 读超时严格对齐（旧版最坏 60s 才回收死连接）；
 *  - 新增设备级连接质量指标：重连次数、心跳 RTT、收发字节、转发请求/失败数、
 *    在线累计时长、最近活跃时间，并在 /api/status 与 /api/metrics 暴露；
 *  - 设备断线时记录 lastError 以便 WebUI 展示。
 *
 * v1.1.0 已有能力（保持兼容）：
 *  - 流式转发协议（hello 握手 + response(fin) + chunk 分片）；
 *  - 设备断线快速失败（500ms 内使在途请求返回 502）；
 *  - 响应竞态修复（超时 / 设备响应 / 断线三方互斥收口）；
 *  - WebUI 添加设备支持自定义 tunnelToken / clientToken；踢下线 API。
 *
 * 运行：npm install && node server.js（或 pm2 start server.js --name ksu-mcp-tunnel）
 * 配置：config.json（见 config.example.json）；也可用环境变量 TUNNEL_CONFIG 指定路径
 * WebUI：https://<域名>/admin（默认账号 admin / admin123，首次登录后请立即修改）
 */
'use strict';

const http = require('http');
const https = require('https');
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { WebSocketServer } = require('ws');

const VERSION = '1.2.0';
const APP_NAME = 'KSU MCP Tunnel Server';
const MAX_PROXY_BODY = 16 * 1024 * 1024; // 代理请求体上限（与设备端一致）
const TOKEN_RE = /^[A-Za-z0-9_.\-]{16,128}$/; // 自定义 Token 规则（留空则自动生成）

// 心跳参数：与设备端 mcpd 的 10s 心跳 / 35s 判死保持一致
const PING_INTERVAL = 10000;  // 每 10s 发一次 WS ping（带时间戳负载用于测 RTT）
const MAX_MISSED_PONGS = 3;   // 连续 3 次未回 pong（≈35s）判定死亡


// ---------------- 配置 ----------------
const CONFIG_PATH = process.env.TUNNEL_CONFIG || path.join(__dirname, 'config.json');

function sha256(s) { return crypto.createHash('sha256').update(String(s)).digest('hex'); }
function randHex(n) { return crypto.randomBytes(n).toString('hex'); }

function loadConfig() {
  const def = {
    port: 8081,
    tls: { cert: '', key: '' },
    tunnelPath: '/tunnel',   // 设备 WebSocket 接入路径
    mcpPath: '/mcp',         // 远端 MCP 客户端访问前缀
    requestTimeout: 90000,   // 转发请求空闲超时（毫秒，每次数据分片重置）
    devices: {},             // deviceId -> { tunnelToken, clientToken }
    admin: { username: 'admin', password: sha256('admin123') }, // WebUI 登录（首次登录后请修改）
  };
  try {
    const c = JSON.parse(fs.readFileSync(CONFIG_PATH, 'utf8'));
    const merged = Object.assign(def, c);
    if (!merged.admin) merged.admin = def.admin;
    if (!merged.admin.password) merged.admin.password = sha256('admin123');
    if (!merged.devices) merged.devices = {};
    return merged;
  } catch (e) {
    console.warn('[warn] 无法读取 ' + CONFIG_PATH + '，使用默认配置:', e.message);
    return def;
  }
}

const CONFIG = loadConfig();

function saveConfig() {
  const out = {
    port: CONFIG.port,
    tls: CONFIG.tls || { cert: '', key: '' },
    tunnelPath: CONFIG.tunnelPath,
    mcpPath: CONFIG.mcpPath,
    requestTimeout: CONFIG.requestTimeout,
    devices: CONFIG.devices,
    admin: CONFIG.admin,
  };
  const tmp = CONFIG_PATH + '.tmp';
  fs.writeFileSync(tmp, JSON.stringify(out, null, 2), { mode: 0o600 });
  fs.renameSync(tmp, CONFIG_PATH);
}

// ---------------- 状态 ----------------
const deviceSockets = new Map(); // deviceId -> ws
const pending = new Map();       // reqId -> { id, device, res, timer, started, settled }
let reqSeq = 0;
const stats = { requests: 0, failures: 0, startedAt: Date.now() };
const logRing = [];
const MAX_LOG = 600;
function log(...a) {
  const line = new Date().toISOString() + ' ' + a.join(' ');
  console.log(...a);
  logRing.push(line);
  if (logRing.length > MAX_LOG) logRing.shift();
}

// ---------------- 设备级质量指标（v1.2.0） ----------------
// 这些指标跨「重连」累计，用于在管理台展示隧道连接质量（延迟 / 重连 / 流量 / 失败）。
const deviceStats = new Map(); // deviceId -> metrics

function devMetrics(device) {
  let m = deviceStats.get(device);
  if (!m) {
    m = {
      device,
      online: false,
      connectedAt: null,     // 本次连接建立时间
      lastSeenAt: null,      // 最近收到设备任何帧 / pong 的时间
      reconnects: 0,         // 累计重连次数（第 2 次起计）
      rttMs: null,           // 最近一次心跳 RTT
      rttMsAvg: null,        // 滑动平均 RTT
      pingsSent: 0,          // 已发送心跳 ping 数
      pingsMissed: 0,        // 未在下一次心跳前收到 pong 的次数（丢包）
      lossPercent: 0,        // 心跳丢包率（0-100）
      bytesIn: 0,            // 设备 -> 服务端（含分片回传）
      bytesOut: 0,           // 服务端 -> 设备（含转发请求）
      requests: 0,           // 转发到该设备的请求数
      failures: 0,           // 转发失败数（超时 / 设备离线）
      onlineMsTotal: 0,      // 累计在线时长
      lastError: '',         // 最近一次断开原因
      lastErrorAt: null,
      serverVersion: null,   // 设备端上报的握手版本（hello 由服务端下发，此字段保留扩展）
      missedPongs: 0,
    };
    deviceStats.set(device, m);
  }
  return m;
}

// metricsView 输出给 WebUI 的指标快照（补齐派生字段）
function metricsView(device) {
  const m = devMetrics(device);
  const onlineMs = m.connectedAt ? Date.now() - m.connectedAt : 0;
  return {
    device,
    online: m.online,
    connectedAt: m.connectedAt,
    lastSeenAt: m.lastSeenAt,
    onlineMs: m.onlineMsTotal + onlineMs,
    sessionMs: m.connectedAt ? onlineMs : 0,
    reconnects: m.reconnects,
    rttMs: m.rttMs,
    rttMsAvg: m.rttMsAvg,
    pingsSent: m.pingsSent,
    pingsMissed: m.pingsMissed,
    lossPercent: m.lossPercent,
    bytesIn: m.bytesIn,
    bytesOut: m.bytesOut,
    requests: m.requests,
    failures: m.failures,
    lastError: m.lastError || null,
    lastErrorAt: m.lastErrorAt,
  };
}

// ---------------- 在途请求管理（统一收口，杜绝重复响应） ----------------
function errJSON(code, message) {
  return JSON.stringify({ jsonrpc: '2.0', error: { code, message } });
}

function armTimer(p) {
  clearTimeout(p.timer);
  p.timer = setTimeout(() => idleTimeout(p), CONFIG.requestTimeout || 90000);
}

function idleTimeout(p) {
  if (p.settled) return;
  p.settled = true;
  pending.delete(p.id);
  stats.failures += 1;
  try {
    if (!p.started) {
      respond(p.res, 504, errJSON(-32000, 'device timeout'), { 'Content-Type': 'application/json' });
    } else if (!p.res.writableEnded) {
      p.res.end();
    }
  } catch (e) { /* ignore */ }
}

function failPending(p, status, msg) {
  if (p.settled) return;
  p.settled = true;
  clearTimeout(p.timer);
  pending.delete(p.id);
  stats.failures += 1;
  try {
    if (!p.started) {
      respond(p.res, status, errJSON(-32000, msg), { 'Content-Type': 'application/json' });
    } else if (!p.res.writableEnded) {
      p.res.end();
    }
  } catch (e) { /* ignore */ }
}

function finishPending(p) {
  if (p.settled) return;
  p.settled = true;
  clearTimeout(p.timer);
  pending.delete(p.id);
  try { if (!p.res.writableEnded) p.res.end(); } catch (e) { /* ignore */ }
}

function decodeBody(s) {
  if (!s) return Buffer.alloc(0);
  try { return Buffer.from(s, 'base64'); } catch (e) { return Buffer.alloc(0); }
}

// ---------------- WebSocket：设备接入 ----------------
const wss = new WebSocketServer({ noServer: true });

wss.on('connection', (ws, req) => {
  const u = new URL(req.url, 'http://localhost');
  const device = u.searchParams.get('device');
  const token = u.searchParams.get('token');
  const dev = CONFIG.devices[device];

  if (!device || !dev || token !== dev.tunnelToken) {
    log('拒绝未授权设备接入:', req.url);
    ws.close(4001, 'unauthorized');
    return;
  }

  // 同设备新连接替换旧连接（支持断线重连）
  const old = deviceSockets.get(device);
  if (old && old !== ws && old.readyState === 1) {
    try { old.close(4000, 'replaced'); } catch (e) { /* ignore */ }
  }
  const m = devMetrics(device);
  // 累计重连次数：本次连接时该设备已有过一次连接即计入
  if (m.connectedAt !== null || m.reconnects > 0 || m.onlineMsTotal > 0) m.reconnects += 1;
  m.online = true;
  m.connectedAt = Date.now();
  m.lastSeenAt = Date.now();
  m.missedPongs = 0;
  m.lastError = '';
  m.lastErrorAt = null;

  ws.meta = { device, connectedAt: Date.now(), remote: req.socket.remoteAddress || '' };
  deviceSockets.set(device, ws);
  ws.isAlive = true;
  ws.missedPongs = 0;
  ws.pingAnswered = true;
  ws.on('pong', (data) => {
    ws.isAlive = true;
    ws.missedPongs = 0;
    ws.pingAnswered = true;
    m.lastSeenAt = Date.now();
    // 心跳 RTT：服务端 ping 时把时间戳放进 payload，pong 原样带回
    const sent = parseInt(String(data), 10);
    if (!isNaN(sent) && sent > 0) {
      const rtt = Date.now() - sent;
      if (rtt >= 0 && rtt < 60000) {
        m.rttMs = rtt;
        m.rttMsAvg = m.rttMsAvg === null ? rtt : Math.round(m.rttMsAvg * 0.7 + rtt * 0.3);
      }
    }
  });

  // 版本握手：告知设备端本服务支持流式转发协议（>=1.1.0）
  try { ws.send(JSON.stringify({ type: 'hello', version: VERSION })); } catch (e) { /* ignore */ }

  ws.on('message', (data) => {
    m.bytesIn += data.length || 0;
    m.lastSeenAt = Date.now();
    let msg;
    try { msg = JSON.parse(data.toString()); } catch (e) { return; }
    if (!msg || msg.id === undefined) return;
    const p = pending.get(msg.id);
    if (!p || p.settled) return;

    if (msg.type === 'response') {
      if (p.started) return; // 重复头帧，忽略
      p.started = true;
      armTimer(p);
      const headers = {};
      const hs = msg.headers || {};
      const get = (k) => {
        for (const key of Object.keys(hs)) if (key.toLowerCase() === k) return hs[key];
        return undefined;
      };
      const ct = get('content-type');
      const sid = get('mcp-session-id');
      if (ct) headers['Content-Type'] = ct;
      if (sid) headers['Mcp-Session-Id'] = sid;
      p.res.writeHead(msg.status || 502, headers);
      const body = decodeBody(msg.body);
      if (body.length && !p.res.writableEnded) p.res.write(body);
      if (msg.fin === true) finishPending(p); // 单帧响应（fin=true）直接结束
      return;
    }

    if (msg.type === 'chunk') {
      if (!p.started) return; // 无响应头的孤儿分片，安全忽略
      armTimer(p);
      const body = decodeBody(msg.body);
      if (body.length && !p.res.writableEnded) p.res.write(body);
      if (msg.done === true) finishPending(p);
    }
  });

  ws.on('close', (code, reason) => {
    if (deviceSockets.get(device) === ws) deviceSockets.delete(device);
    m.online = false;
    if (m.connectedAt) { m.onlineMsTotal += Date.now() - m.connectedAt; m.connectedAt = null; }
    const why = 'code=' + code + (reason && reason.length ? ' (' + reason.toString().slice(0, 80) + ')' : '');
    m.lastError = why;
    m.lastErrorAt = Date.now();
    // 断线快速失败：立即终结该设备所有在途请求
    for (const [, p] of pending) {
      if (p.device === device) {
        m.failures += 1;
        failPending(p, 502, 'device offline');
      }
    }
    log('设备离线:', device, why);
  });
  ws.on('error', (e) => {
    m.lastError = String((e && e.message) || e).slice(0, 160);
    m.lastErrorAt = Date.now();
  });
  log('设备在线:', device, '(' + ws.meta.remote + ')');
});

// 心跳：10s 一次 ping（带时间戳负载），连续 3 次未回 pong（≈35s）判定死亡并回收，
// 期间在途请求由 close 事件快速失败，远端客户端不会悬挂等待。
setInterval(() => {
  for (const [device, ws] of deviceSockets) {
    const m = devMetrics(device);
    if (ws.missedPongs >= MAX_MISSED_PONGS) {
      m.lastError = 'heartbeat timeout (' + MAX_MISSED_PONGS + ' missed pongs ≈' +
        Math.round((PING_INTERVAL * MAX_MISSED_PONGS) / 1000) + 's)';
      m.lastErrorAt = Date.now();
      log('心跳超时，断开设备:', device);
      try { ws.terminate(); } catch (e) { /* ignore */ }
      continue;
    }
    ws.missedPongs = (ws.missedPongs || 0) + 1;
    // 丢包统计：上一拍 ping 未在本拍前收到 pong 即计一次丢包（链路质量早期指标）
    if (ws.pingAnswered === false) m.pingsMissed += 1;
    ws.pingAnswered = false;
    m.pingsSent += 1;
    if (m.pingsSent > 0) m.lossPercent = Math.round((m.pingsMissed * 100) / m.pingsSent);
    try { ws.ping(String(Date.now())); } catch (e) { /* ignore */ }
  }
}, PING_INTERVAL).unref();

// ---------------- HTTP 服务 ----------------
const useTLS = !!(CONFIG.tls && CONFIG.tls.cert && CONFIG.tls.key);
const server = useTLS
  ? https.createServer({
      cert: fs.readFileSync(CONFIG.tls.cert),
      key: fs.readFileSync(CONFIG.tls.key),
    })
  : http.createServer();

server.on('upgrade', (req, socket, head) => {
  const u = new URL(req.url, 'http://localhost');
  if (u.pathname === CONFIG.tunnelPath) {
    wss.handleUpgrade(req, socket, head, (ws) => wss.emit('connection', ws, req));
  } else {
    socket.destroy();
  }
});

// ---------------- WebUI 会话与工具 ----------------
const sessions = new Map();   // token -> { username, expire }
const loginFails = new Map(); // ip -> { count, lockedUntil }
const SESSION_TTL = 7 * 24 * 3600 * 1000;

function parseCookies(req) {
  const out = {};
  const h = req.headers.cookie;
  if (!h) return out;
  for (const pair of h.split(';')) {
    const i = pair.indexOf('=');
    if (i > 0) out[pair.slice(0, i).trim()] = decodeURIComponent(pair.slice(i + 1).trim());
  }
  return out;
}

function isAuthed(req) {
  const t = parseCookies(req).ksu_mcp_admin;
  if (!t) return false;
  const s = sessions.get(t);
  if (!s) return false;
  if (s.expire < Date.now()) { sessions.delete(t); return false; }
  return true;
}

function loginLocked(ip) {
  const f = loginFails.get(ip);
  return !!(f && f.lockedUntil && f.lockedUntil > Date.now());
}

function failLogin(ip) {
  const f = loginFails.get(ip) || { count: 0, lockedUntil: 0 };
  f.count += 1;
  if (f.count >= 5) { f.lockedUntil = Date.now() + 10 * 60 * 1000; f.count = 0; log('登录失败次数过多，IP 锁定 10 分钟:', ip); }
  loginFails.set(ip, f);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('error', reject);
    req.on('end', () => {
      try { resolve(JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}')); }
      catch (e) { reject(new Error('JSON 解析失败')); }
    });
  });
}

function respond(res, status, body, headers) {
  const h = Object.assign({ 'Content-Type': 'application/json' }, headers);
  if (typeof body !== 'string') body = JSON.stringify(body);
  if (res.writableEnded) return;
  res.writeHead(status, h);
  res.end(body);
}

function requireAuth(req, res) {
  if (!isAuthed(req)) {
    respond(res, 401, { ok: false, error: '未登录或会话已过期' });
    return false;
  }
  return true;
}

// ---------------- 设备管理 ----------------
function kickDevice(device, code, reason) {
  const ws = deviceSockets.get(device);
  if (ws) { try { ws.close(code, reason); } catch (e) { /* ignore */ } }
}

function validateCustomToken(key, v) {
  if (v && !TOKEN_RE.test(v)) {
    return key + ' 需为 16-128 位字母/数字/._- 字符（或留空自动生成）';
  }
  return null;
}

// ---------------- API 路由 ----------------
async function handleAPI(req, res, u) {
  const p = u.pathname;

  // 登录（公开）
  if (p === '/api/login' && req.method === 'POST') {
    const ip = req.socket.remoteAddress || '';
    if (loginLocked(ip)) {
      respond(res, 429, { ok: false, error: '尝试次数过多，请 10 分钟后再试' });
      return;
    }
    let body;
    try { body = await readBody(req); } catch (e) { respond(res, 400, { ok: false, error: '参数错误' }); return; }
    if (body.username === CONFIG.admin.username && sha256(body.password || '') === CONFIG.admin.password) {
      loginFails.delete(ip);
      const token = randHex(32);
      sessions.set(token, { username: body.username, expire: Date.now() + SESSION_TTL });
      res.setHeader('Set-Cookie', 'ksu_mcp_admin=' + token + '; Path=/; HttpOnly; SameSite=Strict; Max-Age=' + Math.floor(SESSION_TTL / 1000));
      respond(res, 200, { ok: true });
      log('WebUI 登录成功:', body.username, '(' + ip + ')');
    } else {
      failLogin(ip);
      respond(res, 401, { ok: false, error: '用户名或密码错误' });
    }
    return;
  }

  if (!requireAuth(req, res)) return;

  if (p === '/api/logout' && req.method === 'POST') {
    const t = parseCookies(req).ksu_mcp_admin;
    if (t) sessions.delete(t);
    respond(res, 200, { ok: true });
    return;
  }

  if (p === '/api/status' && req.method === 'GET') {
    const devices = Object.keys(CONFIG.devices).map((d) => {
      const ws = deviceSockets.get(d);
      const dev = CONFIG.devices[d];
      return {
        device: d,
        online: !!(ws && ws.readyState === 1),
        connectedAt: ws && ws.meta ? ws.meta.connectedAt : null,
        remote: ws && ws.meta ? ws.meta.remote : null,
        tunnelToken: dev.tunnelToken,
        clientToken: dev.clientToken,
        metrics: metricsView(d),
      };
    });
    respond(res, 200, {
      ok: true,
      app: APP_NAME,
      version: VERSION,
      port: CONFIG.port,
      tls: useTLS,
      mcpPath: CONFIG.mcpPath,
      uptime: Date.now() - stats.startedAt,
      stats: {
        requests: stats.requests,
        failures: stats.failures,
        active: pending.size,
        devices: devices.length,
        devicesOnline: devices.filter((d) => d.online).length,
      },
      heartbeat: {
        pingIntervalMs: PING_INTERVAL,
        maxMissedPongs: MAX_MISSED_PONGS,
        deadAfterMs: PING_INTERVAL * MAX_MISSED_PONGS,
      },
      adminUser: CONFIG.admin.username,
      devices,
    });
    return;
  }

  // 连接质量指标（专供管理台轮询，避免重复渲染设备 Token）
  if (p === '/api/metrics' && req.method === 'GET') {
    const rows = Object.keys(CONFIG.devices).map(metricsView);
    const online = rows.filter((r) => r.online);
    const rtts = online.map((r) => r.rttMs).filter((v) => typeof v === 'number');
    respond(res, 200, {
      ok: true,
      version: VERSION,
      uptime: Date.now() - stats.startedAt,
      activeRequests: pending.size,
      heartbeat: {
        pingIntervalMs: PING_INTERVAL,
        maxMissedPongs: MAX_MISSED_PONGS,
        deadAfterMs: PING_INTERVAL * MAX_MISSED_PONGS,
      },
      aggregate: {
        devices: rows.length,
        online: online.length,
        reconnects: rows.reduce((a, r) => a + r.reconnects, 0),
        requests: rows.reduce((a, r) => a + r.requests, 0),
        failures: rows.reduce((a, r) => a + r.failures, 0),
        bytesIn: rows.reduce((a, r) => a + r.bytesIn, 0),
        bytesOut: rows.reduce((a, r) => a + r.bytesOut, 0),
        pingsSent: rows.reduce((a, r) => a + r.pingsSent, 0),
        pingsMissed: rows.reduce((a, r) => a + r.pingsMissed, 0),
        rttMsAvg: rtts.length ? Math.round(rtts.reduce((a, b) => a + b, 0) / rtts.length) : null,
      },
      devices: rows,
    });
    return;
  }

  // 添加设备（支持自定义 tunnelToken / clientToken，留空自动生成）
  if (p === '/api/devices' && req.method === 'POST') {
    let body;
    try { body = await readBody(req); } catch (e) { respond(res, 400, { ok: false, error: '参数错误' }); return; }
    const device = String(body.device || '').trim();
    if (!device) { respond(res, 400, { ok: false, error: '设备名不能为空' }); return; }
    if (!/^[A-Za-z0-9_.-]{1,64}$/.test(device)) { respond(res, 400, { ok: false, error: '设备名仅允许字母数字 . _ -，最长 64 字符' }); return; }
    if (CONFIG.devices[device]) { respond(res, 409, { ok: false, error: '设备已存在' }); return; }
    const tunnelToken = body.tunnelToken !== undefined && body.tunnelToken !== null ? String(body.tunnelToken).trim() : '';
    const clientToken = body.clientToken !== undefined && body.clientToken !== null ? String(body.clientToken).trim() : '';
    const errT = validateCustomToken('tunnelToken', tunnelToken);
    if (errT) { respond(res, 400, { ok: false, error: errT }); return; }
    const errC = validateCustomToken('clientToken', clientToken);
    if (errC) { respond(res, 400, { ok: false, error: errC }); return; }
    const tokens = {
      tunnelToken: tunnelToken || randHex(24),
      clientToken: clientToken || randHex(24),
    };
    CONFIG.devices[device] = tokens;
    saveConfig();
    log('WebUI 新增设备:', device);
    respond(res, 200, { ok: true, device: device, tokens });
    return;
  }

  // 删除设备
  const delMatch = p.match(/^\/api\/devices\/([^/]+)$/);
  if (delMatch && req.method === 'DELETE') {
    const device = decodeURIComponent(delMatch[1]);
    if (!CONFIG.devices[device]) { respond(res, 404, { ok: false, error: '设备不存在' }); return; }
    delete CONFIG.devices[device];
    kickDevice(device, 4003, 'removed');
    saveConfig();
    log('WebUI 删除设备:', device);
    respond(res, 200, { ok: true });
    return;
  }

  // 踢下线
  const kickMatch = p.match(/^\/api\/devices\/([^/]+)\/kick$/);
  if (kickMatch && req.method === 'POST') {
    const device = decodeURIComponent(kickMatch[1]);
    if (!CONFIG.devices[device]) { respond(res, 404, { ok: false, error: '设备不存在' }); return; }
    kickDevice(device, 4000, 'kicked by admin');
    log('WebUI 踢下线设备:', device);
    respond(res, 200, { ok: true });
    return;
  }

  // 重置设备 Token
  const rotMatch = p.match(/^\/api\/devices\/([^/]+)\/rotate$/);
  if (rotMatch && req.method === 'POST') {
    const device = decodeURIComponent(rotMatch[1]);
    const dev = CONFIG.devices[device];
    if (!dev) { respond(res, 404, { ok: false, error: '设备不存在' }); return; }
    let body = {};
    try { body = await readBody(req); } catch (e) { /* 默认 both */ }
    const which = body.which || 'both';
    if (which === 'tunnel' || which === 'both') dev.tunnelToken = randHex(24);
    if (which === 'client' || which === 'both') dev.clientToken = randHex(24);
    if (which !== 'tunnel' && which !== 'client' && which !== 'both') { respond(res, 400, { ok: false, error: 'which 参数无效' }); return; }
    saveConfig();
    if (which === 'tunnel' || which === 'both') kickDevice(device, 4003, 'token rotated');
    log('WebUI 重置设备 Token:', device, which);
    respond(res, 200, { ok: true, device: device, tokens: dev });
    return;
  }

  // 修改管理密码
  if (p === '/api/password' && req.method === 'POST') {
    let body;
    try { body = await readBody(req); } catch (e) { respond(res, 400, { ok: false, error: '参数错误' }); return; }
    if (sha256(body.old || '') !== CONFIG.admin.password) { respond(res, 401, { ok: false, error: '旧密码错误' }); return; }
    const np = String(body.new || '');
    if (np.length < 6) { respond(res, 400, { ok: false, error: '新密码至少 6 位' }); return; }
    CONFIG.admin.password = sha256(np);
    saveConfig();
    log('WebUI 修改管理密码');
    respond(res, 200, { ok: true });
    return;
  }

  // 日志
  if (p === '/api/logs' && req.method === 'GET') {
    const n = Math.min(parseInt(u.searchParams.get('n') || '200', 10) || 200, MAX_LOG);
    respond(res, 200, { ok: true, logs: logRing.slice(-n) });
    return;
  }

  respond(res, 404, { ok: false, error: 'not found' });
}

// ---------------- HTTP 请求主路由 ----------------
server.on('request', (req, res) => {
  const u = new URL(req.url, 'http://localhost');

  // 健康检查
  if (u.pathname === '/health') {
    respond(res, 200, 'ok\n', { 'Content-Type': 'text/plain' });
    return;
  }
  if (u.pathname === '/') {
    respond(res, 200, APP_NAME + ' v' + VERSION + ' — WebUI: /admin, MCP: ' + CONFIG.mcpPath + '/<device>, health: /health\n', { 'Content-Type': 'text/plain' });
    return;
  }

  // WebUI 页面
  if (u.pathname === '/admin') {
    fs.readFile(path.join(__dirname, 'admin.html'), (err, data) => {
      if (err) { respond(res, 500, 'admin.html 缺失', { 'Content-Type': 'text/plain' }); return; }
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      res.end(data);
    });
    return;
  }

  // WebUI API
  if (u.pathname.startsWith('/api/')) {
    handleAPI(req, res, u).catch((e) => respond(res, 500, { ok: false, error: String(e && e.message || e) }));
    return;
  }

  // MCP 代理：/mcp/<device>[/...]
  const prefix = CONFIG.mcpPath + '/';
  if (u.pathname.startsWith(prefix)) {
    const rest = u.pathname.slice(prefix.length);
    const slash = rest.indexOf('/');
    const device = slash === -1 ? rest : rest.slice(0, slash);
    const sub = slash === -1 ? '' : rest.slice(slash);

    const dev = CONFIG.devices[device];
    if (!dev) {
      respond(res, 404, errJSON(-32002, 'device not found'));
      return;
    }
    if (!authOK(req, device)) {
      res.setHeader('WWW-Authenticate', 'Bearer realm="ksu-mcp"');
      respond(res, 401, 'unauthorized', { 'Content-Type': 'text/plain' });
      return;
    }
    const ws = deviceSockets.get(device);
    if (!ws || ws.readyState !== 1) {
      stats.failures += 1;
      devMetrics(device).failures += 1;
      respond(res, 502, errJSON(-32000, 'device offline'));
      return;
    }

    stats.requests += 1;
    devMetrics(device).requests += 1;
    const chunks = [];
    let size = 0;
    let rejected = false;
    req.on('data', (c) => {
      if (rejected) return;
      size += c.length;
      if (size > MAX_PROXY_BODY) {
        rejected = true;
        stats.failures += 1;
        devMetrics(device).failures += 1;
        respond(res, 413, errJSON(-32000, 'request body too large'));
        return;
      }
      chunks.push(c);
    });
    req.on('error', () => { /* ignore */ });
    req.on('end', () => {
      if (rejected) return;
      const id = ++reqSeq;
      const msg = {
        type: 'request',
        id,
        method: req.method,
        path: CONFIG.mcpPath + sub,
        headers: pickHeaders(req.headers),
        body: Buffer.concat(chunks).toString('base64'),
      };
      // 注册在途请求：空闲超时（每次分片重置）+ settled 互斥收口
      const p = { id, device, res, started: false, settled: false, timer: null };
      pending.set(id, p);
      armTimer(p);
      const raw = JSON.stringify(msg);
      devMetrics(device).bytesOut += Buffer.byteLength(raw);
      try {
        ws.send(raw, (err) => {
          if (err) {
            devMetrics(device).failures += 1;
            failPending(p, 502, 'device send failed');
          }
        });
      } catch (e) {
        devMetrics(device).failures += 1;
        failPending(p, 502, 'device offline');
      }
    });
    return;
  }

  respond(res, 404, 'not found', { 'Content-Type': 'text/plain' });
});

// 转发必要的请求头；Authorization 由设备侧用自己的本地 Token 覆盖
function pickHeaders(h) {
  const keep = ['content-type', 'accept', 'mcp-session-id'];
  const out = {};
  for (const k of keep) {
    if (h[k] !== undefined) out[k] = h[k];
  }
  return out;
}

function authOK(req, device) {
  const dev = CONFIG.devices[device];
  if (!dev) return false;
  const h = req.headers['authorization'] || '';
  if (h.startsWith('Bearer ') && h.slice(7) === dev.clientToken) return true;
  const u = new URL(req.url, 'http://localhost');
  return u.searchParams.get('token') === dev.clientToken;
}

// ---------------- 启动 ----------------
server.listen(CONFIG.port, () => {
  log(APP_NAME + ' v' + VERSION + ' 已启动:', (useTLS ? 'https' : 'http') + '://0.0.0.0:' + CONFIG.port);
  log('设备接入: ' + CONFIG.tunnelPath + '?device=<id>&token=<tunnelToken>');
  log('远端 MCP 端点: /' + CONFIG.mcpPath.replace(/^\//, '') + '/<device>');
  log('WebUI: /admin (账号 ' + CONFIG.admin.username + ')');
  log('已注册设备: ' + (Object.keys(CONFIG.devices).join(', ') || '(无)'));
});