package inventory

// 内联主机表达式解析（`wdp run --hosts`）：IP/host[:port]、IPv4 尾段区间
// 展开为逐台主机，解析结果交给 FromHosts 组装最小 inventory。
// ~ 与 run.go 的 --hosts flag 文档保持一致。

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/sshcfg"
)

// HostsFromSpecs 解析主机条目列表（host、host:port 或 [ipv6]:port）为主机
// 列表。IPv4 尾段区间（10.8.2.101-104 / 10.8.2.101-10.8.2.104）展开为逐台
// 主机。dc 为组合根注入的连接默认值（可为 nil）。
func HostsFromSpecs(specs []string, dc *conn.Defaults) ([]*model.Host, error) {
	expanded := make([]string, 0, len(specs))
	for _, spec := range specs {
		list, err := ExpandSpecRange(spec)
		if err != nil {
			return nil, err
		}
		expanded = append(expanded, list...)
	}
	out := make([]*model.Host, 0, len(expanded))
	for _, spec := range expanded {
		h, err := parseHostSpec(spec, dc)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// maxRangeSpan 单个区间的最大展开跨度（/24 以内的手滑防护）。
const maxRangeSpan = 256

// ExpandSpecRange 展开 IPv4 尾段区间表达式：
//
//	10.8.2.101-104            → 10.8.2.101 … 10.8.2.104（尾段简写）
//	10.8.2.101-10.8.2.104     → 同上（完整写法，前三段必须一致）
//	host:port 形式保留端口     → 10.8.2.101:2222-104
//
// 非区间（无 "-" 或前缀不是合法 IPv4）原样返回单个元素——
// web-1 这类含连字符的主机名不会被误判（前缀 "web" 不是 IP）。
func ExpandSpecRange(spec string) ([]string, error) {
	host, port := spec, ""
	if h, p, err := net.SplitHostPort(spec); err == nil {
		host, port = h, ":"+p
	}
	dash := strings.LastIndexByte(host, '-')
	if dash <= 0 || dash == len(host)-1 {
		return []string{spec}, nil
	}
	left, right := host[:dash], host[dash+1:]
	if net.ParseIP(left) == nil || strings.Count(left, ".") != 3 {
		return []string{spec}, nil // 含连字符的普通主机名
	}
	lastDot := strings.LastIndexByte(left, '.')
	prefix, loStr := left[:lastDot+1], left[lastDot+1:]
	lo, err := strconv.Atoi(loStr)
	if err != nil {
		return nil, fmt.Errorf("host range %q: left endpoint tail %q is not numeric", spec, loStr)
	}
	hi := 0
	if strings.Contains(right, ".") { // 完整写法：前三段必须与左端一致
		if net.ParseIP(right) == nil || right[:strings.LastIndexByte(right, '.')+1] != prefix {
			return nil, fmt.Errorf("host range %q: right endpoint must share the first three octets with the left", spec)
		}
		hi, err = strconv.Atoi(right[strings.LastIndexByte(right, '.')+1:])
	} else {
		hi, err = strconv.Atoi(right)
	}
	if err != nil {
		return nil, fmt.Errorf("host range %q: right endpoint %q is not numeric", spec, right)
	}
	if hi < lo {
		return nil, fmt.Errorf("host range %q: right endpoint %d < left %d", spec, hi, lo)
	}
	if hi-lo+1 > maxRangeSpan {
		return nil, fmt.Errorf("host range %q expands to %d hosts (max %d)", spec, hi-lo+1, maxRangeSpan)
	}
	out := make([]string, 0, hi-lo+1)
	for n := lo; n <= hi; n++ {
		out = append(out, fmt.Sprintf("%s%d%s", prefix, n, port))
	}
	return out, nil
}

// parseHostSpec 解析主机条目：host、host:port 或 [ipv6]:port，缺省端口 22。
// 连接参数（user/超时）取组合根注入的默认值（dc 可为 nil，取内置默认）；
// ~/.ssh/config 可补全未显式给出的 user/port（条目带端口视为显式）。
func parseHostSpec(spec string, dc *conn.Defaults) (*model.Host, error) {
	host, port := spec, 22
	explicitPort := false
	if h, p, err := net.SplitHostPort(spec); err == nil {
		n, aerr := strconv.Atoi(p)
		if aerr != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("host %q has an invalid port: %s", spec, p)
		}
		host, port = h, n
		explicitPort = true
	} else if strings.Count(spec, ":") > 0 && net.ParseIP(spec) == nil {
		return nil, fmt.Errorf("host %q has an invalid format (expected host, host:port, or [ipv6]:port)", spec)
	}
	user, connectTimeout := "", 0
	if dc != nil { // 组合根注入的已归一化值；nil（测试/独立构造）保持零值
		user, connectTimeout = dc.SSHUser, dc.SSHConnectTimeout
	}
	h := &model.Host{
		Name:              spec,
		Address:           host,
		Port:              port,
		Conn:              "ssh",
		User:              user,
		ConnectTimeoutSec: connectTimeout,
	}
	sshcfg.FillFromSSHConfig(h, false, explicitPort, false)
	return h, nil
}
