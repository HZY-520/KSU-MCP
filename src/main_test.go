package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// main_test.go — 纯逻辑单元测试
//
// 覆盖：命名规范、协议协商、路径隔离、退避抖动、URL 推导、工具注册表一致性、
// 三网络地址构造、日志尾部读取。运行：go test ./...

// ---------- 工具注册表与命名规范 ----------

// toolNameRe MCP 2025-11-25 允许的字符集与长度：1-128，A-Z a-z 0-9 _ - .
var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// canonicalRe {service}_{action}_{resource} 三段式
var canonicalRe = regexp.MustCompile(`^android_(get|list|exec|input|toggle|set|read|write)_[a-z0-9_]+$`)

// nameExceptions 任务书逐字指定的例外名（仅两段）
var nameExceptions = map[string]bool{"android_screenshot": true}

func TestToolRegistryIntegrity(t *testing.T) {
	entries := toolRegistry()
	if len(entries) < 20 {
		t.Fatalf("工具数量过少: %d", len(entries))
	}
	seen := map[string]bool{}
	canonical := map[string]bool{}
	for _, e := range entries {
		if e.def.Name == "" {
			t.Fatal("存在空工具名")
		}
		if seen[e.def.Name] {
			t.Fatalf("工具名重复: %s", e.def.Name)
		}
		seen[e.def.Name] = true
		if !toolNameRe.MatchString(e.def.Name) {
			t.Errorf("工具名不符合 MCP 命名规范字符集/长度: %q", e.def.Name)
		}
		if e.def.Description == "" {
			t.Errorf("工具 %s 缺少 description（AI Agent 依赖它选型）", e.def.Name)
		}
		if len(e.def.Description) < 80 {
			t.Errorf("工具 %s 的 description 过短（%d 字符），不足以指导 Agent",
				e.def.Name, len(e.def.Description))
		}
		if e.def.InputSchema == nil || e.def.InputSchema["type"] != "object" {
			t.Errorf("工具 %s 的 inputSchema 必须是 type=object 的 JSON Schema", e.def.Name)
		}
		if props, ok := e.def.InputSchema["properties"]; ok && props == nil {
			t.Errorf("工具 %s 的 properties 为 nil", e.def.Name)
		}
		if e.impl == nil {
			t.Errorf("工具 %s 没有实现（摆设工具）", e.def.Name)
		}
		if !e.deprecated {
			if !canonicalRe.MatchString(e.def.Name) && !nameExceptions[e.def.Name] {
				t.Errorf("规范命名工具 %s 不符合 {service}_{action}_{resource}", e.def.Name)
			}
			canonical[e.def.Name] = true
		}
	}
	// 弃用别名必须指向一个真实存在的规范工具
	for _, e := range entries {
		if !e.deprecated {
			continue
		}
		if e.replacedBy == "" {
			t.Errorf("弃用工具 %s 未标注替代工具", e.def.Name)
			continue
		}
		if !canonical[e.replacedBy] {
			t.Errorf("弃用工具 %s 指向的替代工具 %s 不存在", e.def.Name, e.replacedBy)
		}
		if !strings.Contains(e.def.Description, "已弃用") {
			t.Errorf("弃用工具 %s 的 description 未标注弃用", e.def.Name)
		}
	}
	// 任务要求：新增 android_* 工具 ≥5 个
	n := 0
	for k := range canonical {
		if strings.HasPrefix(k, "android_") {
			n++
		}
	}
	if n < 5 {
		t.Errorf("android_* 规范工具数量不足 5 个: %d", n)
	}
	// 任务书点名要求的工具必须存在
	for _, want := range []string{
		"android_get_device_info", "android_get_battery_status", "android_get_storage_info",
		"android_get_network_info", "android_list_packages", "android_get_running_processes",
		"android_get_logcat", "android_screenshot", "android_input_text", "android_toggle_setting",
	} {
		if !canonical[want] {
			t.Errorf("缺少任务书要求的工具: %s", want)
		}
	}
	// 每个 schema 属性都要有 description，枚举要有 enum
	for _, e := range entries {
		props, _ := e.def.InputSchema["properties"].(map[string]any)
		for k, v := range props {
			pm, ok := v.(map[string]any)
			if !ok {
				t.Errorf("%s.%s 不是对象 schema", e.def.Name, k)
				continue
			}
			if _, ok := pm["description"]; !ok {
				t.Errorf("%s.%s 缺少 description", e.def.Name, k)
			}
			if pm["type"] == nil {
				t.Errorf("%s.%s 缺少 type", e.def.Name, k)
			}
		}
	}
}

