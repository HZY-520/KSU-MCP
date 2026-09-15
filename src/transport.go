package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// transport.go — MCP 传输层
//
// 支持三种传输：
//   - stdio：单进程读写 JSON-RPC 行（Termux / 本地 MCP 客户端）
//   - Streamable HTTP：/mcp 的 POST（请求）/ GET（服务端消息流）/ DELETE（结束会话）
//   - 传统 SSE：/sse 建立会话，POST /mcp?session_id=... 提交消息
//
// v1.2.0 关键修复：
//   - initialize 做协议版本协商（2025-11-25 → 2024-11-05）
//   - httpSessions 增加 TTL / 容量上限 / 定期回收，修复长期运行的内存泄漏
//   - GET /mcp 长流下发 SSE `retry:` 提示，断流后客户端 3s 自动重连
//   - 工具返回值支持多内容块（text / image）

// ---------- JSON-RPC ----------

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func rpcError(id json.RawMessage, code int, msg string, data any) *RPCResponse {
	return &RPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg, Data: data}}
}

// ---------- MCP 服务端 ----------

type session struct {
	id     string
	send   chan []byte
	cancel context.CancelFunc
}

// httpSession 记录 Streamable HTTP 会话的创建/最近使用时间，用于 TTL 回收
type httpSession struct {
	created  time.Time
	lastSeen time.Time
}

type MCPServer struct {
	cfg          *Config
	mu           sync.Mutex
	sessions     map[string]*session     // 传统 SSE 会话
	httpSessions map[string]*httpSession // Streamable HTTP 会话（Mcp-Session-Id）
}

func newMCPServer(cfg *Config) *MCPServer {
	s := &MCPServer{
		cfg:          cfg,
		sessions:     map[string]*session{},
		httpSessions: map[string]*httpSession{},
	}
	go s.evictLoop()
	return s
}

// hasHTTPSession 校验会话存在并刷新最近使用时间
func (s *MCPServer) hasHTTPSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.httpSessions[id]
	if !ok {
		return false
	}
	if time.Since(sess.lastSeen) > sessionTTL {
		delete(s.httpSessions, id)
		return false
	}
	sess.lastSeen = time.Now()
	return true
}

func (s *MCPServer) addHTTPSession(id string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// 容量保护：超限时先淘汰最久未使用的会话，避免无上限增长
	if len(s.httpSessions) >= sessionMaxCount {
		oldestID := ""
		var oldest time.Time
		for k, v := range s.httpSessions {
			if oldestID == "" || v.lastSeen.Before(oldest) {
				oldestID, oldest = k, v.lastSeen
			}
		}
		if oldestID != "" {
			delete(s.httpSessions, oldestID)
			logf("Streamable HTTP 会话数达上限 %d，淘汰最旧会话 %s", sessionMaxCount, oldestID)
		}
	}
	s.httpSessions[id] = &httpSession{created: now, lastSeen: now}
}

func (s *MCPServer) delHTTPSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.httpSessions, id)
}

// evictLoop 定期回收过期会话（修复 v1.1.0「会话只增不减」的内存泄漏）
func (s *MCPServer) evictLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cut := time.Now().Add(-sessionTTL)
		removed := 0
		s.mu.Lock()
		for k, v := range s.httpSessions {
			if v.lastSeen.Before(cut) {
				delete(s.httpSessions, k)
				removed++
			}
		}
		s.mu.Unlock()
		if removed > 0 {
			logf("回收过期 Streamable HTTP 会话 %d 个", removed)
		}
	}
}

func (s *MCPServer) sessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.httpSessions)
}

// negotiateProtocol 协议版本协商：客户端请求的版本受支持则回显，否则返回服务端最新版本
func negotiateProtocol(requested string) string {
	requested = strings.TrimSpace(requested)
	for _, v := range supportedProtocols {
		if v == requested {
			return requested
		}
	}
	if requested != "" {
		logf("客户端请求的协议版本 %q 不受支持，回退到 %s", requested, protocolVersion)
	}
	return protocolVersion
}

