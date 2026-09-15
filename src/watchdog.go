package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// watchdog.go — 进程守护
//
// 设计目标：无论从 KSU Manager WebUI、终端、还是开机 service.sh 启动，
// mcpd daemon 与隧道客户端都必须保持后台常驻、异常自动恢复。
//
// watchdog 每 10s 巡检：
//   - daemon 未运行 → 拉起
//   - daemon 活着但 /health 连续 3 次探活失败 → 强杀重启
//   - 隧道进程未运行 → 拉起
//   - 隧道进程活着但运行态文件超时未刷新（且已过其计划重连时间）→ 判定卡死并强杀重启
//
// v1.1.0 的 ensureTunnel **只判 pid 存活**，隧道进程卡在重连循环里永远不会被干预；
// v1.2.0 改为「存活 + 新鲜度」双判定，这是问题 6.1 的直接修复。

func cmdWatchdog(args []string) int {
	detached := false
	for _, a := range args {
		if a == "--detach" || a == "--daemonize" {
			detached = true
		}
	}

	// 守护化：setsid 拉起自身后立即返回
	if detached && os.Getenv("MCPD_WATCHDOG_DETACHED") != "1" {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法定位自身路径: %v\n", err)
			return 1
		}
		if err := os.MkdirAll(dataDir(), 0755); err != nil {
			fmt.Fprintf(os.Stderr, "无法创建数据目录: %v\n", err)
			return 1
		}
		logF, err := os.OpenFile(watchdogLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法打开日志文件: %v\n", err)
			return 1
		}
		defer logF.Close()
		child := exec.Command(self, "watchdog")
		child.Stdout = logF
		child.Stderr = logF
		child.Stdin = nil
		child.Env = append(os.Environ(), "MCPD_WATCHDOG_DETACHED=1")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "启动守护进程失败: %v\n", err)
			return 1
		}
		fmt.Printf("mcpd 守护进程已启动 (pid %d)，日志: %s\n", child.Process.Pid, watchdogLogPath())
		return 0
	}

	// 已在独立会话中运行：回避 Manager cgroup，并写 pid 文件
	tryEscapeCgroup()
	if pid, alive := readPidFile(watchdogPidPath()); alive {
		fmt.Fprintf(os.Stderr, "watchdog 已在运行 (pid %d)\n", pid)
		return 0
	}
	if err := os.MkdirAll(dataDir(), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "无法创建数据目录: %v\n", err)
		return 1
	}
	if err := os.WriteFile(watchdogPidPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "无法写入 pid 文件: %v\n", err)
		return 1
	}
	defer os.Remove(watchdogPidPath())

	logf("watchdog 已启动 (pid %d)，每 %ds 巡检 mcpd daemon 与 tunnel（隧道心跳 %s / 判死 %s）",
		os.Getpid(), int(watchdogInterval.Seconds()), tunnelPingInterval, tunnelReadTimeout)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// 启动即巡检一次，随后按周期执行
	deadStreak := 0
	staleStreak := 0
	cfg, _ := loadConfig()
	watchdogTick(cfg, &deadStreak, &staleStreak)
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sigCh:
			logf("watchdog 收到退出信号，停止守护")
			return 0
		case <-ticker.C:
			cfg, err := loadConfig()
			if err != nil {
				logf("watchdog 读取配置失败: %v", err)
				continue
			}
			watchdogTick(cfg, &deadStreak, &staleStreak)
		}
	}
}

func watchdogTick(cfg *Config, deadStreak, staleStreak *int) {
	if daemonDisabled() {
		*deadStreak = 0
		*staleStreak = 0
		return
	}
	ensureDaemon(deadStreak)
	if cfg.Tunnel.Enabled {
		ensureTunnel(cfg, staleStreak)
	}
}

// ensureDaemon 保证 mcpd daemon 运行并可响应健康检查
func ensureDaemon(deadStreak *int) {
	if pid, alive := readPid(); alive {
		if healthOK() {
			*deadStreak = 0
			return
		}
		*deadStreak++
		logf("watchdog: daemon (pid %d) 探活失败 (%d/%d)", pid, *deadStreak, watchdogFailMax)
		if *deadStreak < watchdogFailMax {
			return
		}
		// 进程活着但服务无响应（卡死）：强杀后走拉起流程
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
		_ = os.Remove(pidPath())
		*deadStreak = 0
	} else {
		*deadStreak = 0
	}
	spawnDetached([]string{"daemon", "--detach"}, "MCPD_DETACHED=1", logPath(), "拉起 mcpd daemon")
}