func TestCallToolUnknown(t *testing.T) {
	out, isErr := callTool(defaultConfig(), "definitely_not_a_tool", map[string]any{})
	if !isErr || len(out) != 1 {
		t.Fatalf("未知工具应返回 isError 且带一条内容，得到 %v %v", out, isErr)
	}
	if !strings.Contains(out[0]["text"].(string), "unknown tool") {
		t.Fatalf("未知工具错误文本不正确: %v", out[0]["text"])
	}
}

func TestReadOnlyDisablesExecAndWrite(t *testing.T) {
	cfg := defaultConfig()
	cfg.ReadOnly = true
	if _, isErr := callTool(cfg, "android_exec_shell", map[string]any{"command": "echo hi"}); !isErr {
		t.Error("read_only 下 android_exec_shell 应被禁用")
	}
	if _, isErr := callTool(cfg, "android_write_file", map[string]any{"path": "/tmp/x", "content": "y"}); !isErr {
		t.Error("read_only 下 android_write_file 应被禁用")
	}
	if _, isErr := callTool(cfg, "exec_command", map[string]any{"command": "echo hi"}); !isErr {
		t.Error("read_only 下弃用别名 exec_command 也应被禁用")
	}
}

func TestExecAllowlist(t *testing.T) {
	cfg := defaultConfig()
	cfg.ExecAllowlist = []string{"ls", "cat"}
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"ls", true}, {"ls -l /data", true}, {"cat /proc/version", true},
		{"rm -rf /", false}, {"lsblk", false}, {"catapult", false},
		{"  ls -l", true}, // 前后空白应被忽略
	} {
		if got := allowlisted(cfg.ExecAllowlist, tc.cmd); got != tc.want {
			t.Errorf("allowlisted(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// ---------- 协议协商 ----------

func TestNegotiateProtocol(t *testing.T) {
	for _, v := range supportedProtocols {
		if got := negotiateProtocol(v); got != v {
			t.Errorf("受支持版本 %s 应回显，得到 %s", v, got)
		}
	}
	if got := negotiateProtocol("1999-01-01"); got != protocolVersion {
		t.Errorf("未知版本应回退到 %s，得到 %s", protocolVersion, got)
	}
	if got := negotiateProtocol(""); got != protocolVersion {
		t.Errorf("空版本应回退到 %s，得到 %s", protocolVersion, got)
	}
	if protocolVersion != "2025-11-25" {
		t.Errorf("协议版本应为 2025-11-25，当前 %s", protocolVersion)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		v, min string
		want   bool
	}{
		{"1.1.0", "1.1.0", true}, {"1.2.0", "1.1.0", true},
		{"1.0.9", "1.1.0", false}, {"2.0.0", "1.9.9", true},
		{"1.1", "1.1.0", true}, {"1.1.0-beta", "1.1.0", true},
	}
	for _, c := range cases {
		if got := versionAtLeast(c.v, c.min); got != c.want {
			t.Errorf("versionAtLeast(%q,%q)=%v want %v", c.v, c.min, got, c.want)
		}
	}
}

// ---------- 隧道路径隔离（控制面不外泄） ----------

func TestTunnelForbiddenPath(t *testing.T) {
	forbidden := []string{
		"/api", "/api/", "/api/state", "/api/logs?source=mcpd",
		"/mcp/../api/state", "/mcp/../../api/state", "/./api/state",
		"/x/../api/state", "/api/state/",
	}
	for _, p := range forbidden {
		if !tunnelForbiddenPath(p) {
			t.Errorf("控制面路径应被拒绝: %s", p)
		}
	}
	allowed := []string{
		"/mcp", "/mcp/", "/sse", "/health", "/", "/mcp/x", "/mcpmessages",
		"/apix", "/mcp?session_id=1",
	}
	for _, p := range allowed {
		if tunnelForbiddenPath(p) {
			t.Errorf("正常 MCP 路径不应被拒绝: %s", p)
		}
	}
}

// ---------- 退避抖动 ----------

func TestWithJitterBounds(t *testing.T) {
	for _, d := range []time.Duration{
		tunnelBackoffMin, 5 * time.Second, 16 * time.Second, tunnelBackoffMax,
	} {
		for i := 0; i < 200; i++ {
			got := withJitter(d)
			if got < time.Second || got > tunnelBackoffMax {
				t.Fatalf("withJitter(%v) 越界: %v", d, got)
			}
			// ±20% 抖动（除被上下限夹断的情况）
			if got > time.Second && got < tunnelBackoffMax {
				lo := time.Duration(float64(d) * 0.79)
				hi := time.Duration(float64(d) * 1.21)
				if got < lo || got > hi {
					t.Fatalf("withJitter(%v)=%v 超出 ±20%% 抖动范围", d, got)
				}
			}
		}
	}
	if got := withJitter(0); got != tunnelBackoffMin {
		t.Errorf("withJitter(0) 应为 %v，得到 %v", tunnelBackoffMin, got)
	}
}

func TestTunnelTimingConstants(t *testing.T) {
	// 任务要求：心跳 10s、判死 35s、退避上限 30s
	if tunnelPingInterval != 10*time.Second {
		t.Errorf("心跳应为 10s，当前 %v", tunnelPingInterval)
	}
	if tunnelReadTimeout != 35*time.Second {
		t.Errorf("判死超时应为 35s，当前 %v", tunnelReadTimeout)
	}
	if tunnelBackoffMax != 30*time.Second {
		t.Errorf("退避上限应为 30s，当前 %v", tunnelBackoffMax)
	}
	if tunnelIfacePoll > 10*time.Second {
		t.Errorf("网卡轮询应 ≤10s 才能保证网络切换 30s 内恢复，当前 %v", tunnelIfacePoll)
	}
	// 停滞阈值的不变量：
	//   1) 必须大于 WS 拨号握手超时（15s），否则「正在拨号」会被误判；
	//   2) 至少覆盖 2 个 watchdog 巡检周期，避免单次抖动即触发重启；
	//「正在合法退避等待」由 NextRetryAt 单独保护，不计入停滞判定。
	const dialHandshake = 15 * time.Second
	if tunnelStaleAfter <= dialHandshake {
		t.Errorf("tunnelStaleAfter(%v) 必须大于拨号握手超时 %v", tunnelStaleAfter, dialHandshake)
	}
	if tunnelStaleAfter < 2*watchdogInterval {
		t.Errorf("tunnelStaleAfter(%v) 应至少覆盖 2 个巡检周期", tunnelStaleAfter)
	}
	if watchdogFailMax != 3 {
		t.Errorf("watchdog 判决阈值应为 3，当前 %d", watchdogFailMax)
	}
}

// ---------- 公网 URL 推导 ----------

func TestPublicMCPURL(t *testing.T) {
	cases := []struct {
		server, device, want string
	}{
		{"wss://n.huziyang.top/tunnel", "my-phone", "https://n.huziyang.top/mcp/my-phone"},
		{"ws://n.huziyang.top/tunnel", "my-phone", "http://n.huziyang.top/mcp/my-phone"},
		{"https://example.com/tunnel", "p1", "https://example.com/mcp/p1"},
		{"wss://n.huziyang.top:8443/tunnel", "p1", "https://n.huziyang.top:8443/mcp/p1"},
		{"wss://n.huziyang.top/tunnel", "", "https://n.huziyang.top/mcp/" + defaultTunnelDevice},
		{"", "p1", ""},
		{"not a url", "p1", ""},
	}
	for _, c := range cases {
		cfg := defaultConfig()
		cfg.Tunnel.Server = c.server
		cfg.Tunnel.Device = c.device
		if got := publicMCPURL(cfg); got != c.want {
			t.Errorf("publicMCPURL(%q,%q)=%q want %q", c.server, c.device, got, c.want)
		}
	}
}

// ---------- 三网络地址构造 ----------

func TestBuildEndpoints(t *testing.T) {
	cfg := defaultConfig()
	cfg.Token = "tok"
	cfg.Port = 9123
	cfg.Bind = "127.0.0.1"
	cfg.Tunnel.Server = "wss://n.huziyang.top/tunnel"
	cfg.Tunnel.Device = "my-phone"

	ep := buildEndpoints(cfg)
	if len(ep.Local) != 2 {
		t.Fatalf("本机应有 Streamable HTTP + SSE 两条地址，得到 %d", len(ep.Local))
	}
	if ep.Local[0].URL != "http://127.0.0.1:9123/mcp" || ep.Local[0].Transport != "streamable-http" {
		t.Errorf("本机 Streamable HTTP 地址不正确: %+v", ep.Local[0])
	}
	if ep.Local[1].URL != "http://127.0.0.1:9123/sse" || ep.Local[1].Transport != "sse" {
		t.Errorf("本机 SSE 地址不正确: %+v", ep.Local[1])
	}
	for _, e := range ep.Local {
		if e.AuthType == "" || e.AuthHint == "" {
			t.Errorf("地址必须带鉴权说明: %+v", e)
		}
		if !e.Available {
			t.Errorf("本机地址应始终可用: %+v", e)
		}
	}
	// 未开局域网时，局域网地址应标记不可用并给出原因
	for _, e := range ep.LAN {
		if e.Available {
			t.Errorf("bind=127.0.0.1 时局域网地址不应标记为可用: %+v", e)
		}
		if e.Note == "" {
			t.Errorf("不可用的局域网地址必须给出原因: %+v", e)
		}
	}
	if len(ep.Public) != 1 {
		t.Fatalf("公网地址应有一条，得到 %d", len(ep.Public))
	}
	if ep.Public[0].URL != "https://n.huziyang.top/mcp/my-phone" {
		t.Errorf("公网地址不正确: %s", ep.Public[0].URL)
	}
	if ep.Public[0].AuthType != "client-token" {
		t.Errorf("公网鉴权应为 client-token: %s", ep.Public[0].AuthType)
	}
	// 未配置隧道服务器时给出明确指引
	cfg.Tunnel.Server = ""
	ep2 := buildEndpoints(cfg)
	if len(ep2.Public) != 1 || ep2.Public[0].Available || !strings.Contains(ep2.Public[0].Note, "未配置") {
		t.Errorf("未配置隧道时应给出指引: %+v", ep2.Public)
	}
	// 无 Token 时的鉴权提示
	cfg.Token = ""
	ep3 := buildEndpoints(cfg)
	if ep3.Local[0].AuthType != "none" {
		t.Errorf("无 Token 时 auth_type 应为 none: %s", ep3.Local[0].AuthType)
	}
}

// ---------- UI 聚合状态 ----------

func TestBuildUIState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPD_DATA", dir)
	cfg := defaultConfig()
	cfg.Token = "tok"
	st := buildUIState(cfg)

	for _, k := range []string{"version", "protocol_version", "running", "port", "bind", "token",
		"watchdog", "tunnel", "endpoints", "tools", "sessions", "networks", "lan_ips"} {
		if _, ok := st[k]; !ok {
			t.Errorf("ui-state 缺少字段 %s", k)
		}
	}
	if st["version"] != appVersion {
		t.Errorf("version 不正确: %v", st["version"])
	}
	tools := st["tools"].(map[string]any)
	if tools["canonical"].(int) < 5 || tools["deprecated"].(int) < 1 {
		t.Errorf("工具统计不正确: %v", tools)
	}
	tun := st["tunnel"].(map[string]any)
	for _, k := range []string{"connected", "healthy", "state", "heartbeat_sec", "read_timeout_sec", "backoff_max_sec"} {
		if _, ok := tun[k]; !ok {
			t.Errorf("tunnel 段缺少真实连接态字段 %s（v1.1.0 只判 pid 导致假状态）", k)
		}
	}
	if tun["heartbeat_sec"].(int) != 10 || tun["read_timeout_sec"].(int) != 35 {
		t.Errorf("tunnel 心跳/超时参数不正确: %v %v", tun["heartbeat_sec"], tun["read_timeout_sec"])
	}
	// 必须能被 JSON 序列化（WebUI / CLI 都依赖）
	if _, err := json.Marshal(st); err != nil {
		t.Fatalf("ui-state 无法序列化: %v", err)
	}
}

func TestTunnelMetricsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPD_DATA", dir)
	st := newTunnelState("wss://x/tunnel", "dev", "")
	st.update(func(m *tunnelMetrics) {
		m.State = "connected"
		m.Connected = true
		m.ConnectedAt = time.Now().Unix()
		m.LastFrameAt = time.Now().Unix()
		m.Reconnects = 3
		m.LatencyMs = 42
	})
	got, err := loadTunnelMetrics()
	if err != nil {
		t.Fatalf("运行态应可读取: %v", err)
	}
	if got.State != "connected" || !got.Connected || got.Reconnects != 3 || got.LatencyMs != 42 {
		t.Fatalf("运行态内容不正确: %+v", got)
	}
	if !got.Healthy {
		t.Error("刚收到帧且 connected 时应判定为 healthy")
	}
	// 状态位变化必须立即落盘（不受 1s 节流限制）
	st.update(func(m *tunnelMetrics) { m.State = "reconnecting"; m.Connected = false })
	got2, _ := loadTunnelMetrics()
	if got2.State != "reconnecting" {
		t.Fatalf("状态变化应立即落盘，得到 %s", got2.State)
	}
	// 计划重连时间用于 watchdog 区分「合法退避」与「卡死」
	st.markRetry(30 * time.Second)
	got3, _ := loadTunnelMetrics()
	if got3.NextRetryAt <= time.Now().Unix() {
		t.Error("markRetry 未写入未来的 NextRetryAt")
	}
}

// ---------- 日志 ----------

func TestTailLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPD_DATA", dir)
	lines := []string{}
	for i := 0; i < 500; i++ {
		lines = append(lines, "line")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcpd.log"), []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, errStr := tailLog("mcpd", 100)
	if errStr != "" {
		t.Fatalf("读取日志失败: %s", errStr)
	}
	if len(got) != 100 {
		t.Fatalf("应返回 100 行，得到 %d", len(got))
	}
	if _, errStr := tailLog("tunnel", 10); errStr == "" {
		t.Error("日志文件不存在时应返回错误说明而不是 panic")
	}
	if logFileFor("tunnel") == logFileFor("mcpd") || logFileFor("watchdog") == logFileFor("mcpd") {
		t.Error("不同日志源必须指向不同文件")
	}
}

// ---------- 网络辅助 ----------

func TestIfaceKind(t *testing.T) {
	cases := map[string]string{
		"wlan0": "wifi", "ap0": "wifi", "swlan0": "wifi",
		"rmnet_data0": "cellular", "ccmni0": "cellular", "pdp_ip0": "cellular",
		"eth0": "ethernet", "usb0": "ethernet",
		"tun0": "vpn", "ppp0": "vpn", "wg0": "vpn",
		"lo": "other", "dummy0": "other",
	}
	for name, want := range cases {
		if got := ifaceKind(name); got != want {
			t.Errorf("ifaceKind(%q)=%q want %q", name, got, want)
		}
	}
}

func TestHexLEToIP(t *testing.T) {
	// /proc/net/route 使用小端十六进制
	if got := hexLEToIP("0101A8C0"); got != "192.168.1.1" {
		t.Errorf("hexLEToIP 解析错误: %s", got)
	}
	if got := hexLEToIP("00000000"); got != "0.0.0.0" {
		t.Errorf("默认路由地址解析错误: %s", got)
	}
	if got := hexLEToIP("bad"); got != "" {
		t.Errorf("非法输入应返回空串: %s", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"abc":     "'abc'",
		"a b":     "'a b'",
		"it's":    `'it'\''s'`,
		"semi;rm": "'semi;rm'",
		"$HOME":   "'$HOME'",
		"a`b":     "'a`b'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q)=%q want %q", in, got, want)
		}
	}
}

