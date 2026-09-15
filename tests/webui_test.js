#!/usr/bin/env node
/**
 * KSU-MCP WebUI 冒烟测试（jsdom）
 * ================================
 *
 * 目的：在无 Android 设备的情况下，用真实 DOM + 真实脚本执行来验证
 * `module/webroot/index.html` 的**交互完整性**与**性能约定**。
 *
 * 这一类测试正是 v1.1.0 缺失的：当时 `.hidden` 类在 CSS 中根本不存在，
 * 导致「本机/局域网/公网」分段标签完全失效（点击无反应、三块面板同时平铺），
 * 但没有任何测试能发现它。
 *
 * 覆盖：
 *   A. 数据源：HTTP /api/state 路径 与 内核桥 ksu.exec 回落路径
 *   B. 交互完整性：所有 onclick 都有对应全局函数；点击每个写按钮都真的发出命令
 *   C. 无摆设组件：`.hidden` 规则存在；分段切换真的隐藏面板；按钮禁用态与能力一致
 *   D. 隧道 Token：留空表示沿用已保存值（不再出现「必须重填 Token」的卡点）
 *   E. 降级模式：无 ksu 桥接时给出横幅并禁用写操作
 *   F. 性能约定回归防护：不得使用 background-attachment:fixed / 卡片 backdrop-filter /
 *      基于 left 的开关动画；必须存在 translateX / 44px 触控区 / 日志行数上限
 *
 * 运行：
 *   cd /tmp && npm install jsdom        # 或在任意有 jsdom 的目录
 *   NODE_PATH=/tmp/webuitest/node_modules node tests/webui_test.js
 */
'use strict';

const fs = require('fs');
const path = require('path');

let JSDOM;
try {
  ({ JSDOM } = require('jsdom'));
} catch (e) {
  try {
    ({ JSDOM } = require(path.join('/tmp/webuitest/node_modules/jsdom')));
  } catch (e2) {
    console.error('需要 jsdom：npm install jsdom（或用 NODE_PATH 指向已安装目录）');
    process.exit(2);
  }
}

const HTML_PATH = path.join(__dirname, '..', 'module', 'webroot', 'index.html');
const html = fs.readFileSync(HTML_PATH, 'utf8');

const PASS = [];
const FAIL = [];
function check(name, cond, detail) {
  if (cond) { PASS.push(name); console.log('  \x1b[32m✓\x1b[0m ' + name); }
  else { FAIL.push([name, detail]); console.log('  \x1b[31m✗\x1b[0m ' + name + '  ' + (detail || '')); }
  return !!cond;
}
function section(t) { console.log('\n\x1b[1m' + t + '\x1b[0m'); }

