package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// control.go — 守护进程、启停、状态与配置命令

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
	// 尽力从发起端（KSU Manager 桥接会话）的 cgroup 中独立出来
	tryEscapeCgroup()
	// 记录实际生效配置，供 status / WebUI / watchdog 读取真实监听参数
	if data, err := json.Marshal(cfg); err == nil {
		_ = writeFileAtomic(runtimePath(), data, 0600)
	}
	defer os.Remove(runtimePath())

	srv := newMCPServer(cfg)
	globalServer = srv
	addr := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	logf("MCP server 启动: http://%s/mcp (Streamable HTTP) / http://%s/sse (legacy) (pid %d)",
		addr, addr, os.Getpid())
	logf("协议版本: %s（支持 %s）| 工具: %d 个（规范命名 %d / 弃用别名 %d）",
		protocolVersion, strings.Join(supportedProtocols, ", "),
		len(toolNames()), len(canonicalToolNames()), len(toolNames())-len(canonicalToolNames()))
	if cfg.Token == "" {
		logf("警告: 当前无鉴权 Token（--no-auth），请勿暴露端口")
	}
	if !isLoopback(cfg.Bind) {
		logf("警告: 正在监听 %s（局域网可访问），请确保 Token 强度并定期轮换", cfg.Bind)
	}
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.mux(),
		// 注意：WriteTimeout 必须为 0，否则会掐断 SSE / Streamable HTTP GET 长流
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// 优雅退出：收到 SIGTERM/SIGINT 时先 Shutdown 让活动连接完成，再清理 pid 文件
	srvErr := make(chan error, 1)
	go func() { srvErr <- httpSrv.ListenAndServe() }()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sigCh:
		logf("收到退出信号，正在优雅关闭")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = httpSrv.Shutdown(ctx)
		cancel()
	case err = <-srvErr:
	}
	logf("服务已停止: %v", err)
	return 0
}

// cmdStop 停止全部：先隧道（停止转发），再 daemon，最后 watchdog（防止被自动拉起）
func cmdStop() int {
	stoppedAny := false
	if pid, alive := readPidFile(tunnelPidPath()); alive {
		stopPid(pid, tunnelPidPath())
		_ = os.Remove(tunnelRuntimePath())
		fmt.Printf("tunnel 已停止 (pid %d)\n", pid)
		stoppedAny = true
	}
	if pid, alive := readPid(); alive {
		stopPid(pid, pidPath())
		fmt.Printf("mcpd 已停止 (pid %d)\n", pid)
		stoppedAny = true
	}
	if pid, alive := readPidFile(watchdogPidPath()); alive {
		stopPid(pid, watchdogPidPath())
		fmt.Printf("watchdog 已停止 (pid %d)\n", pid)
		stoppedAny = true
	}
	if !stoppedAny {
		fmt.Println("mcpd 服务未在运行")
	}
	return 0
}

// cmdStart 启动全部服务（等价于 watchdog --detach）
func cmdStart() int {
	if daemonDisabled() {
		fmt.Fprintf(os.Stderr, "注意：开机自启已被禁用（存在 %s/disabled 标记），但本次仍会立即启动服务。\n",
			dataDir())
	}
	if pid, alive := readPidFile(watchdogPidPath()); alive {
		fmt.Printf("watchdog 已在运行 (pid %d)\n", pid)
		return 0
	}
	return spawnWatchdogAndWait()
}

// cmdRestart 重启全部服务：stop → start，供 WebUI 一键重启使用
//
// v1.1.0 的 WebUI 用 "stop; sleep 1; watchdog --detach" 拼命令，无错误传播且
// 依赖固定 sleep；本命令在进程内完成停止与拉起，更可靠。
func cmdRestart() int {
	cmdStop()
	time.Sleep(600 * time.Millisecond)
	return spawnWatchdogAndWait()
}

// spawnWatchdogAndWait 以 setsid 拉起 watchdog 并等待其 pid 文件出现（最多 3s）
func spawnWatchdogAndWait() int {
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
	go func() { _ = child.Wait() }()
	for i := 0; i < 30; i++ {
		if _, ok := readPidFile(watchdogPidPath()); ok {
			fmt.Printf("mcpd 守护进程已启动 (pid %d)，正在拉起 daemon 与隧道\n", child.Process.Pid)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("已拉起 watchdog (pid %d)，但暂未观察到 pid 文件，请用 mcpd status 确认\n", child.Process.Pid)
	return 0
}

// cmdStatus 输出运行状态（JSON）—— 与 WebUI 的 /api/state 使用同一份聚合数据
func cmdStatus() int {
	b, _ := json.MarshalIndent(buildUIState(nil), "", "  ")
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
	source := "mcpd"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--source":
			if i+1 < len(args) {
				source = args[i+1]
				i++
			}
		default:
			if v, err := strconv.Atoi(args[i]); err == nil && v > 0 {
				lines = v
			}
		}
	}
	out, errStr := tailLog(source, lines)
	if errStr != "" {
		fmt.Fprintln(os.Stderr, errStr)
		return 0
	}
	fmt.Println(strings.Join(out, "\n"))
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
