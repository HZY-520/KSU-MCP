package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// tools.go — MCP 工具注册表
//
// 命名规范（MCP 2025-11-25）：{service}_{action}_{resource}
//   - 字符集 A-Z a-z 0-9 _ - .，长度 1-128
//   - service 固定为 android（本模块暴露的是 Android 设备能力）
//   - 每个工具的 Description 面向 AI Agent 编写：说明「做什么 / 参数含义 / 返回什么 /
//     失败与限制」，因为 Agent 完全依赖该文本进行工具选择与参数填充。
//
// v1.1.0 的 11 个扁平命名工具全部保留为 **弃用别名**（deprecated），
// 其 Description 首行标注替代工具，保证老客户端配置不被破坏。

// ToolDef 对应 MCP tools/list 返回的单个工具对象
type ToolDef struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// toolEntry 注册表条目：定义 + 实现 + 弃用元信息
type toolEntry struct {
	def        ToolDef
	deprecated bool
	replacedBy string
	impl       func(cfg *Config, args map[string]any) ([]map[string]any, bool)
}

// ---------- 内容块辅助 ----------

func textContent(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

// jsonOK 以缩进 JSON 文本作为成功结果
func jsonOK(v any) ([]map[string]any, bool) {
	return []map[string]any{textContent(jsonText(v))}, false
}

// jsonResult 以缩进 JSON 文本返回，并显式指定是否错误（用于「有数据但也有错」的场景）
func jsonResult(v any, isErr bool) ([]map[string]any, bool) {
	return []map[string]any{textContent(jsonText(v))}, isErr
}

// errMsg 以 {"error": "..."} 形态返回失败结果（与 v1.1.0 行为保持一致）
func errMsg(msg string) ([]map[string]any, bool) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return []map[string]any{textContent(string(b))}, true
}

// textOK 以纯文本作为成功结果
func textOK(s string) ([]map[string]any, bool) {
	return []map[string]any{textContent(s)}, false
}

// ---------- schema 辅助 ----------

func schema(props map[string]any, required []string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if props == nil {
		m["properties"] = map[string]any{}
	}
	if len(required) > 0 {
		m["required"] = required
	}
	m["additionalProperties"] = false
	return m
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func strPropDefault(desc, def string) map[string]any {
	m := strProp(desc)
	m["default"] = def
	return m
}

func strPropEnum(desc string, vals ...string) map[string]any {
	m := strProp(desc)
	m["enum"] = vals
	return m
}

func strPropEnumDefault(desc, def string, vals ...string) map[string]any {
	m := strPropEnum(desc, vals...)
	m["default"] = def
	return m
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func intPropRange(desc string, min, max int) map[string]any {
	m := intProp(desc)
	m["minimum"] = min
	m["maximum"] = max
	return m
}

func intPropDefault(desc string, def, min, max int) map[string]any {
	m := intPropRange(desc, min, max)
	m["default"] = def
	return m
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func boolPropDefault(desc string, def bool) map[string]any {
	m := boolProp(desc)
	m["default"] = def
	return m
}

func arrProp(desc, itemType string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": desc,
		"items":       map[string]any{"type": itemType},
	}
}

// annotations 组装 MCP 标准注解（帮助 Agent 判断风险等级）
func annotations(title string, readOnly, destructive bool) map[string]any {
	return map[string]any{
		"title":           title,
		"readOnlyHint":    readOnly,
		"destructiveHint": destructive,
		"idempotentHint":  readOnly,
		"openWorldHint":   false,
	}
}

// deprecatedDesc 为弃用别名生成说明（首行显著标注替代工具）
func deprecatedDesc(replacedBy, original string) string {
	if replacedBy == "" {
		return "⚠️ 已弃用（deprecated，v1.1.0 遗留的扁平命名，保留仅为兼容老客户端）。" + original
	}
	return "⚠️ 已弃用（deprecated）：请改用 `" + replacedBy + "`（符合 MCP 2025-11-25 命名规范）。" +
		"本工具在 v1.2.0 及后续版本继续可用，但不再演进。\n\n" + original
}

// ---------- 参数读取 ----------

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
	case int64:
		return int(v)
	}
	return 0
}

