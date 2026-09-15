// ksu-mcpd — 嵌入 Android root 设备的 MCP (Model Context Protocol) 服务端
//
// 传输方式：
//
//	stdio（默认，供本地 MCP 客户端/终端使用）
//	Streamable HTTP（HTTP，POST/GET/DELETE /mcp，Bearer Token 鉴权）
//	SSE（HTTP，/sse，供老客户端兼容）
//
// v1.2.0 变更摘要：
//   - MCP 协议版本协商至 2025-11-25，兼容 2025-06-18 / 2025-03-26 / 2024-11-05
//   - 新增 14 个 android_{action}_{resource} 规范命名工具；原有 11 个扁平命名工具保留并标记弃用
//   - 工具返回值支持 MCP image 内容块（android_screenshot 直接回图，不再塞 JSON 文本）
//   - 隧道心跳 10s / 判死 35s，指数退避叠加抖动，网卡指纹 5s 轮询
//   - 隧道运行态落盘（tunnel.runtime.json），watchdog 由「仅判 pid」升级为「存活 + 新鲜度」双判定
//   - Streamable HTTP 会话增加 TTL / 容量上限 / 定期回收，修复长期运行的内存泄漏
//   - 新增只读控制 API（GET /api/state、/api/logs、/api/tools）供 WebUI 使用，避免高频 fork shell
//
// 除 github.com/gorilla/websocket 外无第三方依赖，可静态编译为 KernelSU / Magisk 模块内嵌二进制。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	appVersion      = "1.2.0"
	serverName      = "ksu-mcpd"
	protocolVersion = "2025-11-25"

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

	// 隧道稳定性相关（v1.2.0：心跳 10s、判死 35s，兼顾 NAT 保活与快速故障感知）
	tunnelPingInterval  = 10 * time.Second
	tunnelReadTimeout   = 35 * time.Second
	tunnelStreamTimeout = 30 * time.Minute // GET/SSE 长流请求的本地转发空闲超时
	tunnelChunkSize     = 64 << 10         // 流式转发分片大小
	streamSingleShotMax = 256 << 10        // 低于该大小的响应单帧回传（兼容旧版服务端）
	tunnelBackoffMin    = 2 * time.Second  // 重连初始退避
	tunnelBackoffMax    = 30 * time.Second // 重连退避上限（任务要求 ≤30s）
	tunnelBackoffReset  = 30 * time.Second // 连接存活超过该时长才认为稳定并重置退避
	tunnelStaleAfter    = 45 * time.Second // 运行态超过该时长未刷新即视为卡死
	tunnelIfacePoll     = 5 * time.Second  // 网卡指纹轮询周期（原 15s 过慢，网络切换恢复超 30s）

	// 进程守护（watchdog）
	watchdogInterval = 10 * time.Second // 巡检周期
	watchdogFailMax  = 3                // 连续探活失败次数达阈值后重启 daemon

	// Streamable HTTP 会话管理（修复无上限增长的内存泄漏）
	sessionTTL      = 30 * time.Minute
	sessionMaxCount = 512
	sseKeepAlive    = 15 * time.Second
	sseRetryHintMs  = 3000 // SSE retry: 提示客户端断流后 3s 自动重连

	// 截屏临时文件保留份数
	screenshotKeep = 5
)

// supportedProtocols 按「新→旧」排列，用于 initialize 版本协商
var supportedProtocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

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
func watchdogPidPath() string   { return filepath.Join(dataDir(), "watchdog.pid") }
func watchdogLogPath() string   { return filepath.Join(dataDir(), "watchdog.log") }
func tmpDir() string            { return filepath.Join(dataDir(), "tmp") }

// logFileFor 返回 WebUI 可选日志源对应的文件路径
func logFileFor(source string) string {
	switch source {
	case "tunnel":
		return tunnelLogPath()
	case "watchdog":
		return watchdogLogPath()
	default:
		return logPath()
	}
}

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

