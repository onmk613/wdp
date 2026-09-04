// Package pushbin 提供内嵌自举载荷的解取：build.sh push 先把各目标平台
// 的 slim 二进制 gzip 压缩进 assets/（manifest.json 记录 SHA256/字节数/
// 版本），控制端再以 -tags pushembed 编译把整个目录 go:embed 进自身。
// 跨平台 push 自举因此不再依赖 wdp.cfg [agent].push_binary 配置。
//
// 默认构建（无 tag）assetsFS 为 nil：Lookup 一律返回 ErrNotEmbedded、
// Platforms 为空，行为与未引入本包前完全一致。载荷二进制由 build.sh 以
// 不带 tag 的方式编译（内嵌产物自身不含内嵌，无递归套娃）。
package pushbin

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"sync"
)

// ErrNotEmbedded 表示当前构建没有该平台的内嵌载荷（默认构建对任何平台如此）。
var ErrNotEmbedded = errors.New("no embedded push binary for platform")

// assetsFS 由 embed.go（-tags pushembed 构建）注入，包级单点开关。
var assetsFS fs.FS

// platformKeyRe 限定平台键字符（载荷文件名由该键拼接，防 manifest 异常
// 引入路径穿越；正常产物键形如 linux_amd64）。
var platformKeyRe = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

// payload 是 manifest.json 中一条载荷记录（SHA256/Size 针对解压后的原始
// 二进制，解取后校验防构建管线失误）。
type payload struct {
	Platform string `json:"platform"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Version  string `json:"version"`
}

type manifest struct {
	Payloads []payload `json:"payloads"`
}

var (
	mu    sync.Mutex // 串行化解取与缓存写
	cache = map[string]string{}

	mfOnce sync.Once
	mf     []payload
	mfErr  error
)

// Platforms 返回内嵌平台键（排序；默认构建返回 nil）。
func Platforms() []string {
	pay, err := loadPayloads()
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(pay))
	for _, p := range pay {
		keys = append(keys, p.Platform)
	}
	sort.Strings(keys)
	return keys
}

// Lookup 返回平台内嵌二进制解压后的可执行文件路径：惰性解压 + SHA256
// 校验，进程内缓存（push 断线自愈重走自举时不重复解压）。临时文件由
// 进程生命周期兜底（CLI 短生命周期，OS 定期清理 /tmp）。
func Lookup(platform string) (string, error) {
	if assetsFS == nil || !platformKeyRe.MatchString(platform) {
		return "", notEmbedded(platform)
	}
	mu.Lock()
	defer mu.Unlock()
	if p, ok := cache[platform]; ok {
		return p, nil
	}
	pay, err := loadPayloads()
	if err != nil {
		return "", err
	}
	var info *payload
	for i := range pay {
		if pay[i].Platform == platform {
			info = &pay[i]
			break
		}
	}
	if info == nil {
		return "", notEmbedded(platform)
	}
	f, err := assetsFS.Open("assets/" + platform + ".bin.gz")
	if err != nil {
		return "", fmt.Errorf("open embedded payload %s: %w", platform, err)
	}
	defer f.Close()
	path, err := extractTo(*info, f)
	if err != nil {
		return "", err
	}
	cache[platform] = path
	return path, nil
}

// extractTo 解压 gzip 载荷到临时文件并校验字节数与 SHA256（0755 可执行）。
func extractTo(info payload, r io.Reader) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("decompress %s: %w", info.Platform, err)
	}
	tmp, err := os.CreateTemp("", "wdp-pushbin-"+info.Platform+"-")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), gz)
	if err != nil {
		return fail(fmt.Errorf("decompress %s: %w", info.Platform, err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if n != info.Size {
		return fail(fmt.Errorf("embedded payload %s size mismatch: manifest %d, got %d", info.Platform, info.Size, n))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != info.SHA256 {
		return fail(fmt.Errorf("embedded payload %s sha256 mismatch: manifest %s, got %s", info.Platform, info.SHA256, got))
	}
	if err := os.Chmod(name, 0o755); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// loadPayloads 解析 manifest.json（一次，结果缓存；assetsFS 为 nil 时返回
// ErrNotEmbedded）。
func loadPayloads() ([]payload, error) {
	if assetsFS == nil {
		return nil, ErrNotEmbedded
	}
	mfOnce.Do(func() {
		raw, err := fs.ReadFile(assetsFS, "assets/manifest.json")
		if err != nil {
			mfErr = fmt.Errorf("read embedded manifest: %w", err)
			return
		}
		var m manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			mfErr = fmt.Errorf("parse embedded manifest: %w", err)
			return
		}
		mf = m.Payloads
	})
	return mf, mfErr
}

func notEmbedded(platform string) error {
	if platform == "" {
		platform = "<empty>"
	}
	return fmt.Errorf("%w: %s (embedded: %v)", ErrNotEmbedded, platform, Platforms())
}