// ---------------------------------------------------------------- 测试数据
const STATE = {
  version: '1.2.0', protocol_version: '2025-11-25', supported_protocols: ['2025-11-25'],
  running: true, pid: 4242, config: '/data/adb/ksu_mcp/config.json', data_dir: '/data/adb/ksu_mcp',
  port: 9123, bind: '127.0.0.1', token: 'abcdef1234567890abcdef1234567890abcdef12',
  read_only: false, exec_timeout: 30, allowlist: [], disabled: false,
  lan_ips: ['192.168.1.23'], networks: [{ name: 'wlan0', kind: 'wifi', ips: ['192.168.1.23'] }],
  watchdog: { running: true, pid: 99, interval_sec: 10, tunnel_stale_sec: 45 },
  sessions: { streamable_http: 2, ttl_sec: 1800, max: 512 },
  tools: { total: 32, canonical: 21, deprecated: 11, names: ['android_get_device_info', 'android_screenshot'] },
  tunnel: {
    enabled: true, running: true, pid: 77, server: 'wss://n.huziyang.top/tunnel', device: 'my-phone',
    ip: '1.2.3.4', token_set: true, connected: true, healthy: true, state: 'connected',
    heartbeat_sec: 10, read_timeout_sec: 35, backoff_max_sec: 30, iface_poll_sec: 5,
    public_url: 'https://n.huziyang.top/mcp/my-phone', server_version: '1.2.0', stream_ok: true,
    reconnects: 3, consecutive_fails: 0, latency_ms: 42, bytes_in: 2048, bytes_out: 4096,
    requests: 12, responses: 12, streams: 1, last_error: '', connected_seconds: 3661
  },
  endpoints: {
    local: [
      { scope: 'local', label: '本机回环 (127.0.0.1)', transport: 'streamable-http', url: 'http://127.0.0.1:9123/mcp', auth_type: 'local-token', auth_hint: 'Bearer <Token>', available: true },
      { scope: 'local', label: '本机回环 (127.0.0.1)', transport: 'sse', url: 'http://127.0.0.1:9123/sse', auth_type: 'local-token', auth_hint: 'Bearer <Token>', available: true }
    ],
    lan: [
      { scope: 'lan', label: 'WiFi · wlan0 (192.168.1.23)', transport: 'streamable-http', url: 'http://192.168.1.23:9123/mcp', auth_type: 'local-token', available: false, note: '局域网访问未开启' }
    ],
    public: [
      { scope: 'public', label: '公网隧道 (任意网络可达)', transport: 'streamable-http', url: 'https://n.huziyang.top/mcp/my-phone', auth_type: 'client-token', auth_hint: 'Bearer <clientToken>', available: true }
    ]
  }
};
const TOOLS = {
  total: 32, canonical: 21, deprecated: 11,
  tools: [
    { name: 'android_get_device_info', title: '设备信息', description: '获取设备信息，返回品牌型号等', deprecated: false, params: [], required: [] },
    { name: 'android_list_packages', title: '应用列表', description: '列出已安装应用', deprecated: false, params: ['filter', 'limit', 'offset'], required: [] },
    { name: 'device_info', title: '【已弃用】', description: '已弃用', deprecated: true, replaced_by: 'android_get_device_info', params: [], required: [] }
  ]
};

