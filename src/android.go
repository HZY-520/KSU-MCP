package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// android.go — Android 设备能力采集层
//
// 本文件集中所有「调用系统命令 / 读取 sysfs / 解析 /proc」的逻辑，
// 供 tools.go 中的 MCP 工具复用。设计原则：
//  1. 能用 Go 标准库直接读的（网卡、statfs、/proc）优先用标准库，避免 fork；
//  2. 需要系统命令时一律走 runCmdOut（直接 exec，不经 shell），杜绝命令注入；
//  3. 只有在确实需要管道/重定向时才用 runShell，且调用方必须先 shellQuote；
//  4. 所有采集函数都返回可 JSON 序列化的结构，字段缺失时为 null 而非空串，
//     便于 AI Agent 稳定解析。

// ---------- 命令执行 ----------

// shellQuote 用单引号包裹字符串，安全地嵌入 sh -c 命令串
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// lookBin 解析系统命令路径：PATH → /system/bin → /system/xbin
func lookBin(name string) string {
	if strings.Contains(name, "/") {
		return name
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/system/bin", "/system/xbin", "/vendor/bin", "/sbin"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

// runCmdOut 直接执行命令（不经 shell），合并 stdout/stderr 返回
func runCmdOut(name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command(lookBin(name), args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// cmdResult 结构化命令执行结果
type cmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// runShellArgs 通过 /system/bin/sh -c 执行命令串，带超时
func runShellArgs(cmdStr string, timeout time.Duration) cmdResult {
	return runShellCwd(cmdStr, timeout, "")
}

// runShellCwd 同 runShellArgs，但可指定工作目录（供 exec_command 的 cwd 参数使用）
func runShellCwd(cmdStr string, timeout time.Duration, cwd string) cmdResult {
	shell := "/system/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		shell = "sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-c", cmdStr)
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := cmdResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var ee *exec.ExitError
		switch {
		case ctx.Err() != nil:
			res.TimedOut = true
			res.ExitCode = 124
		case asExit(err, &ee):
			res.ExitCode = ee.ExitCode()
		default:
			res.ExitCode = -1
		}
	}
	return res
}

func asExit(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// cmdOK 判断命令是否可用（用于能力探测）
func cmdOK(name string) bool {
	p := lookBin(name)
	if strings.Contains(p, "/") {
		_, err := os.Stat(p)
		return err == nil
	}
	return false
}

// ---------- 系统属性 ----------

func getprop(key string) string {
	out, err := runCmdOut("getprop", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// getpropAll 一次性读取全部系统属性
func getpropAll() map[string]string {
	out, err := runCmdOut("getprop")
	props := map[string]string{}
	if err != nil {
		return props
	}
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
	return props
}

// ---------- 设备 / Root 环境 ----------

// rootEnv 描述 root 方案与版本信息
type rootEnv struct {
	Root       bool           `json:"root"`
	UID        int            `json:"uid"`
	KernelSU   bool           `json:"kernelsu"`
	KSUVersion string         `json:"kernelsu_version"`
	KSUTools   string         `json:"kernelsu_manager_or_cli"`
	Magisk     bool           `json:"magisk"`
	MagiskVer  string         `json:"magisk_version"`
	Module     string         `json:"module_version"`
	SuperSU    bool           `json:"supersu"`
	Details    map[string]any `json:"details,omitempty"`
}

// detectRootEnv 探测 root 方案（KernelSU / Magisk / 其他）及版本
// detectRootEnv 探测 root 方案（KernelSU / Magisk / 其他）及版本
//
// 判定不只看目录：部分设备上 KernelSU 以 LKM/内核内置方式运行，未必有
// /data/adb/ksu，但 ksud 命令一定可用；反之亦然。两者取并集。
func detectRootEnv() rootEnv {
	env := rootEnv{
		Root:    os.Getuid() == 0,
		UID:     os.Getuid(),
		Details: map[string]any{},
	}
	ksuDir := dirExists("/data/adb/ksu")
	ksudBin := cmdOK("ksud") || cmdOK("ksu")
	magiskDir := dirExists("/data/adb/magisk") || dirExists("/data/adb/modules/magisk")
	magiskBin := cmdOK("magisk")

	env.KernelSU = ksuDir || ksudBin
	env.Magisk = magiskDir || magiskBin
	env.SuperSU = dirExists("/data/adb/su") || fileExists("/system/xbin/su")

	// KernelSU 版本：ksud 命令 → 属性 → 内核版本串
	if env.KernelSU {
		for _, c := range [][]string{{"ksud", "-V"}, {"ksud", "--version"}, {"ksu", "-V"}} {
			if !cmdOK(c[0]) {
				continue
			}
			if out, err := runCmdOut(c[0], c[1:]...); err == nil && strings.TrimSpace(out) != "" {
				env.KSUVersion = strings.TrimSpace(strings.Split(out, "\n")[0])
				env.KSUTools = c[0]
				break
			}
		}
	}
	if env.KSUVersion == "" {
		if kv := getprop("ro.kernelsu.version"); kv != "" {
			env.KSUVersion = kv
		}
	}
	if env.KSUVersion == "" && env.KernelSU {
		if b, err := os.ReadFile("/proc/version"); err == nil {
			trimmed := strings.TrimSpace(string(b))
			if i := strings.Index(strings.ToLower(trimmed), "kernelsu"); i >= 0 {
				env.KSUVersion = strings.TrimSpace(trimmed[maxInt(0, i-2):])
			}
		}
	}
	if env.Magisk && magiskBin {
		if out, err := runCmdOut("magisk", "-V"); err == nil {
			env.MagiskVer = strings.TrimSpace(strings.Split(out, "\n")[0])
		}
	}
	if env.MagiskVer == "" && env.Magisk {
		if b, err := os.ReadFile("/data/adb/magisk/util_functions.sh"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "MAGISK_VER=") {
					env.MagiskVer = strings.Trim(strings.TrimPrefix(line, "MAGISK_VER="), "\"'")
					break
				}
			}
		}
	}
	if env.Module = moduleSelfVersion(); env.Module != "" {
		env.Details["ksu_mcp_module"] = env.Module
	}
	env.Details["kernelsu_dir"] = ksuDir
	env.Details["magisk_dir"] = magiskDir
	return env
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// moduleSelfVersion 读取本模块 module.prop 的版本号
func moduleSelfVersion() string {
	for _, p := range []string{"/data/adb/modules/ksu_mcp/module.prop", "/data/adb/modules_update/ksu_mcp/module.prop"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "version=") {
				return strings.TrimSpace(strings.TrimPrefix(line, "version="))
			}
		}
	}
	return ""
}

// collectDeviceInfo 采集设备静态信息（对应 android_get_device_info）
func collectDeviceInfo() map[string]any {
	kernel := ""
	if b, err := os.ReadFile("/proc/version"); err == nil {
		kernel = strings.TrimSpace(string(b))
	}
	selinux, _ := runCmdOut("getenforce")
	uptime := int64(0)
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				uptime = int64(v)
			}
		}
	}
	root := detectRootEnv()
	info := map[string]any{
		"brand":               nilIfEmpty(getprop("ro.product.brand")),
		"manufacturer":        nilIfEmpty(getprop("ro.product.manufacturer")),
		"model":               nilIfEmpty(getprop("ro.product.model")),
		"device":              nilIfEmpty(getprop("ro.product.device")),
		"product":             nilIfEmpty(getprop("ro.product.name")),
		"android":             nilIfEmpty(getprop("ro.build.version.release")),
		"sdk":                 nilIfEmpty(getprop("ro.build.version.sdk")),
		"build_id":            nilIfEmpty(getprop("ro.build.id")),
		"build_type":          nilIfEmpty(getprop("ro.build.type")),
		"security_patch":      nilIfEmpty(getprop("ro.build.version.security_patch")),
		"abi":                 nilIfEmpty(getprop("ro.product.cpu.abi")),
		"abi_list":            nilIfEmpty(getprop("ro.product.cpu.abilist")),
		"fingerprint":         nilIfEmpty(getprop("ro.build.fingerprint")),
		"bootloader":          nilIfEmpty(getprop("ro.bootloader")),
		"kernel":              nilIfEmpty(kernel),
		"selinux":             nilIfEmpty(selinux),
		"uptime_seconds":      uptime,
		"root":                root,
		"mcpd_version":        appVersion,
		"supported_protocols": supportedProtocols,
		"time":                time.Now().Format(time.RFC3339),
	}
	return info
}