// serverInstructions 面向 AI Agent 的接入说明（initialize 的 instructions 字段）
func serverInstructions(cfg *Config) string {
	ro := "可读写"
	if cfg.ReadOnly {
		ro = "只读模式（exec / write 已禁用）"
	}
	return fmt.Sprintf(
		"这是运行在 Android 设备上的 MCP 服务端（%s v%s），已获得 root 权限，当前为%s。\n"+
			"工具命名规范为 android_{action}_{resource}：\n"+
			"  - android_get_*：只读信息采集（设备/电池/存储/网络/进程/日志/设置读取）\n"+
			"  - android_list_*：列表查询（已安装应用），支持 filter 与分页 offset/limit\n"+
			"  - android_input_*：向设备注入输入（文本/按键/点击/滑动），会真实改变设备状态\n"+
			"  - android_toggle_setting / android_set_clipboard：修改系统设置与剪贴板\n"+
			"  - android_screenshot 返回 MCP image 内容块，可直接查看屏幕\n"+
			"  - android_exec_shell / android_read_file / android_write_file / android_list_dir：完整文件与命令能力\n"+
			"若要操作界面，建议顺序：android_screenshot 观察 → android_input_tap 聚焦 → android_input_text 输入。\n"+
			"v1.1.0 的扁平命名工具（device_info、exec_command、screenshot 等）仍可用但已弃用，请优先使用 android_* 名称。",
		serverName, appVersion, ro)
}

// ---------- 消息处理 ----------

func (s *MCPServer) handleMessage(raw []byte) *RPCResponse {
	var req RPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcError(nil, -32700, "Parse error", nil)
	}
	if req.JSONRPC != "2.0" {
		return rpcError(req.ID, -32600, "Invalid Request", nil)
	}
	if len(req.ID) == 0 {
		s.handleNotification(&req)
		return nil
	}
	return s.handleRequest(&req)
}

func (s *MCPServer) handleNotification(req *RPCRequest) {
	switch req.Method {
	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed",
		"notifications/progress":
		// 忽略
	default:
		logf("notification: %s", req.Method)
	}
}

func (s *MCPServer) handleRequest(req *RPCRequest) *RPCResponse {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		negotiated := negotiateProtocol(p.ProtocolVersion)
		logf("initialize: 客户端协议 %q → 协商结果 %s", p.ProtocolVersion, negotiated)
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": negotiated,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{
				"name":    serverName,
				"title":   "KSU MCP Server (Android root device)",
				"version": appVersion,
			},
			"instructions": serverInstructions(s.cfg),
		}}
	case "ping":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": toolDefs()}}
	case "tools/call":
		return s.handleToolCall(req)
	case "resources/list":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"resources": []any{}}}
	case "resources/templates/list":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"resourceTemplates": []any{}}}
	case "prompts/list":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"prompts": []any{}}}
	default:
		return rpcError(req.ID, -32601, "Method not found: "+req.Method, nil)
	}
}

func (s *MCPServer) handleToolCall(req *RPCRequest) *RPCResponse {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcError(req.ID, -32602, "Invalid params: "+err.Error(), nil)
	}
	if params.Name == "" {
		return rpcError(req.ID, -32602, "Invalid params: missing tool name", nil)
	}
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}
	started := time.Now()
	content, isErr := callTool(s.cfg, params.Name, args)
	if content == nil {
		content = []map[string]any{textContent("null")}
	}
	logf("tools/call %s (%dms, error=%v)", params.Name, time.Since(started).Milliseconds(), isErr)
	return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": content,
		"isError": isErr,
	}}
}

// ---------- stdio 传输 ----------

func runStdio() {
	cfg, err := loadConfig()
	if err != nil {
		logf("配置加载失败: %v", err)
	}
	srv := newMCPServer(cfg)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), maxReadLine)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		resp := srv.handleMessage(line)
		if resp != nil {
			_ = enc.Encode(resp)
		}
	}
}

// ---------- HTTP 传输 ----------

func (s *MCPServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.sseHandler) // 传统 SSE（旧客户端兼容）
	mux.HandleFunc("/mcp", s.mcpHandler) // Streamable HTTP（新协议，POST/GET/DELETE）
	mux.HandleFunc("/api/state", s.withCORS(s.apiState))
	mux.HandleFunc("/api/logs", s.withCORS(s.apiLogs))
	mux.HandleFunc("/api/tools", s.withCORS(s.apiTools))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ok %s\n", appVersion)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ksu-mcpd %s — Streamable HTTP: /mcp, legacy SSE: /sse, health: /health, WebUI API: /api/state\n", appVersion)
	})
	return mux
}

