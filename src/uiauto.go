package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// uiauto.go — 屏幕控件树（UI hierarchy）工具
//
// 设计目标：用**结构化数据**替代截图做界面识别。
// 截图（android_screenshot）返回的是像素，AI 需要自己估算坐标、判断控件语义，
// 既费 token 又容易点错；本文件的工具直接读取 Android 的 uiautomator 控件树，
// 给出每个可交互元素的文本、资源 id、类名、可见描述与**精确中心坐标**，
// 让 AI 能「一次拿到信息 → 一次点中目标」。
//
// 原有截图工具保持不动（android_screenshot / screenshot 均未修改），
// 本文件新增的工具作为互补：截图用于「看渲染效果」，控件树用于「找控件并操作」。
//
// 实现要点：
//   - `uiautomator dump` 生成 XML 到设备文件，本进程读取后解析（encoding/xml）
//   - dump 常因界面动画报 "could not get idle state"，故带重试 + --compressed 回退
//   - 解析后做 DFS 展平，计算 depth / ref / 中心坐标 / label（自身或子孙文本），
//     并对「与祖先同文本」的冗余节点去重
//   - 短 TTL 内存缓存：get_screen_elements 之后紧跟 find/tap 可复用同一份 dump，
//     省掉一次 0.3~1.5s 的 dump 开销（fresh=true 可强制重新 dump）

// ---------- XML 结构 ----------

type uiNode struct {
	Index         string   `xml:"index,attr"`
	Text          string   `xml:"text,attr"`
	ResourceID    string   `xml:"resource-id,attr"`
	Class         string   `xml:"class,attr"`
	Package       string   `xml:"package,attr"`
	ContentDesc   string   `xml:"content-desc,attr"`
	Checkable     string   `xml:"checkable,attr"`
	Checked       string   `xml:"checked,attr"`
	Clickable     string   `xml:"clickable,attr"`
	Enabled       string   `xml:"enabled,attr"`
	Focusable     string   `xml:"focusable,attr"`
	Focused       string   `xml:"focused,attr"`
	Scrollable    string   `xml:"scrollable,attr"`
	LongClickable string   `xml:"long-clickable,attr"`
	Password      string   `xml:"password,attr"`
	Selected      string   `xml:"selected,attr"`
	Bounds        string   `xml:"bounds,attr"`
	Children      []uiNode `xml:"node"`
}

type uiHierarchy struct {
	XMLName  xml.Name `xml:"hierarchy"`
	Rotation string   `xml:"rotation,attr"`
	Nodes    []uiNode `xml:"node"`
}

// uiElement 展平后的单个控件（供 JSON 输出与选择器匹配）
type uiElement struct {
	Ref       int
	Depth     int
	Text      string
	Label     string // 自身文本，或子孙文本拼接（可点击容器常靠它表达语义）
	ID        string
	Desc      string
	Cls       string
	Pkg       string
	X1, Y1    int
	X2, Y2    int
	Click     bool
	LongClk   bool
	Scroll    bool
	Edit      bool
	Focus     bool
	FocusAble bool
	Check     bool
	Checked   bool
	Selected  bool
	Password  bool
	Enabled   bool
}

func (e uiElement) w() int { return e.X2 - e.X1 }
func (e uiElement) h() int { return e.Y2 - e.Y1 }
func (e uiElement) visible() bool {
	return e.w() > 0 && e.h() > 0 && e.X2 > 0 && e.Y2 > 0
}
func (e uiElement) actionable() bool {
	return e.Click || e.LongClk || e.Scroll || e.Edit || e.Check
}

// uiMeta dump 元信息
type uiMeta struct {
	Rotation   int      `json:"rotation"`
	ScreenW    int      `json:"screen_width"`
	ScreenH    int      `json:"screen_height"`
	TotalNodes int      `json:"total_nodes"`
	KeptNodes  int      `json:"kept_nodes"`
	Packages   []string `json:"packages"`
	Path       string   `json:"dump_path"`
	DumpMs     int64    `json:"dump_ms"`
	Cached     bool     `json:"from_cache"`
}

// ---------- dump ----------

type uiDumpResult struct {
	XML   string
	Path  string
	Meta  uiMeta
	Elems []uiElement
}

var uiCache struct {
	mu   sync.Mutex
	data *uiDumpResult
	at   time.Time
}

const (
	uiCacheTTL      = 2 * time.Second
	uiDumpDefault   = "/data/local/tmp/ksu_mcp_ui.xml"
	uiDumpRetries   = 3
	uiElemHardLimit = 400
)

// androidXMLDump 执行 uiautomator dump 并读取生成的 XML。
//
// 兼容性处理：
//   - 优先 `uiautomator dump --compressed <path>`（体积小、速度快）
//   - 失败回落 `uiautomator dump <path>`
//   - 界面动画会导致 "could not get idle state"，故重试 uiDumpRetries 次
//   - 若 dump 文件 mtime 早于本次命令开始时间，说明是上一轮的陈旧结果，按失败处理
func androidXMLDump(path string) (string, string, error) {
	if path == "" {
		path = uiDumpDefault
	}
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	variants := [][]string{
		{"dump", "--compressed", path},
		{"dump", path},
	}
	var lastErr string
	for attempt := 0; attempt < uiDumpRetries; attempt++ {
		for _, args := range variants {
			start := time.Now()
			out, err := runCmdOut("uiautomator", args...)
			if err != nil {
				lastErr = firstNonEmpty(strings.TrimSpace(out), err.Error())
				continue
			}
			fi, serr := os.Stat(path)
			if serr != nil {
				lastErr = "dump 文件未生成: " + serr.Error()
				continue
			}
			// 陈旧结果判定：文件修改时间早于命令开始时间 1s 以上
			if fi.ModTime().Before(start.Add(-1 * time.Second)) {
				lastErr = "dump 结果陈旧（可能未实际刷新）"
				continue
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				lastErr = "读取 dump 失败: " + rerr.Error()
				continue
			}
			if !strings.Contains(string(data), "<hierarchy") {
				lastErr = "dump 内容非法（缺少 <hierarchy>）: " + truncateErr(string(data), 120)
				continue
			}
			return string(data), path, nil
		}
		time.Sleep(350 * time.Millisecond)
	}
	if lastErr == "" {
		lastErr = "未知错误"
	}
	return "", path, fmt.Errorf("uiautomator dump 失败（已重试 %d 次）: %s；"+
		"若报 could not get idle state 请稍后重试或先停止界面动画", uiDumpRetries, lastErr)
}

