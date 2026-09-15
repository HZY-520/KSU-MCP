package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// tunnel.go — 内网穿透客户端（设备侧）
//
// v1.2.0 稳定性改造：
//   - 心跳 20s → 10s，判死 75s → 35s（与任务参考实践一致，同时满足服务端 10s 心跳）
//   - 断线重连也走指数退避（2s→30s，±20% 抖动），修复 v1.1.0「连上即复位导致退避失效」
//   - 网卡指纹 15s → 5s 轮询，并纳入默认路由与 DNS，网络切换恢复更快
//   - 运行态落盘 tunnel.runtime.json（连接态/延迟/重连次数/最后错误），
//     既是 WebUI 的真实状态来源，也是 watchdog 判定隧道是否卡死的依据
//   - 隧道只转发 MCP 端点（/mcp、/sse、/health），拒绝 /api/* 控制面外泄

// ---------- 隧道协议消息 ----------

type tunnelRequestMsg struct {
	Type    string            `json:"type"`
	ID      int64             `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64
}

// Fin=true 表示本帧为最后一帧（v1.1.0 流式协议；旧版服务端忽略此字段）
type tunnelResponseMsg struct {
	Type    string            `json:"type"`
	ID      int64             `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64
	Fin     bool              `json:"fin"`
}

// 流式转发分片帧（v1.1.0 流式协议）；Done=true 表示传输结束
type tunnelChunkMsg struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
	Body string `json:"body,omitempty"` // base64
	Done bool   `json:"done,omitempty"`
}

// ---------- 隧道运行态（供 WebUI / watchdog / status 读取） ----------

type tunnelMetrics struct {
	State            string `json:"state"` // stopped|connecting|connected|reconnecting|error
	Connected        bool   `json:"connected"`
	Healthy          bool   `json:"healthy"`
	Server           string `json:"server"`
	Device           string `json:"device"`
	IP               string `json:"ip"`
	ServerVersion    string `json:"server_version"`
	StreamOK         bool   `json:"stream_ok"`
	ConnectedAt      int64  `json:"connected_at"`
	ConnectedSeconds int64  `json:"connected_seconds"`
	LastFrameAt      int64  `json:"last_frame_at"`
	LastError        string `json:"last_error"`
	LastErrorAt      int64  `json:"last_error_at"`
	Reconnects       int64  `json:"reconnects"`
	ConsecutiveFails int64  `json:"consecutive_fails"`
	LatencyMs        int64  `json:"latency_ms"`
	PingSent         int64  `json:"ping_sent"`         // 已发送的心跳 ping 数
	PingLost         int64  `json:"ping_lost"`         // 下一次心跳前仍未收到 pong 的次数
	PingLossPercent  int64  `json:"ping_loss_percent"` // 心跳丢包率（0-100）
	BytesIn          int64  `json:"bytes_in"`
	BytesOut         int64  `json:"bytes_out"`
	Requests         int64  `json:"requests"`
	Responses        int64  `json:"responses"`
	Streams          int64  `json:"streams"`
	NextRetryAt      int64  `json:"next_retry_at"`
	UpdatedAt        int64  `json:"updated_at"`
	StartedAt        int64  `json:"started_at"`
	PID              int    `json:"pid"`
}

// tunnelState 带锁的运行态容器，字段变更后按节流策略落盘（原子写）
//
// 节流原因：隧道转发会产生高频帧，若每帧都写文件会造成明显 IO 抖动；
// 因此状态位变化时立即落盘，其余更新最多每秒落盘一次。
type tunnelState struct {
	mu          sync.Mutex
	m           tunnelMetrics
	pid         int
	lastPersist time.Time
	lastState   string
	// writeMu 串行化落盘。v1.2.1 修复：v1.2.0 在锁外写文件，多个
	// handleTunnelRequest goroutine 并发落盘时会出现「旧快照覆盖新快照」
	// （丢更新），且共用同一个 .tmp 路径可能互相踩踏。
	// 运行态文件是 WebUI 的真实状态来源、也是 watchdog 的存活判据，
	// 回归到旧值会造成状态回退甚至误判卡死，必须串行化。
	writeMu sync.Mutex
}