// withCORS 为 WebUI 控制 API 添加 CORS 支持（KernelSU WebUI 从 file:// 源调用本地 127.0.0.1）
//
// 说明：/api/* 只在本地 daemon 上监听，隧道转发已限制为仅 /mcp /sse 路径，
// 因此公网无法触达；CORS 通配不会扩大攻击面（仍需 Bearer Token 才能读取）。
func (s *MCPServer) withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func (s *MCPServer) authOK(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") && strings.TrimPrefix(h, "Bearer ") == s.cfg.Token {
		return true
	}
	if r.URL.Query().Get("token") == s.cfg.Token {
		return true
	}
	return false
}

func (s *MCPServer) mcpHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.mcpPost(w, r)
	case http.MethodGet:
		s.mcpGet(w, r)
	case http.MethodDelete:
		s.mcpDelete(w, r)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// mcpPost — Streamable HTTP 请求入口
func (s *MCPServer) mcpPost(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ksu-mcpd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// 兼容传统 SSE：POST /mcp?session_id=xxx
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		s.legacyMcpPost(w, r, sid)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var req RPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, rpcError(nil, -32700, "Parse error", nil))
		return
	}

	// 会话校验：已携带的会话必须有效；initialize 时创建新会话
	sid := r.Header.Get("Mcp-Session-Id")
	if sid != "" && !s.hasHTTPSession(sid) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(rpcError(req.ID, -32001, "Session not found", nil))
		return
	}

	if len(req.ID) == 0 {
		s.handleNotification(&req)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.handleRequest(&req)
	if req.Method == "initialize" {
		// 复用已有会话：避免客户端重复 initialize 时泄漏会话（v1.1.0 缺陷）
		if sid == "" {
			sid = randID()
			s.addHTTPSession(sid)
		} else {
			s.hasHTTPSession(sid)
		}
		w.Header().Set("Mcp-Session-Id", sid)
	}
	writeJSON(w, resp)
}

// legacyMcpPost — 传统 SSE 客户端的消息提交（保持旧行为）
func (s *MCPServer) legacyMcpPost(w http.ResponseWriter, r *http.Request, sid string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var req RPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, rpcError(nil, -32700, "Parse error", nil))
		return
	}
	if len(req.ID) == 0 {
		s.handleNotification(&req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := s.handleRequest(&req)
	out, _ := json.Marshal(resp)
	s.mu.Lock()
	sess := s.sessions[sid]
	s.mu.Unlock()
	if sess != nil {
		select {
		case sess.send <- out:
		default:
			logf("session %s 发送队列已满", sid)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, resp)
}

// mcpGet — Streamable HTTP 服务端消息流（长连接，无主动消息时保持心跳）
//
// 断流恢复：SSE 规范允许服务端下发 `retry: <ms>`，客户端在连接中断后会按该间隔
// 自动重连并携带同一个 Mcp-Session-Id；设备端会话在 TTL 内保持有效，
// 因此 GET 长流断开后客户端重连即可继续（v1.2.0 新增，v1.1.0 无此提示）。
func (s *MCPServer) mcpGet(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ksu-mcpd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" || !s.hasHTTPSession(sid) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// 断流自动恢复提示
	fmt.Fprintf(w, "retry: %d\n\n", sseRetryHintMs)
	flusher.Flush()

	ctx := r.Context()
	ticker := time.NewTicker(sseKeepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logf("Streamable HTTP 消息流断开: session=%s (客户端将按 retry=%dms 重连)", sid, sseRetryHintMs)
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// mcpDelete — 结束 Streamable HTTP 会话
func (s *MCPServer) mcpDelete(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ksu-mcpd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		s.delHTTPSession(sid)
		logf("Streamable HTTP session closed: %s", sid)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *MCPServer) sseHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ksu-mcpd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sess := &session{id: randID(), send: make(chan []byte, 128), cancel: cancel}
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, sess.id)
		s.mu.Unlock()
	}()

	logf("SSE session opened: %s (remote %s)", sess.id, r.RemoteAddr)
	fmt.Fprintf(w, "retry: %d\n\n", sseRetryHintMs)
	fmt.Fprintf(w, "event: endpoint\ndata: /mcp?session_id=%s\n\n", sess.id)
	flusher.Flush()
	for {
		select {
		case <-ctx.Done():
			logf("SSE session closed: %s", sess.id)
			return
		case msg := <-sess.send:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