// daemonDisabled 判断用户是否通过 disabled 标记关闭了开机自启：
//
//	echo 1 > /data/adb/ksu_mcp/disabled
func daemonDisabled() bool {
	b, err := os.ReadFile(filepath.Join(dataDir(), "disabled"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "1"
}

// tryEscapeCgroup 尽力把自身迁出 Android 应用/ksud 会话的 cgroup：
// 从 KSU Manager WebUI 通过 ksu 桥启动的进程沿用发起方的 cgroup，
// 页面离开后可能被按 cgroup 整组回收。迁移到 system-background / uid_0
// 等持久 cgroup 后即可脱离 Manager 生命周期。写失败静默忽略——
// 由 service.sh 在开机 init 上下文拉起的 watchdog 天然不受影响，属于兜底。
func tryEscapeCgroup() {
	if os.Getuid() != 0 {
		return
	}
	pid := strconv.Itoa(os.Getpid())
	cands := []string{
		"/acct/uid_0/tasks",
		"/dev/cpuset/system-background/tasks",
		"/dev/cpuset/background/tasks",
		"/dev/cpuset/restricted/tasks",
		"/sys/fs/cgroup/cpuset/system-background/tasks",
		"/sys/fs/cgroup/cpuset/background/tasks",
		"/sys/fs/cgroup/system-background/tasks",
		"/sys/fs/cgroup/background/tasks",
	}
	for _, p := range cands {
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		if _, err := f.WriteString(pid); err != nil {
			_ = f.Close()
			continue
		}
		_ = f.Close()
		logf("cgroup 迁移成功: %s", p)
		return
	}
}

// ---------- 网络信息 ----------

// netIface 描述一张已启用网卡及其地址
type netIface struct {
	Name string   `json:"name"`
	Kind string   `json:"kind"` // wifi / cellular / ethernet / vpn / other
	IPs  []string `json:"ips"`
}

// ifaceKind 依据 Android 网卡命名惯例推断链路类型
func ifaceKind(name string) string {
	switch {
	case strings.HasPrefix(name, "wlan"), strings.HasPrefix(name, "ap"), strings.HasPrefix(name, "swlan"):
		return "wifi"
	case strings.HasPrefix(name, "rmnet"), strings.HasPrefix(name, "ccmni"),
		strings.HasPrefix(name, "pdp"), strings.HasPrefix(name, "wwan"):
		return "cellular"
	case strings.HasPrefix(name, "eth"), strings.HasPrefix(name, "usb"), strings.HasPrefix(name, "rndis"):
		return "ethernet"
	case strings.HasPrefix(name, "tun"), strings.HasPrefix(name, "ppp"), strings.HasPrefix(name, "wg"):
		return "vpn"
	default:
		return "other"
	}
}

// netIfaces 枚举所有启用网卡（排除回环）及其 IPv4 地址
func netIfaces() []netIface {
	out := []netIface{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		ips := []string{}
		if as, err := ifc.Addrs(); err == nil {
			for _, a := range as {
				var ip net.IP
				switch v := a.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip4 := ip.To4(); ip4 != nil {
					ips = append(ips, ip4.String())
				}
			}
		}
		sort.Strings(ips)
		out = append(out, netIface{Name: ifc.Name, Kind: ifaceKind(ifc.Name), IPs: ips})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// lanIPs 枚举所有启用网卡的 IPv4 地址（供 status / WebUI 展示局域网接入地址）
func lanIPs() []string {
	var ips []string
	for _, ifc := range netIfaces() {
		ips = append(ips, ifc.IPs...)
	}
	sort.Strings(ips)
	return ips
}

// versionAtLeast 简易 semver 比较（容忍缺失的次版本号）
func versionAtLeast(v, min string) bool {
	parts := func(s string) []int {
		out := []int{}
		for _, p := range strings.Split(s, ".") {
			n, _ := strconv.Atoi(strings.TrimSpace(p))
			out = append(out, n)
		}
		for len(out) < 3 {
			out = append(out, 0)
		}
		return out
	}
	a, b := parts(v), parts(min)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// ---------- 日志 ----------

func logf(format string, args ...any) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(os.Stderr, "%s [%s] %s\n", ts, serverName, fmt.Sprintf(format, args...))
}

// ---------- 进程辅助 ----------

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
	// 僵尸进程（父进程未回收的已死进程）对 kill(pid,0) 仍返回成功：
	// 必须按死亡处理，否则 watchdog 会把僵尸误判为存活，延误拉起
	if isZombie(pid) {
		return pid, false
	}
	return pid, true
}

// isZombie 读取 /proc/<pid>/stat 判断进程是否处于 Z(ombie)/X(dead) 状态
func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false // /proc 不可读时保持原判断（按存活处理）
	}
	i := lastIndexByte(data, ')')
	if i < 0 || i+2 >= len(data) {
		return false
	}
	return data[i+2] == 'Z' || data[i+2] == 'X'
}

func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func readPid() (int, bool) { return readPidFile(pidPath()) }

func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// stopPid 通用停止：TERM → 3s 宽限 → KILL，并清理 pid 文件
func stopPid(pid int, pidFile string) {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		_ = os.Remove(pidFile)
		return
	}
	for i := 0; i < 30; i++ {
		if _, ok := readPidFile(pidFile); !ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, ok := readPidFile(pidFile); ok {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(200 * time.Millisecond)
	}
	_ = os.Remove(pidFile)
}

// writeFileAtomic 原子写文件（先写临时文件再 rename），避免读到半截 JSON
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- CLI ----------

func usage() {
	fmt.Fprintf(os.Stderr, `%s v%s — Android root 设备 MCP 服务端（Streamable HTTP + SSE + 内网穿透）

用法:
  mcpd                     以 stdio 模式运行 MCP 服务（默认，供本地客户端/终端）
  mcpd watchdog [--detach] 启动进程守护（每 10s 巡检，自动拉起/重启 daemon 与隧道）
  mcpd watchdog-stop       停止进程守护
  mcpd watchdog-status     查看进程守护状态（JSON）
  mcpd start               启动全部服务（等价于 watchdog --detach）
  mcpd restart             重启全部服务（stop + start，供 WebUI 使用）
  mcpd daemon [选项]        以 Streamable HTTP + SSE 模式启动服务
       --port N            监听端口（默认 9123）
       --bind ADDR         监听地址（默认 127.0.0.1；局域网请用 0.0.0.0）
       --token X           设置鉴权 Token（默认读取/生成于配置文件）
       --no-auth           关闭鉴权（仅限本机回环调试）
       --detach            守护化（setsid 后台运行，日志写入 mcpd.log）
  mcpd stop                停止全部（tunnel / daemon / watchdog）
  mcpd status              查看运行状态（JSON，含 watchdog / 三网络地址 / 工具数）
  mcpd ui-state            供 WebUI 使用的聚合状态（JSON，单次调用拿到全部数据）
  mcpd ui-bootstrap        供 WebUI 引导使用的最小信息（JSON，含 token / port / api）
  mcpd tools [--json]      列出已注册的 MCP 工具（名称 / 说明 / 是否弃用）
  mcpd tunnel [选项]        启动内网穿透客户端（模块为客户端）
       --server URL        隧道服务端（默认 wss://n.huziyang.top/tunnel）
       --device ID         设备唯一标识
       --token X           隧道接入 Token
       --ip IP             可选：直连 IP（DNS 解析失败时兜底，TLS 仍校验域名）
       --detach            守护化（日志写入 tunnel.log）
  mcpd tunnel-stop         停止隧道客户端
  mcpd tunnel-status       查看隧道状态（JSON，含连接态 / 延迟 / 重连次数）
  mcpd token [--regen]     查看 / 重新生成 Token
  mcpd logs [N]            查看最近 N 行日志（默认 100）
       --source SRC        日志源：mcpd（默认）/ tunnel / watchdog
  mcpd config-set [选项]    修改配置: --port N --bind ADDR --exec-timeout N
                           --read-only true|false --allowlist-add CMD --allowlist-clear
                           --lan true|false --tunnel-enable true|false
                           --tunnel-server URL --tunnel-device ID --tunnel-token X
                           --tunnel-ip IP
  mcpd init-config         初始化配置文件
  mcpd version             输出版本
  mcpd help                显示本帮助
`, serverName, appVersion)
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "daemon", "serve":
			os.Exit(cmdDaemon(args[1:]))
		case "watchdog":
			os.Exit(cmdWatchdog(args[1:]))
		case "watchdog-stop":
			os.Exit(cmdWatchdogStop())
		case "watchdog-status":
			os.Exit(cmdWatchdogStatus())
		case "start":
			os.Exit(cmdStart())
		case "restart":
			os.Exit(cmdRestart())
		case "stop":
			os.Exit(cmdStop())
		case "status":
			os.Exit(cmdStatus())
		case "ui-state":
			os.Exit(cmdUIState())
		case "ui-bootstrap":
			os.Exit(cmdUIBootstrap())
		case "tools":
			os.Exit(cmdTools(args[1:]))
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