// ---------- 电池 ----------

// collectBattery 读取电池状态（对应 android_get_battery_status）
func collectBattery() map[string]any {
	base := "/sys/class/power_supply/battery"
	readFileTrim := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	read := func(f string) string { return readFileTrim(filepath.Join(base, f)) }

	out := map[string]any{
		"level_percent":      atoiOrNil(read("capacity")),
		"status":             nilIfEmpty(read("status")),
		"health":             nilIfEmpty(read("health")),
		"present":            boolOrNil(read("present")),
		"technology":         nilIfEmpty(read("technology")),
		"temperature_c":      nilIfEmpty(scaleOrNil(read("temp"), 10.0)),
		"voltage_v":          nilIfEmpty(scaleOrNil(read("voltage_now"), 1e6)),
		"current_now_ma":     nilIfEmpty(scaleOrNil(read("current_now"), 1000.0)),
		"charge_counter_uah": atoiOrNil(read("charge_counter")),
		"charge_full_uah":    atoiOrNil(read("charge_full")),
		"cycle_count":        atoiOrNil(read("cycle_count")),
		"source":             "sysfs:/sys/class/power_supply/battery",
	}
	// 充电器/输入电源信息
	usb := "/sys/class/power_supply/usb"
	if dirExists(usb) {
		out["usb_online"] = boolOrNil(readFileTrim(filepath.Join(usb, "online")))
		out["usb_voltage_v"] = nilIfEmpty(scaleOrNil(readFileTrim(filepath.Join(usb, "voltage_now")), 1e6))
		out["usb_current_ma"] = nilIfEmpty(scaleOrNil(readFileTrim(filepath.Join(usb, "current_now")), 1000.0))
		out["usb_type"] = nilIfEmpty(readFileTrim(filepath.Join(usb, "type")))
	}
	if out["level_percent"] == nil {
		// 部分 ROM 无 sysfs 电池节点，回落 dumpsys battery
		if raw, err := runCmdOut("dumpsys", "battery"); err == nil && raw != "" {
			kv := map[string]string{}
			for _, line := range strings.Split(raw, "\n") {
				line = strings.TrimSpace(line)
				if i := strings.Index(line, ": "); i > 0 {
					kv[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+2:])
				}
			}
			if v, err := strconv.Atoi(kv["level"]); err == nil {
				out["level_percent"] = v
			}
			if v, ok := kv["status"]; ok {
				out["status"] = batteryStatusName(v)
			}
			if v, err := strconv.Atoi(kv["temperature"]); err == nil {
				out["temperature_c"] = float64(v) / 10.0
			}
			if v, err := strconv.Atoi(kv["voltage"]); err == nil {
				out["voltage_v"] = float64(v) / 1000.0
			}
			out["source"] = "dumpsys battery"
		}
	}
	return out
}

