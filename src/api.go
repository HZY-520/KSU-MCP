package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// api.go — 聚合状态与 WebUI 只读控制 API
//
// 设计动机（问题 1.6 / 2.2 / 4 的共同根因）：
// v1.1.0 的 WebUI 每 5s 通过 ksu 桥 fork 2~3 个 mcpd 进程（status / tunnel-status / logs）
// 才能拼出界面状态，既慢又卡；而隧道 Token 与三网络地址根本拼不出来。
//
// v1.2.0 提供**单一聚合载荷**，CLI、HTTP API、WebUI 三处共用同一份数据：
//   - CLI：mcpd ui-state（一次进程调用拿到全部）
//   - HTTP：GET /api/state（WebUI 走 fetch，零进程开销）
//   - 引导：mcpd ui-bootstrap（WebUI 启动时拿 token/port，之后即可走 HTTP）
//
// 注意：buildUIState 内部**不 fork 任何子进程**（只读文件 + net.Interfaces），
// 因此可以放心高频轮询。

// endpointInfo 单个接入地址的完整描述（供 WebUI「连接信息」卡片渲染）
type endpointInfo struct {
	Scope     string `json:"scope"`     // local | lan | public
	Label     string `json:"label"`     // 展示名，如「本机回环」「WiFi (wlan0)」
	Transport string `json:"transport"` // streamable-http | sse
	URL       string `json:"url"`
	AuthType  string `json:"auth_type"` // local-token | client-token | none
	AuthHint  string `json:"auth_hint"`
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
	Iface     string `json:"iface,omitempty"`
}

// uiStatePayload 三网络层级的接入地址集合
type uiStatePayload struct {
	Local  []endpointInfo `json:"local"`
	LAN    []endpointInfo `json:"lan"`
	Public []endpointInfo `json:"public"`
}

// liveConfig 返回「当前实际生效」的配置：
// 服务运行中时优先读守护进程落盘的 runtime 配置，否则读配置文件。
func liveConfig() *Config {
	cfg, err := loadConfig()
	if err != nil || cfg == nil {
		cfg = defaultConfig()
	}
	if _, ok := readPid(); ok {
		if data, err := os.ReadFile(runtimePath()); err == nil {
			var live Config
			if json.Unmarshal(data, &live) == nil {
				cfg = &live
			}
		}
	}
	return cfg
}

// buildUIState 组装 WebUI / status 所需的完整状态（无子进程 fork）
func buildUIState(cfg *Config) map[string]any {
	if cfg == nil {
		cfg = liveConfig()
	}
	running := false
	pid := 0
	if p, ok := readPid(); ok {
		running = true
		pid = p
	}
	watchdogRunning := false
	watchdogPID := 0
	if p, ok := readPidFile(watchdogPidPath()); ok {
		watchdogRunning = true
		watchdogPID = p
	}
	tunnelRunning := false
	tunnelPID := 0
	if p, ok := readPidFile(tunnelPidPath()); ok {
		tunnelRunning = true
		tunnelPID = p
	}

	tunnel := map[string]any{
		"enabled":          cfg.Tunnel.Enabled,
		"running":          tunnelRunning,
		"pid":              tunnelPID,
		"server":           cfg.Tunnel.Server,
		"device":           cfg.Tunnel.Device,
		"ip":               cfg.Tunnel.IP,
		"token_set":        cfg.Tunnel.Token != "",
		"connected":        false,
		"healthy":          false,
		"state":            "stopped",
		"heartbeat_sec":    int(tunnelPingInterval.Seconds()),
		"read_timeout_sec": int(tunnelReadTimeout.Seconds()),
		"backoff_max_sec":  int(tunnelBackoffMax.Seconds()),
		"iface_poll_sec":   int(tunnelIfacePoll.Seconds()),
		"public_url":       publicMCPURL(cfg),
	}
	if m, err := loadTunnelMetrics(); err == nil {
		tunnel["state"] = m.State
		tunnel["connected"] = m.Connected
		tunnel["healthy"] = m.Healthy
		tunnel["server_version"] = m.ServerVersion
		tunnel["stream_ok"] = m.StreamOK
		tunnel["reconnects"] = m.Reconnects
		tunnel["consecutive_fails"] = m.ConsecutiveFails
		tunnel["latency_ms"] = m.LatencyMs
		tunnel["bytes_in"] = m.BytesIn
		tunnel["bytes_out"] = m.BytesOut
		tunnel["requests"] = m.Requests
		tunnel["responses"] = m.Responses
		tunnel["streams"] = m.Streams
		tunnel["last_error"] = m.LastError
		tunnel["last_error_at"] = m.LastErrorAt
		tunnel["connected_seconds"] = m.ConnectedSeconds
		tunnel["runtime_age_sec"] = time.Now().Unix() - m.UpdatedAt
		tunnel["next_retry_at"] = m.NextRetryAt
		if !tunnelRunning {
			tunnel["state"] = "stopped"
			tunnel["connected"] = false
			tunnel["healthy"] = false
		}
	}

	canonical := canonicalToolNames()
	all := toolNames()
	return map[string]any{
		"version":             appVersion,
		"protocol_version":    protocolVersion,
		"supported_protocols": supportedProtocols,
		"generated_at":        time.Now().Format(time.RFC3339),
		"running":             running,
		"pid":                 pid,
		"config":              configPath(),
		"data_dir":            dataDir(),
		"port":                cfg.Port,
		"bind":                cfg.Bind,
		"token":               cfg.Token,
		"log":                 logPath(),
		"read_only":           cfg.ReadOnly,
		"exec_timeout":        cfg.ExecTimeout,
		"allowlist":           cfg.ExecAllowlist,
		"lan_ips":             lanIPs(),
		"networks":            netIfaces(),
		"disabled":            daemonDisabled(),
		"watchdog": map[string]any{
			"running":          watchdogRunning,
			"pid":              watchdogPID,
			"interval_sec":     int(watchdogInterval.Seconds()),
			"tunnel_stale_sec": int(tunnelStaleAfter.Seconds()),
		},
		"sessions": map[string]any{
			"streamable_http": sessionCountSafe(),
			"ttl_sec":         int(sessionTTL.Seconds()),
			"max":             sessionMaxCount,
		},
		"tools": map[string]any{
			"total":      len(all),
			"canonical":  len(canonical),
			"deprecated": len(all) - len(canonical),
			"names":      canonical,
		},
		"tunnel":    tunnel,
		"endpoints": buildEndpoints(cfg),
	}
}

