// Package inventory 解析 YAML 主机清单：组、组间父子、组变量、主机变量。
//
//	all:
//	  vars: {ssh_user: root}
//	webservers:
//	  hosts:
//	    web1: {host: 10.0.0.11, port: 22}
//	    web2: {conn: agent, agent_url: 'http://10.0.0.12:7602'}
//	  vars: {nginx_port: 8080}
//	  children: [frontend]
package inventory

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"wdp/internal/config"
	"wdp/internal/model"
)

// Inventory 是解析后的主机清单。
type Inventory struct {
	Hosts   []*model.Host
	Groups  map[string]*model.Group
	AllVars map[string]any

	groupMap  map[string][]string       // 组名 → 成员主机名（含 children 展开；all=全部）
	hostsMeta map[string]map[string]any // 主机名 → {name,address,port,conn}
}

type rawGroup struct {
	Hosts    map[string]map[string]any `yaml:"hosts"`
	Vars     map[string]any            `yaml:"vars"`
	Children []string                  `yaml:"children"`
}

type rawInventory map[string]rawGroup

// Load 从文件解析（自动加载同目录 group_vars/ 与 host_vars/ 约定变量）。
func Load(path string) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 inventory 失败: %w", err)
	}
	return parseOne(data, []string{filepath.Dir(path)})
}

// LoadMerge 加载并合并多个 inventory 文件（-i 可重复指定）：
// 同名组合并（主机参数后者覆盖、组变量深合并、children 并集），
// 各文件同目录的 group_vars/host_vars 均按序加载（后者覆盖）。
func LoadMerge(paths []string) (*Inventory, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("未指定 inventory 文件")
	}
	if len(paths) == 1 {
		return Load(paths[0])
	}
	merged := rawInventory{}
	var dirs []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("读取 inventory %s 失败: %w", p, err)
		}
		var raw rawInventory
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("解析 inventory %s 失败: %w", p, err)
		}
		merged = mergeRaw(merged, raw)
		dirs = append(dirs, filepath.Dir(p))
	}
	return build(merged, dirs)
}

// mergeRaw 合并两份原始 inventory（b 覆盖 a）。
func mergeRaw(a, b rawInventory) rawInventory {
	out := rawInventory{}
	for k, g := range a {
		out[k] = g
	}
	for name, g := range b {
		cur, ok := out[name]
		if !ok {
			out[name] = g
			continue
		}
		if cur.Hosts == nil {
			cur.Hosts = g.Hosts
		} else if g.Hosts != nil {
			for hn, hv := range g.Hosts {
				if old := cur.Hosts[hn]; old != nil {
					mergedHost := map[string]any{}
					for k, v := range old {
						mergedHost[k] = v
					}
					for k, v := range hv {
						mergedHost[k] = v
					}
					cur.Hosts[hn] = mergedHost
				} else {
					cur.Hosts[hn] = hv
				}
			}
		}
		cur.Vars = mergeVars(cur.Vars, g.Vars)
		if len(g.Children) > 0 {
			seen := map[string]bool{}
			for _, c := range cur.Children {
				seen[c] = true
			}
			for _, c := range g.Children {
				if !seen[c] {
					cur.Children = append(cur.Children, c)
				}
			}
		}
		out[name] = cur
	}
	return out
}