// batteryStatusName 把 dumpsys 的数字状态码转成 sysfs 风格名称
func batteryStatusName(v string) string {
	switch strings.TrimSpace(v) {
	case "1":
		return "Unknown"
	case "2":
		return "Charging"
	case "3":
		return "Discharging"
	case "4":
		return "Not charging"
	case "5":
		return "Full"
	default:
		return v
	}
}

// ---------- 存储 ----------

// collectStorage 解析 df 输出并附加关键路径的 statfs 结果
func collectStorage(pathFilter string, includePseudo bool) map[string]any {
	pseudo := map[string]bool{
		"tmpfs": true, "devpts": true, "proc": true, "sysfs": true, "cgroup": true,
		"cgroup2": true, "none": true, "debugfs": true, "tracefs": true, "bpf": true,
		"functionfs": true, "configfs": true, "selinuxfs": true, "pstore": true,
		"sockfs": true, "securityfs": true, "fusectl": true, "overlay": true,
		"devtmpfs": true, "hugetlbfs": true, "mqueue": true, "binfmt_misc": true,
	}
	fstypeByMount := mountFSTypes()

	out, err := runCmdOut("df", "-k", "-P")
	if err != nil {
		out, err = runCmdOut("df", "-k")
	}
	partitions := []map[string]any{}
	if err == nil {
		for i, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 6 {
				continue
			}
			if i == 0 && strings.HasPrefix(strings.ToLower(fields[0]), "filesystem") {
				continue // 表头
			}
			mount := strings.Join(fields[5:], " ")
			fsType := fstypeByMount[mount]
			if fsType == "" {
				fsType = fields[0]
			}
			if !includePseudo && pseudo[fsType] {
				continue
			}
			if pathFilter != "" && !strings.Contains(mount, pathFilter) {
				continue
			}
			sizeKB := atoiOrNil(fields[1])
			usedKB := atoiOrNil(fields[2])
			availKB := atoiOrNil(fields[3])
			partitions = append(partitions, map[string]any{
				"filesystem":      fields[0],
				"type":            nilIfEmpty(fsType),
				"mount":           mount,
				"size_kb":         sizeKB,
				"used_kb":         usedKB,
				"available_kb":    availKB,
				"size_bytes":      kbToBytes(sizeKB),
				"used_bytes":      kbToBytes(usedKB),
				"available_bytes": kbToBytes(availKB),
				"use_percent":     nilIfEmpty(strings.TrimSuffix(fields[4], "%")),
			})
		}
	}
	sort.Slice(partitions, func(i, j int) bool {
		return fmt.Sprint(partitions[i]["mount"]) < fmt.Sprint(partitions[j]["mount"])
	})

	// 关键路径精确容量（statfs，无需 root）
	key := []map[string]any{}
	for _, p := range []string{"/", "/data", "/data/adb", "/storage/emulated/0", "/cache", "/system"} {
		if pathFilter != "" && !strings.Contains(p, pathFilter) {
			continue
		}
		if st, ok := statfsInfo(p); ok {
			key = append(key, st)
		}
	}
	return map[string]any{
		"partitions": partitions,
		"count":      len(partitions),
		"key_paths":  key,
		"df_error":   nilIfEmpty(errText(err)),
	}
}

// mountFSTypes 解析 /proc/mounts 得到 mount → fstype 映射
func mountFSTypes() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			out[unescapeMount(f[1])] = f[2]
		}
	}
	return out
}

func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