func TestScaleOrNil(t *testing.T) {
	if got := scaleOrNil("4200", 10.0); got != "420" {
		t.Errorf("温度缩放错误: %s", got)
	}
	if got := scaleOrNil("4231000", 1e6); got != "4.231" {
		t.Errorf("电压缩放错误: %s", got)
	}
	if got := scaleOrNil("", 10); got != "" {
		t.Errorf("空输入应返回空串: %q", got)
	}
	if got := scaleOrNil("abc", 10); got != "" {
		t.Errorf("非法输入应返回空串: %q", got)
	}
}

func TestNilAndBoolHelpers(t *testing.T) {
	if nilIfEmpty("") != nil || nilIfEmpty("  ") != nil {
		t.Error("空白字符串应转为 nil（便于 Agent 稳定解析）")
	}
	if nilIfEmpty("x") != "x" {
		t.Error("非空字符串应原样返回")
	}
	if boolOrNil("1") != true || boolOrNil("0") != false {
		t.Error("boolOrNil 解析 1/0 失败")
	}
	if boolOrNil("enabled") != true || boolOrNil("disabled") != false {
		t.Error("boolOrNil 解析 enabled/disabled 失败")
	}
	if boolOrNil("") != nil || boolOrNil("weird") != nil {
		t.Error("无法识别的值应为 nil")
	}
}

