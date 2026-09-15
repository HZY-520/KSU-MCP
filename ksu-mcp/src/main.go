// ksu-mcpd — 嵌入 Android root 设备的 MCP (Model Context Protocol) 服务端
// 传输方式：stdio（默认，供本地 MCP 客户端/终端使用）
//
//	SSE（HTTP，供 Cherry Studio 等远程客户端使用，默认 127.0.0.1:9123，Bearer Token 鉴权）
//
// 纯标准库实现，无外部依赖，静态编译，适合作为 KernelSU / Magisk 模块内嵌二进制。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	appVersion      = "1.0.0"
	serverName      = "ksu-mcpd"
	protocolVersion = "2025-03-26"

	defaultDataDir = "/data/adb/ksu_mcp"
	defaultPort    = 9123
	defaultBind    = "127.0.0.1"
	defaultTimeout = 30 // exec_command 默认超时（秒）
	maxBodyBytes   = 16 << 20
	maxFileRead    = 4 << 20
	maxReadLine    = 16 << 20

	defaultTunnelServer = "wss://n.huziyang.top/tunnel"
	defaultTunnelDevice = "android-device"
	tunnelReqTimeout    = 90 // 隧道转发本地请求超时（秒）
)

// ---------- 路径 ----------

func dataDir() string {
	if p := os.Getenv("MCPD_DATA"); p != "" {
		return p
	}
	return defaultDataDir
}

func configPath() string {
	if p := os.Getenv("MCPD_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(dataDir(), "config.json")
}

func pidPath() string           { return filepath.Join(dataDir(), "mcpd.pid") }
func logPath() string           { return filepath.Join(dataDir(), "mcpd.log") }
func runtimePath() string       { return filepath.Join(dataDir(), "mcpd.runtime.json") }
func tunnelPidPath() string     { return filepath.Join(dataDir(), "tunnel.pid") }
func tunnelLogPath() string     { return filepath.Join(dataDir(), "tunnel.log") }
func tunnelRuntimePath() string { return filepath.Join(dataDir(), "tunnel.runtime.json") }
func tmpDir() string            { return filepath.Join(dataDir(), "tmp") }

// ---------- 配置 ----------

type TunnelConfig struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"` // wss://n.huziyang.top/tunnel
	Device  string `json:"device"` // 设备唯一标识
	Token   string `json:"token"`  // 隧道接入鉴权 Token
	IP      string `json:"ip"`     // 可选：直连 IP（DNS 解析失败时兜底，TLS 仍校验域名）
}

type Config struct {
	Port          int          `json:"port"`
	Bind          string       `json:"bind"`
	Token         string       `json:"token"`
	ExecTimeout   int          `json:"exec_timeout"`   // 秒
	ExecAllowlist []string     `json:"exec_allowlist"` // 非空时仅允许前缀匹配的命令
	ReadOnly      bool         `json:"read_only"`      // 只读模式：禁用 exec_command / write_file
	Tunnel        TunnelConfig `json:"tunnel"`
}

func defaultConfig() *Config {
	return &Config{
		Port:        defaultPort,
		Bind:        defaultBind,
		ExecTimeout: defaultTimeout,
		Tunnel: TunnelConfig{
			Enabled: false,
			Server:  defaultTunnelServer,
			Device:  defaultTunnelDevice,
		},
	}
}

func loadConfig() (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(configPath())
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return cfg, fmt.Errorf("解析配置失败: %v", err)
		}
	} else if !os.IsNotExist(err) {
		return cfg, err
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		cfg.Port = defaultPort
	}
	if cfg.Bind == "" {
		cfg.Bind = defaultBind
	}
	if cfg.ExecTimeout <= 0 {
		cfg.ExecTimeout = defaultTimeout
	}
	if cfg.Token == "" {
		cfg.Token = genToken()
		_ = saveConfig(cfg)
	}
	return cfg, nil
}

func saveConfig(cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(configPath(), data, 0600)
}

func genToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d%d", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(b)
}

func randID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- 日志 ----------

func logf(format string, args ...any) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(os.Stderr, "%s [%s] %s\n", ts, serverName, fmt.Sprintf(format, args...))
}

// ---------- JSON-RPC / MCP 协议 ----------

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

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ---------- MCP 服务端 ----------

type session struct {
	id     string
	send   chan []byte
	cancel context.CancelFunc
}

type MCPServer struct {
	cfg          *Config
	mu           sync.Mutex
	sessions     map[string]*session // 传统 SSE 会话
	httpSessions map[string]bool     // Streamable HTTP 会话（Mcp-Session-Id）
}

func newMCPServer(cfg *Config) *MCPServer {
	return &MCPServer{
		cfg:          cfg,
		sessions:     map[string]*session{},
		httpSessions: map[string]bool{},
	}
}

func (s *MCPServer) hasHTTPSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.httpSessions[id]
}

func (s *MCPServer) addHTTPSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.httpSessions[id] = true
}

func (s *MCPServer) delHTTPSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.httpSessions, id)
}

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
	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		// 忽略
	default:
		logf("notification: %s", req.Method)
	}
}