func statfsInfo(path string) (map[string]any, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, false
	}
	bs := uint64(st.Bsize)
	total := st.Blocks * bs
	free := st.Bfree * bs
	avail := st.Bavail * bs
	used := total - free
	pct := 0.0
	if total > 0 {
		pct = float64(used) / float64(total) * 100
	}
	return map[string]any{
		"path":            path,
		"size_bytes":      total,
		"used_bytes":      used,
		"available_bytes": avail,
		"use_percent":     round1(pct),
	}, true
}

// ---------- 网络 ----------

// defaultRoute 返回默认路由的网卡名与网关 IP（解析 /proc/net/route）
func defaultRoute() (iface string, gateway string) {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return "", ""
	}
	for i, line := range strings.Split(string(b), "\n") {
		if i == 0 {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if f[1] != "00000000" {
			continue
		}
		return f[0], hexLEToIP(f[2])
	}
	return "", ""
}

// hexLEToIP 把 /proc/net/route 的小端十六进制地址转成点分十进制
func hexLEToIP(h string) string {
	if len(h) != 8 {
		return ""
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", v&0xff, (v>>8)&0xff, (v>>16)&0xff, (v>>24)&0xff)
}

// collectNetwork 采集网络信息（对应 android_get_network_info）
func collectNetwork() map[string]any {
	ifaces := netIfaces()
	defIface, gw := defaultRoute()
	kind := ""
	ips := []string{}
	activeIP := ""
	for _, n := range ifaces {
		if n.Name == defIface {
			kind = n.Kind
			ips = n.IPs
			if len(n.IPs) > 0 {
				activeIP = n.IPs[0]
			}
		}
	}
	dns := []string{}
	for _, k := range []string{"net.dns1", "net.dns2", "net.dns3", "net.dns4"} {
		if v := getprop(k); v != "" {
			dns = append(dns, v)
		}
	}
	info := map[string]any{
		"active_interface": nilIfEmpty(defIface),
		"active_type":      nilIfEmpty(kind),
		"active_ip":        nilIfEmpty(activeIP),
		"active_ips":       ips,
		"gateway":          nilIfEmpty(gw),
		"dns":              dns,
		"hostname":         nilIfEmpty(hostnameOrEmpty()),
		"interfaces":       ifaces,
		"lan_ips":          lanIPs(),
		"airplane_mode":    boolOrNil(settingsGet("global", "airplane_mode_on")),
		"wifi_enabled":     wifiEnabled(),
		"wifi_ssid":        nilIfEmpty(wifiSSID()),
		"wifi_ip":          nilIfEmpty(firstIPOfKind(ifaces, "wifi")),
		"cellular_ip":      nilIfEmpty(firstIPOfKind(ifaces, "cellular")),
		"operator":         nilIfEmpty(firstNonEmpty(getprop("gsm.operator.alpha"), getprop("gsm.sim.operator.alpha"))),
		"operator_iso":     nilIfEmpty(getprop("gsm.operator.iso-country")),
		"network_type":     nilIfEmpty(cellularType()),
		"mobile_data":      boolOrNil(settingsGet("global", "mobile_data")),
		"mcp_listen":       nilIfEmpty(listenSummary()),
	}
	return info
}

func hostnameOrEmpty() string {
	h, _ := os.Hostname()
	return h
}

func firstIPOfKind(ifaces []netIface, kind string) string {
	for _, n := range ifaces {
		if n.Kind == kind && len(n.IPs) > 0 {
			return n.IPs[0]
		}
	}
	return ""
}

// wifiEnabled 判断 WiFi 开关状态（多路探测，兼容不同 ROM）
func wifiEnabled() any {
	if out, err := runCmdOut("cmd", "wifi", "status"); err == nil {
		low := strings.ToLower(out)
		if strings.Contains(low, "wifi is enabled") || strings.Contains(low, "enabled") {
			return true
		}
		if strings.Contains(low, "wifi is disabled") || strings.Contains(low, "disabled") {
			return false
		}
	}
	return boolOrNil(settingsGet("global", "wifi_on"))
}

// wifiSSID 读取当前 WiFi SSID
func wifiSSID() string {
	if out, err := runCmdOut("cmd", "wifi", "status"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "SSID:") {
				v := strings.TrimSpace(strings.TrimPrefix(line, "SSID:"))
				if v != "" && v != "<unknown ssid>" {
					return strings.Trim(v, `"`)
				}
			}
		}
	}
	if out, err := runCmdOut("dumpsys", "wifi"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "SSID:") {
				v := strings.TrimSpace(line[strings.Index(line, "SSID:")+5:])
				v = strings.TrimSuffix(v, ",")
				if v != "" && v != "<unknown ssid>" {
					return strings.Trim(v, `"`)
				}
			}
		}
	}
	return getprop("dhcp.wlan0.ssid")
}