func newTunnelState(server, device, ip string) *tunnelState {
	now := time.Now().Unix()
	return &tunnelState{
		m: tunnelMetrics{
			State: "connecting", Server: server, Device: device, IP: ip,
			StartedAt: now, UpdatedAt: now, PID: os.Getpid(),
		},
		pid:       os.Getpid(),
		lastState: "connecting",
	}
}

// update 在锁内修改运行态，并按节流策略落盘
func (t *tunnelState) update(fn func(m *tunnelMetrics)) { t.apply(fn, false) }

// updateForce 无视节流立即落盘。
// 用于「状态位未变但字段必须让 watchdog 立刻看到」的场景（如 markRetry 写入
// NextRetryAt）——若被节流吞掉，watchdog 会把合法退避误判为卡死。
func (t *tunnelState) updateForce(fn func(m *tunnelMetrics)) { t.apply(fn, true) }

func (t *tunnelState) apply(fn func(m *tunnelMetrics), force bool) {
	t.mu.Lock()
	fn(&t.m)
	now := time.Now()
	t.m.UpdatedAt = now.Unix()
	t.m.PID = t.pid
	if t.m.ConnectedAt > 0 && t.m.Connected {
		t.m.ConnectedSeconds = now.Unix() - t.m.ConnectedAt
	}
	if !t.m.Connected {
		t.m.ConnectedSeconds = 0
	}
	t.m.Healthy = t.m.Connected && now.Unix()-t.m.LastFrameAt < int64(tunnelReadTimeout.Seconds())
	stateChanged := t.m.State != t.lastState
	if !force && !stateChanged && now.Sub(t.lastPersist) < time.Second {
		t.mu.Unlock()
		return
	}
	t.lastState = t.m.State
	t.lastPersist = now
	t.mu.Unlock()
	t.persistLatest()
}

// persistLatest 串行化落盘，并始终写入「当前最新」快照。
//
// 关键点：快照在 writeMu 之内重新获取，因此无论多少个 goroutine 并发调用，
// 最后写入文件的一定是最新数据，不会出现旧值覆盖新值的丢更新。
func (t *tunnelState) persistLatest() {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	snap := t.m
	t.mu.Unlock()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(tunnelRuntimePath(), b, 0644)
}

func (t *tunnelState) snapshot() tunnelMetrics {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m
}

// saveQuiet 仅刷新运行态时间戳（心跳周期调用，作为「进程活着且在工作」的证据）
func (t *tunnelState) saveQuiet() { t.update(func(m *tunnelMetrics) {}) }

// markRetry 记录下一次重连时间。
// watchdog 据此区分「正在正常退避等待」与「进程卡死」——只有超过 NextRetryAt
// 仍未刷新运行态才会被判为卡死，避免把合法退避误判为故障。
func (t *tunnelState) markRetry(wait time.Duration) {
	at := time.Now().Add(wait).Unix()
	t.updateForce(func(m *tunnelMetrics) { m.NextRetryAt = at })
}

// loadTunnelMetrics 读取隧道运行态（watchdog / status / WebUI 使用）
func loadTunnelMetrics() (*tunnelMetrics, error) {
	b, err := os.ReadFile(tunnelRuntimePath())
	if err != nil {
		return nil, err
	}
	var m tunnelMetrics
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ---------- 隧道转发用的本地 HTTP 客户端 ----------

// tunnelHTTPClient 隧道转发共用的本地 HTTP 客户端：keep-alive 复用连接，
// 保证 Streamable HTTP 同一会话的 initialize / GET / 后续 POST 走同一链路
var tunnelHTTPClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	},
}