func (s *MCPServer) handleRequest(req *RPCRequest) *RPCResponse {
	switch req.Method {
	case "initialize":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{"name": serverName, "version": appVersion},
		}}
	case "ping":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.toolDefs()}}
	case "tools/call":
		return s.handleToolCall(req)
	case "resources/list":
		return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"resources": []any{}}}
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
	_ = json.Unmarshal(req.Params, &params)
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}
	out, isErr := s.callTool(params.Name, args)
	if out == "" {
		out = "null"
	}
	content := []map[string]any{{"type": "text", "text": out}}
	return &RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"content": content, "isError": isErr}}
}

// ---------- 工具定义 ----------

func schema(props map[string]any, required []string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func (s *MCPServer) toolDefs() []ToolDef {
	return []ToolDef{
		{
			Name:        "device_info",
			Description: "获取设备信息：品牌、型号、Android 版本、SDK、内核、SELinux、root 状态等",
			InputSchema: schema(nil, nil),
		},
		{
			Name:        "exec_command",
			Description: "以 root 权限执行 shell 命令（受 read_only 与 exec_allowlist 约束），返回 stdout/stderr/退出码",
			InputSchema: schema(map[string]any{
				"command": strProp("要执行的 shell 命令，例如：ls -l /data"),
				"timeout": intProp("超时秒数，默认 30"),
				"cwd":     strProp("工作目录，可选"),
			}, []string{"command"}),
		},
		{
			Name:        "read_file",
			Description: "读取文件内容（文本自动按 utf8 返回，二进制自动转 base64，也可强制指定编码）",
			InputSchema: schema(map[string]any{
				"path":      strProp("文件绝对路径"),
				"encoding":  strProp("输出编码：utf8（默认）或 base64"),
				"max_bytes": intProp("最多读取字节数，默认 4MB"),
			}, []string{"path"}),
		},
		{
			Name:        "write_file",
			Description: "写入或追加文件内容（read_only 模式下禁用）",
			InputSchema: schema(map[string]any{
				"path":     strProp("文件绝对路径"),
				"content":  strProp("文件内容"),
				"encoding": strProp("内容编码：utf8（默认）或 base64"),
				"append":   boolProp("是否追加，默认 false（覆盖）"),
			}, []string{"path", "content"}),
		},
		{
			Name:        "list_dir",
			Description: "列出目录内容（名称/类型/大小/权限/修改时间）",
			InputSchema: schema(map[string]any{
				"path": strProp("目录绝对路径，默认 /"),
			}, nil),
		},
		{
			Name:        "app_list",
			Description: "列出已安装应用包名，可按关键字过滤",
			InputSchema: schema(map[string]any{
				"contains": strProp("包名过滤关键字，可选"),
			}, nil),
		},
		{
			Name:        "battery_info",
			Description: "获取电池信息：电量、状态、温度、电压等",
			InputSchema: schema(nil, nil),
		},
		{
			Name:        "network_info",
			Description: "获取网络信息：网卡、IP、主机名、WiFi 等",
			InputSchema: schema(nil, nil),
		},
		{
			Name:        "screenshot",
			Description: "截取屏幕，返回 PNG 的 base64 与保存路径",
			InputSchema: schema(nil, nil),
		},
		{
			Name:        "clipboard_get",
			Description: "读取剪贴板文本（Android 13+ 或 root 可用时）",
			InputSchema: schema(nil, nil),
		},
		{
			Name:        "getprop",
			Description: "读取系统属性，不传 key 时返回全部属性",
			InputSchema: schema(map[string]any{
				"key": strProp("属性名，如 ro.product.model；省略则返回全部"),
			}, nil),
		},
	}
}

// ---------- 参数辅助 ----------

func argString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func argBool(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

func errJSON(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

// ---------- 命令执行辅助 ----------

func runCmdOut(name string, args ...string) (string, error) {
	path := name
	if !strings.Contains(name, "/") {
		if p, err := exec.LookPath(name); err == nil {
			path = p
		} else if _, err := os.Stat("/system/bin/" + name); err == nil {
			path = "/system/bin/" + name
		}
	}
	var out bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func getprop(key string) string {
	out, err := runCmdOut("getprop", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func allowlisted(list []string, cmd string) bool {
	for _, p := range list {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return true
		}
	}
	return false
}

// ---------- 工具实现 ----------

func (s *MCPServer) callTool(name string, args map[string]any) (string, bool) {
	switch name {
	case "device_info":
		return toolDeviceInfo(), false
	case "exec_command":
		return toolExecCommand(s.cfg, args)
	case "read_file":
		return toolReadFile(args)
	case "write_file":
		return toolWriteFile(s.cfg, args)
	case "list_dir":
		return toolListDir(args)
	case "app_list":
		return toolAppList(args)
	case "battery_info":
		return toolBatteryInfo()
	case "network_info":
		return toolNetworkInfo()
	case "screenshot":
		return toolScreenshot()
	case "clipboard_get":
		return toolClipboardGet()
	case "getprop":
		return toolGetprop(args)
	default:
		return errJSON("unknown tool: " + name), true
	}
}

func toolDeviceInfo() string {
	kernel := ""
	if b, err := os.ReadFile("/proc/version"); err == nil {
		kernel = strings.TrimSpace(string(b))
	}
	selinux, _ := runCmdOut("getenforce")
	info := map[string]any{
		"brand":       getprop("ro.product.brand"),
		"model":       getprop("ro.product.model"),
		"device":      getprop("ro.product.device"),
		"android":     getprop("ro.build.version.release"),
		"sdk":         getprop("ro.build.version.sdk"),
		"abi":         getprop("ro.product.cpu.abi"),
		"fingerprint": getprop("ro.build.fingerprint"),
		"kernel":      kernel,
		"selinux":     selinux,
		"uid":         os.Getuid(),
		"root":        os.Getuid() == 0,
		"time":        time.Now().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	return string(b)
}

func toolExecCommand(cfg *Config, args map[string]any) (string, bool) {
	if cfg.ReadOnly {
		return errJSON("服务器处于只读模式（read_only=true），已禁用 exec_command"), true
	}
	cmdStr := argString(args, "command")
	if cmdStr == "" {
		return errJSON("command 不能为空"), true
	}
	if len(cfg.ExecAllowlist) > 0 && !allowlisted(cfg.ExecAllowlist, cmdStr) {
		return errJSON(fmt.Sprintf("命令不在允许列表内: %s", cmdStr)), true
	}
	timeout := cfg.ExecTimeout
	if v := argInt(args, "timeout"); v > 0 {
		timeout = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	shell := "/system/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		shell = "sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", cmdStr)
	if cwd := argString(args, "cwd"); cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	exitCode := 0
	timedOut := false
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			timedOut = true
			exitCode = 124
		} else {
			exitCode = -1
		}
	}
	res := map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": exitCode,
		"timed_out": timedOut,
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	return string(b), false
}

func toolReadFile(args map[string]any) (string, bool) {
	path := argString(args, "path")
	if path == "" {
		return errJSON("path 不能为空"), true
	}
	encoding := argString(args, "encoding")
	if encoding == "" {
		encoding = "utf8"
	}
	maxBytes := argInt(args, "max_bytes")
	if maxBytes <= 0 {
		maxBytes = maxFileRead
	}
	fi, err := os.Stat(path)
	if err != nil {
		return errJSON(err.Error()), true
	}
	if fi.IsDir() {
		return errJSON("目标是一个目录"), true
	}
	f, err := os.Open(path)
	if err != nil {
		return errJSON(err.Error()), true
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return errJSON(err.Error()), true
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	out := map[string]any{"path": path, "size": fi.Size(), "truncated": truncated, "encoding": encoding}
	if encoding == "base64" {
		out["content"] = base64.StdEncoding.EncodeToString(data)
	} else if utf8.Valid(data) {
		out["content"] = string(data)
		out["encoding"] = "utf8"
	} else {
		out["content"] = base64.StdEncoding.EncodeToString(data)
		out["encoding"] = "base64"
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), false
}

func toolWriteFile(cfg *Config, args map[string]any) (string, bool) {
	if cfg.ReadOnly {
		return errJSON("服务器处于只读模式（read_only=true），已禁用 write_file"), true
	}
	path := argString(args, "path")
	content := argString(args, "content")
	if path == "" {
		return errJSON("path 不能为空"), true
	}
	encoding := argString(args, "encoding")
	appendMode := argBool(args, "append")

	var data []byte
	if encoding == "base64" {
		d, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return errJSON("base64 解码失败: " + err.Error()), true
		}
		data = d
	} else {
		data = []byte(content)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return errJSON(err.Error()), true
	}
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0644)
	if err != nil {
		return errJSON(err.Error()), true
	}
	n, err := f.Write(data)
	f.Close()
	if err != nil {
		return errJSON(err.Error()), true
	}
	b, _ := json.Marshal(map[string]any{"path": path, "written": n, "append": appendMode})
	return string(b), false
}

func toolListDir(args map[string]any) (string, bool) {
	path := argString(args, "path")
	if path == "" {
		path = "/"
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return errJSON(err.Error()), true
	}
	type entry struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size,omitempty"`
		Mode string `json:"mode"`
	}
	list := make([]entry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		et := "file"
		switch {
		case e.IsDir():
			et = "dir"
		case info.Mode()&os.ModeSymlink != 0:
			et = "symlink"
		}
		list = append(list, entry{Name: e.Name(), Type: et, Size: info.Size(), Mode: info.Mode().String()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, _ := json.MarshalIndent(map[string]any{"path": path, "count": len(list), "entries": list}, "", "  ")
	return string(b), false
}

func toolAppList(args map[string]any) (string, bool) {
	out, err := runCmdOut("pm", "list", "packages")
	if err != nil {
		return errJSON("pm 不可用: " + err.Error()), true
	}
	contains := argString(args, "contains")
	var apps []string
	for _, line := range strings.Split(out, "\n") {
		pkg := strings.TrimPrefix(strings.TrimSpace(line), "package:")
		if pkg == "" {
			continue
		}
		if contains != "" && !strings.Contains(pkg, contains) {
			continue
		}
		apps = append(apps, pkg)
	}
	sort.Strings(apps)
	b, _ := json.MarshalIndent(map[string]any{"count": len(apps), "packages": apps}, "", "  ")
	return string(b), false
}

func toolBatteryInfo() (string, bool) {
	base := "/sys/class/power_supply/battery"
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join(base, f))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	tempRaw := read("temp")
	tempC := ""
	if v, err := strconv.Atoi(tempRaw); err == nil {
		tempC = fmt.Sprintf("%.1f", float64(v)/10.0)
	}
	volRaw := read("voltage_now")
	volV := ""
	if v, err := strconv.Atoi(volRaw); err == nil {
		volV = fmt.Sprintf("%.3f", float64(v)/1e6)
	}
	info := map[string]any{
		"capacity":       read("capacity"),
		"status":         read("status"),
		"health":         read("health"),
		"temp_raw":       tempRaw,
		"temp_celsius":   tempC,
		"voltage_raw_uV": volRaw,
		"voltage_V":      volV,
		"technology":     read("technology"),
	}
	if info["capacity"] == "" {
		if out, err := runCmdOut("dumpsys", "battery"); err == nil {
			info["dumpsys"] = out
		}
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	return string(b), false
}

func toolNetworkInfo() (string, bool) {
	interfaces := []map[string]any{}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, ifc := range ifaces {
			if ifc.Flags&net.FlagUp == 0 {
				continue
			}
			addrs := []string{}
			if as, err := ifc.Addrs(); err == nil {
				for _, a := range as {
					addrs = append(addrs, a.String())
				}
			}
			interfaces = append(interfaces, map[string]any{
				"name":    ifc.Name,
				"mac":     ifc.HardwareAddr.String(),
				"addrs":   addrs,
				"mtu":     ifc.MTU,
				"looping": ifc.Flags&net.FlagLoopback != 0,
			})
		}
	}
	hostname, _ := os.Hostname()
	info := map[string]any{
		"hostname":     hostname,
		"interfaces":   interfaces,
		"wifi_ssid":    getprop("dhcp.wlan0.ssid"),
		"wifi_gateway": getprop("dhcp.wlan0.gateway"),
		"dns1":         getprop("net.dns1"),
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	return string(b), false
}

func toolScreenshot() (string, bool) {
	if err := os.MkdirAll(tmpDir(), 0755); err != nil {
		return errJSON(err.Error()), true
	}
	p := filepath.Join(tmpDir(), fmt.Sprintf("screen_%d.png", time.Now().Unix()))
	out, err := runCmdOut("screencap", "-p", p)
	if err != nil {
		return errJSON("screencap 失败: " + out), true
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return errJSON(err.Error()), true
	}
	b, _ := json.MarshalIndent(map[string]any{
		"path":        p,
		"size":        len(data),
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, "", "  ")
	return string(b), false
}

func toolClipboardGet() (string, bool) {
	out, err := runCmdOut("cmd", "clipboard", "get-text")
	if err != nil {
		return errJSON("剪贴板不可用: " + err.Error()), true
	}
	b, _ := json.Marshal(map[string]any{"text": out})
	return string(b), false
}

func toolGetprop(args map[string]any) (string, bool) {
	key := argString(args, "key")
	if key != "" {
		b, _ := json.Marshal(map[string]any{"key": key, "value": getprop(key)})
		return string(b), false
	}
	out, err := runCmdOut("getprop")
	if err != nil {
		return errJSON(err.Error()), true
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		rest := line[1:]
		idx := strings.Index(rest, "]")
		if idx < 0 {
			continue
		}
		k := rest[:idx]
		v := ""
		after := rest[idx+1:]
		if i := strings.Index(after, "["); i >= 0 && strings.HasSuffix(after, "]") {
			v = after[i+1 : len(after)-1]
		}
		props[k] = v
	}
	b, _ := json.MarshalIndent(map[string]any{"count": len(props), "props": props}, "", "  ")
	return string(b), false
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

// ---------- HTTP 传输（Streamable HTTP + 传统 SSE） ----------

func (s *MCPServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.sseHandler) // 传统 SSE（旧客户端兼容）
	mux.HandleFunc("/mcp", s.mcpHandler) // Streamable HTTP（新协议，POST/GET/DELETE）
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok %s\n", appVersion)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ksu-mcpd %s — Streamable HTTP endpoint: /mcp, legacy SSE: /sse, health: /health\n", appVersion)
	})
	return mux
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
		resp := rpcError(nil, -32700, "Parse error", nil)
		writeJSON(w, resp)
		return
	}

	// 会话校验：已携带的会话必须有效；初始化时创建会话
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
		sid = randID()
		s.addHTTPSession(sid)
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
		resp := rpcError(nil, -32700, "Parse error", nil)
		writeJSON(w, resp)
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

	ctx := r.Context()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
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

// ---------- 守护进程 ----------

func readPidFile(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

func readPid() (int, bool) { return readPidFile(pidPath()) }

func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func cmdDaemon(args []string) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		return 1
	}

	detached := false
	var pass []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--detach":
			detached = true
		case "--port":
			if i+1 < len(args) {
				if v, e := strconv.Atoi(args[i+1]); e == nil && v > 0 && v <= 65535 {
					cfg.Port = v
					pass = append(pass, "--port", args[i+1])
				}
				i++
			}
		case "--bind":
			if i+1 < len(args) {
				cfg.Bind = args[i+1]
				pass = append(pass, "--bind", args[i+1])
				i++
			}
		case "--token":
			if i+1 < len(args) {
				cfg.Token = args[i+1]
				pass = append(pass, "--token", args[i+1])
				i++
			}
		case "--no-auth":
			cfg.Token = ""
			pass = append(pass, "--no-auth")
		}
	}

	// 安全约束：监听非回环地址（局域网/公网）时不允许关闭鉴权
	if !isLoopback(cfg.Bind) && cfg.Token == "" {
		fmt.Fprintf(os.Stderr, "错误：监听非回环地址时必须启用 Token 鉴权（请用 --token 指定，或先执行 mcpd token 获取）\n")
		return 1
	}

	if pid, alive := readPid(); alive {
		fmt.Fprintf(os.Stderr, "mcpd 已在运行 (pid %d)\n", pid)
		return 1
	}

	// 守护化：以 setsid 方式重新拉起自身后立即退出
	if detached && os.Getenv("MCPD_DETACHED") != "1" {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法定位自身路径: %v\n", err)
			return 1
		}
		if err := os.MkdirAll(dataDir(), 0755); err != nil {
			fmt.Fprintf(os.Stderr, "无法创建数据目录: %v\n", err)
			return 1
		}
		logF, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法打开日志文件: %v\n", err)
			return 1
		}
		defer logF.Close()
		child := exec.Command(self, append([]string{"daemon"}, pass...)...)
		child.Stdout = logF
		child.Stderr = logF
		child.Stdin = nil
		child.Env = append(os.Environ(), "MCPD_DETACHED=1")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "启动守护进程失败: %v\n", err)
			return 1
		}
		fmt.Printf("mcpd daemon 已启动 (pid %d)，日志: %s\n", child.Process.Pid, logPath())
		return 0
	}

	if err := os.MkdirAll(dataDir(), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "无法创建数据目录: %v\n", err)
		return 1
	}
	if err := os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "无法写入 pid 文件: %v\n", err)
		return 1
	}
	defer os.Remove(pidPath())
	// 记录实际生效配置，供 status 命令读取真实监听参数
	if data, err := json.Marshal(cfg); err == nil {
		_ = os.WriteFile(runtimePath(), data, 0600)
	}
	defer os.Remove(runtimePath())

	srv := newMCPServer(cfg)
	addr := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	logf("MCP server 启动: http://%s/mcp (Streamable HTTP) / http://%s/sse (legacy) (pid %d)", addr, addr, os.Getpid())
	if cfg.Token == "" {
		logf("警告: 当前无鉴权 Token（--no-auth），请勿暴露端口")
	}
	if !isLoopback(cfg.Bind) {
		logf("警告: 正在监听 %s（局域网可访问），请确保 Token 强度并定期轮换", cfg.Bind)
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.mux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	err = httpSrv.ListenAndServe()
	logf("服务已停止: %v", err)
	return 0
}

