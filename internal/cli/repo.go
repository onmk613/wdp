package cli

// wdp repo：chart 仓库客户端（服务端 GET /charts/*，Helm 兼容索引）。
//
//	wdp repo list  --repo http://s:7603/charts            应用与版本一览
//	wdp repo show  <name> [--version 1.2.0]               chart 详情（元信息/相位/摘要）
//	wdp repo pull  <name> [--version] [--dest charts]     拉取解包（目录形态，可直接被 run 引用）
//	                                                   --tgz 改存 .tgz 包（lint/plan 直接吃）
//	wdp repo push  <app.tgz>                              登录后走控制台上传（app:upload 权限）
//
// 认证：--repo-auth user:pass（或 WDP_REPO_AUTH 环境变量，格式同）。
// --repo 也接受不带 /charts 路径的基址（自动补全）。拉取一律校验索引
// digest（sha256），防传输损坏与索引之外的替换。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/chart"
	"wdp/internal/i18n"
)

// repoIndexFile CLI 侧只需要条目字段的子集（digest 校验/展示用）。
type repoIndexFile struct {
	ApiVersion string                     `yaml:"apiVersion"`
	Entries    map[string][]repoEntryView `yaml:"entries"`
}

type repoEntryView struct {
	Name        string            `yaml:"name"`
	Version     string            `yaml:"version"`
	Description string            `yaml:"description"`
	Created     string            `yaml:"created"`
	Digest      string            `yaml:"digest"`
	Urls        []string          `yaml:"urls"`
	Annotations map[string]string `yaml:"annotations"`
}

// repoFlags --repo/--repo-auth 两枚（list/show/pull/push/run 共用口径）。
type repoFlags struct {
	base string
	auth string
}

func (f *repoFlags) add(c *cobra.Command) {
	c.Flags().StringVar(&f.base, "repo", "", i18n.T(
		"chart repository base URL (e.g. http://server:7603/charts); env WDP_REPO",
		"chart 仓库基址（如 http://server:7603/charts）；环境变量 WDP_REPO"))
	c.Flags().StringVar(&f.auth, "repo-auth", "", i18n.T(
		"repository credentials user:password; env WDP_REPO_AUTH",
		"仓库凭据 user:password；环境变量 WDP_REPO_AUTH"))
}

func (f *repoFlags) resolve() (string, string, error) {
	base := f.base
	if base == "" {
		base = os.Getenv("WDP_REPO")
	}
	auth := f.auth
	if auth == "" {
		auth = os.Getenv("WDP_REPO_AUTH")
	}
	if base == "" {
		return "", "", fmt.Errorf("chart repository not set: pass --repo or set WDP_REPO")
	}
	base = strings.TrimRight(base, "/")
	// 便捷：给控制台根地址即视为 chart 仓库（端点挂在 /charts 下）
	if !strings.HasSuffix(base, "/charts") && filepath.Ext(base) == "" && !strings.Contains(base[strings.Index(base, "://")+3:], "/") {
		base += "/charts"
	}
	if auth != "" && !strings.Contains(auth, ":") {
		return "", "", fmt.Errorf("--repo-auth expects user:password")
	}
	return base, auth, nil
}