// ---------- 设置项白名单 ----------

func TestSettingSpecs(t *testing.T) {
	names := settingNames()
	if len(names) < 10 {
		t.Fatalf("支持的设置项过少: %d", len(names))
	}
	for _, want := range []string{"wifi", "bluetooth", "airplane_mode", "mobile_data",
		"location", "auto_rotate", "brightness", "volume_music", "dnd"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少设置项: %s", want)
		}
	}
	for name, spec := range settingSpecs() {
		switch spec.Kind {
		case "bool":
			if spec.Namespace == "" || spec.Key == "" {
				t.Errorf("布尔设置项 %s 必须指定 namespace/key", name)
			}
		case "int":
			if spec.Max <= spec.Min {
				t.Errorf("整数设置项 %s 的 min/max 不合理: %d/%d", name, spec.Min, spec.Max)
			}
		case "enum":
			if spec.Namespace == "" {
				t.Errorf("枚举设置项 %s 必须指定 namespace", name)
			}
		default:
			t.Errorf("设置项 %s 的 kind 非法: %s", name, spec.Kind)
		}
	}
	// 越界值必须被拒绝（不落到真实设备命令）
	if _, ok := setSetting("brightness", nil, intPtr(999)); ok {
		t.Error("brightness=999 越界应被拒绝")
	}
	if _, ok := setSetting("not_exist", boolPtr(true), nil); ok {
		t.Error("未知设置项应被拒绝")
	}
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// ---------- 输入参数辅助 ----------