// parseUIXML 解析 XML 并展平
func parseUIXML(data string) (*uiHierarchy, []uiElement, error) {
	var h uiHierarchy
	if err := xml.Unmarshal([]byte(data), &h); err != nil {
		return nil, nil, fmt.Errorf("解析控件树 XML 失败: %v", err)
	}
	elems := []uiElement{}
	ref := 0
	var walk func(n uiNode, depth int, ancestorTexts []string)
	walk = func(n uiNode, depth int, ancestorTexts []string) {
		x1, y1, x2, y2 := parseBounds(n.Bounds)
		el := uiElement{
			Depth: depth, Text: strings.TrimSpace(n.Text),
			ID: strings.TrimSpace(n.ResourceID), Desc: strings.TrimSpace(n.ContentDesc),
			Cls: shortClass(n.Class), Pkg: n.Package,
			X1: x1, Y1: y1, X2: x2, Y2: y2,
			Click: atoiBool(n.Clickable), LongClk: atoiBool(n.LongClickable),
			Scroll: atoiBool(n.Scrollable), Edit: isEditableClass(n.Class),
			Focus: atoiBool(n.Focused), FocusAble: atoiBool(n.Focusable),
			Check: atoiBool(n.Checkable), Checked: atoiBool(n.Checked),
			Selected: atoiBool(n.Selected), Password: atoiBool(n.Password),
			Enabled: atoiBoolDefault(n.Enabled, true),
		}
		// label：自身文本，否则取子孙文本（可点击容器靠子孙表达语义）
		el.Label = el.Text
		if el.Label == "" {
			el.Label = collectChildText(n, 64)
		}
		// 去重：文本与祖先完全相同且自身不可交互的节点跳过（纯粹冗余）
		if el.Label != "" && !el.actionable() {
			for _, at := range ancestorTexts {
				if at == el.Label {
					goto descend
				}
			}
		}
		if el.visible() {
			el.Ref = ref
			ref++
			elems = append(elems, el)
		}
	descend:
		childAnc := ancestorTexts
		if el.Label != "" {
			childAnc = append(append([]string{}, ancestorTexts...), el.Label)
		}
		for _, c := range n.Children {
			walk(c, depth+1, childAnc)
		}
	}
	for _, n := range h.Nodes {
		walk(n, 0, nil)
	}
	return &h, elems, nil
}

// collectChildText 收集子孙节点的文本（最多 maxLen 个字符）
func collectChildText(n uiNode, maxLen int) string {
	parts := []string{}
	var walk func(x uiNode)
	walk = func(x uiNode) {
		for _, c := range x.Children {
			t := strings.TrimSpace(firstNonEmpty(c.Text, c.ContentDesc))
			if t != "" {
				parts = append(parts, t)
			}
			if len(strings.Join(parts, " ")) >= maxLen {
				return
			}
			walk(c)
		}
	}
	walk(n)
	s := strings.Join(parts, " ")
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

func parseBounds(s string) (int, int, int, int) {
	var x1, y1, x2, y2 int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "[%d,%d][%d,%d]", &x1, &y1, &x2, &y2); err != nil {
		return 0, 0, 0, 0
	}
	return x1, y1, x2, y2
}

// shortClass 把 android.widget.Button 化简为 Button（省 token）
func shortClass(c string) string {
	c = strings.TrimSpace(c)
	if i := strings.LastIndex(c, "."); i >= 0 {
		return c[i+1:]
	}
	return c
}

func isEditableClass(c string) bool {
	low := strings.ToLower(c)
	for _, k := range []string{"edittext", "autocompletetextview", "searchview", "searchautocomplete"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

func atoiBool(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "true") }

func atoiBoolDefault(s string, def bool) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	return strings.EqualFold(s, "true")
}

// loadUI 获取控件树（带短 TTL 缓存 + 展平）
func loadUI(fresh bool) (*uiDumpResult, error) {
	uiCache.mu.Lock()
	if !fresh && uiCache.data != nil && time.Since(uiCache.at) < uiCacheTTL {
		d := *uiCache.data
		d.Meta.Cached = true
		uiCache.mu.Unlock()
		return &d, nil
	}
	uiCache.mu.Unlock()

	t0 := time.Now()
	raw, path, err := androidXMLDump("")
	if err != nil {
		return nil, err
	}
	h, elems, err := parseUIXML(raw)
	if err != nil {
		return nil, err
	}
	maxX, maxY := 0, 0
	for _, e := range elems {
		if e.X2 > maxX {
			maxX = e.X2
		}
		if e.Y2 > maxY {
			maxY = e.Y2
		}
	}
	pkgSet := map[string]bool{}
	pkgs := []string{}
	for _, e := range elems {
		if e.Pkg != "" && !pkgSet[e.Pkg] {
			pkgSet[e.Pkg] = true
			pkgs = append(pkgs, e.Pkg)
		}
	}
	res := &uiDumpResult{
		XML:  raw,
		Path: path,
		Meta: uiMeta{
			Rotation: atoiDefaultInt(h.Rotation), ScreenW: maxX, ScreenH: maxY,
			TotalNodes: countNodes(h), KeptNodes: len(elems), Packages: pkgs,
			Path: path, DumpMs: time.Since(t0).Milliseconds(),
		},
		Elems: elems,
	}
	uiCache.mu.Lock()
	uiCache.data = res
	uiCache.at = time.Now()
	uiCache.mu.Unlock()
	return res, nil
}

