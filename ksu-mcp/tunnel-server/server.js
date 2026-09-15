#!/usr/bin/env node
/**
 * KSU MCP Tunnel Server
 * 内网穿透服务端：设备（模块端 mcpd tunnel）主动连接本服务，远端 MCP 客户端
 * 通过 https://n.huziyang.top/mcp/<device> 访问设备上的 MCP 服务。
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

const VERSION = "1.0.0";
const APP_NAME = 'KSU MCP Tunnel Server';

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
    requestTimeout: 90000,   // 转发请求超时（毫秒）
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
const pending = new Map();       // reqId -> { done, timer, device }
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
  ws.meta = { device, connectedAt: Date.now(), remote: req.socket.remoteAddress || '' };
  deviceSockets.set(device, ws);
  ws.isAlive = true;
  ws.on('pong', () => { ws.isAlive = true; });

  ws.on('message', (data) => {
    let msg;
    try { msg = JSON.parse(data.toString()); } catch (e) { return; }
    if (msg && msg.type === 'response' && msg.id !== undefined) {
      const p = pending.get(msg.id);
      if (p) {
        clearTimeout(p.timer);
        pending.delete(msg.id);
        p.done(msg);
      }
    }
  });

  ws.on('close', () => {
    if (deviceSockets.get(device) === ws) deviceSockets.delete(device);
    log('设备离线:', device);
  });
  ws.on('error', () => { /* ignore */ });
  log('设备在线:', device, '(' + ws.meta.remote + ')');
});

// 心跳：清理死连接
setInterval(() => {
  for (const [device, ws] of deviceSockets) {
    if (!ws.isAlive) {
      log('心跳超时，断开设备:', device);
      try { ws.terminate(); } catch (e) { /* ignore */ }
      continue;
    }
    ws.isAlive = false;
    try { ws.ping(); } catch (e) { /* ignore */ }
  }
}, 30000).unref();

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

function genDeviceTokens() { return { tunnelToken: randHex(24), clientToken: randHex(24) }; }

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
      };
    });
    respond(res, 200, {
      ok: true,
      app: APP_NAME,
      version: VERSION,
      port: CONFIG.port,
      tls: useTLS,
      uptime: Date.now() - stats.startedAt,
      stats: {
        requests: stats.requests,
        failures: stats.failures,
        active: pending.size,
      },
      adminUser: CONFIG.admin.username,
      devices,
    });
    return;
  }

  // 添加设备
  if (p === '/api/devices' && req.method === 'POST') {
    let body;
    try { body = await readBody(req); } catch (e) { respond(res, 400, { ok: false, error: '参数错误' }); return; }
    const device = String(body.device || '').trim();
    if (!device) { respond(res, 400, { ok: false, error: '设备名不能为空' }); return; }
    if (!/^[A-Za-z0-9_.-]{1,64}$/.test(device)) { respond(res, 400, { ok: false, error: '设备名仅允许字母数字 . _ -，最长 64 字符' }); return; }
    if (CONFIG.devices[device]) { respond(res, 409, { ok: false, error: '设备已存在' }); return; }
    CONFIG.devices[device] = genDeviceTokens();
    saveConfig();
    log('WebUI 新增设备:', device);
    respond(res, 200, { ok: true, device: device, tokens: CONFIG.devices[device] });
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
      respond(res, 404, JSON.stringify({ jsonrpc: '2.0', error: { code: -32002, message: 'device not found' } }));
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
      respond(res, 502, JSON.stringify({ jsonrpc: '2.0', error: { code: -32000, message: 'device offline' } }));
      return;
    }

    stats.requests += 1;
    let chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('error', () => { /* ignore */ });
    req.on('end', () => {
      const id = ++reqSeq;
      const msg = {
        type: 'request',
        id,
        method: req.method,
        path: CONFIG.mcpPath + sub,
        headers: pickHeaders(req.headers),
        body: Buffer.concat(chunks).toString('base64'),
      };
      // 设备响应到达后：回写状态、必要响应头、原文 body（头键名大小写不敏感）
      const done = (rmsg) => {
        const headers = {};
        const hs = rmsg.headers || {};
        const get = (k) => {
          for (const key of Object.keys(hs)) {
            if (key.toLowerCase() === k) return hs[key];
          }
          return undefined;
        };
        const ct = get('content-type');
        const sid = get('mcp-session-id');
        if (ct) headers['Content-Type'] = ct;
        if (sid) headers['Mcp-Session-Id'] = sid;
        let body = Buffer.alloc(0);
        try { body = Buffer.from(rmsg.body || '', 'base64'); } catch (e) { /* ignore */ }
        res.writeHead(rmsg.status || 502, headers);
        res.end(body);
      };
      const timer = setTimeout(() => {
        pending.delete(id);
        stats.failures += 1;
        respond(res, 504, JSON.stringify({ jsonrpc: '2.0', error: { code: -32000, message: 'device timeout' } }));
      }, CONFIG.requestTimeout || 90000);

      pending.set(id, { done, timer, device });
      try {
        ws.send(JSON.stringify(msg), (err) => {
          if (err) {
            clearTimeout(timer);
            pending.delete(id);
            stats.failures += 1;
            respond(res, 502, JSON.stringify({ jsonrpc: '2.0', error: { code: -32000, message: 'device send failed' } }));
          }
        });
      } catch (e) {
        clearTimeout(timer);
        pending.delete(id);
        stats.failures += 1;
        respond(res, 502, JSON.stringify({ jsonrpc: '2.0', error: { code: -32000, message: 'device offline' } }));
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