func TestArgHelpers(t *testing.T) {
	args := map[string]any{
		"s": "x", "i": float64(7), "i2": 9, "b": true,
		"arr": []any{"a", "b", "", "  c "},
	}
	if argString(args, "s") != "x" || argString(args, "missing") != "" {
		t.Error("argString 不正确")
	}
	if argInt(args, "i") != 7 || argInt(args, "i2") != 9 || argInt(args, "missing") != 0 {
		t.Error("argInt 不正确")
	}
	if !argBool(args, "b") || argBool(args, "missing") {
		t.Error("argBool 不正确")
	}
	if p := argIntPtr(args, "i"); p == nil || *p != 7 {
		t.Error("argIntPtr 不正确")
	}
	if argIntPtr(args, "missing") != nil {
		t.Error("argIntPtr 应区分「未传」")
	}
	if p := argBoolPtr(map[string]any{"z": false}, "z"); p == nil || *p {
		t.Error("argBoolPtr 应能取到显式 false")
	}
	if argBoolPtr(args, "nope") != nil {
		t.Error("argBoolPtr 应区分「未传」")
	}
	got := argStringSlice(args, "arr")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("argStringSlice 应过滤空白项并裁剪首尾空格: %v", got)
	}
	if len(argStringSlice(map[string]any{"x": []any{"", "   ", 42}}, "x")) != 0 {
		t.Error("argStringSlice 应丢弃空串与非字符串项")
	}
}