func argBool(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

// argBoolPtr 区分「未传」与「显式 false」
func argBoolPtr(args map[string]any, key string) *bool {
	if v, ok := args[key].(bool); ok {
		return &v
	}
	return nil
}

// argIntPtr 区分「未传」与「显式 0」
func argIntPtr(args map[string]any, key string) *int {
	switch v := args[key].(type) {
	case float64:
		n := int(v)
		return &n
	case int:
		n := v
		return &n
	case int64:
		n := int(v)
		return &n
	}
	return nil
}

func argStringSlice(args map[string]any, key string) []string {
	out := []string{}
	if arr, ok := args[key].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

// ---------- 注册表 ----------

func toolRegistry() []toolEntry {
	return []toolEntry{
		// ================= 设备信息类（只读） =================
		{
			def: ToolDef{
				Name:  "android_get_device_info",
				Title: "设备信息",
				Description: "获取本机 Android 设备的静态硬件与系统信息，用于识别设备型号、系统版本与 root 环境。\n\n" +
					"何时使用：需要知道设备是什么、跑什么系统、是否已 root、装了哪个 root 方案时。\n\n" +
					"返回 JSON 字段：\n" +
					"- brand / manufacturer / model / device / product：厂商与机型标识\n" +
					"- android：Android 版本号（如 14）；sdk：API 级别（如 34）\n" +
					"- build_id / build_type / security_patch：构建号、构建类型、安全补丁日期\n" +
					"- abi / abi_list：主 ABI 与全部 ABI\n" +
					"- fingerprint：构建指纹；bootloader：引导程序版本\n" +
					"- kernel：/proc/version 内核串；selinux：Enforcing/Permissive\n" +
					"- uptime_seconds：开机时长（秒）\n" +
					"- root：含 root(bool)/uid/kernelsu(bool)/kernelsu_version/magisk(bool)/magisk_version/module_version\n" +
					"- mcpd_version：本 MCP 服务端版本；supported_protocols：支持的 MCP 协议版本列表\n\n" +
					"无参数。所有取值均为只读，不会修改设备。字段取不到时为 null。",
				InputSchema: schema(nil, nil),
				Annotations: annotations("设备信息", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(collectDeviceInfo())
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_battery_status",
				Title: "电池状态",
				Description: "读取电池电量、充放电状态、温度、电压、电流与充电器信息。\n\n" +
					"何时使用：需要判断设备是否需要充电、是否过热、是否在充电，或做功耗/温度监控时。\n\n" +
					"返回 JSON 字段：\n" +
					"- level_percent：电量百分比 0-100（整数）\n" +
					"- status：Charging / Discharging / Full / Not charging / Unknown\n" +
					"- health：Good / Overheat / Dead / Cold 等\n" +
					"- temperature_c：电池温度（摄氏度，浮点）；voltage_v：电压（伏）\n" +
					"- current_now_ma：瞬时电流（毫安，充电为正还是负取决于内核实现）\n" +
					"- charge_counter_uah / charge_full_uah / cycle_count：库仑计与循环次数（部分 ROM 无此节点）\n" +
					"- usb_online / usb_type / usb_voltage_v / usb_current_ma：充电器信息\n" +
					"- source：数据来源（sysfs 或 dumpsys battery 回落）\n\n" +
					"无参数。只读。若 sysfs 无电池节点会自动回落到 `dumpsys battery`。",
				InputSchema: schema(nil, nil),
				Annotations: annotations("电池状态", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(collectBattery())
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_storage_info",
				Title: "存储信息",
				Description: "获取各分区与关键目录的存储占用情况，用于判断设备是否空间不足。\n\n" +
					"何时使用：需要查看剩余空间、定位哪个分区快满、写入大文件前做容量预检时。\n\n" +
					"参数：\n" +
					"- path_filter（可选，字符串）：只返回挂载点路径包含该子串的条目，例如 \"/data\"；省略返回全部\n" +
					"- include_pseudo（可选，布尔，默认 false）：是否包含 tmpfs/proc/sysfs 等伪文件系统\n\n" +
					"返回 JSON：\n" +
					"- partitions：数组，每项含 filesystem/type/mount/size_kb/used_kb/available_kb/size_bytes/used_bytes/available_bytes/use_percent\n" +
					"- count：分区条数\n" +
					"- key_paths：数组，用 statfs 精确测量的关键路径（/、/data、/data/adb、/storage/emulated/0、/cache、/system），\n" +
					"  每项含 path/size_bytes/used_bytes/available_bytes/use_percent\n" +
					"- df_error：df 命令失败原因（成功时为 null）\n\n" +
					"字节数均为整数，便于直接比较。只读。",
				InputSchema: schema(map[string]any{
					"path_filter":    strProp("只保留挂载点包含该子串的分区，例如 \"/data\"；省略表示不过滤"),
					"include_pseudo": boolPropDefault("是否包含 tmpfs/proc/sysfs 等伪文件系统，默认 false", false),
				}, nil),
				Annotations: annotations("存储信息", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(collectStorage(argString(args, "path_filter"), argBool(args, "include_pseudo")))
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_network_info",
				Title: "网络信息",
				Description: "获取当前网络连接状态：网络类型、IP 地址、网关、DNS、WiFi 名称与蜂窝运营商。\n\n" +
					"何时使用：需要知道设备当前走 WiFi 还是移动数据、局域网 IP 是多少、连的是哪个 SSID，\n" +
					"或在网络切换/排障时确认当前链路。\n\n" +
					"返回 JSON 字段：\n" +
					"- active_interface / active_type：默认路由所在网卡名与其类型（wifi/cellular/ethernet/vpn/other）\n" +
					"- active_ip / active_ips：默认路由网卡的 IPv4 地址\n" +
					"- gateway：默认网关；dns：DNS 服务器数组\n" +
					"- interfaces：全部启用网卡数组，每项含 name/kind/ips\n" +
					"- lan_ips：全部非回环 IPv4（局域网可达地址候选）\n" +
					"- wifi_enabled / wifi_ssid / wifi_ip：WiFi 开关、SSID、WiFi 地址\n" +
					"- cellular_ip / operator / operator_iso / network_type：蜂窝地址、运营商、国家码、制式（如 LTE/NR）\n" +
					"- mobile_data / airplane_mode：移动数据与飞行模式开关状态（bool 或 null）\n" +
					"- mcp_listen：本 MCP 服务的监听地址（bind:port）\n\n" +
					"无参数。只读。WiFi SSID 在 Android 高版本上可能因权限返回 null。",
				InputSchema: schema(nil, nil),
				Annotations: annotations("网络信息", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(collectNetwork())
			},
		},
		{
			def: ToolDef{
				Name:  "android_list_packages",
				Title: "应用列表",
				Description: "列出设备上已安装的应用包名，支持类型过滤、关键字过滤与分页。\n\n" +
					"何时使用：需要找出某个 App 的包名（配合 am start / pm 操作）、统计安装量、\n" +
					"或确认某应用是否安装时。列表可能很长，请配合 filter 与分页参数使用。\n\n" +
					"参数：\n" +
					"- filter（可选）：包名子串，大小写不敏感，例如 \"wechat\" 或 \"com.tencent\"\n" +
					"- state（可选，默认 all）：all=全部；third_party=仅第三方；system=仅系统；enabled=仅已启用；disabled=仅已禁用\n" +
					"- offset（可选，默认 0）：跳过的条数，用于翻页\n" +
					"- limit（可选，默认 100，最大 500）：本页返回条数\n" +
					"- sort（可选，默认 asc）：asc=包名升序；desc=包名降序\n\n" +
					"返回 JSON：total（过滤后的总数）、offset、limit、count（本页条数）、has_more（是否还有下一页）、\n" +
					"state、filter、packages（本页包名数组）。\n\n" +
					"翻页方式：拿到 has_more=true 时，用 offset=offset+count 再次调用。只读。",
				InputSchema: schema(map[string]any{
					"filter": strProp("包名子串过滤（大小写不敏感），例如 \"com.tencent\"；省略返回全部"),
					"state":  strPropEnumDefault("过滤应用类型，默认 all", "all", "all", "third_party", "system", "enabled", "disabled"),
					"offset": intPropDefault("跳过前 N 条（分页偏移），默认 0", 0, 0, 100000),
					"limit":  intPropDefault("本页返回条数，默认 100，最大 500", 100, 1, 500),
					"sort":   strPropEnumDefault("包名排序方向，默认升序", "asc", "asc", "desc"),
				}, nil),
				Annotations: annotations("应用列表", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(collectPackages(
					argString(args, "filter"),
					orDefault(argString(args, "state"), "all"),
					argInt(args, "offset"),
					defaultInt(argInt(args, "limit"), 100),
					argString(args, "sort") == "desc",
				))
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_running_processes",
				Title: "运行进程",
				Description: "获取当前运行中的进程列表（PID / 父 PID / 用户 / 内存占用 / 名称），支持过滤与排序。\n\n" +
					"何时使用：排查哪个进程在吃内存、确认某应用是否在运行、找 PID 以便进一步操作时。\n\n" +
					"参数：\n" +
					"- filter（可选）：进程名子串过滤（大小写不敏感），例如 \"chrome\"\n" +
					"- user（可选）：按进程所属用户过滤，例如 \"u0_a123\"、\"root\"、\"system\"\n" +
					"- sort_by（可选，默认 mem）：mem=按 RSS 内存降序；vsz=按虚拟内存降序；pid=按 PID 升序；name=按名称升序\n" +
					"- limit（可选，默认 50，最大 200）：返回条数上限（过滤排序后截断）\n\n" +
					"返回 JSON：total（过滤后总数）、count（返回条数）、filter、user、sort_by、\n" +
					"processes（数组，每项含 pid/ppid/user/rss/vsz/name；rss 与 vsz 单位为 KB）。\n\n" +
					"只读。首次调用在部分 ROM 上需 1-2 秒（遍历 /proc）。",
				InputSchema: schema(map[string]any{
					"filter":  strProp("进程名子串过滤（大小写不敏感）；省略返回全部"),
					"user":    strProp("按进程所属用户过滤，例如 u0_a123 / root / system；省略不过滤"),
					"sort_by": strPropEnumDefault("排序方式，默认按内存(RSS)降序", "mem", "mem", "vsz", "pid", "name"),
					"limit":   intPropDefault("返回条数上限，默认 50，最大 200", 50, 1, 200),
				}, nil),
				Annotations: annotations("运行进程", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(collectProcesses(
					argString(args, "filter"),
					argString(args, "user"),
					orDefault(argString(args, "sort_by"), "mem"),
					defaultInt(argInt(args, "limit"), 50),
				))
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_logcat",
				Title: "读取日志",
				Description: "读取 Android 系统日志（logcat）的快照，支持按 tag、优先级与关键字过滤。\n\n" +
					"何时使用：排查应用崩溃、看系统报错、追踪某模块的输出时。\n\n" +
					"参数：\n" +
					"- tags（可选，字符串数组）：只保留这些 tag 的日志，例如 [\"AndroidRuntime\",\"ActivityManager\"]。\n" +
					"  底层用 `logcat -s`，此时未列出的 tag 全部被静默\n" +
					"- priority（可选，默认 V）：最低优先级阈值，V=Verbose D=Debug I=Info W=Warn E=Error F=Fatal\n" +
					"- buffer（可选，默认 main）：main / system / crash / events / all（all 会合并 main+system+crash）\n" +
					"- filter（可选）：对整行做大小写不敏感的子串过滤，例如 \"Exception\"\n" +
					"- lines（可选，默认 200，最大 2000）：读取最近多少行\n\n" +
					"返回 JSON：count（命中行数）、tags、priority、buffer、filter、lines（日志行数组，threadtime 格式，\n" +
					"每行形如 `06-12 10:00:00.000  1234  5678 I Tag     : message`）。\n\n" +
					"注意：这是**快照**（`logcat -d`），不会持续阻塞；只读。tags 与 filter 同时给出时先按 tag 取再用 filter 筛。",
				InputSchema: schema(map[string]any{
					"tags":     arrProp("只保留这些 logcat tag，例如 [\"AndroidRuntime\"]；省略表示不过滤 tag", "string"),
					"priority": strPropEnumDefault("最低日志优先级，默认 V", "V", "V", "D", "I", "W", "E", "F"),
					"buffer":   strPropEnumDefault("读取哪个日志缓冲区，默认 main", "main", "main", "system", "crash", "events", "all"),
					"filter":   strProp("对日志整行做大小写不敏感的子串过滤，例如 \"Exception\"；省略不过滤"),
					"lines":    intPropDefault("读取最近多少行，默认 200，最大 2000", 200, 1, 2000),
				}, nil),
				Annotations: annotations("读取日志", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(collectLogcat(
					argStringSlice(args, "tags"),
					orDefault(argString(args, "priority"), "V"),
					orDefault(argString(args, "buffer"), "main"),
					argString(args, "filter"),
					defaultInt(argInt(args, "lines"), 200),
				))
			},
		},
		{
			def: ToolDef{
				Name:  "android_screenshot",
				Title: "屏幕截图",
				Description: "截取当前屏幕，并以 MCP **image** 内容块直接返回 PNG 图片，同时附带保存路径等文本元信息。\n\n" +
					"何时使用：需要「看到」设备当前画面时——验证 UI 状态、确认某操作后的界面、\n" +
					"读取屏幕上的文字或按钮位置（配合 android_input_tap 的坐标使用）。\n\n" +
					"参数：\n" +
					"- display_id（可选，默认 0）：要截取的显示 ID，0 表示默认屏幕；多屏设备可传其他 ID\n" +
					"- save（可选，默认 true）：是否在设备上保留 PNG 文件；false 时返回后立即删除\n\n" +
					"返回：第一个内容块为 `{\"type\":\"image\",\"mimeType\":\"image/png\"}` 的图片本身；\n" +
					"第二个内容块为 JSON 文本，含 path（保存路径，未保存时为 null）、size_bytes、mime_type、display_id。\n\n" +
					"注意：\n" +
					"- 依赖系统 `screencap`，个别 ROM 不可用或需 root\n" +
					"- 图片较大（1440p 屏约 1-3 MB），返回超过 8 MB 时会拒绝并提示改用 display_id\n" +
					"- 为节省存储，设备上最多保留最近 5 张截图，旧文件自动清理",
				InputSchema: schema(map[string]any{
					"display_id": intPropDefault("要截取的显示 ID，0=默认屏幕", 0, 0, 32),
					"save":       boolPropDefault("是否在设备上保留 PNG 文件，默认 true", true),
				}, nil),
				Annotations: annotations("屏幕截图", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				data, path, errStr := captureScreenshot(argInt(args, "display_id"), !hasBoolFalse(args, "save"))
				if errStr != "" {
					return errMsg(errStr)
				}
				if len(data) > 8<<20 {
					return errMsg(fmt.Sprintf("截图过大（%d 字节 > 8MB 上限），请改用 display_id 指定较小屏幕", len(data)))
				}
				meta := map[string]any{
					"path":       nilIfEmpty(path),
					"size_bytes": len(data),
					"mime_type":  "image/png",
					"display_id": argInt(args, "display_id"),
				}
				return []map[string]any{
					{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": "image/png"},
					textContent(jsonText(meta)),
				}, false
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_setting",
				Title: "读取设置项",
				Description: "读取一个系统开关/设置的当前值。与 android_toggle_setting 使用同一套设置名。\n\n" +
					"何时使用：修改设置前后做确认，或单纯查询 WiFi / 蓝牙 / 飞行模式 / 亮度 / 音量 / 勿扰的当前状态。\n\n" +
					"参数：\n" +
					"- setting（必填）：设置名，取值见 android_toggle_setting。\n" +
					"  布尔类：wifi、bluetooth、airplane_mode、mobile_data、location、auto_rotate、stay_awake\n" +
					"  整数类：brightness(0-255)、volume_music/volume_ring/volume_alarm/volume_notification(0-15)\n" +
					"  枚举类：dnd（off/priority/total_silence/alarms_only）\n\n" +
					"返回 JSON：setting、kind（bool/int/enum）、value（规范化后的值，bool 或整数或枚举名；读取不到为 null）、\n" +
					"raw（底层原始字符串）；整数类额外返回 min/max。\n\n" +
					"只读。",
				InputSchema: schema(map[string]any{
					"setting": strPropEnum("要读取的设置名", settingNames()...),
				}, []string{"setting"}),
				Annotations: annotations("读取设置项", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				name := argString(args, "setting")
				if name == "" {
					return errMsg("setting 不能为空，可选值: " + strings.Join(settingNames(), ", "))
				}
				v, ok := getSetting(name)
				return jsonResult(v, !ok)
			},
		},

		// ================= 设备控制类 =================
		{
			def: ToolDef{
				Name:  "android_input_text",
				Title: "注入文本",
				Description: "把一段文本注入到当前获得焦点的输入框中（等价于用户用键盘逐字输入）。\n\n" +
					"何时使用：需要在设备上填写搜索框、表单、聊天输入框时。\n\n" +
					"使用前提：**目标输入框必须已经获得焦点**。请先用 android_screenshot 确认界面，\n" +
					"必要时先用 android_input_tap 点击输入框使其获得焦点，再调用本工具。\n\n" +
					"参数：\n" +
					"- text（必填）：要输入的文本。支持空格、中文、标点；底层直接把整串作为单个参数传给 `input text`，\n" +
					"  不经 shell 解析，因此引号/反斜杠/分号等特殊字符不会被当成命令。\n" +
					"  已知限制：部分 ROM 上字面量 \"%s\" 会被解释为空格。\n\n" +
					"返回：纯文本结果。成功时内容为空；失败时文本为错误说明（含 stderr）。\n\n" +
					"注意：本工具**会改变设备状态**（产生真实输入）。输入中文需要系统输入法支持（`input text` 走 IME 注入）。",
				InputSchema: schema(map[string]any{
					"text": strProp("要注入的文本内容，例如 \"hello world\" 或 \"你好\""),
				}, []string{"text"}),
				Annotations: annotations("注入文本", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				out, ok := inputText(argString(args, "text"))
				if !ok {
					return errMsg(out)
				}
				return textOK(out)
			},
		},
		{
			def: ToolDef{
				Name:  "android_input_key",
				Title: "注入按键",
				Description: "发送一个按键事件（HOME / BACK / ENTER / 音量 / 电源等）。\n\n" +
					"何时使用：需要返回上级菜单、回到桌面、确认对话框、唤醒/息屏、调音量时。\n\n" +
					"参数：\n" +
					"- keycode（必填）：按键名或数字码。常用名：KEYCODE_HOME(3)、KEYCODE_BACK(4)、\n" +
					"  KEYCODE_ENTER(66)、KEYCODE_DEL(67)、KEYCODE_WAKEUP(224)、KEYCODE_SLEEP(223)、\n" +
					"  KEYCODE_POWER(26)、KEYCODE_VOLUME_UP(24)、KEYCODE_VOLUME_DOWN(25)、\n" +
					"  KEYCODE_APP_SWITCH(187)、KEYCODE_MENU(82)、KEYCODE_ESCAPE(111)\n\n" +
					"返回：纯文本结果（成功为空，失败为错误说明）。\n\n" +
					"注意：KEYCODE_POWER / KEYCODE_SLEEP 会改变屏幕状态，KEYCODE_HOME 会切走当前应用，\n" +
					"这些都会中断用户正在做的事，请谨慎使用。",
				InputSchema: schema(map[string]any{
					"keycode": strProp("按键名或数字码，例如 KEYCODE_HOME 或 3"),
				}, []string{"keycode"}),
				Annotations: annotations("注入按键", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				out, ok := inputKey(argString(args, "keycode"))
				if !ok {
					return errMsg(out)
				}
				return textOK(out)
			},
		},
		{
			def: ToolDef{
				Name:  "android_input_tap",
				Title: "点击坐标",
				Description: "在屏幕指定坐标模拟一次点击。\n\n" +
					"何时使用：点击按钮/开关/图标；让输入框获得焦点以便随后 android_input_text。\n\n" +
					"参数：\n" +
					"- x（必填）：横坐标（像素，从屏幕左边缘起）\n" +
					"- y（必填）：纵坐标（像素，从屏幕顶边缘起）\n\n" +
					"如何取坐标：先调用 android_screenshot 看画面，再按图片像素比例估算；\n" +
					"如果知道分辨率，坐标即图片中的像素位置。\n\n" +
					"返回：纯文本结果（成功为空，失败为错误说明）。\n\n" +
					"注意：坐标超出屏幕范围时系统会静默忽略或触发边缘手势；本工具会改变设备状态。",
				InputSchema: schema(map[string]any{
					"x": intPropRange("横坐标（像素）", 0, 100000),
					"y": intPropRange("纵坐标（像素）", 0, 100000),
				}, []string{"x", "y"}),
				Annotations: annotations("点击坐标", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				out, ok := inputTap(argInt(args, "x"), argInt(args, "y"))
				if !ok {
					return errMsg(out)
				}
				return textOK(out)
			},
		},
		{
			def: ToolDef{
				Name:  "android_input_swipe",
				Title: "滑动/手势",
				Description: "在屏幕上执行一次滑动手势（从起点拖到终点），可用于滚动、解锁图案、返回手势。\n\n" +
					"何时使用：需要上下滚动列表、下拉通知栏、做出返回手势或解锁时。\n\n" +
					"参数：\n" +
					"- x1/y1（必填）：起点坐标（像素）\n" +
					"- x2/y2（必填）：终点坐标（像素）\n" +
					"- duration_ms（可选，默认 300）：滑动耗时（毫秒）。时间越短越快（可能被识别为 fling），\n" +
					"  越长越慢（可用于精确拖拽）。\n\n" +
					"常用示例：下拉通知栏 swipe(540,0 → 540,1500, 400)；向上滚动列表 swipe(540,1500 → 540,500, 300)。\n\n" +
					"返回：纯文本结果（成功为空，失败为错误说明）。\n\n" +
					"注意：本工具会改变设备状态。",
				InputSchema: schema(map[string]any{
					"x1":          intPropRange("起点横坐标（像素）", 0, 100000),
					"y1":          intPropRange("起点纵坐标（像素）", 0, 100000),
					"x2":          intPropRange("终点横坐标（像素）", 0, 100000),
					"y2":          intPropRange("终点纵坐标（像素）", 0, 100000),
					"duration_ms": intPropDefault("滑动耗时（毫秒），默认 300", 300, 1, 20000),
				}, []string{"x1", "y1", "x2", "y2"}),
				Annotations: annotations("滑动/手势", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				out, ok := inputSwipe(
					argInt(args, "x1"), argInt(args, "y1"),
					argInt(args, "x2"), argInt(args, "y2"),
					defaultInt(argInt(args, "duration_ms"), 300),
				)
				if !ok {
					return errMsg(out)
				}
				return textOK(out)
			},
		},
		{
			def: ToolDef{
				Name:  "android_toggle_setting",
				Title: "切换系统设置",
				Description: "打开/关闭系统开关，或设置亮度、音量、勿扰模式。\n\n" +
					"何时使用：需要开/关 WiFi、蓝牙、飞行模式、移动数据、定位、自动旋转、常亮，\n" +
					"或调整亮度与音量时。\n\n" +
					"参数：\n" +
					"- setting（必填）：设置名。\n" +
					"  布尔类（用 value=true/false）：wifi、bluetooth、airplane_mode、mobile_data、location、auto_rotate、stay_awake\n" +
					"  整数类（用 value=整数）：brightness(0-255)、volume_music / volume_ring / volume_alarm / volume_notification(0-15)\n" +
					"  枚举类：dnd（value=true→on，false→off；也可用 mode 指定 priority/total_silence/alarms_only）\n" +
					"- value（按 setting 类型）：布尔类传 true/false；整数类传整数\n" +
					"- mode（仅 dnd 可选）：on / off / priority / total_silence / alarms_only\n\n" +
					"返回 JSON：setting、applied（是否成功）、applied_cmd（实际生效的命令）、\n" +
					"attempts（各候选命令的尝试结果，含 exit_code 与 stderr）、state（写入后读回的真实状态）、note（补充说明）。\n\n" +
					"实现说明：不同 ROM 支持的命令不同，本工具会按候选顺序尝试（svc / cmd / settings put）并在成功后读回校验，\n" +
					"因此返回值中的 state 才是可信状态，不要只看 applied。\n\n" +
					"注意：\n" +
					"- 会真实改变设备状态；关闭 WiFi/移动数据可能导致本 MCP 服务失联（局域网/公网隧道断开）\n" +
					"- 飞行模式切换后无线网络需要数秒恢复\n" +
					"- 部分设置需要 root 或会被 SELinux 拦截，此时 applied=false 并给出原因",
				InputSchema: schema(map[string]any{
					"setting":   strPropEnum("要切换的设置名", settingNames()...),
					"value":     boolProp("布尔类设置的目标值：true=开，false=关。整数类设置请改用 int_value"),
					"int_value": intPropRange("整数类设置（brightness / volume_*）的目标值", 0, 255),
					"mode":      strPropEnum("仅 dnd 使用的模式", "on", "off", "priority", "total_silence", "alarms_only"),
				}, []string{"setting"}),
				Annotations: annotations("切换系统设置", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				name := argString(args, "setting")
				if name == "" {
					return errMsg("setting 不能为空，可选值: " + strings.Join(settingNames(), ", "))
				}
				// dnd 支持 mode 字符串
				if name == "dnd" {
					if mode := argString(args, "mode"); mode != "" {
						r := runShellArgs("cmd notification set_dnd "+mode, 10*time.Second)
						st, _ := getSetting("dnd")
						out := map[string]any{
							"setting": "dnd", "applied": r.ExitCode == 0, "applied_cmd": "cmd notification set_dnd " + mode,
							"exit_code": r.ExitCode, "stderr": nilIfEmpty(strings.TrimSpace(r.Stderr)), "state": st,
						}
						return jsonResult(out, r.ExitCode != 0)
					}
				}
				v, ok := setSetting(name, argBoolPtr(args, "value"), argIntPtr(args, "int_value"))
				return jsonResult(v, !ok)
			},
		},
		{
			def: ToolDef{
				Name:  "android_set_clipboard",
				Title: "写入剪贴板",
				Description: "把文本写入系统剪贴板。\n\n" +
					"何时使用：需要让用户或其他应用粘贴一段文本（如生成的密码、链接、命令）时。\n\n" +
					"参数：\n" +
					"- text（必填）：要写入剪贴板的文本\n\n" +
					"返回：纯文本结果（成功为空，失败为错误说明）。\n\n" +
					"配套工具：android_get_clipboard 可读回校验。\n" +
					"注意：Android 10+ 限制后台应用读剪贴板，写入一般不受限；本工具会改变设备状态。",
				InputSchema: schema(map[string]any{
					"text": strProp("要写入剪贴板的文本内容"),
				}, []string{"text"}),
				Annotations: annotations("写入剪贴板", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				out, ok := setClipboard(argString(args, "text"))
				if !ok {
					return errMsg(out)
				}
				return textOK(out)
			},
		},

		// ================= 文件与命令（规范命名） =================
		{
			def: ToolDef{
				Name:  "android_exec_shell",
				Title: "执行 Shell",
				Description: "以 root 权限执行一条 shell 命令并返回 stdout / stderr / 退出码。这是本服务最强的工具，\n" +
					"能读写任意文件、启停服务、修改系统。\n\n" +
					"何时使用：其他专用工具覆盖不到的设备操作。优先使用专用工具（如 android_get_device_info），\n" +
					"它们更安全、返回结构更规整。\n\n" +
					"参数：\n" +
					"- command（必填）：完整 shell 命令，例如 \"ls -l /data/local/tmp\"、\"pm list users\"。\n" +
					"  支持管道、重定向、&& 等 shell 语法\n" +
					"- timeout（可选，默认取服务端配置 exec_timeout，通常 30 秒）：超时秒数\n" +
					"- cwd（可选）：命令的工作目录\n\n" +
					"返回 JSON：stdout（字符串）、stderr（字符串）、exit_code（整数，超时为 124，启动失败为 -1）、\n" +
					"timed_out（bool）。\n\n" +
					"限制与注意：\n" +
					"- 服务端可配置 read_only=true 完全禁用本工具（此时返回错误）\n" +
					"- 服务端可配置 exec_allowlist 白名单，只放行前缀匹配的命令（如 [\"ls\",\"cat\"]）\n" +
					"- 命令在 /system/bin/sh 下执行，交互式命令（top、vi 等）会一直跑到超时，请改用 -n 1 之类的批处理参数\n" +
					"- 破坏性命令（rm -rf、格式化分区）请务必先与用户确认",
				InputSchema: schema(map[string]any{
					"command": strProp("要执行的 shell 命令，例如 \"ls -l /data\""),
					"timeout": intPropRange("超时秒数，默认取服务端 exec_timeout（通常 30）", 1, 3600),
					"cwd":     strProp("工作目录，可选；省略则用进程默认目录"),
				}, []string{"command"}),
				Annotations: annotations("执行 Shell", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return toolExecCommand(cfg, args)
			},
		},
		{
			def: ToolDef{
				Name:  "android_read_file",
				Title: "读取文件",
				Description: "读取设备上任意文件的内容。文本按 UTF-8 原样返回，二进制自动转为 base64。\n\n" +
					"何时使用：查看配置、日志、状态文件、脚本内容时。\n\n" +
					"参数：\n" +
					"- path（必填）：文件绝对路径，例如 \"/data/adb/ksu_mcp/config.json\"\n" +
					"- encoding（可选，默认 utf8）：utf8=强制按文本返回；base64=强制 base64。\n" +
					"  不传时会自动判断：内容不是合法 UTF-8 就自动转 base64\n" +
					"- max_bytes（可选，默认 4194304 即 4MB）：最多读取的字节数\n\n" +
					"返回 JSON：path、size（文件真实大小）、truncated（是否因 max_bytes 被截断）、\n" +
					"encoding（实际使用的编码）、content（内容字符串）。\n\n" +
					"注意：目标必须是普通文件（目录请用 android_list_dir）；无权访问或文件不存在时返回 {\"error\":\"...\"}。\n" +
					"读取大文件会显著增加上下文长度，建议先传较小的 max_bytes。",
				InputSchema: schema(map[string]any{
					"path":      strProp("文件绝对路径"),
					"encoding":  strPropEnumDefault("输出编码，默认 utf8（自动判断二进制）", "utf8", "utf8", "base64"),
					"max_bytes": intPropRange("最多读取字节数，默认 4MB", 1, 67108864),
				}, []string{"path"}),
				Annotations: annotations("读取文件", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return toolReadFile(args)
			},
		},
		{
			def: ToolDef{
				Name:  "android_write_file",
				Title: "写入文件",
				Description: "把内容写入（覆盖或追加）设备上的文件，父目录不存在时自动创建。\n\n" +
					"何时使用：修改配置、写脚本、保存抓取到的数据时。\n\n" +
					"参数：\n" +
					"- path（必填）：文件绝对路径\n" +
					"- content（必填）：写入内容。encoding=base64 时必须是合法的 base64 字符串\n" +
					"- encoding（可选，默认 utf8）：utf8 或 base64（写二进制时用）\n" +
					"- append（可选，默认 false）：false=覆盖原文件；true=追加到末尾\n\n" +
					"返回 JSON：path、written（实际写入字节数）、append。\n\n" +
					"限制与注意：\n" +
					"- 服务端 read_only=true 时本工具被完全禁用\n" +
					"- 文件权限固定为 0644，父目录为 0755\n" +
					"- append=false 会**不可恢复地清空**原文件，覆盖系统文件前请先备份",
				InputSchema: schema(map[string]any{
					"path":     strProp("文件绝对路径"),
					"content":  strProp("文件内容；encoding=base64 时必须是合法 base64"),
					"encoding": strPropEnumDefault("内容编码，默认 utf8", "utf8", "utf8", "base64"),
					"append":   boolPropDefault("是否追加到文件末尾，默认 false（覆盖）", false),
				}, []string{"path", "content"}),
				Annotations: annotations("写入文件", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return toolWriteFile(cfg, args)
			},
		},
		{
			def: ToolDef{
				Name:  "android_list_dir",
				Title: "列出目录",
				Description: "列出目录下的直接子项，含名称、类型、大小与权限。\n\n" +
					"何时使用：浏览文件系统结构、确认文件是否存在时。\n\n" +
					"参数：\n" +
					"- path（可选，默认 \"/\"）：目录绝对路径\n\n" +
					"返回 JSON：path、count（条目数）、entries（数组，每项含 name/type(dir|file|symlink)/size/mode）。\n" +
					"条目按名称升序排列。注意：只列一层，不递归；不会跟随符号链接。",
				InputSchema: schema(map[string]any{
					"path": strPropDefault("目录绝对路径，默认 \"/\"", "/"),
				}, nil),
				Annotations: annotations("列出目录", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return toolListDir(args)
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_prop",
				Title: "读取系统属性",
				Description: "读取 Android 系统属性（getprop）。\n\n" +
					"何时使用：查询 ro.* / persist.* / gsm.* / dhcp.* 等属性，获取系统状态或设备标识。\n\n" +
					"参数：\n" +
					"- key（可选）：属性名，如 \"ro.product.model\"。省略时返回**全部**属性（数量很大，数百条）。\n\n" +
					"返回 JSON：\n" +
					"- 指定 key 时：{key, value}（值不存在时为 null）\n" +
					"- 省略 key 时：{count, props}，props 为属性名到值的映射\n\n" +
					"只读。省略 key 会返回数百条属性，除非确实需要全局视图，否则请指定 key。",
				InputSchema: schema(map[string]any{
					"key": strProp("属性名，如 ro.product.model；省略则返回全部属性（数量很大）"),
				}, nil),
				Annotations: annotations("读取系统属性", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return toolGetprop(args)
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_clipboard",
				Title: "读取剪贴板",
				Description: "读取系统剪贴板中的文本内容。\n\n" +
					"何时使用：需要拿到用户刚复制的内容（链接、验证码、文本）时。\n\n" +
					"参数：无。\n\n" +
					"返回 JSON：{text}，text 为剪贴板文本（为空时是空字符串）。\n\n" +
					"限制：依赖系统 `cmd clipboard get-text`，Android 10+ 起**后台应用读取剪贴板受限**，\n" +
					"本服务以 root 运行通常可用，但个别 ROM 仍会返回空或失败（此时返回 {\"error\":\"...\"}）。\n" +
					"只读。",
				InputSchema: schema(nil, nil),
				Annotations: annotations("读取剪贴板", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return toolClipboardGet()
			},
		},

		// ================= v1.1.0 遗留别名（deprecated） =================
		deprecated("device_info", "android_get_device_info", "获取设备信息：品牌、型号、Android 版本、SDK、内核、SELinux、root 状态等",
			schema(nil, nil),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return jsonOK(collectDeviceInfo()) }),
		deprecated("exec_command", "android_exec_shell", "以 root 权限执行 shell 命令（受 read_only 与 exec_allowlist 约束），返回 stdout/stderr/退出码",
			schema(map[string]any{
				"command": strProp("要执行的 shell 命令，例如：ls -l /data"),
				"timeout": intProp("超时秒数，默认 30"),
				"cwd":     strProp("工作目录，可选"),
			}, []string{"command"}),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return toolExecCommand(cfg, args) }),
		deprecated("read_file", "android_read_file", "读取文件内容（文本自动按 utf8 返回，二进制自动转 base64，也可强制指定编码）",
			schema(map[string]any{
				"path":      strProp("文件绝对路径"),
				"encoding":  strProp("输出编码：utf8（默认）或 base64"),
				"max_bytes": intProp("最多读取字节数，默认 4MB"),
			}, []string{"path"}),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return toolReadFile(args) }),
		deprecated("write_file", "android_write_file", "写入或追加文件内容（read_only 模式下禁用）",
			schema(map[string]any{
				"path":     strProp("文件绝对路径"),
				"content":  strProp("文件内容"),
				"encoding": strProp("内容编码：utf8（默认）或 base64"),
				"append":   boolProp("是否追加，默认 false（覆盖）"),
			}, []string{"path", "content"}),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return toolWriteFile(cfg, args) }),
		deprecated("list_dir", "android_list_dir", "列出目录内容（名称/类型/大小/权限/修改时间）",
			schema(map[string]any{
				"path": strProp("目录绝对路径，默认 /"),
			}, nil),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return toolListDir(args) }),
		deprecated("app_list", "android_list_packages", "列出已安装应用包名，可按关键字过滤（无分页，包多时响应很长）",
			schema(map[string]any{
				"contains": strProp("包名过滤关键字，可选"),
			}, nil),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(collectPackages(argString(args, "contains"), "all", 0, 0, false))
			}),
		deprecated("battery_info", "android_get_battery_status", "获取电池信息：电量、状态、温度、电压等",
			schema(nil, nil),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return jsonOK(collectBattery()) }),
		deprecated("network_info", "android_get_network_info", "获取网络信息：网卡、IP、主机名、WiFi 等",
			schema(nil, nil),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return jsonOK(collectNetwork()) }),
		deprecated("screenshot", "android_screenshot", "截取屏幕，返回 PNG 的 base64 与保存路径（base64 塞在 JSON 文本里，Agent 无法直接看图）",
			schema(nil, nil),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				data, path, errStr := captureScreenshot(0, true)
				if errStr != "" {
					return errMsg(errStr)
				}
				return jsonOK(map[string]any{"path": path, "size": len(data), "data_base64": base64.StdEncoding.EncodeToString(data)})
			}),
		deprecated("clipboard_get", "android_get_clipboard", "读取剪贴板文本（Android 13+ 或 root 可用时）",
			schema(nil, nil),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return toolClipboardGet() }),
		deprecated("getprop", "android_get_prop", "读取系统属性，不传 key 时返回全部属性",
			schema(map[string]any{
				"key": strProp("属性名，如 ro.product.model；省略则返回全部"),
			}, nil),
			func(cfg *Config, args map[string]any) ([]map[string]any, bool) { return toolGetprop(args) }),
	}
}

// deprecated 构造一个弃用别名条目（自动加弃用前缀说明）
func deprecated(name, replacedBy, desc string, in map[string]any, impl func(*Config, map[string]any) ([]map[string]any, bool)) toolEntry {
	return toolEntry{
		def: ToolDef{
			Name:        name,
			Title:       "【已弃用】" + replacedBy,
			Description: deprecatedDesc(replacedBy, desc),
			InputSchema: in,
			Annotations: map[string]any{"title": "已弃用的旧名称: " + name, "deprecated": true},
		},
		deprecated: true,
		replacedBy: replacedBy,
		impl:       impl,
	}
}

// toolDefs 返回 tools/list 的工具定义数组
func toolDefs() []ToolDef {
	entries := toolRegistry()
	out := make([]ToolDef, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.def)
	}
	return out
}

// toolNames 返回全部注册工具名（按注册顺序）
func toolNames() []string {
	out := []string{}
	for _, e := range toolRegistry() {
		out = append(out, e.def.Name)
	}
	return out
}

// canonicalToolNames 返回非弃用工具名（规范命名部分）
func canonicalToolNames() []string {
	out := []string{}
	for _, e := range toolRegistry() {
		if !e.deprecated {
			out = append(out, e.def.Name)
		}
	}
	return out
}

// callTool 分发工具调用
func callTool(cfg *Config, name string, args map[string]any) ([]map[string]any, bool) {
	for _, e := range toolRegistry() {
		if e.def.Name == name {
			return e.impl(cfg, args)
		}
	}
	return errMsg("unknown tool: " + name)
}

// ---------- 通用小工具 ----------

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func defaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// hasBoolFalse 判断某布尔参数是否被显式设为 false（未传时按「默认 true」处理）
func hasBoolFalse(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return !v
	}
	return false
}

// ---------- 遗留工具实现（内部复用 android.go 的采集函数） ----------

// toolExecCommand 实现 exec_command / android_exec_shell
func toolExecCommand(cfg *Config, args map[string]any) ([]map[string]any, bool) {
	if cfg.ReadOnly {
		return errMsg("服务器处于只读模式（read_only=true），已禁用命令执行")
	}
	cmdStr := argString(args, "command")
	if strings.TrimSpace(cmdStr) == "" {
		return errMsg("command 不能为空")
	}
	if len(cfg.ExecAllowlist) > 0 && !allowlisted(cfg.ExecAllowlist, cmdStr) {
		return errMsg(fmt.Sprintf("命令不在允许列表内: %s", cmdStr))
	}
	timeout := cfg.ExecTimeout
	if v := argInt(args, "timeout"); v > 0 {
		timeout = v
	}
	r := runShellCwd(cmdStr, time.Duration(timeout)*time.Second, argString(args, "cwd"))
	return jsonOK(map[string]any{
		"stdout":    r.Stdout,
		"stderr":    r.Stderr,
		"exit_code": r.ExitCode,
		"timed_out": r.TimedOut,
	})
}

func allowlisted(list []string, cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, p := range list {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if trimmed == p || strings.HasPrefix(trimmed, p+" ") {
			return true
		}
	}
	return false
}

// toolReadFile 实现 read_file / android_read_file
func toolReadFile(args map[string]any) ([]map[string]any, bool) {
	path := argString(args, "path")
	if path == "" {
		return errMsg("path 不能为空")
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
		return errMsg(err.Error())
	}
	if fi.IsDir() {
		return errMsg("目标是一个目录")
	}
	f, err := os.Open(path)
	if err != nil {
		return errMsg(err.Error())
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return errMsg(err.Error())
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	out := map[string]any{"path": path, "size": fi.Size(), "truncated": truncated, "encoding": encoding}
	switch {
	case encoding == "base64":
		out["content"] = base64.StdEncoding.EncodeToString(data)
	case utf8.Valid(data):
		out["content"] = string(data)
		out["encoding"] = "utf8"
	default:
		out["content"] = base64.StdEncoding.EncodeToString(data)
		out["encoding"] = "base64"
	}
	return jsonOK(out)
}

// toolWriteFile 实现 write_file / android_write_file
func toolWriteFile(cfg *Config, args map[string]any) ([]map[string]any, bool) {
	if cfg.ReadOnly {
		return errMsg("服务器处于只读模式（read_only=true），已禁用写文件")
	}
	path := argString(args, "path")
	content := argString(args, "content")
	if path == "" {
		return errMsg("path 不能为空")
	}
	var data []byte
	if argString(args, "encoding") == "base64" {
		d, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return errMsg("base64 解码失败: " + err.Error())
		}
		data = d
	} else {
		data = []byte(content)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return errMsg(err.Error())
	}
	flags := os.O_CREATE | os.O_WRONLY
	appendMode := argBool(args, "append")
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0644)
	if err != nil {
		return errMsg(err.Error())
	}
	n, err := f.Write(data)
	_ = f.Close()
	if err != nil {
		return errMsg(err.Error())
	}
	return jsonOK(map[string]any{"path": path, "written": n, "append": appendMode})
}

// toolListDir 实现 list_dir / android_list_dir
func toolListDir(args map[string]any) ([]map[string]any, bool) {
	path := argString(args, "path")
	if path == "" {
		path = "/"
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return errMsg(err.Error())
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
	return jsonOK(map[string]any{"path": path, "count": len(list), "entries": list})
}

// toolClipboardGet 实现 clipboard_get / android_get_clipboard
func toolClipboardGet() ([]map[string]any, bool) {
	out, ok := getClipboard()
	if !ok {
		return errMsg(out)
	}
	return jsonOK(map[string]any{"text": out})
}

// toolGetprop 实现 getprop / android_get_prop
func toolGetprop(args map[string]any) ([]map[string]any, bool) {
	key := argString(args, "key")
	if key != "" {
		return jsonOK(map[string]any{"key": key, "value": nilIfEmpty(getprop(key))})
	}
	props := getpropAll()
	return jsonOK(map[string]any{"count": len(props), "props": props})
}