// cellularType 读取蜂窝网络制式
func cellularType() string {
	if v := getprop("gsm.network.type"); v != "" {
		return v
	}
	if out, err := runCmdOut("dumpsys", "telephony.registry"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "mDataConnectionState=") || strings.Contains(line, "getNetworkType") {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// listenSummary 概览 MCP 服务监听地址（供网络工具展示公网/局域网可达性）
func listenSummary() string {
	cfg, err := loadConfig()
	if err != nil || cfg == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port)
}

// ---------- 应用包 ----------

// collectPackages 列出已安装应用（对应 android_list_packages，支持过滤与分页）
//
// state 取值：all（默认）/ third_party / system / enabled / disabled
func collectPackages(filter, state string, offset, limit int, sortDesc bool) map[string]any {
	args := []string{"list", "packages"}
	switch state {
	case "third_party":
		args = append(args, "-3")
	case "system":
		args = append(args, "-s")
	case "enabled":
		args = append(args, "-e")
	case "disabled":
		args = append(args, "-d")
	default:
		state = "all"
	}
	out, err := runCmdOut("pm", args...)
	if err != nil {
		return map[string]any{"error": "pm 不可用: " + errText(err), "packages": []string{}, "count": 0}
	}
	lf := strings.ToLower(filter)
	all := []string{}
	for _, line := range strings.Split(out, "\n") {
		pkg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "package:"))
		if pkg == "" {
			continue
		}
		// 去掉 pm -f 风格的 "包名=路径" 后缀
		if i := strings.Index(pkg, "="); i > 0 {
			pkg = pkg[:i]
		}
		if lf != "" && !strings.Contains(strings.ToLower(pkg), lf) {
			continue
		}
		all = append(all, pkg)
	}
	sort.Strings(all)
	if sortDesc {
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
	}
	total := len(all)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	page := all[offset:end]
	return map[string]any{
		"total":    total,
		"offset":   offset,
		"limit":    limit,
		"count":    len(page),
		"has_more": end < total,
		"state":    state,
		"filter":   nilIfEmpty(filter),
		"packages": page,
	}
}

// ---------- 进程 ----------

// collectProcesses 获取运行进程列表（对应 android_get_running_processes）
func collectProcesses(filter, user, sortBy string, limit int) map[string]any {
	raw, err := runCmdOut("ps", "-A", "-o", "PID,PPID,USER,RSS,VSZ,NAME")
	header := []string{}
	rows := []map[string]any{}
	if err != nil || !strings.Contains(raw, "PID") {
		// 回落到不带 -o 的 ps，自行按列位置解析
		raw, err = runCmdOut("ps", "-A")
		if err != nil {
			return map[string]any{"error": "ps 不可用: " + errText(err), "processes": []any{}, "count": 0}
		}
		rows = parsePsLegacy(raw)
	} else {
		lines := strings.Split(raw, "\n")
		for i, line := range lines {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			if i == 0 || strings.EqualFold(f[0], "PID") {
				header = f
				continue
			}
			if len(f) < len(header) {
				continue
			}
			row := map[string]any{}
			for ci, col := range header {
				val := f[ci]
				switch strings.ToUpper(col) {
				case "PID", "PPID", "RSS", "VSZ":
					row[strings.ToLower(col)] = atoiOrNil(val)
				default:
					row[strings.ToLower(col)] = val
				}
			}
			// NAME 可能含空格，取剩余部分
			if len(f) > len(header) {
				rest := strings.Join(f[len(header)-1:], " ")
				row["name"] = rest
			}
			rows = append(rows, row)
		}
	}
	lf := strings.ToLower(filter)
	uf := strings.ToLower(user)
	out := rows[:0]
	for _, r := range rows {
		name := fmt.Sprint(r["name"])
		if lf != "" && !strings.Contains(strings.ToLower(name), lf) {
			continue
		}
		if uf != "" && !strings.Contains(strings.ToLower(fmt.Sprint(r["user"])), uf) {
			continue
		}
		out = append(out, r)
	}
	less := func(i, j int) bool { return fmt.Sprint(out[i]["pid"]) < fmt.Sprint(out[j]["pid"]) }
	switch sortBy {
	case "mem":
		less = func(i, j int) bool { return numOf(out[i]["rss"]) > numOf(out[j]["rss"]) }
	case "vsz":
		less = func(i, j int) bool { return numOf(out[i]["vsz"]) > numOf(out[j]["vsz"]) }
	case "name":
		less = func(i, j int) bool { return fmt.Sprint(out[i]["name"]) < fmt.Sprint(out[j]["name"]) }
	case "pid":
		less = func(i, j int) bool { return numOf(out[i]["pid"]) < numOf(out[j]["pid"]) }
	}
	sort.SliceStable(out, less)
	total := len(out)
	if limit > 0 && limit < total {
		out = out[:limit]
	}
	return map[string]any{
		"total":     total,
		"count":     len(out),
		"filter":    nilIfEmpty(filter),
		"user":      nilIfEmpty(user),
		"sort_by":   sortBy,
		"processes": out,
	}
}

