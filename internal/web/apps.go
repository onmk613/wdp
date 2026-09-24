package web

// 应用管理：chart tgz 版本化入库（<data>/apps/<name>/<version>.tgz），
// 作用域（池/组/标签）与执行。执行在 server 进程内复用 CLI 同一条
// 流水线：chart.LoadWithLimits(tgz) → PhasePlays → inventory.FromHosts →
// executor；Reporter 落 runs/run_tasks 供前端轮询。
// 本文件：应用模块共享的常量与通用辅助（上传上限、制品路径、tgz 落盘/打包、作用域校验、chart 元信息读取）。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"wdp/internal/chart"
)

var appNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
var versionRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// maxUploadBytes 上传请求体总量上限：与 chart 解包上限（2GiB，见
// chart.Limits 默认值）对齐——上传的包终归要过解包限制，请求体放得更大
// 只会招灌盘（ParseMultipartForm 的参数只是内存阈值，不是总量上限）。
// 变量而非常量：测试收窄用。
var maxUploadBytes int64 = 2 << 30

// 上传互斥是 Server.uploadMu（server.go）：把"版本预检 → 制品归位 → 版本
// 入库"整体关进临界区。约束：不持锁时并发上传同版本可同时通过预检，后到
// 者 rename 覆盖前者制品、入库失败后又把已入库版本正引用的制品删掉；
// 上传低频，一把锁足够。

// runQueueTimeout run 排队等主机闸门的上限：闸门被长执行占用时排队不能
// 无限等（goroutine 只增不减，server 关停也无法中断）。
const runQueueTimeout = 30 * time.Minute

// appsDir 应用制品根目录。
func (s *Server) appsDir() string {
	if s.opts.DataDir == "" {
		return "wdp-data/apps"
	}
	return filepath.Join(s.opts.DataDir, "apps")
}

// appTgzPath 版本制品的最终落盘路径。
func (s *Server) appTgzPath(name, version string) string {
	return filepath.Join(s.appsDir(), name, version+".tgz")
}

// writeMultipartErr multipart 解析/读取错误分流：请求体超限 413，其余 400。
func writeMultipartErr(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds %d bytes limit", mbe.Limit))
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// labelKeySlice 标签 JSON → 排序键切片（scopeWriteAllowed 的入参形态；
// perm.go 的 labelKeys 返回 map，这里转切片）。
func labelKeySlice(labels string) []string {
	m := labelKeys(labels)
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// scopeWriteCheck scope 写入校验，返回越权目标类别（"" = 通过）。整体
// 不通过时逐类探测，供 403 信息指明是哪类目标越权（pool/group/label）。
func (s *Server) scopeWriteCheck(r *http.Request, verb string, pools, groups []string, labelKs []string) string {
	if s.scopeWriteAllowed(r, verb, pools, groups, labelKs) {
		return ""
	}
	switch {
	case !s.scopeWriteAllowed(r, verb, pools, nil, nil):
		return "pool"
	case !s.scopeWriteAllowed(r, verb, nil, groups, nil):
		return "group"
	default:
		return "label"
	}
}

// artifactReferenced (name, version) 制品路径当前是否被库中版本引用
// （建应用失败路径的删除防误判：重名并发先到者可能已把同一路径入库）。
func (s *Server) artifactReferenced(name, version string) bool {
	a, err := s.st.AppByName(name)
	if err != nil {
		return false
	}
	ref, herr := s.st.HasVersion(a.ID, version)
	return herr == nil && ref
}

// firstHostValues 按主机名排序取首个 hostValues。约束：map 迭代随机，
// 多主机 marker values 不一致时选哪台必须确定，否则 run 级 values 每次
// 执行都不一样；空 map 返回 nil（调用方保留原 values）。
func firstHostValues(hostValues map[string]map[string]any) map[string]any {
	if len(hostValues) == 0 {
		return nil
	}
	names := make([]string, 0, len(hostValues))
	for name := range hostValues {
		names = append(names, name)
	}
	slices.Sort(names)
	return hostValues[names[0]]
}

// saveTgzTemp 把上传的 tgz 落临时文件（此时还不知道 chart.yaml 的名称/
// 版本，读取元信息后再归位），返回 (临时路径, sha256, size)。
func (s *Server) saveTgzTemp(r *http.Request, fileField string) (string, string, int64, error) {
	f, _, err := r.FormFile(fileField)
	if err != nil {
		return "", "", 0, fmt.Errorf("missing %s file: %w", fileField, err)
	}
	defer f.Close()
	tmp, err := os.CreateTemp("", "wdp-upload-*.tgz")
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), f)
	cerr := tmp.Close()
	if err != nil || cerr != nil {
		os.Remove(tmp.Name())
		return "", "", 0, fmt.Errorf("write tgz: %v/%v", err, cerr)
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), n, nil
}

// moveTgz 把临时 tgz 归位到 <apps>/<name>/<version>.tgz（同路径覆盖）。
func (s *Server) moveTgz(tmpPath, name, version string) (string, error) {
	dir := filepath.Join(s.appsDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, version+".tgz")
	if err := os.Rename(tmpPath, dst); err != nil {
		// 跨设备回退（/tmp 为 tmpfs 的部署形态）：先复制到目标侧 .part
		// 再原子改名——直接 O_TRUNC 写最终路径，复制中途失败/崩溃会留下
		// 半截制品永久占坑该版本号
		part := dst + ".part"
		if err := copyFile(tmpPath, part); err != nil {
			os.Remove(part)
			return "", err
		}
		if err := os.Rename(part, dst); err != nil {
			os.Remove(part)
			return "", err
		}
	}
	return dst, nil
}

// chartMetaOf 读取 tgz 中 chart.yaml 的名称/版本/描述与全部可用相位（走
// 完整 chart 加载，破包/缺 deploy.yaml 等结构问题在上传即拒绝——同一
// 加载器执行期也会用）。版本号以 chart.yaml 的 version 为唯一来源（真实
// 版本控制）。
func chartMetaOf(tgzPath string) (name, version, description string, phases []string, err error) {
	ch, err := chart.LoadWithLimits(tgzPath, chart.Limits{})
	if err != nil {
		return "", "", "", nil, fmt.Errorf("chart load failed: %w", err)
	}
	defer ch.Close()
	name, version = ch.Meta.Name, ch.Meta.Version
	if !appNameRe.MatchString(name) {
		return "", "", "", nil, fmt.Errorf("chart.yaml name %q is invalid (letters, digits, . _ -, must start alphanumeric)", name)
	}
	if !versionRe.MatchString(version) {
		return "", "", "", nil, fmt.Errorf("chart.yaml version %q is invalid (letters, digits, . _ -, must start alphanumeric)", version)
	}
	return name, version, ch.Meta.Description, ch.PhaseNames(), nil
}

// chartPhasesOf 提取已落盘制品的可用相位（编辑器保存路径：spec 写入的
// 相位文件打包后从成品读回，保证库里的相位与制品一致）。
func chartPhasesOf(tgzPath string) ([]string, error) {
	ch, err := chart.LoadWithLimits(tgzPath, chart.Limits{})
	if err != nil {
		return nil, fmt.Errorf("chart load failed: %w", err)
	}
	defer ch.Close()
	return ch.PhaseNames(), nil
}

// ---- 工具 ----

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