// ---------- 客户端主流程 ----------

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
	defer os.Remove(tunnelRuntimePath())
	tryEscapeCgroup()

	st := newTunnelState(tc.Server, tc.Device, tc.IP)
	st.update(func(m *tunnelMetrics) { m.State = "connecting" })

	logf("tunnel 客户端启动: server=%s device=%s ip=%s (pid %d), 心跳 %s / 判死 %s",
		tc.Server, tc.Device, tc.IP, os.Getpid(), tunnelPingInterval, tunnelReadTimeout)

	// 优雅退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	var connMu sync.Mutex
	var active *websocket.Conn
	forceReconnect := func(reason string) {
		connMu.Lock()
		c := active
		connMu.Unlock()
		if c != nil {
			logf("强制重连隧道: %s", reason)
			_ = c.Close()
		}
	}
	go func() {
		<-sigCh
		logf("tunnel 收到退出信号，正在停止")
		st.update(func(m *tunnelMetrics) { m.State = "stopped"; m.Connected = false })
		forceReconnect("进程退出")
		stopOnce.Do(func() { close(stopCh) })
	}()

	// 网络切换检测：网卡/IP/默认路由/DNS 集合变化时立即断开重连。
	// v1.2.0 由 15s 缩短到 5s，并纳入默认路由与 DNS —— WiFi↔移动数据切换
	// 即使 IP 未变（DHCP 续租同址）也能被路由变化捕获，满足 30s 内恢复的验收要求。
	ifaceWatcherDone := make(chan struct{})
	defer close(ifaceWatcherDone)
	go func() {
		prev := ifaceSig()
		t := time.NewTicker(tunnelIfacePoll)
		defer t.Stop()
		for {
			select {
			case <-ifaceWatcherDone:
				return
			case <-t.C:
				cur := ifaceSig()
				if cur != prev {
					prev = cur
					forceReconnect("检测到网络接口/路由变化")
				}
			}
		}
	}()

	backoff := tunnelBackoffMin
	attempt := 0
	for {
		select {
		case <-stopCh:
			return 0
		default:
		}

		st.update(func(m *tunnelMetrics) {
			if attempt == 0 {
				m.State = "connecting"
			} else {
				m.State = "reconnecting"
			}
			m.Connected = false
		})

		dialStart := time.Now()
		conn, err := dialTunnel(tc, st)
		if err != nil {
			wait := withJitter(backoff)
			attempt++
			st.update(func(m *tunnelMetrics) {
				m.State = "reconnecting"
				m.Connected = false
				m.LastError = truncateErr(err.Error(), 300)
				m.LastErrorAt = time.Now().Unix()
				m.ConsecutiveFails++
				m.Reconnects++
				m.LatencyMs = time.Since(dialStart).Milliseconds()
			})
			logf("连接 %s 失败: %v（%.1fs 后重试，连续失败 %d 次）", tc.Server, err, wait.Seconds(), attempt)
			st.markRetry(wait)
			select {
			case <-stopCh:
				return 0
			case <-time.After(wait):
			}
			if backoff < tunnelBackoffMax {
				backoff *= 2
				if backoff > tunnelBackoffMax {
					backoff = tunnelBackoffMax
				}
			}
			continue
		}

		connMu.Lock()
		active = conn
		connMu.Unlock()

		connectStart := time.Now()
		st.update(func(m *tunnelMetrics) {
			m.State = "connected"
			m.Connected = true
			m.ConnectedAt = time.Now().Unix()
			m.LastFrameAt = time.Now().Unix()
			m.LastError = ""
			m.LatencyMs = time.Since(dialStart).Milliseconds()
			m.ConsecutiveFails = 0
		})

		// gorilla/websocket 仅允许单写者：本连接所有数据帧写操作经 wm 串行化
		var wm sync.Mutex
		err = serveTunnel(conn, cfg, &wm, st)

		connMu.Lock()
		active = nil
		connMu.Unlock()
		_ = conn.Close()

		lived := time.Since(connectStart)
		// 只有连接稳定存活超过 tunnelBackoffReset 才认为链路健康并复位退避，
		// 否则继续沿用（并放大）退避，避免「连上 2 秒就断」时的重连风暴。
		if lived >= tunnelBackoffReset {
			backoff = tunnelBackoffMin
			attempt = 0
		}
		st.update(func(m *tunnelMetrics) {
			m.Connected = false
			m.State = "reconnecting"
			m.LastError = truncateErr(fmt.Sprint(err), 300)
			m.LastErrorAt = time.Now().Unix()
			m.Reconnects++
		})
		wait := withJitter(backoff)
		logf("隧道连接断开（存活 %.1fs）: %v（%.1fs 后重连）", lived.Seconds(), err, wait.Seconds())
		st.markRetry(wait)
		select {
		case <-stopCh:
			return 0
		case <-time.After(wait):
		}
		if backoff < tunnelBackoffMax {
			backoff *= 2
			if backoff > tunnelBackoffMax {
				backoff = tunnelBackoffMax
			}
		}
	}
}