// parsePsLegacy 解析无 -o 参数的 ps -A 输出（Android toybox 默认列）
func parsePsLegacy(raw string) []map[string]any {
	rows := []map[string]any{}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if i == 0 && strings.EqualFold(f[0], "USER") {
			continue
		}
		// 默认列： USER PID PPID VSZ RSS WCHAN ADDR S NAME...
		row := map[string]any{"user": f[0]}
		for idx, key := range []string{"pid", "ppid", "vsz", "rss"} {
			if idx+1 < len(f) {
				row[key] = atoiOrNil(f[idx+1])
			}
		}
		if len(f) >= 9 {
			row["name"] = strings.Join(f[8:], " ")
		} else {
			row["name"] = f[len(f)-1]
		}
		rows = append(rows, row)
	}
	return rows
}

// ---------- logcat ----------

// collectLogcat 抓取最近日志（对应 android_get_logcat）
func collectLogcat(tags []string, priority, buffer, filter string, lines int) map[string]any {
	args := []string{"-d", "-v", "threadtime"}
	if buffer != "" && buffer != "all" {
		args = append(args, "-b", buffer)
	} else if buffer == "all" {
		for _, b := range []string{"main", "system", "crash"} {
			args = append(args, "-b", b)
		}
	}
	if lines > 0 {
		args = append(args, "-t", strconv.Itoa(lines))
	}
	if len(tags) > 0 {
		args = append(args, "-s")
		for _, t := range tags {
			args = append(args, t+":"+priority)
		}
	}
	out, err := runCmdOut("logcat", args...)
	if err != nil && strings.TrimSpace(out) == "" {
		return map[string]any{"error": "logcat 不可用: " + errText(err), "lines": []string{}, "count": 0}
	}
	all := strings.Split(strings.TrimRight(out, "\n"), "\n")
	kept := make([]string, 0, len(all))
	for _, l := range all {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(l), strings.ToLower(filter)) {
			continue
		}
		kept = append(kept, l)
	}
	return map[string]any{
		"count":    len(kept),
		"tags":     tags,
		"priority": priority,
		"buffer":   nilIfEmpty(buffer),
		"filter":   nilIfEmpty(filter),
		"lines":    kept,
	}
}

// ---------- 输入注入 ----------

// inputText 向当前焦点注入文本（对应 android_input_text）
func inputText(text string) (string, bool) {
	if text == "" {
		return "text 不能为空", false
	}
	// 直接 exec 传单参数，避免任何 shell 解析；Android 的 input 会把 text 之后的参数以空格连接，
	// 单参数即原样文本，因此空格/引号/反斜杠均不会被破坏
	out, err := runCmdOut("input", "text", text)
	if err != nil {
		return fmt.Sprintf("input text 失败: %s %s", errText(err), out), false
	}
	return out, true
}

// inputKey 注入按键（对应 android_input_key）
func inputKey(code string) (string, bool) {
	if code == "" {
		return "keycode 不能为空（可用 KEYCODE_HOME 或 3）", false
	}
	out, err := runCmdOut("input", "keyevent", code)
	if err != nil {
		return fmt.Sprintf("input keyevent 失败: %s %s", errText(err), out), false
	}
	return out, true
}

// inputTap 注入点击（对应 android_input_tap）
func inputTap(x, y int) (string, bool) {
	out, err := runCmdOut("input", "tap", strconv.Itoa(x), strconv.Itoa(y))
	if err != nil {
		return fmt.Sprintf("input tap 失败: %s %s", errText(err), out), false
	}
	return out, true
}

// inputSwipe 注入滑动（对应 android_input_swipe）
func inputSwipe(x1, y1, x2, y2, durationMs int) (string, bool) {
	args := []string{"swipe", strconv.Itoa(x1), strconv.Itoa(y1), strconv.Itoa(x2), strconv.Itoa(y2)}
	if durationMs > 0 {
		args = append(args, strconv.Itoa(durationMs))
	}
	out, err := runCmdOut("input", args...)
	if err != nil {
		return fmt.Sprintf("input swipe 失败: %s %s", errText(err), out), false
	}
	return out, true
}

// ---------- 系统设置 ----------

// settingSpec 描述一个可读写的系统开关
type settingSpec struct {
	Namespace string
	Key       string
	Kind      string // bool / int / enum
	Min       int
	Max       int
	Stream    string
}

