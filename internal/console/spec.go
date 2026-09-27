package console

// 应用 spec 的物化与入库：编辑器/上传共用的 chart 内容读写、部分保存
// 语义、verbatim 三件套、打包归位（HTTP 语义留在传输层）。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"wdp/internal/chart"
)

// 与 web 传输层同源的输入校验（曾各持一份；现在 console 是唯一事实来源，
// web 的别名指向这里）。
var (
	AppNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	VersionRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

// ---- 图形化编辑器：chart 内容读写（spec）----

type SpecFile struct {
	Path    string  `json:"path"`
	Content *string `json:"content,omitempty"`
	Binary  bool    `json:"binary,omitempty"`
	Size    int64   `json:"size"`
}

// AppSpec 是编辑器的 chart 内容快照。
type AppSpec struct {
	Name        string     `json:"name"`
	Version     string     `json:"version"`
	Description string     `json:"description"`
	ValuesYAML  string     `json:"values_yaml"`
	DeployYAML  string     `json:"deploy_yaml"`
	Files       []SpecFile `json:"files"`
	Pools       []string   `json:"pools"`
	Groups      []string   `json:"groups"`
	Labels      string     `json:"labels"`
}

// ChartPhaseSpecReq 相位属性声明（chart.yaml phases.<名>）。
type ChartPhaseSpecReq struct {
	Release      bool   `json:"release" yaml:"release"`             // 部署语义（required 校验/可逆性/marker）
	Record       bool   `json:"record" yaml:"record"`               // 记部署记录
	ClearsMarker bool   `json:"clears_marker" yaml:"clears_marker"` // 成功后清 marker
	ValuesFrom   string `json:"values_from" yaml:"values_from"`     // 空 | chart | marker
}

// ChartMetaReq 是 chart.yaml 的可编辑字段集（与 chart.Meta 一一对应，
// check_mode 三态：nil=未声明；yaml 侧 omitempty：空值不落盘，语义等于缺省）。
type ChartMetaReq struct {
	Required          []string                     `json:"required" yaml:"required,omitempty"`                     // 必须提供的 values 点路径
	MarkerDir         string                       `json:"marker_dir" yaml:"marker_dir,omitempty"`                 // release marker 目录（空 = /var/lib/wdp）
	NoMarker          bool                         `json:"no_marker" yaml:"no_marker,omitempty"`                   // 不写 marker
	CheckMode         *bool                        `json:"check_mode" yaml:"check_mode,omitempty"`                 // 脚本模块 check 预演
	InventoryOverride []string                     `json:"inventory_override" yaml:"inventory_override,omitempty"` // 允许 inventory 覆盖的 values 键
	SensitiveValues   []string                     `json:"sensitive_values" yaml:"sensitive_values,omitempty"`     // 落盘脱敏的 values 点路径
	Phases            map[string]ChartPhaseSpecReq `json:"phases" yaml:"phases,omitempty"`                         // 生命周期相位声明
}

// chartMetaYAML 是 chart.yaml 的读侧投影（check_mode 保三态：Node
// 零值 = 未声明）。
type chartMetaYAML struct {
	Required          []string                     `yaml:"required"`
	MarkerDir         string                       `yaml:"marker_dir"`
	NoMarker          bool                         `yaml:"no_marker"`
	CheckMode         yaml.Node                    `yaml:"check_mode"`
	InventoryOverride []string                     `yaml:"inventory_override"`
	SensitiveValues   []string                     `yaml:"sensitive_values"`
	Phases            map[string]ChartPhaseSpecReq `yaml:"phases"`
}

// toChartMeta 转换为 API 形态（check_mode 宽容解析，与 chart 包口径一致）。
func (y *chartMetaYAML) toChartMeta() *ChartMetaReq {
	m := &ChartMetaReq{
		Required: y.Required, MarkerDir: y.MarkerDir, NoMarker: y.NoMarker,
		InventoryOverride: y.InventoryOverride, SensitiveValues: y.SensitiveValues, Phases: y.Phases,
	}
	if y.CheckMode.Kind != 0 && y.CheckMode.Tag != "!!null" {
		v := parseCheckModeNode(y.CheckMode.Value)
		m.CheckMode = &v
	}
	return m
}

// parseCheckModeNode 解析 check_mode 取值（supported/true/false 及宽容布尔）。
func parseCheckModeNode(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "supported", "true", "yes", "on", "1":
		return true
	default:
		return false
	}
}