func TestSchemaHelpers(t *testing.T) {
	s := schema(map[string]any{
		"e": strPropEnum("枚举", "a", "b"),
		"n": intPropRange("数字", 1, 10),
		"d": intPropDefault("默认值", 5, 0, 100),
		"f": boolPropDefault("开关", true),
	}, []string{"e"})
	if s["type"] != "object" || s["additionalProperties"] != false {
		t.Errorf("schema 基本结构不正确: %v", s)
	}
	req, _ := s["required"].([]string)
	if len(req) != 1 || req[0] != "e" {
		t.Errorf("required 不正确: %v", s["required"])
	}
	props := s["properties"].(map[string]any)
	if len(props["e"].(map[string]any)["enum"].([]string)) != 2 {
		t.Error("enum 未写入")
	}
	if props["n"].(map[string]any)["minimum"] != 1 || props["n"].(map[string]any)["maximum"] != 10 {
		t.Error("minimum/maximum 未写入")
	}
	if props["d"].(map[string]any)["default"] != 5 {
		t.Error("default 未写入")
	}
	if props["f"].(map[string]any)["default"] != true {
		t.Error("布尔 default 未写入")
	}
	// 无参数工具的 properties 必须是空对象而不是 nil（部分客户端会崩）
	empty := schema(nil, nil)
	if empty["properties"] == nil {
		t.Error("无参数 schema 的 properties 应为空对象")
	}
}