// settingSpecs 支持读写的系统设置白名单（键即 android_toggle_setting 的 setting 参数）
func settingSpecs() map[string]settingSpec {
	return map[string]settingSpec{
		"wifi": {
			Namespace: "global", Key: "wifi_on", Kind: "bool",
		},
		"bluetooth": {
			Namespace: "global", Key: "bluetooth_on", Kind: "bool",
		},
		"airplane_mode": {
			Namespace: "global", Key: "airplane_mode_on", Kind: "bool",
		},
		"mobile_data": {
			Namespace: "global", Key: "mobile_data", Kind: "bool",
		},
		"location": {
			Namespace: "secure", Key: "location_mode", Kind: "bool",
		},
		"auto_rotate": {
			Namespace: "system", Key: "accelerometer_rotation", Kind: "bool",
		},
		"stay_awake": {
			Namespace: "global", Key: "stay_on_while_plugged_in", Kind: "bool",
		},
		"brightness": {
			Namespace: "system", Key: "screen_brightness", Kind: "int", Min: 0, Max: 255,
		},
		"volume_music":        {Kind: "int", Min: 0, Max: 15, Stream: "3"},
		"volume_ring":         {Kind: "int", Min: 0, Max: 15, Stream: "2"},
		"volume_alarm":        {Kind: "int", Min: 0, Max: 15, Stream: "4"},
		"volume_notification": {Kind: "int", Min: 0, Max: 15, Stream: "5"},
		"dnd":                 {Namespace: "global", Kind: "enum", Key: "zen_mode"},
	}
}