// repoHTTP 带凭据的请求（basic auth；无凭据匿名——服务端会回 401 质询）。
func repoGet(url, auth string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		u, p, _ := strings.Cut(auth, ":")
		req.SetBasicAuth(u, p)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("authentication required (401): pass --repo-auth user:password")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// fetchRepoIndex 拉取并解析索引。
func fetchRepoIndex(base, auth string) (*repoIndexFile, error) {
	resp, err := repoGet(base+"/index.yaml", auth)
	if err != nil {
		return nil, fmt.Errorf("fetch index: %w", err)
	}
	defer resp.Body.Close()
	var idx repoIndexFile
	if err := yaml.NewDecoder(resp.Body).Decode(&idx); err != nil {
		return nil, fmt.Errorf("parse index.yaml: %w", err)
	}
	if idx.Entries == nil {
		return nil, fmt.Errorf("index.yaml has no entries (not a chart repository?)")
	}
	return &idx, nil
}

// resolveEntry 定位目标版本：@约束/显式 --version/缺省最新（首条）。
func resolveEntry(idx *repoIndexFile, name, version string) (*repoEntryView, error) {
	entries := idx.Entries[name]
	if len(entries) == 0 {
		return nil, fmt.Errorf("chart %q not found in repository", name)
	}
	if version == "" {
		return &entries[0], nil // 索首即最新
	}
	for i := range entries {
		if entries[i].Version == version {
			return &entries[i], nil
		}
	}
	return nil, fmt.Errorf("chart %q version %s not found (available: %s)", name, version, versionList(entries))
}

func versionList(entries []repoEntryView) string {
	vs := make([]string, len(entries))
	for i, e := range entries {
		vs[i] = e.Version
	}
	return strings.Join(vs, ", ")
}

// downloadChart 下载制品到临时文件并校验 digest；返回临时路径（调用方
// 负责删除）与实际字节摘要。
func downloadChart(base, auth string, e *repoEntryView) (string, string, error) {
	if len(e.Urls) == 0 {
		return "", "", fmt.Errorf("index entry for %s@%s has no url", e.Name, e.Version)
	}
	u := e.Urls[0]
	if !strings.Contains(u, "://") {
		// 索引内相对路径（形如 /charts/<name>-<ver>.tgz，锚在服务根）：
		// 基址去掉尾部 /charts 还原服务根再拼，避免 /charts/charts/…
		//（会撞 SPA fallback，拿回 index.html 还 200，digest 必错）
		u = strings.TrimSuffix(base, "/charts") + u
	}
	resp, err := repoGet(u, auth)
	if err != nil {
		return "", "", fmt.Errorf("download %s: %w", u, err)
	}
	defer resp.Body.Close()

	tmp, err := os.CreateTemp("", "wdp-pull-*.tgz")
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	cerr := tmp.Close()
	if err != nil || cerr != nil {
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("download write: %v", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	want := strings.TrimPrefix(e.Digest, "sha256:")
	if want != "" && want != got {
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("digest mismatch for %s@%s: index says sha256:%s, downloaded %s", e.Name, e.Version, want, got)
	}
	return tmp.Name(), got, nil
}

// ---- 命令 ----

// repoHelp 返回 `wdp repo` 的长帮助（调用时求值）。
func repoHelp() string {
	return i18n.T(`Chart repository client.

The server (wdp server) exposes the app library as a Helm-compatible repository under /charts:
  wdp repo list --repo http://server:7603/charts
  wdp repo show  nginx --version 1.2.0 --repo http://server:7603/charts
  wdp repo pull  nginx --dest ./charts          # unpack into charts/nginx/, ready for run
  wdp repo push  nginx-1.2.0.tgz --repo http://server:7603/charts --repo-auth admin:pass

Credentials go through --repo-auth user:password (or the WDP_REPO_AUTH environment variable);
--repo can also be preset with WDP_REPO. pull/push and run --repo downloads all verify the
sha256 digest.
`, `chart 仓库客户端。

服务端（wdp server）把应用库以 Helm 兼容仓库暴露在 /charts 下：
  wdp repo list --repo http://server:7603/charts
  wdp repo show  nginx --version 1.2.0 --repo http://server:7603/charts
  wdp repo pull  nginx --dest ./charts          # 解包为 charts/nginx/，run 直接引用
  wdp repo push  nginx-1.2.0.tgz --repo http://server:7603/charts --repo-auth admin:pass

凭据走 --repo-auth user:password（或 WDP_REPO_AUTH 环境变量）；
--repo 也可用 WDP_REPO 环境变量预设。pull/push 与 run --repo 的
拉取都做 sha256 digest 校验。
`)
}

func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "repo",
		Short: i18n.T("chart repository client (list/show/pull/push against the wdp console chart repo)",
			"chart 仓库客户端（对 wdp 控制台 chart 仓库做 list/show/pull/push）"),
		Long: repoHelp(),
	}
	cmd.AddCommand(newRepoListCmd(), newRepoShowCmd(), newRepoPullCmd(), newRepoPushCmd())
	return cmd
}

func newRepoListCmd() *cobra.Command {
	var f repoFlags
	cmd := &cobra.Command{
		Use: "list",
		Short: i18n.T("list charts and versions in the repository",
			"列出仓库中的 chart 与版本"),
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			base, auth, err := f.resolve()
			if err != nil {
				return err
			}
			idx, err := fetchRepoIndex(base, auth)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(idx.Entries))
			for n := range idx.Entries {
				names = append(names, n)
			}
			sort.Strings(names)
			if len(names) == 0 {
				fmt.Println("(repository is empty)")
				return nil
			}
			for _, n := range names {
				entries := idx.Entries[n]
				fmt.Printf("%s  (%d versions)\n", n, len(entries))
				for i, e := range entries {
					if i >= 3 { // 列表只示近三版，全量用 show
						fmt.Printf("    … +%d more (wdp repo show %s)\n", len(entries)-3, n)
						break
					}
					desc := e.Description
					if len(desc) > 48 {
						desc = desc[:48] + "…"
					}
					fmt.Printf("    %-12s %s\n", e.Version, desc)
				}
			}
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func newRepoShowCmd() *cobra.Command {
	var f repoFlags
	var version string
	cmd := &cobra.Command{
		Use: "show <name>",
		Short: i18n.T("show chart details (metadata, phases, digest) from the repository",
			"查看仓库中 chart 的详情（元信息、相位、摘要）"),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			base, auth, err := f.resolve()
			if err != nil {
				return err
			}
			idx, err := fetchRepoIndex(base, auth)
			if err != nil {
				return err
			}
			name := args[0]
			entries := idx.Entries[name]
			if len(entries) == 0 {
				return fmt.Errorf("chart %q not found in repository", name)
			}
			if version == "" {
				e := &entries[0]
				printEntry(e, true)
				fmt.Printf("all versions: %s\n", versionList(entries))
				return nil
			}
			e, err := resolveEntry(idx, name, version)
			if err != nil {
				return err
			}
			printEntry(e, true)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&version, "version", "", "chart version (default: latest)")
	return cmd
}

func printEntry(e *repoEntryView, detailed bool) {
	fmt.Printf("%s %s\n", e.Name, e.Version)
	if e.Description != "" {
		fmt.Printf("  description: %s\n", e.Description)
	}
	fmt.Printf("  created:     %s\n", e.Created)
	fmt.Printf("  digest:      %s\n", e.Digest)
	if p := e.Annotations["phases"]; p != "" {
		fmt.Printf("  phases:      %s\n", p)
	}
	if detailed && len(e.Urls) > 0 {
		fmt.Printf("  url:         %s\n", e.Urls[0])
	}
}

func newRepoPullCmd() *cobra.Command {
	var f repoFlags
	var version, dest string
	keepTgz := false
	cmd := &cobra.Command{
		Use: "pull <name>",
		Short: i18n.T("download a chart from the repository (verified against index digest)",
			"从仓库拉取 chart 并按索引 digest 校验"),
		Long: i18n.T(`Download a chart as a directory (default ./charts/<name>/, the same resolution
root bare playbooks use for chart refs — ready for wdp run right after the pull); --tgz stores it as a
<name>-<version>.tgz package instead (lint/plan/render consume packages directly).
All downloaded content is verified against the index digest (sha256).
`, `拉取 chart 并落为目录形态（默认 ./charts/<name>/，与裸 playbook 的
chart 引用解析根一致——拉完即可 wdp run）；--tgz 改存
<name>-<version>.tgz 包（lint/plan/render 可直接消费包形态）。
下载内容一律校验索引 digest（sha256）。`),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			base, auth, err := f.resolve()
			if err != nil {
				return err
			}
			idx, err := fetchRepoIndex(base, auth)
			if err != nil {
				return err
			}
			name := args[0]
			e, err := resolveEntry(idx, name, version)
			if err != nil {
				return err
			}
			tmp, got, err := downloadChart(base, auth, e)
			if err != nil {
				return err
			}
			defer os.Remove(tmp)

			if dest == "" {
				dest = "charts"
			}
			if keepTgz {
				out := filepath.Join(dest, e.Name+"-"+e.Version+".tgz")
				if err := os.MkdirAll(dest, 0o755); err != nil {
					return err
				}
				if err := copyFile(tmp, out); err != nil {
					return err
				}
				fmt.Printf("pulled %s@%s (sha256:%s)\n  → %s\n", e.Name, e.Version, got[:16]+"…", out)
				return nil
			}
			out := filepath.Join(dest, e.Name)
			if err := chart.ExtractTo(tmp, out); err != nil {
				return err
			}
			fmt.Printf("pulled %s@%s (sha256:%s)\n  → %s/ (chart.yaml ✓)\n", e.Name, e.Version, got[:16]+"…", out)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&version, "version", "", "chart version (default: latest)")
	cmd.Flags().StringVar(&dest, "dest", "charts", "destination directory (default ./charts)")
	cmd.Flags().BoolVar(&keepTgz, "tgz", false, "keep as .tgz package instead of unpacking")
	return cmd
}

// newRepoPushCmd 登录控制台（basic 凭据换会话）后复用应用上传端点：
// chart.yaml 的 name/version 决定落位，权限/审计/去重口径与 Web 上传
// 完全一致。
func newRepoPushCmd() *cobra.Command {
	var f repoFlags
	cmd := &cobra.Command{
		Use: "push <chart.tgz>",
		Short: i18n.T("upload a chart package to the console (login with --repo-auth, then app upload)",
			"上传 chart 包到控制台（用 --repo-auth 登录后走应用上传）"),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			base, auth, err := f.resolve()
			if err != nil {
				return err
			}
			if auth == "" {
				return fmt.Errorf("push needs credentials: pass --repo-auth user:password")
			}
			// 控制台根（去 /charts 尾）：登录与上传都是控制台 API
			root := strings.TrimSuffix(strings.TrimSuffix(base, "/charts"), "/")
			u, p, _ := strings.Cut(auth, ":")

			jar := &memoryJar{}
			client := &http.Client{Jar: jar}
			// 1) 登录换会话 cookie
			lr, err := client.Post(root+"/api/login", "application/json",
				strings.NewReader(fmt.Sprintf(`{"User":%q,"Password":%q}`, u, p)))
			if err != nil {
				return fmt.Errorf("login: %w", err)
			}
			body, _ := io.ReadAll(io.LimitReader(lr.Body, 512))
			lr.Body.Close()
			if lr.StatusCode != http.StatusOK {
				return fmt.Errorf("login failed (HTTP %d): %s", lr.StatusCode, strings.TrimSpace(string(body)))
			}
			// 2) multipart 上传（字段名 tgz 与 Web 端一致）
			fh, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer fh.Close()
			buf := &bytes.Buffer{}
			mw := multipart.NewWriter(buf)
			part, err := mw.CreateFormFile("tgz", filepath.Base(args[0]))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, fh); err != nil {
				return err
			}
			if err := mw.Close(); err != nil {
				return err
			}
			pr, err := client.Post(root+"/api/apps/upload", mw.FormDataContentType(), buf)
			if err != nil {
				return fmt.Errorf("upload: %w", err)
			}
			ubody, _ := io.ReadAll(io.LimitReader(pr.Body, 4096))
			pr.Body.Close()
			if pr.StatusCode >= 300 {
				return fmt.Errorf("upload failed (HTTP %d): %s", pr.StatusCode, strings.TrimSpace(string(ubody)))
			}
			fmt.Printf("pushed %s → %s\n  %s\n", args[0], root, strings.TrimSpace(string(ubody)))
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

// repoFetch run --repo 的引用兜底器：本地 charts/<name>/ 缺失时按索引
// 拉取（@version 约束/最新版），digest 校验后解包缓存到本地——下一次
// run 直接命中缓存，仓库不可达也能离线执行。
type repoFetch struct{ base, auth string }

func (rf *repoFetch) fetchInto(name, atVersion, destDir string) error {
	base, auth, err := (&repoFlags{base: rf.base, auth: rf.auth}).resolve()
	if err != nil {
		return err
	}
	idx, err := fetchRepoIndex(base, auth)
	if err != nil {
		return err
	}
	e, err := resolveEntry(idx, name, atVersion)
	if err != nil {
		return err
	}
	tmp, _, err := downloadChart(base, auth, e)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := chart.ExtractTo(tmp, destDir); err != nil {
		return err
	}
	fmt.Printf("repo: fetched %s@%s → %s (cached for offline runs)\n", name, e.Version, destDir)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// memoryJar 最小 cookie jar：只存登录会话，push 流程够用（不引第三方
// cookiejar 依赖面）。
type memoryJar struct{ cookie *http.Cookie }

func (j *memoryJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	for _, c := range cookies {
		if c.Name != "" {
			j.cookie = c
		}
	}
}
func (j *memoryJar) Cookies(_ *url.URL) []*http.Cookie {
	if j.cookie == nil {
		return nil
	}
	return []*http.Cookie{j.cookie}
}