// ensureTunnel 保证隧道客户端「活着且在工作」。
//
// 判定逻辑（v1.2.0）：
//  1. pid 不存在 → 配置完整则拉起
//  2. pid 存在且运行态文件缺失 → 计一次异常（进程刚启动时可能尚未落盘）
//  3. 运行态 UpdatedAt 距今超过 tunnelStaleAfter，且已过 NextRetryAt（不在合法退避等待中）
//     → 连续 watchdogFailMax 次判定卡死，强杀并重启进程
//  4. 运行态显示持续重连且失败次数很高 → 打印显著告警（配置/鉴权类问题不会自愈，
//     但保留自动重连，不做无意义重启）
func ensureTunnel(cfg *Config, staleStreak *int) {
	pid, alive := readPidFile(tunnelPidPath())
	if !alive {
		*staleStreak = 0
		if cfg.Tunnel.Server == "" || cfg.Tunnel.Device == "" || cfg.Tunnel.Token == "" {
			return
		}
		_ = os.Remove(tunnelRuntimePath()) // 清掉上次进程遗留的状态，避免误导 WebUI
		spawnDetached([]string{"tunnel", "--detach"}, "MCPD_TUNNEL_DETACHED=1", tunnelLogPath(), "拉起 tunnel 客户端")
		return
	}

	now := time.Now().Unix()
	m, err := loadTunnelMetrics()
	if err != nil {
		*staleStreak++
		logf("watchdog: 隧道进程 %d 存活但运行态不可读 (%d/%d): %v", pid, *staleStreak, watchdogFailMax, err)
		if *staleStreak >= watchdogFailMax {
			restartStuckTunnel(pid, "运行态文件不可读")
			*staleStreak = 0
		}
		return
	}

	age := now - m.UpdatedAt
	inRetryWindow := m.NextRetryAt > now
	switch {
	case inRetryWindow:
		// 正在按退避计划等待重连：属正常行为
		*staleStreak = 0
	case age > int64(tunnelStaleAfter.Seconds()):
		*staleStreak++
		logf("watchdog: 隧道运行态已 %ds 未刷新（阈值 %ds，state=%s，已过重连时刻）(%d/%d)",
			age, int(tunnelStaleAfter.Seconds()), m.State, *staleStreak, watchdogFailMax)
		if *staleStreak >= watchdogFailMax {
			restartStuckTunnel(pid, fmt.Sprintf("运行态停滞 %ds", age))
			*staleStreak = 0
		}
	default:
		*staleStreak = 0
	}

	// 长时间重连失败告警（含鉴权/配置类错误提示）
	if !m.Connected && m.ConsecutiveFails >= 5 {
		if isAuthLikeError(m.LastError) {
			logf("watchdog: 隧道疑似鉴权/配置错误（连续失败 %d 次），请检查 tunnelToken 与 device 名: %s",
				m.ConsecutiveFails, m.LastError)
		} else if m.ConsecutiveFails%30 == 0 {
			logf("watchdog: 隧道持续重连中（连续失败 %d 次）: %s", m.ConsecutiveFails, m.LastError)
		}
	}
}

// isAuthLikeError 粗略识别鉴权/配置类失败（这类错误重试不会自愈，需要提示用户）
func isAuthLikeError(msg string) bool {
	low := strings.ToLower(msg)
	for _, k := range []string{"unauthorized", "401", "403", "forbidden", "bad handshake", "token"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// restartStuckTunnel 强杀卡死的隧道进程并重新拉起
func restartStuckTunnel(pid int, reason string) {
	logf("watchdog: 隧道进程 %d 卡死（%s），强杀并重启", pid, reason)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(300 * time.Millisecond)
	_ = os.Remove(tunnelPidPath())
	_ = os.Remove(tunnelRuntimePath())
	spawnDetached([]string{"tunnel", "--detach"}, "MCPD_TUNNEL_DETACHED=1", tunnelLogPath(), "重启 tunnel 客户端")
}

// spawnDetached 以 setsid 独立会话拉起 mcpd 子命令（日志重定向到指定文件）
func spawnDetached(args []string, envKV, logFile, action string) {
	self, err := os.Executable()
	if err != nil {
		logf("watchdog: %s失败（定位自身路径失败）: %v", action, err)
		return
	}
	logF, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		logF = os.Stderr
	}
	child := exec.Command(self, args...)
	child.Stdout = logF
	child.Stderr = logF
	child.Stdin = nil
	child.Env = append(os.Environ(), envKV)
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		logf("watchdog: %s失败: %v", action, err)
		return
	}
	// 异步 Wait 回收子进程退出状态，避免其成为僵尸进程
	go func() { _ = child.Wait() }()
	logf("watchdog: %s (pid %d)", action, child.Process.Pid)
}

// healthOK 通过 /health 探活本地 MCP 服务（读取运行中进程实际端口）
func healthOK() bool {
	port := defaultPort
	if data, err := os.ReadFile(runtimePath()); err == nil {
		var live Config
		if json.Unmarshal(data, &live) == nil && live.Port > 0 {
			port = live.Port
		}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func cmdWatchdogStop() int {
	pid, alive := readPidFile(watchdogPidPath())
	if !alive {
		fmt.Println("watchdog 未在运行")
		return 0
	}
	stopPid(pid, watchdogPidPath())
	fmt.Printf("watchdog 已停止 (pid %d)\n", pid)
	return 0
}

func cmdWatchdogStatus() int {
	running := false
	pid := 0
	if p, ok := readPidFile(watchdogPidPath()); ok {
		running = true
		pid = p
	}
	out := map[string]any{
		"running":          running,
		"pid":              pid,
		"interval_sec":     int(watchdogInterval.Seconds()),
		"fail_max":         watchdogFailMax,
		"tunnel_stale_sec": int(tunnelStaleAfter.Seconds()),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	// 退出码即结论：0=运行中，2=未运行。
	// 供 service.sh / boot-completed.sh 可靠判断，替代 v1.1.0 中
	// `grep -q '"running": true'` 这种一旦 JSON 格式微调就失效的脆弱写法。
	if !running {
		return 2
	}
	return 0
}