// mergeVars 深合并变量 map（b 覆盖 a；嵌套 map 递归，标量与列表整体替换）。
func mergeVars(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if am, ok := out[k].(map[string]any); ok {
			if bm, ok := v.(map[string]any); ok {
				out[k] = mergeVars(am, bm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// Parse 解析 YAML 内容（不加载 group_vars/host_vars 目录；测试与内联构造用）。
func Parse(data []byte) (*Inventory, error) {
	return parseOne(data, nil)
}

func parseOne(data []byte, varDirs []string) (*Inventory, error) {
	var raw rawInventory
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 inventory 失败: %w", err)
	}
	return build(raw, varDirs)
}

// build 由原始结构构建 Inventory：排序保证确定性，挂接 group_vars/host_vars 后计算变量域。
func build(raw rawInventory, varDirs []string) (*Inventory, error) {

	inv := &Inventory{
		Groups:  map[string]*model.Group{},
		AllVars: map[string]any{},
	}

	// all 组
	if all, ok := raw["all"]; ok {
		for k, v := range all.Vars {
			inv.AllVars[k] = v
		}
	}

	// 按组名排序遍历（all 最先），保证同一主机在多个组定义时
	// 参数合并结果确定：后处理的组（字典序）覆盖先处理的组
	groupNames := make([]string, 0, len(raw))
	for gname := range raw {
		groupNames = append(groupNames, gname)
	}
	sort.Strings(groupNames)

	hostRaw := map[string]map[string]any{} // 主机名 → 合并后的原始参数
	hostIndex := map[string]*model.Host{}
	for _, gname := range groupNames {
		g := raw[gname]
		if gname == "all" {
			for hname, hvars := range g.Hosts {
				mergeHostRaw(hostRaw, hname, hvars)
			}
			continue
		}
		grp := &model.Group{Name: gname, Vars: g.Vars, Children: g.Children}
		hnames := make([]string, 0, len(g.Hosts))
		for hname := range g.Hosts {
			hnames = append(hnames, hname)
		}
		sort.Strings(hnames)
		for _, hname := range hnames {
			mergeHostRaw(hostRaw, hname, g.Hosts[hname])
			grp.HostNames = append(grp.HostNames, hname)
		}
		inv.Groups[gname] = grp
	}
	// 构建主机对象并回填组成员
	inv.Hosts = make([]*model.Host, 0, len(hostRaw))
	for hname, hvars := range hostRaw {
		h, err := buildHost(hname, hvars)
		if err != nil {
			return nil, fmt.Errorf("主机 %s: %w", hname, err)
		}
		hostIndex[hname] = h
		inv.Hosts = append(inv.Hosts, h)
	}
	for _, grp := range inv.Groups {
		for _, hname := range grp.HostNames {
			grp.Hosts = append(grp.Hosts, hostIndex[hname])
		}
	}

	// 校验 children 引用
	for _, grp := range inv.Groups {
		for _, c := range grp.Children {
			if _, ok := inv.Groups[c]; !ok {
				return nil, fmt.Errorf("组 %s 引用了不存在的子组 %s", grp.Name, c)
			}
		}
	}

	// 约定变量目录（按传入顺序，后者覆盖）：group_vars/<组>.yaml、host_vars/<主机>.yaml
	for _, dir := range varDirs {
		if err := inv.loadVarDirs(dir); err != nil {
			return nil, err
		}
	}

	sort.Slice(inv.Hosts, func(i, j int) bool { return inv.Hosts[i].Name < inv.Hosts[j].Name })
	inv.applyVars(hostIndex)
	inv.precomputeTopology()
	return inv, nil
}

// loadVarDirs 加载目录约定变量。
func (inv *Inventory) loadVarDirs(dir string) error {
	if err := inv.loadGroupVars(filepath.Join(dir, "group_vars")); err != nil {
		return err
	}
	return inv.loadHostVars(filepath.Join(dir, "host_vars"))
}

// loadGroupVars 加载 group_vars/<组名>.yaml（all.yaml 合入全局变量；
// 文件对应组不存在时忽略——组在其它 inventory 中定义的场景已由合并阶段处理）。
func (inv *Inventory) loadGroupVars(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !isYAMLName(e.Name()) {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		vars, err := readVarsYAML(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("group_vars/%s: %w", e.Name(), err)
		}
		if name == "all" {
			inv.AllVars = mergeVars(inv.AllVars, vars)
			continue
		}
		if g, ok := inv.Groups[name]; ok {
			g.Vars = mergeVars(g.Vars, vars)
		}
	}
	return nil
}

// loadHostVars 加载 host_vars/<主机名>.yaml（合并进主机原始变量）。
func (inv *Inventory) loadHostVars(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", dir, err)
	}
	byName := map[string]*model.Host{}
	for _, h := range inv.Hosts {
		byName[h.Name] = h
	}
	for _, e := range entries {
		if e.IsDir() || !isYAMLName(e.Name()) {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		h, ok := byName[name]
		if !ok {
			continue
		}
		vars, err := readVarsYAML(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("host_vars/%s: %w", e.Name(), err)
		}
		h.Vars = mergeVars(h.Vars, vars)
	}
	return nil
}

func isYAMLName(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

func readVarsYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var vars map[string]any
	if err := yaml.Unmarshal(data, &vars); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	return vars, nil
}

// mergeHostRaw 将主机条目参数合并进累积表（同主机多处定义：后处理的组覆盖）。
func mergeHostRaw(hostRaw map[string]map[string]any, name string, vars map[string]any) {
	if _, ok := hostRaw[name]; !ok {
		hostRaw[name] = map[string]any{}
	}
	for k, v := range vars {
		hostRaw[name][k] = v
	}
}

// precomputeTopology 预计算组拓扑与主机元信息（内置变量 groups/hosts 的数据源）。
func (inv *Inventory) precomputeTopology() {
	inv.groupMap = map[string][]string{"all": hostNames(inv.Hosts)}
	for name, g := range inv.Groups {
		inv.groupMap[name] = expandGroup(inv, g, map[string]bool{})
	}
	inv.hostsMeta = make(map[string]map[string]any, len(inv.Hosts))
	for _, h := range inv.Hosts {
		inv.hostsMeta[h.Name] = map[string]any{
			"name": h.Name, "address": h.Address, "port": h.Port, "conn": h.Conn,
		}
	}
}

// GroupsMap 返回组名→成员主机名（含 children 展开；all = 全部主机）。
func (inv *Inventory) GroupsMap() map[string][]string { return inv.groupMap }

// HostsMeta 返回主机名→{name,address,port,conn}。
func (inv *Inventory) HostsMeta() map[string]map[string]any { return inv.hostsMeta }

// HostByName 按名查找主机（delegate_to 等场景；未找到返回 nil）。
func (inv *Inventory) HostByName(name string) *model.Host {
	for _, h := range inv.Hosts {
		if h.Name == name {
			return h
		}
	}
	return nil
}

// AddDynamicGroup 运行时（group_by 模块）添加动态组：主机加入组并重建拓扑缓存。
// 后续 play 的 hosts 选择与 .groups 内置变量即可引用该组。
func (inv *Inventory) AddDynamicGroup(name string, members []string) {
	grp, ok := inv.Groups[name]
	if !ok {
		grp = &model.Group{Name: name}
		inv.Groups[name] = grp
	}
	seen := map[string]bool{}
	for _, h := range grp.Hosts {
		seen[h.Name] = true
	}
	for _, m := range members {
		if seen[m] {
			continue
		}
		if h := inv.HostByName(m); h != nil {
			grp.Hosts = append(grp.Hosts, h)
			grp.HostNames = append(grp.HostNames, m)
			seen[m] = true
		}
	}
	inv.precomputeTopology()
}

func hostNames(hosts []*model.Host) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return out
}

// hostKeys 是主机条目中的连接参数键，其余进入 Vars。
var hostKeys = map[string]bool{
	"host": true, "port": true, "user": true, "password": true, "password_env": true,
	"key_path": true, "key_passphrase": true, "key_passphrase_env": true,
	"conn": true, "agent_url": true, "agent_port": true,
	"host_key_check": true, "known_hosts": true, "connect_timeout": true,
	"token": true, "token_env": true, "ca_file": true, "cert_file": true, "key_file": true,
	"binary_path": true, "keep_agent": true,
	"become_password": true, "become_password_env": true,
	"tls": true, "insecure_skip_verify": true,
}

func buildHost(name string, vars map[string]any) (*model.Host, error) {
	// 连接默认值取 wdp.cfg 的 [ssh]（主机条目未显式指定的键生效）
	cfg := config.Current()
	h := &model.Host{
		Name:              name,
		Vars:              map[string]any{},
		Conn:              "ssh",
		Port:              22,
		User:              cfg.SSHUser(),
		HostKeyCheck:      cfg.SSHHostKeyCheck(),
		KnownHosts:        cfg.SSH.KnownHosts,
		ConnectTimeoutSec: cfg.SSHConnectTimeout(),
	}
	for k, v := range vars {
		if !hostKeys[k] {
			h.Vars[k] = v
			continue
		}
		switch k {
		case "host":
			h.Address = fmt.Sprint(v)
		case "port":
			h.Port = toInt(v, 22)
		case "user":
			h.User = fmt.Sprint(v)
		case "password":
			h.Password = fmt.Sprint(v)
		case "password_env":
			h.PasswordEnv = fmt.Sprint(v)
		case "key_path":
			h.KeyPath = fmt.Sprint(v)
		case "key_passphrase":
			h.KeyPassphrase = fmt.Sprint(v)
		case "key_passphrase_env":
			h.KeyPassphraseEnv = fmt.Sprint(v)
		case "conn":
			h.Conn = fmt.Sprint(v)
		case "agent_url":
			h.AgentURL = fmt.Sprint(v)
		case "agent_port":
			h.AgentPort = toInt(v, 0)
		case "host_key_check":
			// 严格解析：非布尔值直接报错（静默当 false 会关闭指纹校验）
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("host_key_check: %w", err)
			}
			h.HostKeyCheck = b
		case "known_hosts":
			h.KnownHosts = fmt.Sprint(v)
		case "connect_timeout":
			h.ConnectTimeoutSec = toInt(v, cfg.SSHConnectTimeout())
		case "token":
			h.Token = fmt.Sprint(v)
		case "token_env":
			h.TokenEnv = fmt.Sprint(v)
		case "ca_file":
			h.CAFile = fmt.Sprint(v)
		case "cert_file":
			h.CertFile = fmt.Sprint(v)
		case "key_file":
			h.KeyFile = fmt.Sprint(v)
		case "binary_path":
			h.BinaryPath = fmt.Sprint(v)
		case "keep_agent":
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("keep_agent: %w", err)
			}
			h.KeepAgent = b
		case "become_password":
			h.BecomePassword = fmt.Sprint(v)
		case "become_password_env":
			h.BecomePasswordEnv = fmt.Sprint(v)
		case "tls":
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("tls: %w", err)
			}
			h.TLS = b
		case "insecure_skip_verify":
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("insecure_skip_verify: %w", err)
			}
			h.InsecureSkipVerify = b
		}
	}
	if h.Address == "" {
		h.Address = name
	}
	return h, nil
}