func cmdStop() int {
	pid, alive := readPid()
	if !alive {
		fmt.Println("mcpd 未在运行")
		return 0
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "停止失败: %v\n", err)
		return 1
	}
	for i := 0; i < 50; i++ {
		if _, ok := readPid(); !ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(pidPath())
	fmt.Printf("mcpd 已停止 (pid %d)\n", pid)
	return 0
}

func cmdStatus() int {
	running := false
	pid := 0
	if p, ok := readPid(); ok {
		running = true
		pid = p
	}
	cfg, _ := loadConfig()
	// 运行中时优先读取守护进程写入的实际生效配置
	if running {
		if data, err := os.ReadFile(runtimePath()); err == nil {
			var live Config
			if json.Unmarshal(data, &live) == nil {
				cfg = &live
			}
		}
	}
	out := map[string]any{
		"running":      running,
		"pid":          pid,
		"version":      appVersion,
		"config":       configPath(),
		"port":         cfg.Port,
		"bind":         cfg.Bind,
		"token":        cfg.Token,
		"log":          logPath(),
		"read_only":    cfg.ReadOnly,
		"exec_timeout": cfg.ExecTimeout,
		"allowlist":    cfg.ExecAllowlist,
		"tunnel": map[string]any{
			"enabled": cfg.Tunnel.Enabled,
			"server":  cfg.Tunnel.Server,
			"device":  cfg.Tunnel.Device,
		},
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}

// ---------- 内网穿透客户端 ----------

type tunnelRequestMsg struct {
	Type    string            `json:"type"`
	ID      int64             `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64
}

type tunnelResponseMsg struct {
	Type    string            `json:"type"`
	ID      int64             `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64
}

func cmdTunnel(args []string) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		return 1
	}
	tc := cfg.Tunnel

	detached := false
	var pass []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--detach":
			detached = true
		case "--server":
			if i+1 < len(args) {
				tc.Server = args[i+1]
				pass = append(pass, "--server", args[i+1])
				i++
			}
		case "--device":
			if i+1 < len(args) {
				tc.Device = args[i+1]
				pass = append(pass, "--device", args[i+1])
				i++
			}
		case "--token":
			if i+1 < len(args) {
				tc.Token = args[i+1]
				pass = append(pass, "--token", args[i+1])
				i++
			}
		case "--ip":
			if i+1 < len(args) {
				tc.IP = args[i+1]
				pass = append(pass, "--ip", args[i+1])
				i++
			}
		}
	}

	if tc.Server == "" || tc.Device == "" || tc.Token == "" {
		fmt.Fprintf(os.Stderr, "隧道配置不完整：需要 server / device / token\n（可通过 CLI 参数或 config.json 的 tunnel 段配置）\n")
		return 1
	}

	if pid, alive := readPidFile(tunnelPidPath()); alive {
		fmt.Fprintf(os.Stderr, "tunnel 已在运行 (pid %d)\n", pid)
		return 1
	}

	// 守护化
	if detached && os.Getenv("MCPD_TUNNEL_DETACHED") != "1" {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法定位自身路径: %v\n", err)
			return 1
		}
		if err := os.MkdirAll(dataDir(), 0755); err != nil {
			fmt.Fprintf(os.Stderr, "无法创建数据目录: %v\n", err)
			return 1
		}
		logF, err := os.OpenFile(tunnelLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法打开日志文件: %v\n", err)
			return 1
		}
		defer logF.Close()
		child := exec.Command(self, append([]string{"tunnel"}, pass...)...)
		child.Stdout = logF
		child.Stderr = logF
		child.Stdin = nil
		child.Env = append(os.Environ(), "MCPD_TUNNEL_DETACHED=1")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "启动 tunnel 失败: %v\n", err)
			return 1
		}
		fmt.Printf("tunnel 已启动 (pid %d)，日志: %s\n", child.Process.Pid, tunnelLogPath())
		return 0
	}

	if err := os.MkdirAll(dataDir(), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "无法创建数据目录: %v\n", err)
		return 1
	}
	if err := os.WriteFile(tunnelPidPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "无法写入 pid 文件: %v\n", err)
		return 1
	}
	defer os.Remove(tunnelPidPath())
	_ = os.WriteFile(tunnelRuntimePath(), []byte(fmt.Sprintf(`{"server":%q,"device":%q,"ip":%q}`, tc.Server, tc.Device, tc.IP)), 0644)
	defer os.Remove(tunnelRuntimePath())

	logf("tunnel 客户端启动: server=%s device=%s ip=%s (pid %d)", tc.Server, tc.Device, tc.IP, os.Getpid())
	// 优雅退出：收到 SIGTERM/SIGINT 时关闭活动连接并退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	stopCh := make(chan struct{})
	var connMu sync.Mutex
	var active *websocket.Conn
	go func() {
		<-sigCh
		logf("tunnel 收到退出信号，正在停止")
		connMu.Lock()
		if active != nil {
			_ = active.Close() // 使 serveTunnel 的 ReadMessage 立即返回
		}
		connMu.Unlock()
		close(stopCh)
	}()
	backoff := 2
	for {
		select {
		case <-stopCh:
			os.Exit(0)
		default:
		}
		conn, err := dialTunnel(tc)
		if err != nil {
			logf("连接 %s 失败: %v（%ds 后重试）", tc.Server, err, backoff)
			select {
			case <-stopCh:
				os.Exit(0)
			case <-time.After(time.Duration(backoff) * time.Second):
			}
			if backoff < 30 {
				backoff *= 2
			}
			continue
		}
		connMu.Lock()
		active = conn
		connMu.Unlock()
		backoff = 2
		err = serveTunnel(conn, cfg, tc)
		connMu.Lock()
		active = nil
		connMu.Unlock()
		_ = conn.Close()
		logf("隧道连接断开: %v（2s 后重连）", err)
		select {
		case <-stopCh:
			os.Exit(0)
		case <-time.After(2 * time.Second):
		}
	}
}