// sessionCountSafe 读取当前 HTTP 会话数（无 daemon 实例时返回 0）
func sessionCountSafe() int {
	if globalServer == nil {
		return 0
	}
	return globalServer.sessionCount()
}

// globalServer 指向本进程内的 MCP 服务实例（daemon 模式下非 nil），
// 供状态聚合读取会话数；stdio / CLI 模式下为 nil。
var globalServer *MCPServer

// buildEndpoints 构造本地 / 局域网 / 公网三个网络层级的接入地址
func buildEndpoints(cfg *Config) uiStatePayload {
	port := cfg.Port
	localHost := "127.0.0.1"
	if !isLoopback(cfg.Bind) && cfg.Bind != "0.0.0.0" {
		localHost = cfg.Bind
	}
	lanEnabled := !isLoopback(cfg.Bind)

	auth := "local-token"
	authHint := "Authorization: Bearer <本机 Token>"
	if cfg.Token == "" {
		auth = "none"
		authHint = "服务端未启用鉴权"
	}

	p := uiStatePayload{
		Local: []endpointInfo{
			{
				Scope: "local", Label: "本机回环 (127.0.0.1)", Transport: "streamable-http",
				URL:      fmt.Sprintf("http://%s:%d/mcp", localHost, port),
				AuthType: auth, AuthHint: authHint, Available: true,
			},
			{
				Scope: "local", Label: "本机回环 (127.0.0.1)", Transport: "sse",
				URL:      fmt.Sprintf("http://%s:%d/sse", localHost, port),
				AuthType: auth, AuthHint: authHint, Available: true,
			},
		},
		LAN:    []endpointInfo{},
		Public: []endpointInfo{},
	}

	for _, n := range netIfaces() {
		for _, ip := range n.IPs {
			note := ""
			if !lanEnabled {
				note = "局域网访问未开启：请在「运行配置」中打开「局域网访问」开关（将监听 0.0.0.0）"
			}
			p.LAN = append(p.LAN,
				endpointInfo{
					Scope: "lan", Label: fmt.Sprintf("%s · %s (%s)", ifaceKindLabel(n.Kind), n.Name, ip),
					Transport: "streamable-http", URL: fmt.Sprintf("http://%s:%d/mcp", ip, port),
					AuthType: auth, AuthHint: authHint, Available: lanEnabled, Note: note, Iface: n.Name,
				},
				endpointInfo{
					Scope: "lan", Label: fmt.Sprintf("%s · %s (%s)", ifaceKindLabel(n.Kind), n.Name, ip),
					Transport: "sse", URL: fmt.Sprintf("http://%s:%d/sse", ip, port),
					AuthType: auth, AuthHint: authHint, Available: lanEnabled, Note: note, Iface: n.Name,
				},
			)
		}
	}

	pub := publicMCPURL(cfg)
	if pub == "" {
		p.Public = append(p.Public, endpointInfo{
			Scope: "public", Label: "公网隧道", Transport: "streamable-http",
			AuthType: "client-token", AuthHint: "Authorization: Bearer <服务端下发的 clientToken>",
			Available: false, Note: "未配置隧道服务器：请在内网穿透面板填写服务器地址、设备名与 Token",
		})
	} else {
		connected := false
		if m, err := loadTunnelMetrics(); err == nil {
			connected = m.Connected
		}
		note := ""
		if !cfg.Tunnel.Enabled {
			note = "隧道未启用：打开「启用隧道」开关后保存"
		} else if !connected {
			note = "隧道未连接：设备正在自动重连（可查看隧道面板的实时状态与重连次数）"
		}
		p.Public = append(p.Public, endpointInfo{
			Scope: "public", Label: "公网隧道 (任意网络可达)", Transport: "streamable-http",
			URL: pub, AuthType: "client-token", AuthHint: "Authorization: Bearer <服务端下发的 clientToken>",
			Available: connected, Note: note,
		})
	}
	return p
}