func atoiDefaultInt(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

func countNodes(h *uiHierarchy) int {
	n := 0
	var walk func(x uiNode)
	walk = func(x uiNode) {
		n++
		for _, c := range x.Children {
			walk(c)
		}
	}
	for _, r := range h.Nodes {
		walk(r)
	}
	return n
}

// ---------- 元素 JSON（紧凑、省略空值以省 token） ----------

func elementJSON(e uiElement, withBounds bool) map[string]any {
	m := map[string]any{"ref": e.Ref}
	if e.Cls != "" {
		m["class"] = e.Cls
	}
	if e.Label != "" {
		m["label"] = e.Label
	}
	if e.Text != "" && e.Text != e.Label {
		m["text"] = e.Text
	}
	if e.Desc != "" && e.Desc != e.Label {
		m["desc"] = e.Desc
	}
	if e.ID != "" {
		m["id"] = e.ID
	}
	if withBounds {
		m["bounds"] = []int{e.X1, e.Y1, e.X2, e.Y2}
	}
	m["center"] = []int{(e.X1 + e.X2) / 2, (e.Y1 + e.Y2) / 2}
	// 布尔量只在 true 时输出（enabled 只在 false 时输出），显著减小载荷
	if e.Click {
		m["clickable"] = true
	}
	if e.Edit {
		m["editable"] = true
	}
	if e.Scroll {
		m["scrollable"] = true
	}
	if e.LongClk {
		m["long_clickable"] = true
	}
	if e.Check {
		m["checkable"] = true
	}
	if e.Checked {
		m["checked"] = true
	}
	if e.Selected {
		m["selected"] = true
	}
	if e.Focus {
		m["focused"] = true
	}
	if e.Password {
		m["password"] = true
	}
	if !e.Enabled {
		m["enabled"] = false
	}
	return m
}

// ---------- 选择器 ----------

// uiSelector 控件选择器（各条件为 AND 关系；全部为空则匹配所有可交互元素）
type uiSelector struct {
	Text         string
	TextContains string
	ID           string
	IDContains   string
	Desc         string
	DescContains string
	ClassContain string
	Ref          int
	Clickable    *bool
	Scrollable   *bool
	Editable     *bool
	Enabled      *bool
	Checked      *bool
	Password     *bool
}

func (s uiSelector) isEmpty() bool {
	return s.Text == "" && s.TextContains == "" && s.ID == "" && s.IDContains == "" &&
		s.Desc == "" && s.DescContains == "" && s.ClassContain == "" && s.Ref < 0 &&
		s.Clickable == nil && s.Scrollable == nil && s.Editable == nil &&
		s.Enabled == nil && s.Checked == nil && s.Password == nil
}

func (s uiSelector) match(e uiElement) bool {
	if s.Ref >= 0 && e.Ref != s.Ref {
		return false
	}
	if s.Text != "" && e.Text != s.Text && e.Label != s.Text {
		return false
	}
	if s.TextContains != "" {
		hay := strings.ToLower(e.Text + " " + e.Label)
		if !strings.Contains(hay, strings.ToLower(s.TextContains)) {
			return false
		}
	}
	if s.ID != "" && e.ID != s.ID {
		return false
	}
	if s.IDContains != "" && !strings.Contains(strings.ToLower(e.ID), strings.ToLower(s.IDContains)) {
		return false
	}
	if s.Desc != "" && e.Desc != s.Desc {
		return false
	}
	if s.DescContains != "" && !strings.Contains(strings.ToLower(e.Desc), strings.ToLower(s.DescContains)) {
		return false
	}
	if s.ClassContain != "" && !strings.Contains(strings.ToLower(e.Cls), strings.ToLower(s.ClassContain)) {
		return false
	}
	if s.Clickable != nil && e.Click != *s.Clickable {
		return false
	}
	if s.Scrollable != nil && e.Scroll != *s.Scrollable {
		return false
	}
	if s.Editable != nil && e.Edit != *s.Editable {
		return false
	}
	if s.Enabled != nil && e.Enabled != *s.Enabled {
		return false
	}
	if s.Checked != nil && e.Checked != *s.Checked {
		return false
	}
	if s.Password != nil && e.Password != *s.Password {
		return false
	}
	return true
}

func selectorFromArgs(args map[string]any) uiSelector {
	return uiSelector{
		Text:         argString(args, "text"),
		TextContains: argString(args, "text_contains"),
		ID:           argString(args, "id"),
		IDContains:   argString(args, "id_contains"),
		Desc:         argString(args, "desc"),
		DescContains: argString(args, "desc_contains"),
		ClassContain: argString(args, "class_contains"),
		Ref:          argIntDefault(args, "ref", -1),
		Clickable:    argBoolPtr(args, "clickable"),
		Scrollable:   argBoolPtr(args, "scrollable"),
		Editable:     argBoolPtr(args, "editable"),
		Enabled:      argBoolPtr(args, "enabled"),
		Checked:      argBoolPtr(args, "checked"),
		Password:     argBoolPtr(args, "password"),
	}
}

func argIntDefault(args map[string]any, key string, def int) int {
	if p := argIntPtr(args, key); p != nil {
		return *p
	}
	return def
}

func matchElements(elems []uiElement, sel uiSelector) []uiElement {
	out := []uiElement{}
	for _, e := range elems {
		if sel.match(e) {
			out = append(out, e)
		}
	}
	return out
}

// ---------- 工具定义 ----------

func uiToolEntries() []toolEntry {
	return []toolEntry{
		{
			def: ToolDef{
				Name:  "android_get_screen_elements",
				Title: "屏幕控件（结构化）",
				Description: "**做界面识别时优先用本工具，而不是截图。** 读取当前屏幕的控件树，直接返回结构化元素列表" +
					"（文本 / 资源 id / 类名 / 可见描述 / 精确中心坐标），一次调用即可拿到「屏幕上有什么、能点什么、点哪里」。\n\n" +
					"何时使用：需要知道当前界面有哪些按钮/输入框/列表项、要点击某个可见文字、要填写某个输入框、\n" +
					"要判断某控件是否可点/是否已勾选/哪个输入框正处于焦点。\n" +
					"何时改用截图（android_screenshot）：需要看渲染效果、图片内容、被遮挡的视觉信息，或控件树里查不到目标。\n\n" +
					"参数：\n" +
					"- only_interactive（默认 true）：只返回可交互元素（可点/可滚/可输入/可勾选/可长按）" +
					"以及承载文本的叶子节点；设为 false 返回全部可见节点\n" +
					"- filter（可选）：对 label/text/id/desc 做大小写不敏感子串过滤\n" +
					"- max_elements（默认 80，最大 400）：返回条数上限（先保证 token 可控）\n" +
					"- include_bounds（默认 true）：是否返回 [x1,y1,x2,y2] 像素边界；只要坐标时可设 false 省 token\n" +
					"- fresh（默认 false）：默认复用 2 秒内的控件树缓存（省 0.3~1.5s）；需要最新状态时设 true\n\n" +
					"返回 JSON：\n" +
					"- meta：{rotation, screen_width, screen_height, total_nodes, kept_nodes, packages, dump_path, dump_ms, from_cache}\n" +
					"- elements：数组，每项含 ref（本次 dump 内的稳定序号）、class、label（自身文本或子孙文本，\n" +
					"  可点击容器用它表达语义）、center [x,y]、bounds [x1,y1,x2,y2]；\n" +
					"  非空时才出现的字段：text、desc、id；\n" +
					"  为 true 时才出现的标记：clickable、editable、scrollable、long_clickable、checkable、checked、\n" +
					"  selected、focused、password；仅当控件被禁用时出现 enabled:false\n" +
					"- truncated：是否因 max_elements 被截断\n\n" +
					"使用要点：\n" +
					"- 拿到结果后可直接用 android_input_tap(center) 点击，或用 android_tap_element(text=...) 一步到位\n" +
					"- ref 只在同一次屏幕状态下有效（界面变化后需重新获取）\n" +
					"- 依赖系统 uiautomator，个别 ROM 不可用（会返回带原因的 isError）\n" +
					"- 若报 could not get idle state：稍后重试即可（本工具内部已重试 3 次）",
				InputSchema: schema(map[string]any{
					"only_interactive": boolPropDefault("只返回可交互元素与文本节点，默认 true", true),
					"filter":           strProp("对 label/text/id/desc 做子串过滤（大小写不敏感）；省略不过滤"),
					"max_elements":     intPropDefault("返回元素条数上限，默认 80，最大 400", 80, 1, 400),
					"include_bounds":   boolPropDefault("是否返回像素边界 [x1,y1,x2,y2]，默认 true", true),
					"fresh":            boolPropDefault("是否强制重新抓取控件树（忽略 2s 缓存），默认 false", false),
				}, nil),
				Annotations: annotations("屏幕控件（结构化）", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				res, err := loadUI(argBool(args, "fresh"))
				if err != nil {
					return errMsg(err.Error())
				}
				onlyInteractive := !hasBoolFalse(args, "only_interactive")
				filter := strings.ToLower(argString(args, "filter"))
				maxN := defaultInt(argInt(args, "max_elements"), 80)
				if maxN > uiElemHardLimit {
					maxN = uiElemHardLimit
				}
				withBounds := !hasBoolFalse(args, "include_bounds")

				out := []map[string]any{}
				for _, e := range res.Elems {
					if onlyInteractive && !e.actionable() && e.Label == "" && e.ID == "" && e.Desc == "" {
						continue
					}
					if filter != "" {
						hay := strings.ToLower(e.Label + " " + e.Text + " " + e.ID + " " + e.Desc + " " + e.Cls)
						if !strings.Contains(hay, filter) {
							continue
						}
					}
					out = append(out, elementJSON(e, withBounds))
					if len(out) >= maxN {
						break
					}
				}
				return jsonOK(map[string]any{
					"meta":      res.Meta,
					"count":     len(out),
					"truncated": len(out) >= maxN,
					"elements":  out,
					"tip": "用 android_input_tap(x,y)=center 点击，或 android_tap_element(text/id=...) 一步点击；" +
						"输入框用 android_set_element_text(text=..., id=...)",
				})
			},
		},
		{
			def: ToolDef{
				Name:  "android_dump_ui_hierarchy",
				Title: "完整控件树",
				Description: "导出当前屏幕的**完整控件树**（原始 XML 或完整 JSON 树），用于深度排查：" +
					"当 android_get_screen_elements 的紧凑视图不够用（需要看到全部层级、无文本节点、布局容器）时使用。\n\n" +
					"参数：\n" +
					"- format（默认 json）：json=完整结构化树（保留层级与全部属性）；xml=原始 uiautomator XML 文本\n" +
					"- max_nodes（默认 300，最大 2000）：JSON 模式下最多输出多少个节点，防止超大界面把上下文撑爆\n" +
					"- fresh（默认 true）：是否强制重新抓取（默认不复用缓存，保证拿到最新树）\n\n" +
					"返回 JSON字段：\n" +
					"- meta：同 android_get_screen_elements（含屏幕尺寸、旋转、节点总数、dump 耗时）\n" +
					"- format=json 时：tree（嵌套节点数组，每节点含 class/text/id/desc/bounds/center/depth/布尔标记/children）、\n" +
					"  returned_nodes、truncated\n" +
					"- format=xml 时：xml（原始文本）、size_bytes\n\n" +
					"成本提示：完整树通常有 100~800 个节点，XML 文本常达 10~80 KB，**明显比 android_get_screen_elements 费 token**。\n" +
					"常规界面识别请优先用 android_get_screen_elements；本工具仅用于它解释不了的疑难场景。",
				InputSchema: schema(map[string]any{
					"format":    strPropEnumDefault("输出格式，默认 json", "json", "json", "xml"),
					"max_nodes": intPropDefault("JSON 模式最多输出节点数，默认 300，最大 2000", 300, 1, 2000),
					"fresh":     boolPropDefault("是否强制重新抓取控件树，默认 true", true),
				}, nil),
				Annotations: annotations("完整控件树", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				res, err := loadUI(!hasBoolFalse(args, "fresh"))
				if err != nil {
					return errMsg(err.Error())
				}
				if argString(args, "format") == "xml" {
					return jsonOK(map[string]any{
						"meta":       res.Meta,
						"format":     "xml",
						"size_bytes": len(res.XML),
						"xml":        res.XML,
					})
				}
				var h uiHierarchy
				if err := xml.Unmarshal([]byte(res.XML), &h); err != nil {
					return errMsg("解析控件树失败: " + err.Error())
				}
				limit := defaultInt(argInt(args, "max_nodes"), 300)
				if limit > 2000 {
					limit = 2000
				}
				n := 0
				var conv func(node uiNode, depth int) map[string]any
				conv = func(node uiNode, depth int) map[string]any {
					n++
					x1, y1, x2, y2 := parseBounds(node.Bounds)
					m := map[string]any{
						"depth":  depth,
						"class":  shortClass(node.Class),
						"bounds": []int{x1, y1, x2, y2},
						"center": []int{(x1 + x2) / 2, (y1 + y2) / 2},
					}
					if t := strings.TrimSpace(node.Text); t != "" {
						m["text"] = t
					}
					if v := strings.TrimSpace(node.ResourceID); v != "" {
						m["id"] = v
					}
					if v := strings.TrimSpace(node.ContentDesc); v != "" {
						m["desc"] = v
					}
					if v := strings.TrimSpace(node.Package); v != "" {
						m["package"] = v
					}
					for k, v := range map[string]string{
						"clickable": node.Clickable, "long_clickable": node.LongClickable,
						"scrollable": node.Scrollable, "checkable": node.Checkable,
						"checked": node.Checked, "selected": node.Selected,
						"focused": node.Focused, "focusable": node.Focusable,
						"password": node.Password,
					} {
						if atoiBool(v) {
							m[k] = true
						}
					}
					if !atoiBoolDefault(node.Enabled, true) {
						m["enabled"] = false
					}
					kids := []map[string]any{}
					for _, c := range node.Children {
						if n >= limit {
							break
						}
						kids = append(kids, conv(c, depth+1))
					}
					if len(kids) > 0 {
						m["children"] = kids
					}
					return m
				}
				tree := []map[string]any{}
				for _, r := range h.Nodes {
					if n >= limit {
						break
					}
					tree = append(tree, conv(r, 0))
				}
				return jsonOK(map[string]any{
					"meta":           res.Meta,
					"format":         "json",
					"returned_nodes": n,
					"truncated":      n >= limit,
					"tree":           tree,
				})
			},
		},
		{
			def: ToolDef{
				Name:  "android_find_element",
				Title: "查找控件",
				Description: "按选择器在当前屏幕控件树中**精确查找**控件，返回全部匹配项及其坐标。" +
					"适合「先确认目标存在 / 有多个候选需要挑选」的场景；若只想直接点，用 android_tap_element 一步完成更省钱。\n\n" +
					"选择器参数（多个条件为 AND 关系，至少给一个）：\n" +
					"- text / text_contains：匹配控件文本或可见标签（label）\n" +
					"- id / id_contains：匹配资源 id，如 com.tencent.mm:id/btn\n" +
					"- desc / desc_contains：匹配 content-desc（无障碍描述）\n" +
					"- class_contains：按类名子串匹配，如 EditText、Button、RecyclerView\n" +
					"- clickable / scrollable / editable / enabled / checked / password：按布尔属性筛选\n" +
					"- ref：直接按上一次 android_get_screen_elements 返回的 ref 定位（仅同一屏幕状态有效）\n" +
					"- limit（默认 5，最大 50）：最多返回几个匹配项\n" +
					"- fresh（默认 false）：是否强制重新抓取（默认复用 2s 内缓存）\n\n" +
					"返回 JSON：\n" +
					"- meta（屏幕/dump 信息）、total_matches（匹配总数）、returned（返回条数）\n" +
					"- matches：数组，每项含 ref、class、label/text/id/desc、bounds、center 与 true 才出现的布尔标记\n" +
					"- hint：找到 1 个时给出一句话点击建议\n\n" +
					"常见用法：查登录按钮 → {text:\"登录\"}；查所有可点项 → {clickable:true}；查输入框 → {editable:true}。",
				InputSchema: schema(map[string]any{
					"text":           strProp("精确匹配控件文本或可见标签"),
					"text_contains":  strProp("文本/标签子串匹配（大小写不敏感）"),
					"id":             strProp("精确匹配资源 id"),
					"id_contains":    strProp("资源 id 子串匹配"),
					"desc":           strProp("精确匹配 content-desc"),
					"desc_contains":  strProp("content-desc 子串匹配"),
					"class_contains": strProp("类名子串匹配，如 EditText"),
					"clickable":      boolProp("仅匹配可点击（true）/ 不可点击（false）"),
					"scrollable":     boolProp("仅匹配可滚动"),
					"editable":       boolProp("仅匹配可输入"),
					"enabled":        boolProp("仅匹配启用/禁用"),
					"checked":        boolProp("仅匹配已勾选/未勾选"),
					"password":       boolProp("仅匹配密码框"),
					"ref":            intPropRange("按上一次控件列表的 ref 定位（仅同屏幕状态有效）", 0, 100000),
					"limit":          intPropDefault("最多返回几个匹配项，默认 5，最大 50", 5, 1, 50),
					"fresh":          boolPropDefault("是否强制重新抓取控件树，默认 false", false),
				}, nil),
				Annotations: annotations("查找控件", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				sel := selectorFromArgs(args)
				if sel.isEmpty() {
					return errMsg("至少需要一个选择器条件（text / text_contains / id / id_contains / " +
						"desc / desc_contains / class_contains / clickable / scrollable / editable / enabled / checked / password / ref）")
				}
				res, err := loadUI(argBool(args, "fresh"))
				if err != nil {
					return errMsg(err.Error())
				}
				matches := matchElements(res.Elems, sel)
				limit := defaultInt(argInt(args, "limit"), 5)
				if limit > 50 {
					limit = 50
				}
				out := []map[string]any{}
				for i, e := range matches {
					if i >= limit {
						break
					}
					out = append(out, elementJSON(e, true))
				}
				resp := map[string]any{
					"meta":          res.Meta,
					"total_matches": len(matches),
					"returned":      len(out),
					"matches":       out,
				}
				if len(matches) == 1 {
					resp["hint"] = fmt.Sprintf("已唯一定位，可用 android_input_tap(x=%d, y=%d) 点击，或 android_tap_element 同名选择器直接点",
						(matches[0].X1+matches[0].X2)/2, (matches[0].Y1+matches[0].Y2)/2)
				} else if len(matches) == 0 {
					resp["hint"] = "未找到匹配控件。可先用 android_get_screen_elements 看当前屏有哪些元素（界面可能已变化，或目标需先滚动/等待）"
				}
				return jsonOK(resp)
			},
		},
		{
			def: ToolDef{
				Name:  "android_tap_element",
				Title: "按选择器点击",
				Description: "**按选择器定位并点击控件**（自动取其中心坐标），把「查找 + 计算坐标 + 点击」合并为一次调用，" +
					"是最省时省钱的点击方式；比先 android_find_element 再 android_input_tap 少一次往返。\n\n" +
					"参数：\n" +
					"- 选择器：与 android_find_element 完全相同（text / text_contains / id / id_contains / desc /\n" +
					"  desc_contains / class_contains / clickable / scrollable / editable / enabled / checked / password / ref）\n" +
					"- match_index（默认 0）：匹配到多个时点第几个（0 起，按屏幕从上到下顺序）\n" +
					"- wait_ms（默认 0，最大 15000）：先等待目标出现再点（适合加载中的界面）\n" +
					"- verify（默认 true）：点击后重新抓取控件树并报告界面是否发生变化（便于确认点到了）\n\n" +
					"返回 JSON：tapped（是否点到）、element（被点控件的 label/id/bounds/center）、\n" +
					"match_total（匹配数）、changed（verify=true 时：屏幕元素集合是否变化）、elapsed_ms。\n\n" +
					"注意：本工具会真实改变设备状态。若点完没反应，检查是否点到了被遮挡的控件——\n" +
					"可改用 android_get_screen_elements 查看目标 center 是否被上层元素覆盖。",
				InputSchema: schema(map[string]any{
					"text":           strProp("精确匹配控件文本或可见标签，如 \"登录\""),
					"text_contains":  strProp("文本/标签子串匹配"),
					"id":             strProp("精确匹配资源 id"),
					"id_contains":    strProp("资源 id 子串匹配"),
					"desc":           strProp("精确匹配 content-desc"),
					"desc_contains":  strProp("content-desc 子串匹配"),
					"class_contains": strProp("类名子串匹配"),
					"ref":            intPropRange("按控件列表的 ref 定位", 0, 100000),
					"clickable":      boolProp("仅匹配可点击"),
					"match_index":    intPropDefault("匹配到多个时点第几个（0 起），默认 0", 0, 0, 1000),
					"wait_ms":        intPropDefault("先等待目标出现的毫秒数，默认 0（不等待），最大 15000", 0, 0, 15000),
					"verify":         boolPropDefault("点击后校验界面是否变化，默认 true", true),
					"fresh":          boolPropDefault("是否强制重新抓取控件树，默认 true", true),
				}, nil),
				Annotations: annotations("按选择器点击", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				sel := selectorFromArgs(args)
				if sel.isEmpty() {
					return errMsg("至少需要一个选择器条件（text / id / desc / class_contains / ref 等）")
				}
				if sel.Ref >= 0 && argInt(args, "wait_ms") > 0 {
					return errMsg("ref 与 wait_ms 不能同时使用：ref 仅在同一次屏幕状态下有效，等待场景请改用文本/id 选择器")
				}
				t0 := time.Now()
				waitMs := argInt(args, "wait_ms")
				var matches []uiElement
				if waitMs > 0 {
					m, _, err := waitForSelector(sel, true, time.Duration(waitMs)*time.Millisecond)
					if err != nil {
						return errMsg(err.Error())
					}
					matches = m
				} else {
					res, err := loadUI(argBool(args, "fresh"))
					if err != nil {
						return errMsg(err.Error())
					}
					matches = matchElements(res.Elems, sel)
				}
				if len(matches) == 0 {
					return jsonOK(map[string]any{
						"tapped": false, "match_total": 0, "elapsed_ms": time.Since(t0).Milliseconds(),
						"hint": "未找到匹配控件；可先用 android_get_screen_elements 查看当前屏，必要时先滚动或等待",
					})
				}
				idx := argInt(args, "match_index")
				if idx < 0 || idx >= len(matches) {
					idx = 0
				}
				target := matches[idx]
				cx, cy := (target.X1+target.X2)/2, (target.Y1+target.Y2)/2
				verify := !hasBoolFalse(args, "verify")
				before := ""
				if verify {
					before = uiFingerprint()
				}
				if out, ok := inputTap(cx, cy); !ok {
					return errMsg("点击失败: " + out)
				}
				changed := false
				if verify {
					time.Sleep(280 * time.Millisecond)
					changed = uiFingerprint() != before
				}
				return jsonOK(map[string]any{
					"tapped":      true,
					"element":     elementJSON(target, true),
					"match_total": len(matches),
					"changed":     changed,
					"elapsed_ms":  time.Since(t0).Milliseconds(),
				})
			},
		},
		{
			def: ToolDef{
				Name:  "android_set_element_text",
				Title: "按选择器输入文本",
				Description: "**按选择器定位输入框并写入文本**（替代「手动点击输入框 + android_input_text」两步操作）。\n\n" +
					"参数：\n" +
					"- 选择器：同 android_find_element（常用 text/id/desc/class_contains=\"EditText\"/editable=true）\n" +
					"- content（必填）：要写入的文本\n" +
					"- clear_first（默认 true）：先清空原内容（定位到末尾后按 DEL，次数=原文本长度+8）\n" +
					"- submit（默认 false）：输入完成后按回车（KEYCODE_ENTER），适合搜索框\n" +
					"- verify（默认 true）：输入后重新抓取控件树，核对控件文本是否已变为目标内容\n\n" +
					"返回 JSON：filled（是否执行成功）、element（目标控件）、cleared（是否清空）、submitted、\n" +
					"verified（verify=true 时：控件文本是否等于 content）、current_text（输入后读回的实际文本）、elapsed_ms。\n\n" +
					"注意：会改变设备状态；中文输入依赖系统输入法（走 IME 注入）。\n" +
					"若 verified=false，说明可能未真正聚焦到该框——可改用 android_find_element 确认坐标后手动点击再输入。",
				InputSchema: schema(map[string]any{
					"content":        strProp("要写入的文本内容（必填）"),
					"text":           strProp("选择器：精确匹配控件文本/标签"),
					"text_contains":  strProp("选择器：文本子串匹配"),
					"id":             strProp("选择器：精确匹配资源 id"),
					"id_contains":    strProp("选择器：资源 id 子串匹配"),
					"desc":           strProp("选择器：精确匹配 content-desc"),
					"desc_contains":  strProp("选择器：content-desc 子串匹配"),
					"class_contains": strProp("选择器：类名子串匹配，如 EditText"),
					"ref":            intPropRange("选择器：按 ref 定位", 0, 100000),
					"editable":       boolProp("选择器：仅匹配可输入控件"),
					"clear_first":    boolPropDefault("先清空原内容，默认 true", true),
					"submit":         boolPropDefault("输入后按回车，默认 false", false),
					"verify":         boolPropDefault("输入后读回校验，默认 true", true),
					"fresh":          boolPropDefault("是否强制重新抓取控件树，默认 true", true),
				}, []string{"content"}),
				Annotations: annotations("按选择器输入文本", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				content := argString(args, "content")
				if content == "" {
					return errMsg("content 不能为空（要写入的文本）")
				}
				sel := selectorFromArgs(args)
				if sel.isEmpty() {
					return errMsg("至少需要一个选择器条件（常用 id / text / class_contains=\"EditText\" / editable=true）")
				}
				t0 := time.Now()
				res, err := loadUI(argBool(args, "fresh"))
				if err != nil {
					return errMsg(err.Error())
				}
				matches := matchElements(res.Elems, sel)
				if len(matches) == 0 {
					return jsonOK(map[string]any{
						"filled": false, "elapsed_ms": time.Since(t0).Milliseconds(),
						"hint": "未找到匹配控件；可先用 android_get_screen_elements 确认输入框的 id 或 editable 属性",
					})
				}
				target := matches[0]
				cx, cy := (target.X1+target.X2)/2, (target.Y1+target.Y2)/2
				if out, ok := inputTap(cx, cy); !ok {
					return errMsg("聚焦输入框失败: " + out)
				}
				time.Sleep(180 * time.Millisecond)

				cleared := false
				if !hasBoolFalse(args, "clear_first") {
					// 移动到行尾后按 DEL：次数取原文本长度 + 余量
					_, _ = runCmdOut("input", "keyevent", "123") // KEYCODE_MOVE_END
					n := len([]rune(target.Text)) + 8
					if n > 128 {
						n = 128
					}
					for i := 0; i < n; i++ {
						_, _ = runCmdOut("input", "keyevent", "67") // KEYCODE_DEL
					}
					cleared = true
				}
				if out, ok := inputText(content); !ok {
					return errMsg("输入文本失败: " + out)
				}
				submitted := false
				if argBool(args, "submit") {
					_, _ = runCmdOut("input", "keyevent", "66") // KEYCODE_ENTER
					submitted = true
				}
				result := map[string]any{
					"filled": true, "element": elementJSON(target, true),
					"cleared": cleared, "submitted": submitted,
					"elapsed_ms": time.Since(t0).Milliseconds(),
				}
				if !hasBoolFalse(args, "verify") {
					time.Sleep(300 * time.Millisecond)
					if after, err2 := loadUI(true); err2 == nil {
						cur := ""
						if m := matchElements(after.Elems, sel); len(m) > 0 {
							cur = firstNonEmpty(m[0].Text, m[0].Label)
						} else if m := matchElements(after.Elems, uiSelector{Editable: uiTrue()}); len(m) > 0 {
							cur = m[0].Text
						}
						result["verified"] = cur == content
						result["current_text"] = cur
					}
				}
				return jsonOK(result)
			},
		},
		{
			def: ToolDef{
				Name:  "android_wait_for_element",
				Title: "等待控件出现/消失",
				Description: "轮询控件树，等待某个控件**出现**或**消失**，用于处理加载中、启动页、弹窗等异步界面。" +
					"比「AI 反复 sleep + 截图」省时且不浪费 token。\n\n" +
					"参数：\n" +
					"- 选择器：同 android_find_element（不支持 ref —— ref 会随界面变化失效）\n" +
					"- state（默认 present）：present=等它出现；absent=等它消失\n" +
					"- timeout_ms（默认 10000，最大 60000）：最长等待时间\n" +
					"- interval_ms（默认 500，最小 200）：轮询间隔\n\n" +
					"返回 JSON：satisfied（是否满足条件）、state、elapsed_ms、polls（轮询次数）、\n" +
					"element（state=present 且命中时返回该控件信息）。\n\n" +
					"成本提示：本工具在设备端轮询，不额外消耗 AI 上下文；典型用法是点击/打开应用后等某个标志控件出现。",
				InputSchema: schema(map[string]any{
					"text":           strProp("选择器：精确匹配控件文本/标签"),
					"text_contains":  strProp("选择器：文本子串匹配"),
					"id":             strProp("选择器：精确匹配资源 id"),
					"id_contains":    strProp("选择器：资源 id 子串匹配"),
					"desc":           strProp("选择器：精确匹配 content-desc"),
					"desc_contains":  strProp("选择器：content-desc 子串匹配"),
					"class_contains": strProp("选择器：类名子串匹配"),
					"clickable":      boolProp("选择器：仅匹配可点击"),
					"state":          strPropEnumDefault("等待出现(present)还是消失(absent)，默认 present", "present", "present", "absent"),
					"timeout_ms":     intPropDefault("最长等待毫秒数，默认 10000，最大 60000", 10000, 200, 60000),
					"interval_ms":    intPropDefault("轮询间隔毫秒，默认 500，最小 200", 500, 200, 5000),
				}, nil),
				Annotations: annotations("等待控件出现/消失", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				sel := selectorFromArgs(args)
				if sel.isEmpty() {
					return errMsg("至少需要一个选择器条件（text / id / desc / class_contains 等）")
				}
				if sel.Ref >= 0 {
					return errMsg("等待场景不支持 ref（ref 会随界面变化失效），请改用 text/id/desc 选择器")
				}
				absent := argString(args, "state") == "absent"
				timeout := time.Duration(defaultInt(argInt(args, "timeout_ms"), 10000)) * time.Millisecond
				interval := time.Duration(defaultInt(argInt(args, "interval_ms"), 500)) * time.Millisecond
				if interval < 200*time.Millisecond {
					interval = 200 * time.Millisecond
				}
				t0 := time.Now()
				polls := 0
				var last []uiElement
				for {
					polls++
					res, err := loadUI(true)
					if err != nil {
						return errMsg(err.Error())
					}
					last = matchElements(res.Elems, sel)
					found := len(last) > 0
					if (!absent && found) || (absent && !found) {
						out := map[string]any{
							"satisfied": true, "state": map[bool]string{true: "absent", false: "present"}[absent],
							"elapsed_ms": time.Since(t0).Milliseconds(), "polls": polls,
						}
						if !absent {
							out["element"] = elementJSON(last[0], true)
						}
						return jsonOK(out)
					}
					if time.Since(t0) >= timeout {
						out := map[string]any{
							"satisfied": false, "state": map[bool]string{true: "absent", false: "present"}[absent],
							"elapsed_ms": time.Since(t0).Milliseconds(), "polls": polls,
							"hint": "超时未满足条件；可先用 android_get_screen_elements 看当前屏实际内容（界面可能与预期不同）",
						}
						if !absent && len(last) == 0 {
							out["partial"] = "条件未命中"
						}
						return jsonOK(out)
					}
					time.Sleep(interval)
				}
			},
		},
		{
			def: ToolDef{
				Name:  "android_scroll_to_element",
				Title: "滚动查找控件",
				Description: "沿指定方向**反复滚动并查找控件**，直到找到或达到次数上限。" +
					"用于列表/长页面中定位不在当前屏的目标（避免 AI 手动「滑动→截图→判断」的多次往返）。\n\n" +
					"参数：\n" +
					"- 选择器：同 android_find_element（不支持 ref）\n" +
					"- direction（默认 down）：**内容滚动方向**；down=看下面的内容（手指上滑）、up=看上面的内容、\n" +
					"  right=看右侧内容（手指左滑）、left=看左侧内容\n" +
					"- max_scrolls（默认 5，最大 20）：最多滚动几次\n" +
					"- distance_percent（默认 60，范围 20-90）：每次滑动占屏幕对应边长的百分比\n" +
					"- settle_ms（默认 350）：每次滚动后等待界面稳定的毫秒数\n\n" +
					"返回 JSON：found（是否找到）、scrolls（实际滚动次数）、elapsed_ms、element（找到时的控件信息，含 center 可直接点）。\n\n" +
					"注意：会改变设备状态（滚动）。若 found=false，可能是列表已到底、目标在别的标签页，或需要改用\n" +
					"android_get_screen_elements 确认当前页面结构。",
				InputSchema: schema(map[string]any{
					"text":             strProp("选择器：精确匹配控件文本/标签"),
					"text_contains":    strProp("选择器：文本子串匹配"),
					"id":               strProp("选择器：精确匹配资源 id"),
					"id_contains":      strProp("选择器：资源 id 子串匹配"),
					"desc":             strProp("选择器：精确匹配 content-desc"),
					"desc_contains":    strProp("选择器：content-desc 子串匹配"),
					"class_contains":   strProp("选择器：类名子串匹配"),
					"direction":        strPropEnumDefault("内容滚动方向，默认 down（看下面的内容）", "down", "down", "up", "left", "right"),
					"max_scrolls":      intPropDefault("最多滚动次数，默认 5，最大 20", 5, 1, 20),
					"distance_percent": intPropDefault("每次滑动距离占屏幕边长百分比，默认 60", 60, 20, 90),
					"settle_ms":        intPropDefault("每次滚动后等待稳定的毫秒数，默认 350", 350, 100, 3000),
				}, nil),
				Annotations: annotations("滚动查找控件", false, true),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				sel := selectorFromArgs(args)
				if sel.isEmpty() {
					return errMsg("至少需要一个选择器条件（text / id / desc / class_contains 等）")
				}
				if sel.Ref >= 0 {
					return errMsg("滚动查找不支持 ref，请改用 text/id/desc 选择器")
				}
				direction := orDefault(argString(args, "direction"), "down")
				maxScrolls := defaultInt(argInt(args, "max_scrolls"), 5)
				if maxScrolls > 20 {
					maxScrolls = 20
				}
				pct := defaultInt(argInt(args, "distance_percent"), 60)
				if pct < 20 {
					pct = 20
				}
				if pct > 90 {
					pct = 90
				}
				settle := time.Duration(defaultInt(argInt(args, "settle_ms"), 350)) * time.Millisecond

				t0 := time.Now()
				scrolls := 0
				for {
					res, err := loadUI(true)
					if err != nil {
						return errMsg(err.Error())
					}
					if m := matchElements(res.Elems, sel); len(m) > 0 {
						return jsonOK(map[string]any{
							"found":      true,
							"scrolls":    scrolls,
							"elapsed_ms": time.Since(t0).Milliseconds(),
							"element":    elementJSON(m[0], true),
						})
					}
					if scrolls >= maxScrolls {
						return jsonOK(map[string]any{
							"found": false, "scrolls": scrolls, "elapsed_ms": time.Since(t0).Milliseconds(),
							"screen": map[string]any{"w": res.Meta.ScreenW, "h": res.Meta.ScreenH},
							"hint":   "滚动到上限仍未找到；可能已到列表底部、目标在别的标签页，或选择器写法与控件不符",
						})
					}
					w, h := res.Meta.ScreenW, res.Meta.ScreenH
					if w <= 0 || h <= 0 {
						return errMsg("无法确定屏幕尺寸，滚动中止")
					}
					cx, cy := w/2, h/2
					dx, dy := w*pct/100, h*pct/100
					var x1, y1, x2, y2 int
					switch direction {
					case "up":
						x1, y1, x2, y2 = cx, cy-dy/2, cx, cy+dy/2
					case "left":
						x1, y1, x2, y2 = cx+dx/2, cy, cx-dx/2, cy
					case "right":
						x1, y1, x2, y2 = cx-dx/2, cy, cx+dx/2, cy
					default: // down
						x1, y1, x2, y2 = cx, cy+dy/2, cx, cy-dy/2
					}
					if out, ok := inputSwipe(x1, y1, x2, y2, 320); !ok {
						return errMsg("滚动失败: " + out)
					}
					scrolls++
					time.Sleep(settle)
				}
			},
		},
		{
			def: ToolDef{
				Name:  "android_get_foreground_app",
				Title: "前台应用",
				Description: "获取当前屏幕**最前台的应用包名与 Activity 名**，用于在执行界面操作前确认「现在在哪个 App / 哪个页面」。\n\n" +
					"参数：无。\n\n" +
					"返回 JSON：\n" +
					"- package：前台应用包名（如 com.tencent.mm）\n" +
					"- activity：前台 Activity 全名（如 .ui.LauncherUI）\n" +
					"- focus：mCurrentFocus 原始值（如 com.tencent.mm/.ui.LauncherUI）\n" +
					"- source：数据来源（dumpsys window / dumpsys activity）\n\n" +
					"成本提示：本工具只读一次 dumpsys，比截图与控件树都轻。\n" +
					"典型用法：操作前先确认前台应用，操作后再次调用确认是否跳转成功（比抓两次控件树更省）。\n\n" +
					"依赖系统 dumpsys，个别 ROM 输出格式不同（此时 package/activity 可能为 null，focus 字段仍可用）。",
				InputSchema: schema(nil, nil),
				Annotations: annotations("前台应用", true, false),
			},
			impl: func(cfg *Config, args map[string]any) ([]map[string]any, bool) {
				return jsonOK(foregroundApp())
			},
		},
	}
}

// componentRe 匹配 dumpsys 输出里的「包名/Activity」组件（兼容各种包裹格式）
var componentRe = regexp.MustCompile(`([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)+)/([A-Za-z0-9_$.]+)`)

// foregroundApp 读取前台应用（多路探测，兼容不同 ROM 的输出格式）
//
// dumpsys 的原始行有多种包裹形式，例如：
//
//	mCurrentFocus=Window{2f1e0a3 u0 com.tencent.mm/.ui.LauncherUI}
//	mFocusedApp=ActivityRecord{1a2b3c u0 com.tencent.mm/.ui.LauncherUI t123}
//	mResumedActivity: ActivityRecord{... com.x/.MainActivity t45}
//
// 因此不按「最后一个空格分隔字段」取值，而是用正则抓取组件本身。
func foregroundApp() map[string]any {
	out := map[string]any{"package": nil, "activity": nil, "focus": nil, "source": nil}
	keys := []string{"mCurrentFocus=", "mFocusedApp=", "mResumedActivity:", "mFocusedActivity:", "topResumedActivity="}

	try := func(raw, source string) bool {
		for _, line := range strings.Split(raw, "\n") {
			hasKey := false
			for _, k := range keys {
				if strings.Contains(line, k) {
					hasKey = true
					break
				}
			}
			if !hasKey {
				continue
			}
			m := componentRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			pkg, act := m[1], m[2]
			if strings.HasPrefix(act, ".") {
				act = pkg + act
			}
			out["package"] = pkg
			out["activity"] = act
			out["focus"] = pkg + "/" + m[2]
			out["source"] = source
			return true
		}
		return false
	}

	if raw, err := runCmdOut("dumpsys", "window", "windows"); err == nil || raw != "" {
		if try(raw, "dumpsys window") {
			return out
		}
	}
	if raw, err := runCmdOut("dumpsys", "activity", "activities"); err == nil || raw != "" {
		if try(raw, "dumpsys activity") {
			return out
		}
	}
	return out
}

// ---------- 辅助 ----------

// waitForSelector 轮询等待选择器命中（仅用于 tap_element 的 wait_ms）
func waitForSelector(sel uiSelector, wantPresent bool, timeout time.Duration) ([]uiElement, time.Duration, error) {
	t0 := time.Now()
	for {
		res, err := loadUI(true)
		if err != nil {
			return nil, time.Since(t0), err
		}
		m := matchElements(res.Elems, sel)
		if (wantPresent && len(m) > 0) || (!wantPresent && len(m) == 0) {
			return m, time.Since(t0), nil
		}
		if time.Since(t0) >= timeout {
			return m, time.Since(t0), nil
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// uiFingerprint 用「元素标签 + 坐标」的哈希粗判界面是否变化（比整树对比便宜）
func uiFingerprint() string {
	res, err := loadUI(true)
	if err != nil {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, e := range res.Elems {
		if e.Label == "" && e.ID == "" {
			continue
		}
		fmt.Fprintf(&b, "%s|%s|%d,%d;", e.Label, e.ID, e.X1, e.Y1)
		n++
		if n >= 40 {
			break
		}
	}
	return b.String()
}

// uiTrue/uiFalse 返回布尔指针（用于构造「显式 true/false」的选择器条件）
func uiTrue() *bool  { v := true; return &v }
func uiFalse() *bool { v := false; return &v }