func dialTunnel(tc TunnelConfig) (*websocket.Conn, error) {
	u, err := url.Parse(tc.Server)
	if err != nil {
		return nil, fmt.Errorf("server URL 无效: %v", err)
	}
	q := u.Query()
	q.Set("device", tc.Device)
	q.Set("token", tc.Token)
	u.RawQuery = q.Encode()
	// Android CA 池：GOOS=linux 的二进制在 Android 上读不到 /etc/ssl/certs，
	// 需手动加载 /system/etc/security/cacerts 等系统 CA 目录，否则 TLS 校验必失败
	rootCAs := androidCACertPool()
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, TLSClientConfig: &tls.Config{RootCAs: rootCAs}}
	// DNS 兜底：配置了 tunnel.ip 时直连该 IP（TLS ServerName/SNI 仍用域名校验证书）
	if ip := strings.TrimSpace(tc.IP); ip != "" {
		dialer.NetDial = func(network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				port = "443"
			}
			raddr := net.JoinHostPort(ip, port)
			c, err := net.DialTimeout(network, raddr, 10*time.Second)
			if err != nil {
				return nil, err
			}
			logf("DNS 兜底直连: %s -> %s", addr, raddr)
			return c, nil
		}
	}
	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return nil, err
	}
	logf("隧道已连接: %s (device=%s)", tc.Server, tc.Device)
	return conn, nil
}