// ---------- 版本/常量一致性 ----------

func TestAppVersion(t *testing.T) {
	if appVersion != "1.2.1" {
		t.Errorf("版本号应为 1.2.1，当前 %s", appVersion)
	}
	if serverName != "ksu-mcpd" {
		t.Errorf("服务名被意外修改: %s", serverName)
	}
	if sseRetryHintMs <= 0 {
		t.Error("SSE retry 提示必须为正数（断流自动恢复依赖它）")
	}
	if sessionMaxCount <= 0 || sessionTTL <= 0 {
		t.Error("会话回收参数必须为正（修复 v1.1.0 内存泄漏）")
	}
	if screenshotKeep <= 0 {
		t.Error("截屏文件清理阈值必须为正")
	}
}

func TestDeprecatedDesc(t *testing.T) {
	d := deprecatedDesc("android_get_device_info", "原始说明")
	if !strings.Contains(d, "android_get_device_info") || !strings.Contains(d, "弃用") {
		t.Errorf("弃用说明应包含替代工具与弃用标记: %s", d)
	}
	if !strings.Contains(d, "原始说明") {
		t.Error("弃用说明应保留原始描述")
	}
	if d2 := deprecatedDesc("", "原始"); !strings.Contains(d2, "弃用") {
		t.Error("无替代工具时也应标注弃用")
	}
}

// TestTunnelStateConcurrentPersist 并发落盘不得丢更新。
//
// 回归 v1.2.0 的缺陷：快照在锁内取、文件在锁外写，多个
// handleTunnelRequest goroutine 并发落盘时旧快照会覆盖新快照。
// 运行态文件既是 WebUI 的真实状态来源，也是 watchdog 的存活判据，
// 回退到旧值会造成状态回退甚至误判卡死。
func TestTunnelStateConcurrentPersist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPD_DATA", dir)
	st := newTunnelState("wss://x/tunnel", "dev", "")
	st.update(func(m *tunnelMetrics) { m.State = "connected"; m.Connected = true; m.LastFrameAt = time.Now().Unix() })

	const N = 150
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 强制落盘：制造并发写压力
			st.updateForce(func(m *tunnelMetrics) {
				m.Requests++
				m.BytesIn += 1024
				m.BytesOut += 512
			})
		}()
	}
	wg.Wait()
	st.updateForce(func(m *tunnelMetrics) {})

	got, err := loadTunnelMetrics()
	if err != nil {
		t.Fatalf("运行态应可读取: %v", err)
	}
	if got.Requests != N {
		t.Fatalf("并发落盘丢更新：期望 Requests=%d 实际 %d", N, got.Requests)
	}
	if got.BytesIn != N*1024 || got.BytesOut != N*512 {
		t.Fatalf("并发落盘字节统计不一致：in=%d out=%d", got.BytesIn, got.BytesOut)
	}
	// 文件内容必须是合法 JSON（共用 .tmp 路径曾被并发踩踏）
	var probe tunnelMetrics
	if err := json.Unmarshal(mustRead(t, tunnelRuntimePath()), &probe); err != nil {
		t.Fatalf("运行态文件不是合法 JSON: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", p, err)
	}
	return b
}