func toInt(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case string:
		var n int
		if _, err := fmt.Sscanf(x, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// applyVars 为每台主机计算最终变量：all < 父组 < 子组 < 主机。
func (inv *Inventory) applyVars(hostIndex map[string]*model.Host) {
	// 展开组归属（含 children 递归）
	membership := map[string][]string{} // host → 有序组名列表
	var walk func(group string, chain []string)
	visited := map[string]bool{}
	walk = func(group string, chain []string) {
		if visited[group] {
			return
		}
		visited[group] = true
		grp, ok := inv.Groups[group]
		if !ok {
			return
		}
		cur := append(append([]string{}, chain...), group)
		for _, h := range grp.Hosts {
			membership[h.Name] = append(membership[h.Name], cur...)
		}
		for _, c := range grp.Children {
			walk(c, cur)
		}
	}
	names := make([]string, 0, len(inv.Groups))
	for n := range inv.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		visited = map[string]bool{}
		walk(n, nil)
	}

	for _, h := range inv.Hosts {
		merged := map[string]any{}
		for k, v := range inv.AllVars {
			merged[k] = v
		}
		for _, g := range membership[h.Name] {
			for k, v := range inv.Groups[g].Vars {
				merged[k] = v
			}
		}
		for k, v := range h.Vars {
			merged[k] = v
		}
		merged["inventory_hostname"] = h.Name
		groups := append([]string{}, membership[h.Name]...)
		sort.Strings(groups)
		merged["group_names"] = groups
		h.Vars = merged
	}
}

// Select 按模式选择主机。表达式语法：
//
//	逗号联合：webservers,dbservers；! 前缀排除：all,!web1
//	:& 交集：webservers:&production（可链式 a:&b:&c）
//	通配：web*（同时匹配主机名与组名，组名展开为成员；? 单字符）
func (inv *Inventory) Select(pattern string) ([]*model.Host, error) {
	include := map[string]bool{}
	exclude := map[string]bool{}
	for _, token := range strings.Split(pattern, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		neg := strings.HasPrefix(token, "!")
		if neg {
			token = strings.TrimSpace(token[1:])
		}
		// :& 交集链
		set := map[string]bool{}
		for i, seg := range strings.Split(token, ":&") {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				return nil, fmt.Errorf("主机模式 %q 存在空片段", token)
			}
			matched, err := inv.matchSegment(seg)
			if err != nil {
				return nil, err
			}
			if i == 0 {
				set = matched
				continue
			}
			for n := range set {
				if !matched[n] {
					delete(set, n)
				}
			}
		}
		target := include
		if neg {
			target = exclude
		}
		for n := range set {
			target[n] = true
		}
	}

	var out []*model.Host
	for _, h := range inv.Hosts {
		if include[h.Name] && !exclude[h.Name] {
			out = append(out, h)
		}
	}
	return out, nil
}