// androidCACertPool 构建用于 Android 系统的 CA 证书池：
// 优先系统池（Linux/桌面环境），并额外加载 Android 各 CA 目录（.0 哈希文件 / .pem / .crt）。
func androidCACertPool() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	dirs := []string{
		"/system/etc/security/cacerts",          // Android 9-13 系统 CA
		"/apex/com.android.conscrypt/cacerts",   // Android 14+（Conscrypt APEX）
		"/data/misc/keychain/cacerts-added",     // 用户手动安装的 CA
	}
	loaded := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".0") && !strings.HasSuffix(name, ".pem") && !strings.HasSuffix(name, ".crt") && !strings.HasSuffix(name, ".cer") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			if pool.AppendCertsFromPEM(data) {
				loaded++
			}
		}
	}
	if loaded > 0 {
		logf("已加载 Android 系统 CA: %d 个", loaded)
	}
	return pool
}

func serveTunnel(conn *websocket.Conn, cfg *Config, tc TunnelConfig) error {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var msg tunnelRequestMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Type != "request" {
			continue
		}
		go handleTunnelRequest(conn, cfg, &msg)
	}
}

func handleTunnelRequest(conn *websocket.Conn, cfg *Config, msg *tunnelRequestMsg) {
	resp := tunnelResponseMsg{Type: "response", ID: msg.ID, Status: 502, Headers: map[string]string{}, Body: ""}

	body, err := base64.StdEncoding.DecodeString(msg.Body)
	if err != nil {
		body = []byte(msg.Body)
	}
	path := msg.Path
	if path == "" {
		path = "/mcp"
	}
	localURL := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Port, path)
	req, err := http.NewRequest(msg.Method, localURL, bytes.NewReader(body))
	if err != nil {
		resp.Body = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"device request error"}}`))
		_ = conn.WriteJSON(resp)
		return
	}
	for k, v := range msg.Headers {
		if k == "authorization" || k == "host" {
			continue
		}
		req.Header.Set(k, v)
	}
	// 本地 MCP 服务鉴权：始终注入本地 Token
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	client := &http.Client{Timeout: tunnelReqTimeout * time.Second}
	httpResp, err := client.Do(req)
	if err != nil {
		resp.Body = base64.StdEncoding.EncodeToString([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"device local server unavailable"}}`))
		_ = conn.WriteJSON(resp)
		return
	}
	defer httpResp.Body.Close()
	rbody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyBytes))
	if err != nil {
		rbody = []byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"device local read error"}}`)
	}
	resp.Status = httpResp.StatusCode
	if ct := httpResp.Header.Get("Content-Type"); ct != "" {
		resp.Headers["Content-Type"] = ct
	}
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		resp.Headers["Mcp-Session-Id"] = sid
	}
	resp.Body = base64.StdEncoding.EncodeToString(rbody)
	_ = conn.WriteJSON(resp)
}

func cmdTunnelStop() int {
	pid, alive := readPidFile(tunnelPidPath())
	if !alive {
		fmt.Println("tunnel 未在运行")
		return 0
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "停止失败: %v\n", err)
		return 1
	}
	// 宽限 3 秒，仍未退出则升级 SIGKILL
	for i := 0; i < 30; i++ {
		if _, ok := readPidFile(tunnelPidPath()); !ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, ok := readPidFile(tunnelPidPath()); ok {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(200 * time.Millisecond)
	}
	_ = os.Remove(tunnelPidPath())
	_ = os.Remove(tunnelRuntimePath())
	fmt.Printf("tunnel 已停止 (pid %d)\n", pid)
	return 0
}

func cmdTunnelStatus() int {
	running := false
	pid := 0
	server := ""
	device := ""
	if p, ok := readPidFile(tunnelPidPath()); ok {
		running = true
		pid = p
		if data, err := os.ReadFile(tunnelRuntimePath()); err == nil {
			var live struct {
				Server string `json:"server"`
				Device string `json:"device"`
			}
			if json.Unmarshal(data, &live) == nil {
				server = live.Server
				device = live.Device
			}
		}
	}
	cfg, _ := loadConfig()
	if server == "" {
		server = cfg.Tunnel.Server
	}
	if device == "" {
		device = cfg.Tunnel.Device
	}
	out := map[string]any{
		"running": running,
		"pid":     pid,
		"enabled": cfg.Tunnel.Enabled,
		"server":  server,
		"device":  device,
		"token":   cfg.Tunnel.Token != "",
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}

func cmdToken(regen bool) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		return 1
	}
	if regen {
		cfg.Token = genToken()
		if err := saveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "保存失败: %v\n", err)
			return 1
		}
	}
	fmt.Println(cfg.Token)
	return 0
}

func cmdLogs(args []string) int {
	lines := 100
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			lines = v
		}
	}
	data, err := os.ReadFile(logPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "暂无日志")
		return 0
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	start := 0
	if len(all) > lines {
		start = len(all) - lines
	}
	fmt.Println(strings.Join(all[start:], "\n"))
	return 0
}

func cmdInitConfig() int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败: %v\n", err)
		return 1
	}
	fmt.Printf("config: %s\nport: %d\nbind: %s\ntoken: %s\n", configPath(), cfg.Port, cfg.Bind, cfg.Token)
	return 0
}

func cmdConfigSet(args []string) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		return 1
	}
	changed := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				if v, e := strconv.Atoi(args[i+1]); e == nil && v > 0 && v <= 65535 {
					cfg.Port = v
					changed = true
				}
				i++
			}
		case "--bind":
			if i+1 < len(args) {
				cfg.Bind = args[i+1]
				changed = true
				i++
			}
		case "--exec-timeout":
			if i+1 < len(args) {
				if v, e := strconv.Atoi(args[i+1]); e == nil && v > 0 {
					cfg.ExecTimeout = v
					changed = true
				}
				i++
			}
		case "--read-only":
			if i+1 < len(args) {
				cfg.ReadOnly = args[i+1] == "true" || args[i+1] == "1"
				changed = true
				i++
			}
		case "--allowlist-clear":
			cfg.ExecAllowlist = nil
			changed = true
		case "--allowlist-add":
			if i+1 < len(args) {
				cfg.ExecAllowlist = append(cfg.ExecAllowlist, args[i+1])
				changed = true
				i++
			}
		case "--lan":
			if i+1 < len(args) {
				if args[i+1] == "true" || args[i+1] == "1" {
					cfg.Bind = "0.0.0.0"
				} else {
					cfg.Bind = "127.0.0.1"
				}
				changed = true
				i++
			}
		case "--tunnel-enable":
			if i+1 < len(args) {
				cfg.Tunnel.Enabled = args[i+1] == "true" || args[i+1] == "1"
				changed = true
				i++
			}
		case "--tunnel-server":
			if i+1 < len(args) {
				cfg.Tunnel.Server = args[i+1]
				changed = true
				i++
			}
		case "--tunnel-device":
			if i+1 < len(args) {
				cfg.Tunnel.Device = args[i+1]
				changed = true
				i++
			}
		case "--tunnel-token":
			if i+1 < len(args) {
				cfg.Tunnel.Token = args[i+1]
				changed = true
				i++
			}
		case "--tunnel-ip":
			if i+1 < len(args) {
				cfg.Tunnel.IP = args[i+1]
				changed = true
				i++
			}
		}
	}
	if changed {
		if err := saveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "保存失败: %v\n", err)
			return 1
		}
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Println(string(b))
	return 0
}

func usage() {
	fmt.Fprintf(os.Stderr, `%s v%s — Android root 设备 MCP 服务端（Streamable HTTP + SSE + 内网穿透）