// ---------------------------------------------------------------- 环境构造
function makeDom(opts) {
  opts = opts || {};
  const commands = [];
  const fetches = [];
  const toasts = [];

  const dom = new JSDOM(html, {
    runScripts: 'dangerously',
    pretendToBeVisual: true,
    beforeParse(window) {
      window.AbortController = AbortController;
      window.confirm = () => true;
      if (opts.bridge) {
        window.ksu = {
          exec(cmd, optJson, cbName) {
            commands.push(cmd);
            let out = '';
            if (/ui-bootstrap/.test(cmd)) {
              out = JSON.stringify({
                version: '1.2.0', running: true, port: 9123, bind: '127.0.0.1',
                token: STATE.token, has_token: true, api_base: 'http://127.0.0.1:9123',
                api_state: 'http://127.0.0.1:9123/api/state', data_dir: '/data/adb/ksu_mcp'
              });
            } else if (/ui-state/.test(cmd)) {
              out = JSON.stringify(STATE);
            } else if (/logs/.test(cmd)) {
              out = 'line1\nline2\nline3';
            } else if (/token --regen/.test(cmd)) {
              out = STATE.token;
            } else {
              out = '操作完成';
            }
            setTimeout(() => { if (window[cbName]) window[cbName](0, out, ''); }, 0);
          }
        };
      }
      if (opts.httpOk) {
        window.fetch = (url, init) => {
          fetches.push({ url, auth: (init && init.headers && init.headers.Authorization) || '' });
          let body = null;
          if (/\/api\/state/.test(url)) body = STATE;
          else if (/\/api\/tools/.test(url)) body = TOOLS;
          else if (/\/api\/logs/.test(url)) body = { source: 'mcpd', lines: ['line1', 'line2', 'line3'], count: 3, error: null };
          if (!body) return Promise.reject(new Error('404'));
          return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
        };
      } else {
        window.fetch = () => Promise.reject(new Error('blocked'));
      }
      // 捕获 toast 文案（用于断言不会误报「必须填写 Token」）
      window.__toasts = toasts;
      const origSetTimeout = window.setTimeout;
      window.setTimeout = function (fn, ms) {
        if (typeof fn === 'function') {
          const src = fn.toString();
          if (src.indexOf("el.className = 'toast show") >= 0) {
            // 记录 toast 文本需读取 DOM，改为包裹后延迟读取
            return origSetTimeout(function () { fn(); toasts.push(window.document.getElementById('toast').textContent); }, ms);
          }
        }
        return origSetTimeout(fn, ms);
      };
    }
  });
  return { dom, commands, fetches, toasts };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---------------------------------------------------------------- 用例
async function run() {
  // 静态检查前先剥离注释：说明性注释里会提到「不要用 background-attachment:fixed」，
  // 直接全文匹配会产生假阳性。
  const cssRaw = html.match(/<style>([\s\S]*?)<\/style>/)[1];
  const cssRules = cssRaw.replace(/\/\*[\s\S]*?\*\//g, '');
  const jsRaw = html.match(/<script>([\s\S]*?)<\/script>/)[1];
  const jsCode = jsRaw.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');

  // ============================ F. 静态性能/结构约定
  section('F. 静态约定（性能与可用性回归防护）');
  check('CSS 中不存在 background-attachment:fixed（v1.1.0 滚动卡顿根因）',
    !/background-attachment\s*:\s*fixed/.test(cssRules));
  check('卡片未使用 backdrop-filter（仅顶栏允许一次）',
    (() => {
      const cardBlock = cssRules.match(/\.card\{[^}]*\}/g) || [];
      return cardBlocksOk(cardBlock);
      function cardBlocksOk(blocks) { return blocks.every((b) => !/backdrop-filter/.test(b)); }
    })());
  check('开关滑块动画使用 transform:translateX（不是 left）',
    /\.sw input:checked\+i::after\{[^}]*transform:translateX/.test(cssRules.replace(/\s+/g, ' ')) &&
    !/\.sw input:checked\+i::after\{[^}]*left:/.test(cssRules.replace(/\s+/g, ' ')));
  check('存在 .hidden{display:none!important} 规则（v1.1.0 致命缺陷修复）',
    /\.hidden\s*\{[^}]*display\s*:\s*none\s*!important/.test(cssRules.replace(/\s+/g, ' ')));
  check('未动画 box-shadow（不可走合成器）',
    !/@keyframes[^{]*\{(?:[^{}]*\{[^}]*box-shadow[^}]*\})/.test(cssRules));
  check('按钮触控区 ≥44px（.btn / .kv 行高）',
    /\.btn\{[^}]*min-height:var\(--tap\)/.test(cssRules.replace(/\s+/g, ' ')) &&
    /--tap:44px/.test(cssRules));
  check('日志容器为固定高度且使用 contain',
    /pre\{[^}]*height:230px/.test(cssRules.replace(/\s+/g, ' ')) &&
    /pre\{[^}]*contain:content/.test(cssRules.replace(/\s+/g, ' ')));
  check('脚本中无 JS 涟漪实现（避免 getBoundingClientRect 强制回流）',
    !/classList\.add\('ripple'\)|className = 'ripple'/.test(jsCode) && !/getBoundingClientRect/.test(jsCode));
  check('日志行数上限常量存在（MAX_LOG_LINES）', /MAX_LOG_LINES\s*=\s*\d+/.test(html));
  check('轮询为自适应调度（schedulePoll + 失败退避），非固定 setInterval 全量刷新',
    /function schedulePoll/.test(html) && /POLL_MAX/.test(html) && /visibilitychange/.test(html));
  check('四大核心面板齐备（运行状态/连接信息/内网穿透/MCP 工具）',
    ['cardStatus', 'cardNet', 'cardTunnel', 'cardTools'].every((id) => html.indexOf('id="' + id + '"') >= 0));

  // ============================ A/B/C. HTTP 数据源
  section('A/C. 正常模式（HTTP /api/state + KernelSU 桥接）');
  {
    const { dom, commands, fetches, toasts } = makeDom({ bridge: true, httpOk: true });
    const w = dom.window;
    const d = w.document;
    await sleep(160);

    check('启动后通过 HTTP 拉取 /api/state（零进程开销路径）',
      fetches.some((f) => /\/api\/state/.test(f.url)), JSON.stringify(fetches.map(f => f.url)));
    check('/api/state 请求携带 Bearer Token',
      fetches.some((f) => f.auth === 'Bearer ' + STATE.token));
    check('数据源标识显示「本地 API」',
      d.getElementById('srcChip').textContent.indexOf('本地 API') >= 0,
      d.getElementById('srcChip').textContent);

    // B. 所有 onclick 都有对应全局函数
    const onclicks = Array.from(d.querySelectorAll('[onclick]'));
    check('页面存在可交互的 onclick 元素（≥15 个）', onclicks.length >= 15, String(onclicks.length));
    const missing = [];
    onclicks.forEach((el) => {
      const m = el.getAttribute('onclick').match(/^\s*([A-Za-z_$][\w$]*)\s*\(/);
      if (!m) { missing.push(el.getAttribute('onclick')); return; }
      if (typeof w[m[1]] !== 'function') missing.push(m[1]);
    });
    check('所有 onclick 均指向已定义的全局函数（无死绑定）', missing.length === 0, JSON.stringify(missing));

    // C. 分段标签真的生效
    check('默认显示「本机」面板，隐藏局域网与公网面板',
      d.getElementById('tab-local').className.indexOf('hidden') < 0 &&
      d.getElementById('tab-lan').className.indexOf('hidden') >= 0 &&
      d.getElementById('tab-wan').className.indexOf('hidden') >= 0);
    w.selTab('lan');
    check('点击「局域网」后真实切换（本机隐藏、局域网显示）',
      d.getElementById('tab-local').className.indexOf('hidden') >= 0 &&
      d.getElementById('tab-lan').className.indexOf('hidden') < 0);
    check('切换后「局域网」按钮为选中态',
      d.querySelector('#seg button[data-tab="lan"]').className.indexOf('on') >= 0);
    w.selTab('wan');
    check('点击「公网」后真实切换', d.getElementById('tab-wan').className.indexOf('hidden') < 0);

    // 三网络地址渲染
    const localUrls = Array.from(d.querySelectorAll('#tab-local code')).map((c) => c.textContent);
    check('本机面板渲染 Streamable HTTP 与 SSE 两条地址',
      localUrls.some((u) => u === 'http://127.0.0.1:9123/mcp') &&
      localUrls.some((u) => u === 'http://127.0.0.1:9123/sse'), JSON.stringify(localUrls));
    const wanUrls = Array.from(d.querySelectorAll('#tab-wan code')).map((c) => c.textContent);
    check('公网面板渲染隧道域名地址',
      wanUrls.some((u) => u === 'https://n.huziyang.top/mcp/my-phone'), JSON.stringify(wanUrls));
    const lanUrls = Array.from(d.querySelectorAll('#tab-lan code')).map((c) => c.textContent);
    check('局域网面板渲染设备局域网 IP 地址',
      lanUrls.some((u) => u === 'http://192.168.1.23:9123/mcp'), JSON.stringify(lanUrls));
    check('每个地址都带鉴权说明',
      Array.from(d.querySelectorAll('.ep-a')).every((e) => /鉴权/.test(e.textContent)));
    check('不可用地址带原因说明',
      Array.from(d.querySelectorAll('.ep-n')).some((e) => e.className.indexOf('warn') >= 0 && e.textContent.length > 0));

    // 状态面板
    check('运行状态显示「运行中」与 PID',
      d.getElementById('pillText').textContent === '运行中' && d.getElementById('pidVal').textContent === '4242');
    check('守护徽标显示运行中', /守护运行中/.test(d.getElementById('watchChip').textContent));

    // 隧道面板：真实连接态 + 质量指标
    check('隧道胶囊显示已连接', /已连接/.test(d.getElementById('tunPillText').textContent),
      d.getElementById('tunPillText').textContent);
    check('隧道质量指标：延迟 42ms',
      d.getElementById('tmLatency').textContent === '42 ms', d.getElementById('tmLatency').textContent);
    check('隧道质量指标：重连次数与连接时长',
      d.getElementById('tmReconn').textContent === '3' &&
      /1 时 1 分/.test(d.getElementById('tmUptime').textContent), d.getElementById('tmUptime').textContent);
    check('隧道质量指标：收发字节',
      /收发/.test(d.getElementById('tmBytes').parentNode.textContent));

    // D. Token 不回显 + 留空沿用
    check('隧道 Token 输入框保持为空（密钥不回显）', d.getElementById('tunToken').value === '');
    check('Token 状态提示「已配置（留空不修改）」',
      d.getElementById('tunTokenState').textContent.indexOf('已配置') >= 0,
      d.getElementById('tunTokenState').textContent);
    const before = commands.length;
    w.saveTunnel(d.getElementById('tunConnect').dataset.label ? null : null);
    await sleep(80);
    const cfgCmds = commands.slice(before).filter((c) => /config-set/.test(c));
    check('留空 Token 保存隧道配置时不下发 --tunnel-token（沿用已保存值）',
      cfgCmds.length > 0 && cfgCmds.every((c) => c.indexOf('--tunnel-token') < 0),
      JSON.stringify(cfgCmds));
    check('留空 Token 保存不再误报「必须填写 tunnelToken」',
      !toasts.some((t) => /首次配置必须填写/.test(t)), JSON.stringify(toasts));

    // B. 点击每个写操作按钮，必须真的发出命令
    const byText = (t) => Array.from(d.querySelectorAll('.btn')).find((b) => b.textContent.trim() === t);
    const actionTests = [
      ['启动', [/(^|\s)start(\s|$)/], () => byText('启动')],
      ['重启', [/(^|\s)restart(\s|$)/], () => byText('重启')],
      ['停止', [/(^|\s)stop(\s|$)/], () => byText('停止')],
      ['断开隧道', [/tunnel-stop/], () => d.getElementById('tunStop')],
      ['重新生成 Token', [/token --regen/], () => byText('重新生成')],
      ['保存并重启服务', [/config-set/, /(^|\s)restart(\s|$)/], () => byText('保存并重启服务')]
    ];
    for (const [label, res, getter] of actionTests) {
      const btn = getter();
      if (!btn) { check('写按钮存在：' + label, false, '未找到按钮'); continue; }
      if (btn.disabled) {
        check('「' + label + '」按钮在可操作场景下未被禁用', false, '按钮处于 disabled 状态');
        continue;
      }
      const n = commands.length;
      try {
        btn.click();
      } catch (e) {
        check('点击「' + label + '」不抛异常', false, String(e && e.message));
        continue;
      }
      await sleep(120);
      const issued = commands.slice(n);
      // res 为「期望出现的正则数组」：每条正则至少匹配到一条命令
      check('点击「' + label + '」真实下发命令',
        res.every((r) => issued.some((c) => r.test(c))), JSON.stringify(issued));
      check('「' + label + '」按钮文案已恢复（不会卡在「处理中」）',
        btn.textContent.indexOf('处理中') < 0 && btn.textContent.indexOf('中') < 0 || btn.textContent.trim() === label,
        btn.textContent);
    }

    // 「立即连接」在隧道已连接时本来就是禁用态（正确行为），
    // 因此先模拟「未连接」再验证它真的下发 tunnel 命令。
    STATE.tunnel.connected = false;
    STATE.tunnel.state = 'reconnecting';
    await w.refresh(false);
    await sleep(50);
    const connectBtn = d.getElementById('tunConnect');
    check('隧道未连接时「立即连接」按钮恢复可点', !connectBtn.disabled);
    const nConn = commands.length;
    connectBtn.click();
    await sleep(120);
    check('点击「立即连接」真实下发 tunnel 命令',
      commands.slice(nConn).some((c) => /(^|\s)tunnel(\s|$)/.test(c)), JSON.stringify(commands.slice(nConn)));
    STATE.tunnel.connected = true;
    STATE.tunnel.state = 'connected';

    // 工具面板
    w.toggleTools(d.getElementById('toolToggle'));
    await sleep(60);
    check('展开工具列表后确实显示工具行',
      d.querySelectorAll('#toolList .tool').length === 3, String(d.querySelectorAll('#toolList .tool').length));
    check('工具行标注弃用与替代关系',
      d.querySelector('#toolList .tool.dep .badge.dep') &&
      /android_get_device_info/.test(d.querySelector('#toolList .tool.dep').textContent));
    w.document.getElementById('toolFilter').value = 'packages';
    w.renderTools();
    check('工具搜索过滤生效',
      d.querySelectorAll('#toolList .tool').length === 1,
      String(d.querySelectorAll('#toolList .tool').length));

    // 日志
    check('日志面板渲染内容', d.getElementById('logs').textContent.indexOf('line1') >= 0,
      d.getElementById('logs').textContent.slice(0, 40));
    check('日志经过上限裁剪函数（trimLog 存在）', typeof w.trimLog === 'function');

    // 面板不可见时不渲染（降负载）
    const writePreSrc = w.writePre.toString();
    check('writePre 仅在内容变化时赋值（避免无谓重排）', /el\.textContent !== text/.test(writePreSrc));

    check('页面无「点击无响应」的占位按钮：所有 .btn 都有 onclick 或为输入型控件',
      Array.from(d.querySelectorAll('button.btn')).every((b) =>
        b.hasAttribute('onclick') || b.closest('.ep') || b.id === 'eyeBtn' || b.id === 'toolToggle' ||
        b.hasAttribute('onclick')),
      JSON.stringify(Array.from(d.querySelectorAll('button.btn'))
        .filter((b) => !b.hasAttribute('onclick') && !b.closest('.ep') && b.id !== 'eyeBtn' && b.id !== 'toolToggle')
        .map((b) => b.id || b.textContent.trim())));

    dom.window.close();
  }

  // ============================ A/E. 内核桥回落 + 降级
  section('A/E. 内核桥回落与降级模式');
  {
    const { dom, commands, toasts } = makeDom({ bridge: true, httpOk: false });
    const w = dom.window;
    const d = w.document;
    await sleep(200);

    check('fetch 不可用时自动回落到 ksu.exec 的 ui-state（单次调用拿全量数据）',
      commands.some((c) => /ui-state/.test(c)), JSON.stringify(commands));
    check('回落路径下数据源标识为「内核桥」',
      d.getElementById('srcChip').textContent.indexOf('内核桥') >= 0,
      d.getElementById('srcChip').textContent);
    check('回落路径下状态仍正确渲染',
      d.getElementById('pillText').textContent === '运行中' &&
      d.getElementById('tmLatency').textContent === '42 ms');
    check('回落路径下未误显示降级横幅', d.getElementById('degrade').className.indexOf('hidden') >= 0);
    check('回落路径只调用一次 ui-state（v1.1.0 需 2~3 次）',
      commands.filter((c) => /ui-state/.test(c)).length === 1,
      JSON.stringify(commands));
    check('日志回落走 mcpd logs --source',
      commands.some((c) => /logs \d+ --source/.test(c)), JSON.stringify(commands));
    dom.window.close();
  }

  {
    const { dom, toasts } = makeDom({ bridge: false, httpOk: false });
    const w = dom.window;
    const d = w.document;
    await sleep(200);

    check('无 ksU 桥接时显示只读降级横幅（不再静默失败）',
      d.getElementById('degrade').className.indexOf('hidden') < 0);
    check('降级横幅说明必须通过 KernelSU Manager 打开',
      /KernelSU Manager/.test(d.getElementById('degrade').textContent));
    check('降级模式下写操作按钮被禁用（避免摆设观感）',
      d.getElementById('tunConnect').disabled && d.getElementById('tunServer').disabled &&
      d.getElementById('cfgPort').disabled);
    check('降级模式下启停按钮被禁用',
      Array.from(d.querySelectorAll('.btn[data-label]')).every((b) => b.disabled));
    check('降级模式下「工具列表」仍可展开（只读能力保留）',
      !d.getElementById('toolToggle').disabled);
    check('降级模式下状态胶囊给出「桥接不可用」提示',
      /桥接不可用/.test(d.getElementById('pillText').textContent),
      d.getElementById('pillText').textContent);
    dom.window.close();
  }

  // ---------------------------------------------------------------- 汇总
  console.log('\n' + '='.repeat(60));
  console.log('通过 ' + PASS.length + ' 项，失败 ' + FAIL.length + ' 项');
  if (FAIL.length) {
    console.log('\n失败明细:');
    FAIL.forEach(([n, dt]) => console.log('  - ' + n + ': ' + dt));
    return 1;
  }
  console.log('\x1b[32m全部用例通过\x1b[0m');
  return 0;
}

run().then((c) => process.exit(c)).catch((e) => {
  console.error('测试执行异常:', e);
  process.exit(1);
});