// withJitter 给退避时长叠加 ±20% 抖动，避免多设备同时重连造成惊群
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return tunnelBackoffMin
	}
	delta := float64(d) * 0.2
	j := (rand.Float64()*2 - 1) * delta
	out := time.Duration(float64(d) + j)
	if out < time.Second {
		out = time.Second
	}
	if out > tunnelBackoffMax {
		out = tunnelBackoffMax
	}
	return out
}

func truncateErr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ifaceSig 生成当前网络环境指纹：接口名 + 地址 + 默认路由。
//
// 相比 v1.1.0（仅接口名+地址）额外纳入默认路由，
// 使「WiFi↔移动数据」切换在 IP 不变时也能被识别。
// 刻意不纳入 DNS：读取 DNS 需要 fork getprop，而本函数每 5s 调用一次，
// 默认路由变化已足以覆盖所有真实网络切换场景。
func ifaceSig() string {
	strs := []string{}
	for _, n := range netIfaces() {
		strs = append(strs, n.Name+"="+strings.Join(n.IPs, ","))
	}
	if iface, gw := defaultRoute(); iface != "" {
		strs = append(strs, "default="+iface+"/"+gw)
	}
	sort.Strings(strs)
	return strings.Join(strs, "|")
}

// dialTunnel 建立到隧道服务端的 WebSocket 连接
func dialTunnel(tc TunnelConfig, st *tunnelState) (*websocket.Conn, error) {
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
		"/system/etc/security/cacerts",        // Android 9-13 系统 CA
		"/apex/com.android.conscrypt/cacerts", // Android 14+（Conscrypt APEX）
		"/data/misc/keychain/cacerts-added",   // 用户手动安装的 CA
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
			if !strings.HasSuffix(name, ".0") && !strings.HasSuffix(name, ".pem") &&
				!strings.HasSuffix(name, ".crt") && !strings.HasSuffix(name, ".cer") {
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

// serveTunnel 在单连接上循环读取服务端转发的请求并分发到独立 goroutine。
// 所有数据帧写操作经 wm 串行化（gorilla/websocket 仅允许一个并发写者）。
func serveTunnel(conn *websocket.Conn, cfg *Config, wm *sync.Mutex, st *tunnelState) error {
	streamOK := false
	serverVersion := ""

	// 心跳保活：每 10s ping（payload 携带发送时间戳用于测 RTT）。
	// 35s 内未收到任何帧（网络静默断连 / NAT 超时）即判定连接死亡并触发重连。
	_ = conn.SetReadDeadline(time.Now().Add(tunnelReadTimeout))
	conn.SetPongHandler(func(appData string) error {
		now := time.Now()
		if ms, err := strconv.ParseInt(appData, 10, 64); err == nil && ms > 0 {
			rtt := now.UnixMilli() - ms
			if rtt >= 0 && rtt < 60000 {
				st.update(func(m *tunnelMetrics) { m.LatencyMs = rtt })
			}
		}
		st.update(func(m *tunnelMetrics) { m.LastFrameAt = now.Unix() })
		return conn.SetReadDeadline(time.Now().Add(tunnelReadTimeout))
	})
	// 心跳丢包统计：上一拍 ping 未在下一次发 ping 前收到 pong 即计一次丢包。
	// （10s 间隔 / 35s 判死，容忍丢 2 拍不断连，因此丢包率是链路质量的早期指标）
	var pingMu sync.Mutex
	pingAnswered := true
	markAnswered := func() {
		pingMu.Lock()
		pingAnswered = true
		pingMu.Unlock()
	}
	updatePong := conn.PongHandler()
	conn.SetPongHandler(func(appData string) error {
		markAnswered()
		return updatePong(appData)
	})

	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		t := time.NewTicker(tunnelPingInterval)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				pingMu.Lock()
				missed := !pingAnswered
				pingAnswered = false
				pingMu.Unlock()

				payload := []byte(strconv.FormatInt(time.Now().UnixMilli(), 10))
				// WriteControl 可与其它写并发调用（gorilla 保证），因此不走 wm，
				// 避免数据帧写阻塞时把心跳一起拖死
				err := conn.WriteControl(websocket.PingMessage, payload, time.Now().Add(5*time.Second))
				st.update(func(m *tunnelMetrics) {
					m.PingSent++
					if missed {
						m.PingLost++
					}
					if m.PingSent > 0 {
						m.PingLossPercent = m.PingLost * 100 / m.PingSent
					}
				})
				if err != nil {
					return
				}
				// 心跳同时刷新运行态文件，作为 watchdog 的「进程活着且在工作」证据
				st.saveQuiet()
			}
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(tunnelReadTimeout))
		st.update(func(m *tunnelMetrics) {
			m.LastFrameAt = time.Now().Unix()
			m.BytesIn += int64(len(data))
			m.StreamOK = streamOK
			m.ServerVersion = serverVersion
		})
		var head struct {
			Type    string `json:"type"`
			ID      int64  `json:"id"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			continue
		}
		switch head.Type {
		case "hello":
			// 服务端版本握手：≥1.1.0 支持流式转发协议
			streamOK = versionAtLeast(head.Version, "1.1.0")
			serverVersion = head.Version
			st.update(func(m *tunnelMetrics) { m.StreamOK = streamOK; m.ServerVersion = serverVersion })
			logf("tunnel 握手: 服务端版本 %s（流式转发: %v）", head.Version, streamOK)
		case "request":
			var msg tunnelRequestMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			st.update(func(m *tunnelMetrics) { m.Requests++ })
			go handleTunnelRequest(conn, cfg, &msg, wm, streamOK, st)
		}
	}
}

// tunnelForbiddenPath 判断远端请求路径是否属于「控制面」，禁止经隧道转发。
//
// 修复要点：/api/* 是设备端 WebUI 控制 API（会返回本地 Token），
// 绝不能让公网隧道转发；否则任何持有 clientToken 的人都能读到设备本地 Token。
//
// 先做路径规范化再判断：`/mcp/../api/state` 这类穿越写法必须与 `/api/state`
// 同等对待（否则本地 ServeMux 的 cleanPath 重定向可能把请求导向控制面）。
func tunnelForbiddenPath(rawPath string) bool {
	p := rawPath
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	if clean == "/api" || strings.HasPrefix(clean, "/api/") {
		return true
	}
	return strings.Contains(clean, "..")
}

// handleTunnelRequest 将一条远端 HTTP 请求转发到本地 MCP 服务并回传响应。
// v1.1.0 及以上服务端支持流式协议：响应头帧 + 数据分片帧 + 结束帧，
// 保证 Streamable HTTP GET 长流 / 大响应（截屏等）经隧道稳定传输；
// 旧版服务端则退化为单帧回传，保证兼容。
func handleTunnelRequest(conn *websocket.Conn, cfg *Config, msg *tunnelRequestMsg, wm *sync.Mutex, streamOK bool, st *tunnelState) {
	fail := func(status int, errmsg string) {
		body := map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": -32000, "message": errmsg}}
		raw, _ := json.Marshal(body)
		writeTunnelJSON(conn, wm, st, tunnelResponseMsg{
			Type: "response", ID: msg.ID, Status: status,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    base64.StdEncoding.EncodeToString(raw), Fin: true,
		})
	}

	path := msg.Path
	if path == "" {
		path = "/mcp"
	}
	if tunnelForbiddenPath(path) {
		logf("隧道拒绝转发控制面路径: %s", path)
		fail(403, "control API is not exposed over tunnel")
		return
	}

	body, err := base64.StdEncoding.DecodeString(msg.Body)
	if err != nil {
		body = []byte(msg.Body)
	}
	localURL := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Port, path)
	req, err := http.NewRequest(msg.Method, localURL, bytes.NewReader(body))
	if err != nil {
		fail(502, "device request error")
		return
	}
	for k, v := range msg.Headers {
		if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "host") {
			continue
		}
		req.Header.Set(k, v)
	}
	// 本地 MCP 服务鉴权：始终注入本地 Token
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	// GET /mcp 属于长流（SSE 保活），空闲超时放宽到 30 分钟；其余请求 90s
	timeout := tunnelReqTimeout * time.Second
	if msg.Method == http.MethodGet {
		timeout = tunnelStreamTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	httpResp, err := tunnelHTTPClient.Do(req.WithContext(ctx))
	if err != nil {
		fail(502, "device local server unavailable")
		return
	}
	defer httpResp.Body.Close()

	headers := map[string]string{}
	if ct := httpResp.Header.Get("Content-Type"); ct != "" {
		headers["Content-Type"] = ct
	}
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		headers["Mcp-Session-Id"] = sid
	}

	if msg.Method == http.MethodGet && streamOK {
		// 流式转发：HTTP 流（SSE / 事件帧）边读边推，远端客户端实时收到数据
		writeTunnelJSON(conn, wm, st, tunnelResponseMsg{Type: "response", ID: msg.ID, Status: httpResp.StatusCode, Headers: headers, Body: "", Fin: false})
		st.update(func(m *tunnelMetrics) { m.Streams++ })
		buf := make([]byte, tunnelChunkSize)
		for {
			n, rerr := httpResp.Body.Read(buf)
			if n > 0 {
				writeTunnelJSON(conn, wm, st, tunnelChunkMsg{Type: "chunk", ID: msg.ID, Body: base64.StdEncoding.EncodeToString(buf[:n])})
			}
			if rerr != nil {
				writeTunnelJSON(conn, wm, st, tunnelChunkMsg{Type: "chunk", ID: msg.ID, Done: true})
				st.update(func(m *tunnelMetrics) { m.Responses++ })
				return
			}
		}
	}

	// 有限响应：整体读取（上限 16MB 与本地一致）
	data, rerr := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyBytes))
	if rerr != nil && len(data) == 0 {
		fail(502, "device local read error")
		return
	}
	// 小响应/旧版服务端：单帧回传
	if !streamOK || len(data) <= streamSingleShotMax {
		writeTunnelJSON(conn, wm, st, tunnelResponseMsg{
			Type: "response", ID: msg.ID, Status: httpResp.StatusCode,
			Headers: headers, Body: base64.StdEncoding.EncodeToString(data), Fin: true,
		})
		st.update(func(m *tunnelMetrics) { m.Responses++ })
		return
	}
	// 大响应：头帧 + 分片 + 结束帧
	writeTunnelJSON(conn, wm, st, tunnelResponseMsg{Type: "response", ID: msg.ID, Status: httpResp.StatusCode, Headers: headers, Body: "", Fin: false})
	for off := 0; off < len(data); off += tunnelChunkSize {
		end := off + tunnelChunkSize
		if end > len(data) {
			end = len(data)
		}
		writeTunnelJSON(conn, wm, st, tunnelChunkMsg{Type: "chunk", ID: msg.ID, Body: base64.StdEncoding.EncodeToString(data[off:end])})
	}
	writeTunnelJSON(conn, wm, st, tunnelChunkMsg{Type: "chunk", ID: msg.ID, Done: true})
	st.update(func(m *tunnelMetrics) { m.Responses++ })
}

// writeTunnelJSON 串行化写入 WS 并累计出站字节数。
//
// v1.2.1 修复：v1.2.0 的 BytesOut 只被声明与读取、从未累加，
// 导致 WebUI「收发字节」恒显示 0 B（由稳定性长跑指标发现）。
// 这里改用 Marshal + WriteMessage，既避免 WriteJSON 的二次序列化，
// 又能精确统计实际写入的字节数。
func writeTunnelJSON(conn *websocket.Conn, wm *sync.Mutex, st *tunnelState, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	wm.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(20 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, raw)
	wm.Unlock()
	if err == nil && st != nil {
		n := int64(len(raw))
		st.update(func(m *tunnelMetrics) { m.BytesOut += n })
	}
}

// ---------- 隧道状态命令 ----------

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
	if p, ok := readPidFile(tunnelPidPath()); ok {
		running = true
		pid = p
	}
	cfg, _ := loadConfig()

	out := map[string]any{
		"running":          running,
		"pid":              pid,
		"enabled":          cfg.Tunnel.Enabled,
		"server":           cfg.Tunnel.Server,
		"device":           cfg.Tunnel.Device,
		"ip":               cfg.Tunnel.IP,
		"token":            cfg.Tunnel.Token != "",
		"connected":        false,
		"healthy":          false,
		"state":            "stopped",
		"heartbeat_sec":    int(tunnelPingInterval.Seconds()),
		"read_timeout_sec": int(tunnelReadTimeout.Seconds()),
	}

	// 优先使用隧道客户端落盘的真实连接态（v1.1.0 只看 pid，会把卡死误报为「已连接」）
	if m, err := loadTunnelMetrics(); err == nil {
		out["state"] = m.State
		out["connected"] = m.Connected
		out["healthy"] = m.Healthy
		out["server"] = firstNonEmpty(m.Server, cfg.Tunnel.Server)
		out["device"] = firstNonEmpty(m.Device, cfg.Tunnel.Device)
		out["ip"] = firstNonEmpty(m.IP, cfg.Tunnel.IP)
		out["server_version"] = m.ServerVersion
		out["stream_ok"] = m.StreamOK
		out["metrics"] = m
		out["runtime_age_sec"] = time.Now().Unix() - m.UpdatedAt
		if !running {
			// pid 已消失但运行态还是 connected：进程异常退出，修正展示
			out["state"] = "stopped"
			out["connected"] = false
			out["healthy"] = false
		}
	}
	if !running {
		out["state"] = "stopped"
	}
	// 公网接入地址（远端 MCP 客户端使用）
	out["public_url"] = publicMCPURL(cfg)
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}

// publicMCPURL 由隧道配置推导公网 MCP 端点（wss://host/tunnel → https://host/mcp/<device>）
func publicMCPURL(cfg *Config) string {
	server := strings.TrimSpace(cfg.Tunnel.Server)
	if server == "" {
		return ""
	}
	// wss→https、ws→http
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := "https"
	if u.Scheme == "ws" || u.Scheme == "http" {
		scheme = "http"
	}
	device := strings.TrimSpace(cfg.Tunnel.Device)
	if device == "" {
		device = defaultTunnelDevice
	}
	return fmt.Sprintf("%s://%s/mcp/%s", scheme, u.Host, device)
}