用法:
  mcpd                     以 stdio 模式运行 MCP 服务（默认，供本地客户端/终端）
  mcpd daemon [选项]        以 Streamable HTTP + SSE 模式启动服务
       --port N            监听端口（默认 9123）
       --bind ADDR         监听地址（默认 127.0.0.1；局域网请用 0.0.0.0）
       --token X           设置鉴权 Token（默认读取/生成于配置文件）
       --no-auth           关闭鉴权（仅限本机回环调试）
       --detach            守护化（setsid 后台运行，日志写入 mcpd.log）
  mcpd stop                停止 daemon
  mcpd status              查看运行状态（JSON）
  mcpd tunnel [选项]        启动内网穿透客户端（模块为客户端）
       --server URL        隧道服务端（默认 wss://n.huziyang.top/tunnel）
       --device ID         设备唯一标识
       --token X           隧道接入 Token
       --ip IP             可选：直连 IP（DNS 解析失败时兜底，TLS 仍校验域名）
       --detach            守护化（日志写入 tunnel.log）
  mcpd tunnel-stop         停止隧道客户端
  mcpd tunnel-status       查看隧道状态（JSON）
  mcpd token [--regen]     查看 / 重新生成 Token
  mcpd logs [N]            查看最近 N 行日志（默认 100）
  mcpd config-set [选项]    修改配置: --port N --bind ADDR --exec-timeout N
                           --read-only true|false --allowlist-add CMD --allowlist-clear
                           --lan true|false --tunnel-enable true|false
                           --tunnel-server URL --tunnel-device ID --tunnel-token X
                           --tunnel-ip IP
  mcpd init-config         初始化配置文件
  mcpd version             输出版本
`, serverName, appVersion)
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "daemon", "serve":
			os.Exit(cmdDaemon(args[1:]))
		case "stop":
			os.Exit(cmdStop())
		case "status":
			os.Exit(cmdStatus())
		case "tunnel":
			os.Exit(cmdTunnel(args[1:]))
		case "tunnel-stop":
			os.Exit(cmdTunnelStop())
		case "tunnel-status":
			os.Exit(cmdTunnelStatus())
		case "token":
			regen := false
			for _, a := range args[1:] {
				if a == "--regen" {
					regen = true
				}
			}
			os.Exit(cmdToken(regen))
		case "logs":
			os.Exit(cmdLogs(args[1:]))
		case "config-set":
			os.Exit(cmdConfigSet(args[1:]))
		case "init-config":
			os.Exit(cmdInitConfig())
		case "version":
			fmt.Println(appVersion)
			os.Exit(0)
		case "help", "-h", "--help":
			usage()
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "未知命令: %s\n", args[0])
			usage()
			os.Exit(2)
		}
	}
	runStdio()
}