const specTextLimit = 512 << 10 // 单文件可编辑上限（超出按二进制列出）

// ReadSpecFromDir 从 chart 目录读取编辑器快照（values.yaml/deploy.yaml
// 是 spec 的独立字段；chart.yaml 原文进 files 且投影出 ChartMeta；其余
// 文本文件可编辑，二进制只列元信息）。
func ReadSpecFromDir(dir, name, version, description string) (*AppSpec, error) {
	spec := &AppSpec{Name: name, Version: version, Description: description}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// 只处理普通文件：符号链接在 WalkDir 里 d.IsDir() 为 false，
		// 但 os.ReadFile 会跟随它。解包侧（chart/tgz.go）已拒绝指向归档
		// 外的链接，这里再挡一层——非普通文件一律不读内容。
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		fi, ferr := d.Info()
		if ferr != nil {
			return ferr
		}
		// 三件套读失败必须上抛（错误带文件名，由调用方按基础设施错误
		// 返回）：静默跳过会让编辑器拿到空底本快照，保存时 values 兜底
		// "{}\n"、chart.yaml 走生成路径再读失败即丢 marker_dir/phases 元
		// 数据——都是用户以为读到了、实际丢内容的数据问题
		switch rel {
		case "chart.yaml":
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			// 原文进 files（IDE 纯文本编辑、注释保真）
			c := string(b)
			spec.Files = append(spec.Files, SpecFile{Path: rel, Content: &c, Size: fi.Size()})
			return nil
		case "values.yaml":
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			spec.ValuesYAML = string(b)
			return nil
		case "deploy.yaml":
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			spec.DeployYAML = string(b)
			return nil
		}
		sf := SpecFile{Path: rel, Size: fi.Size()}
		if fi.Size() <= specTextLimit {
			b, rerr := os.ReadFile(path)
			switch {
			case rerr != nil:
				// 读失败上抛（区别于二进制：混为一谈会把权限/竞态问题
				// 当二进制只列元信息，保存即丢内容）
				return rerr
			case utf8.Valid(b):
				c := string(b)
				sf.Content = &c
			default:
				sf.Binary = true
			}
		} else {
			sf.Binary = true
		}
		spec.Files = append(spec.Files, sf)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(spec.Files, func(a, b SpecFile) int { return strings.Compare(a.Path, b.Path) })
	return spec, nil
}

type SpecReq struct {
	Version     string     `json:"version"`
	Description string     `json:"description"`
	Files       []SpecFile `json:"files"` // 新增/修改；Binary 条目忽略
	DeleteFiles []string   `json:"delete_files"`
	Pools       []string   `json:"pools"`
	Groups      []string   `json:"groups"`
	Labels      string     `json:"labels"`
	BaseVersion string     `json:"base_version"` // 修改时的底本版本（空 = 最新）
}

// specFileContent 返回 files 里该路径的显式提交内容（nil = 未提交；
// IDE 以 verbatim 形式提交三件套，空串同样是有效提交）。
func SpecFileContent(req *SpecReq, rel string) (string, bool) {
	for _, f := range req.Files {
		if !f.Binary && f.Path == rel && f.Content != nil {
			return *f.Content, true
		}
	}
	return "", false
}

// specFileContent0 三件套取值：files 显式提交（含空串）优先；未提交用
// 底本原文（部分保存语义）。
func specFileContent0(req *SpecReq, rel string, baseFile func(string) string) string {
	if f, ok := SpecFileContent(req, rel); ok {
		return f
	}
	return baseFile(rel)
}