// settingNames 返回支持的设置名（排序后，供 schema enum 使用）
func settingNames() []string {
	names := []string{}
	for k := range settingSpecs() {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// settingsGet 读取 settings 值
func settingsGet(ns, key string) string {
	out, err := runCmdOut("settings", "get", ns, key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// getSetting 读取指定设置的当前值（对应 android_get_setting）
func getSetting(name string) (map[string]any, bool) {
	spec, ok := settingSpecs()[name]
	if !ok {
		return map[string]any{"error": "不支持的设置项: " + name, "supported": settingNames()}, false
	}
	out := map[string]any{"setting": name, "kind": spec.Kind}
	switch spec.Kind {
	case "int":
		if spec.Stream != "" {
			raw, err := runCmdOut("media", "volume", "--stream", spec.Stream, "--get")
			if err == nil {
				out["raw"] = strings.TrimSpace(raw)
				out["value"] = atoiOrNil(strings.TrimSpace(raw))
			}
		} else {
			raw := settingsGet(spec.Namespace, spec.Key)
			out["raw"] = raw
			out["value"] = atoiOrNil(raw)
		}
		out["min"] = spec.Min
		out["max"] = spec.Max
	case "enum":
		raw := settingsGet(spec.Namespace, spec.Key)
		out["raw"] = raw
		out["value"] = dndModeName(raw)
	case "bool":
		if name == "wifi" {
			out["value"] = wifiEnabled()
			out["raw"] = settingsGet("global", "wifi_on")
			break
		}
		raw := settingsGet(spec.Namespace, spec.Key)
		out["raw"] = raw
		out["value"] = boolOrNil(raw)
	}
	return out, true
}

// dndModeName 把 zen_mode 数值映射为语义名
func dndModeName(raw string) any {
	switch strings.TrimSpace(raw) {
	case "0":
		return "off"
	case "1":
		return "priority"
	case "2":
		return "total_silence"
	case "3":
		return "alarms_only"
	case "":
		return nil
	default:
		return raw
	}
}

// setSetting 切换系统设置（对应 android_toggle_setting）
func setSetting(name string, boolVal *bool, intVal *int) (map[string]any, bool) {
	spec, ok := settingSpecs()[name]
	if !ok {
		return map[string]any{"error": "不支持的设置项: " + name, "supported": settingNames()}, false
	}
	steps := []string{}
	note := ""
	switch spec.Kind {
	case "int":
		if intVal == nil {
			return map[string]any{"error": "设置项 " + name + " 需要整数参数 value"}, false
		}
		v := *intVal
		if v < spec.Min || v > spec.Max {
			return map[string]any{"error": fmt.Sprintf("value 超出范围 [%d, %d]", spec.Min, spec.Max)}, false
		}
		if spec.Stream != "" {
			steps = append(steps, fmt.Sprintf("media volume --stream %s --set %d", spec.Stream, v))
		} else {
			steps = append(steps, fmt.Sprintf("settings put %s %s %d", spec.Namespace, spec.Key, v))
		}
	case "enum":
		mode := ""
		if boolVal == nil {
			return map[string]any{"error": "设置项 " + name + " 需要 value 参数（on/off/priority/total_silence/alarms_only）"}, false
		}
		if *boolVal {
			mode = "on"
		} else {
			mode = "off"
		}
		steps = append(steps, "cmd notification set_dnd "+mode)
	case "bool":
		if boolVal == nil {
			return map[string]any{"error": "设置项 " + name + " 需要布尔 value（true=开 / false=关）"}, false
		}
		on := *boolVal
		switch name {
		case "wifi":
			if on {
				steps = append(steps, "svc wifi enable", "cmd wifi set-wifi-enabled enabled")
			} else {
				steps = append(steps, "svc wifi disable", "cmd wifi set-wifi-enabled disabled")
			}
		case "bluetooth":
			if on {
				steps = append(steps, "cmd bluetooth_manager enable", "svc bluetooth enable")
			} else {
				steps = append(steps, "cmd bluetooth_manager disable", "svc bluetooth disable")
			}
		case "airplane_mode":
			v := "0"
			js := "false"
			if on {
				v = "1"
				js = "true"
			}
			steps = append(steps,
				"settings put global airplane_mode_on "+v,
				"am broadcast -a android.intent.action.AIRPLANE_MODE --ez state "+js,
			)
			note = "飞行模式变更后移动网络/WiFi 需要数秒恢复"
		case "mobile_data":
			if on {
				steps = append(steps, "svc data enable")
			} else {
				steps = append(steps, "svc data disable")
			}
		case "location":
			v := "0"
			b := "false"
			if on {
				v = "3"
				b = "true"
			}
			steps = append(steps,
				"cmd location set-location-enabled "+b,
				"settings put secure location_mode "+v,
			)
		case "auto_rotate":
			v := "0"
			if on {
				v = "1"
			}
			steps = append(steps, "settings put system accelerometer_rotation "+v)
		case "stay_awake":
			v := "0"
			if on {
				v = "7" // AC + USB + 无线充电
			}
			steps = append(steps, "settings put global stay_on_while_plugged_in "+v)
		default:
			v := "0"
			if on {
				v = "1"
			}
			steps = append(steps, fmt.Sprintf("settings put %s %s %s", spec.Namespace, spec.Key, v))
		}
	}
	// 依次尝试，任一成功即可（不同 ROM 支持的实现不同）
	results := []map[string]any{}
	for _, s := range steps {
		r := runShellArgs(s, 10*time.Second)
		ok := r.ExitCode == 0
		results = append(results, map[string]any{
			"command":   s,
			"ok":        ok,
			"exit_code": r.ExitCode,
			"stderr":    nilIfEmpty(strings.TrimSpace(r.Stderr)),
		})
		if ok {
			// 读回校验
			st, _ := getSetting(name)
			return map[string]any{
				"setting":     name,
				"applied":     true,
				"applied_cmd": s,
				"attempts":    results,
				"state":       st,
				"note":        nilIfEmpty(note),
			}, true
		}
	}
	return map[string]any{
		"setting":  name,
		"applied":  false,
		"attempts": results,
		"error":    "所有切换方式均失败（可能需要 root、SELinux 放行或该 ROM 不支持）",
	}, false
}

// ---------- 剪贴板 ----------

func getClipboard() (string, bool) {
	out, err := runCmdOut("cmd", "clipboard", "get-text")
	if err != nil {
		return "剪贴板不可用: " + errText(err), false
	}
	return out, true
}

// setClipboard 写入剪贴板（对应 android_set_clipboard）
func setClipboard(text string) (string, bool) {
	if text == "" {
		return "text 不能为空", false
	}
	out, err := runCmdOut("cmd", "clipboard", "set-text", text)
	if err != nil {
		return fmt.Sprintf("cmd clipboard set-text 失败: %s %s", errText(err), out), false
	}
	return out, true
}

// ---------- 截屏 ----------

// captureScreenshot 截屏并返回 PNG 字节与保存路径（对应 android_screenshot）
func captureScreenshot(displayID int, save bool) ([]byte, string, string) {
	if err := os.MkdirAll(tmpDir(), 0755); err != nil {
		return nil, "", "创建临时目录失败: " + errText(err)
	}
	p := filepath.Join(tmpDir(), fmt.Sprintf("screen_%d.png", time.Now().UnixNano()/1e6))
	args := []string{"-p"}
	if displayID > 0 {
		args = append(args, "-d", strconv.Itoa(displayID))
	}
	args = append(args, p)
	out, err := runCmdOut("screencap", args...)
	if err != nil {
		return nil, "", fmt.Sprintf("screencap 失败: %s %s", errText(err), out)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, "", "读取截图失败: " + errText(err)
	}
	if !save {
		_ = os.Remove(p)
		p = ""
	} else {
		pruneScreenshots()
	}
	return data, p, ""
}

// pruneScreenshots 只保留最近 screenshotKeep 个截图，避免长期占用存储
func pruneScreenshots() {
	entries, err := os.ReadDir(tmpDir())
	if err != nil {
		return
	}
	type f struct {
		name string
		mod  time.Time
	}
	files := []f{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "screen_") || !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{e.Name(), info.ModTime()})
	}
	if len(files) <= screenshotKeep {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, it := range files[screenshotKeep:] {
		_ = os.Remove(filepath.Join(tmpDir(), it.name))
	}
}

// ---------- 小工具 ----------

func nilIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func atoiOrNil(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return v
}

// scaleOrNil 把字符串按除数缩放（如 temp 需 /10、voltage_now 需 /1e6）
func scaleOrNil(s string, div float64) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%g", round3(v/div))
}

func boolOrNil(s string) any {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return nil
	}
}

func kbToBytes(v any) any {
	n, ok := v.(int64)
	if !ok {
		return nil
	}
	return n * 1024
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

func numOf(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// jsonText 把任意值序列化为缩进 JSON 文本
func jsonText(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, errText(err))
	}
	return string(b)
}