func ifaceKindLabel(kind string) string {
	switch kind {
	case "wifi":
		return "WiFi"
	case "cellular":
		return "移动数据"
	case "ethernet":
		return "有线/USB"
	case "vpn":
		return "VPN"
	default:
		return "其他"
	}
}

// ---------- HTTP 只读控制 API ----------

func (s *MCPServer) apiState(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ksu-mcpd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, buildUIState(s.cfg))
}

// apiLogs 返回指定日志源的尾部内容
//
// 参数：source=mcpd|tunnel|watchdog（默认 mcpd）、n=行数（默认 200，上限 2000）
func (s *MCPServer) apiLogs(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ksu-mcpd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "mcpd"
	}
	n := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 {
		n = v
	}
	if n > 2000 {
		n = 2000
	}
	lines, errStr := tailLog(source, n)
	writeJSON(w, map[string]any{
		"source": source,
		"lines":  lines,
		"count":  len(lines),
		"error":  nilIfEmpty(errStr),
	})
}

// apiTools 返回已注册工具的完整定义（供 WebUI 工具面板渲染）
func (s *MCPServer) apiTools(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ksu-mcpd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	type toolItem struct {
		Name        string   `json:"name"`
		Title       string   `json:"title,omitempty"`
		Description string   `json:"description"`
		Deprecated  bool     `json:"deprecated"`
		ReplacedBy  string   `json:"replaced_by,omitempty"`
		Params      []string `json:"params"`
		Required    []string `json:"required"`
	}
	items := []toolItem{}
	for _, e := range toolRegistry() {
		params := []string{}
		required := []string{}
		if props, ok := e.def.InputSchema["properties"].(map[string]any); ok {
			for k := range props {
				params = append(params, k)
			}
			sort.Strings(params)
		}
		if req, ok := e.def.InputSchema["required"].([]string); ok {
			required = req
		}
		items = append(items, toolItem{
			Name: e.def.Name, Title: e.def.Title, Description: e.def.Description,
			Deprecated: e.deprecated, ReplacedBy: e.replacedBy,
			Params: params, Required: required,
		})
	}
	writeJSON(w, map[string]any{
		"total":      len(items),
		"canonical":  len(canonicalToolNames()),
		"deprecated": len(items) - len(canonicalToolNames()),
		"tools":      items,
	})
}

// tailLog 读取日志文件尾部若干行
func tailLog(source string, n int) ([]string, string) {
	path := logFileFor(source)
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{}, fmt.Sprintf("读取 %s 失败: %v", path, err)
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) == 1 && all[0] == "" {
		return []string{}, ""
	}
	start := 0
	if len(all) > n {
		start = len(all) - n
	}
	return all[start:], ""
}

// ---------- CLI：ui-state / ui-bootstrap / tools ----------

func cmdUIState() int {
	b, _ := json.MarshalIndent(buildUIState(nil), "", "  ")
	fmt.Println(string(b))
	return 0
}

// cmdUIBootstrap 输出 WebUI 引导所需的最小信息。
//
// WebUI 需要先拿到本地 Token 才能调用 /api/state（鉴权要求 Token，
// 而 Token 又不能对未鉴权请求开放），因此由 ksu 桥调用本命令做一次引导，
// 之后所有高频读取都改走 HTTP，避免反复 fork 进程。
func cmdUIBootstrap() int {
	cfg := liveConfig()
	running := false
	if _, ok := readPid(); ok {
		running = true
	}
	out := map[string]any{
		"version":          appVersion,
		"protocol_version": protocolVersion,
		"running":          running,
		"port":             cfg.Port,
		"bind":             cfg.Bind,
		"token":            cfg.Token,
		"has_token":        cfg.Token != "",
		"api_base":         fmt.Sprintf("http://127.0.0.1:%d", cfg.Port),
		"api_state":        fmt.Sprintf("http://127.0.0.1:%d/api/state", cfg.Port),
		"bin":              "/data/adb/modules/ksu_mcp/bin/mcpd",
		"data_dir":         dataDir(),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}

func cmdTools(args []string) int {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		}
	}
	if asJSON {
		names := toolNames()
		b, _ := json.MarshalIndent(map[string]any{
			"total":      len(names),
			"canonical":  len(canonicalToolNames()),
			"deprecated": len(names) - len(canonicalToolNames()),
			"names":      names,
		}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	for _, e := range toolRegistry() {
		mark := "  "
		if e.deprecated {
			mark = "× "
		}
		first := e.def.Description
		if i := strings.IndexByte(first, '\n'); i >= 0 {
			first = first[:i]
		}
		fmt.Printf("%s%-30s %s\n", mark, e.def.Name, first)
	}
	fmt.Printf("\n共 %d 个工具（规范命名 %d 个，弃用别名 %d 个）\n",
		len(toolNames()), len(canonicalToolNames()), len(toolNames())-len(canonicalToolNames()))
	return 0
}