// ApplySpec 把编辑内容落到 workDir（在底本目录副本上就地修改）。
func ApplySpec(workDir, name string, req *SpecReq) error {
	if !AppNameRe.MatchString(name) {
		return fmt.Errorf("invalid app name %q", name)
	}
	if !VersionRe.MatchString(req.Version) {
		return fmt.Errorf("invalid version %q (letters, digits, . _ -, must start alphanumeric)", req.Version)
	}
	baseFile := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(workDir, rel))
		if err != nil {
			return ""
		}
		return string(b)
	}
	// files 里的三件套按原文落盘（IDE 纯文本编辑：注释/格式保真）。
	// 显式提交的内容（含空串）按提交值落盘；未提交才走「保留底本」的
	// 部分保存语义。此前空提交被当未提交：清空 deploy.yaml/values.yaml
	// 静默回滚成底本，新建空文件被静默丢弃——都是用户以为已保存、
	// 实际内容错误的数据问题
	deploy := specFileContent0(req, "deploy.yaml", baseFile)
	if strings.TrimSpace(deploy) == "" {
		return fmt.Errorf("deploy.yaml must not be empty")
	}
	values := specFileContent0(req, "values.yaml", baseFile)
	if strings.TrimSpace(values) == "" {
		values = "{}\n"
	}
	// values.yaml 需可解析（提前拦截语法错误）
	var probe map[string]any
	if err := yaml.Unmarshal([]byte(values), &probe); err != nil {
		return fmt.Errorf("values.yaml parse failed: %w", err)
	}
	chartYAML := ""
	if f, ok := SpecFileContent(req, "chart.yaml"); ok {
		// verbatim chart.yaml：name/version 与请求字段对账——库版本号
		// （app_versions.version）与包内 chart.yaml 说法不一致会让后续
		// 升级/回退链路拿错制品，宁可当场拒绝
		var meta struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		}
		if err := yaml.Unmarshal([]byte(f), &meta); err != nil {
			return fmt.Errorf("chart.yaml parse failed: %w", err)
		}
		if meta.Name != name {
			return fmt.Errorf("chart.yaml name %q does not match app name %q (keep them identical)", meta.Name, name)
		}
		if meta.Version != req.Version {
			return fmt.Errorf("chart.yaml version %q does not match requested version %q (keep them identical)", meta.Version, req.Version)
		}
		chartYAML = f
	} else if b := baseFile("chart.yaml"); strings.TrimSpace(b) != "" && ChartYAMLVersion(b) == req.Version {
		// 部分保存：未提交 chart.yaml 且底本版本号与请求一致时原样保留
		//（注释保真）。版本号不一致（升版本保存）必须走生成路径把新版本
		// 号写进去——包内 version 与库版本对账是硬约束
		chartYAML = b
	} else {
		var err error
		chartYAML, err = BuildChartYAML(workDir, name, req)
		if err != nil {
			return err
		}
	}
	// 三件套 0600：对齐 plan.Write/marker 的敏感口径——values.yaml 可含
	// 密码等敏感入参，不因"中间工作目录/打包制品"就放宽权限
	for _, f := range []struct{ rel, body string }{
		{"chart.yaml", chartYAML},
		{"values.yaml", values},
		{"deploy.yaml", deploy},
	} {
		if err := os.WriteFile(filepath.Join(workDir, f.rel), []byte(f.body), 0o600); err != nil {
			return err
		}
	}
	for _, f := range req.Files {
		if f.Binary || f.Content == nil || strings.TrimSpace(f.Path) == "" {
			continue
		}
		// 三件套已在上面落盘（verbatim 或生成），跳过防重复写
		switch f.Path {
		case "chart.yaml", "values.yaml", "deploy.yaml":
			continue
		}
		if err := WriteChartFile(workDir, f.Path, *f.Content); err != nil {
			return err
		}
	}
	for _, p := range req.DeleteFiles {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		abs, err := SecureJoin(workDir, filepath.ToSlash(p))
		if err != nil {
			return err
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// 保存前用同一加载器自检：deploy.yaml 语法/结构问题当场返回
	ch, err := chart.LoadWithLimits(workDir, chart.Limits{})
	if err != nil {
		return fmt.Errorf("saved chart does not load: %w", err)
	}
	return ch.Close()
}

// BuildChartYAML 生成 chart.yaml：保留底本已有的元数据字段（上传的
// chart 升版本保存不丢 marker_dir/phases 等）。
func BuildChartYAML(workDir, name string, req *SpecReq) (string, error) {
	var m *ChartMetaReq
	if b, err := os.ReadFile(filepath.Join(workDir, "chart.yaml")); err == nil {
		var y chartMetaYAML
		if yaml.Unmarshal(b, &y) == nil {
			m = y.toChartMeta()
		}
	}
	out := struct {
		Name         string `yaml:"name"`
		Version      string `yaml:"version"`
		Description  string `yaml:"description"`
		ChartMetaReq `yaml:",inline"`
	}{
		Name:        name,
		Version:     req.Version,
		Description: strings.ReplaceAll(strings.TrimSpace(req.Description), "\n", " "),
	}
	if m != nil {
		out.ChartMetaReq = *m
	}
	b, err := yaml.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// BizError spec 物化过程的业务错误（结构/语法/一致性校验不过）——
// 传输层对它回 400 + 原文，区别于基础设施错误（500）。
type BizError struct{ error }

// prepareSpecWorkspace 把编辑内容物化到独立工作目录：编辑模式（appID>0）
// 解包底本版本并复制为基底（未提交的文件——二进制/charts 子包——原样
// 保留），新建模式从空目录开始；随后 applySpec 应用编辑内容。调用方负责
// os.RemoveAll(workDir)。store.ErrNotFound 原样透传（底本版本不存在）。
func (a *AppService) PrepareWorkspace(appID int64, baseVersion, name string, req *SpecReq) (string, error) {
	workDir, err := os.MkdirTemp("", "wdp-spec-")
	if err != nil {
		return "", err
	}
	if appID > 0 {
		baseTgz, err := a.Store.VersionTgz(appID, baseVersion)
		if err != nil {
			os.RemoveAll(workDir)
			return "", err
		}
		// 底本解包 → 复制到自有工作目录（ch.Close 会清理其临时目录）。
		// LoadWithLimits 失败时返回 nil，必须先判错再解引用。
		ch, lerr := chart.LoadWithLimits(baseTgz, chart.Limits{})
		if lerr != nil {
			os.RemoveAll(workDir)
			return "", fmt.Errorf("prepare workspace: load base chart: %w", lerr)
		}
		lerr = CopyDir(ch.Dir, workDir)
		ch.Close()
		if lerr != nil {
			os.RemoveAll(workDir)
			return "", fmt.Errorf("prepare workspace: %w", lerr)
		}
	}
	if err := ApplySpec(workDir, name, req); err != nil {
		os.RemoveAll(workDir)
		return "", &BizError{err}
	}
	return workDir, nil
}

// PackAndStore 把工作目录打包为 tgz 并归位到应用版本目录，返回归档
// 路径、sha256 与大小（入库版本制品由它产出）。

func (a *AppService) PackAndStore(workDir, name, version string) (string, string, int64, error) {
	dir := filepath.Join(a.AppsDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", 0, err
	}
	dst := filepath.Join(dir, version+".tgz")
	tmp := dst + ".part"
	if err := PackChart(workDir, tmp); err != nil {
		os.Remove(tmp)
		return "", "", 0, err
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", "", 0, err
	}
	sha, err := FileSha256(tmp)
	if err != nil {
		os.Remove(tmp)
		return "", "", 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", "", 0, err
	}
	return dst, sha, fi.Size(), nil
}

// ChartYAMLVersion 读 chart.yaml 顶层 version（解析失败返回空串）。
func ChartYAMLVersion(content string) string {
	var meta struct {
		Version string `yaml:"version"`
	}
	if yaml.Unmarshal([]byte(content), &meta) != nil {
		return ""
	}
	return meta.Version
}