// matchSegment 解析单个选择片段：all / * / 组名 / 主机名 / 通配模式。
func (inv *Inventory) matchSegment(seg string) (map[string]bool, error) {
	out := map[string]bool{}
	switch {
	case seg == "all" || seg == "*":
		for _, h := range inv.Hosts {
			out[h.Name] = true
		}
		return out, nil
	case strings.ContainsAny(seg, "*?["):
		matched := false
		for name, g := range inv.Groups {
			if ok, _ := path.Match(seg, name); ok {
				matched = true
				for _, h := range expandGroup(inv, g, map[string]bool{}) {
					out[h] = true
				}
			}
		}
		for _, h := range inv.Hosts {
			if ok, _ := path.Match(seg, h.Name); ok {
				matched = true
				out[h.Name] = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("未找到匹配 %q 的主机或组", seg)
		}
		return out, nil
	default:
		if g, ok := inv.Groups[seg]; ok {
			for _, h := range expandGroup(inv, g, map[string]bool{}) {
				out[h] = true
			}
			return out, nil
		}
		if inv.hostExists(seg) {
			out[seg] = true
			return out, nil
		}
		return nil, fmt.Errorf("未找到主机或组: %s", seg)
	}
}

func (inv *Inventory) hostExists(name string) bool {
	for _, h := range inv.Hosts {
		if h.Name == name {
			return true
		}
	}
	return false
}

func expandGroup(inv *Inventory, g *model.Group, seen map[string]bool) []string {
	if seen[g.Name] {
		return nil
	}
	seen[g.Name] = true
	out := make([]string, 0, len(g.Hosts))
	for _, h := range g.Hosts {
		out = append(out, h.Name)
	}
	for _, c := range g.Children {
		if cg, ok := inv.Groups[c]; ok {
			out = append(out, expandGroup(inv, cg, seen)...)
		}
	}
	return out
}
