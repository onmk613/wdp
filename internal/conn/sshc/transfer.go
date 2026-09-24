package sshc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"

	"wdp/internal/conn"
	"wdp/internal/shellquote"
)

// sftpCopy 在 SFTP 通道上执行 copy 并响应 ctx：SFTP 协议本身无 deadline，
// 取消时强制关闭文件与 SFTP 客户端解除阻塞（连接降级为 exec 流式通道，
// 下次传输自动走 streamUpload——它原生响应 ctx）。killSftp 只关闭不置
// nil：并发共享该连接的传输持有的指针继续安全（已关闭通道上的操作返回
// 错误），不会 TOCTOU nil 解引用。
func (c *Conn) sftpCopy(ctx context.Context, f *sftp.File, r io.Reader) error {
	if ctx == nil {
		_, err := io.Copy(f, r)
		return err
	}
	type res struct{ err error }
	done := make(chan res, 1)
	go func() {
		_, err := io.Copy(f, r)
		done <- res{err}
	}()
	select {
	case r := <-done:
		return r.err
	case <-ctx.Done():
		_ = f.Close()
		c.killSftp()
		<-done // 回收 goroutine（Copy 已因连接关闭而出错返回）
		return ctx.Err()
	}
}

// UploadFile 上传文件（SFTP 优先，降级 exec）。
func (c *Conn) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	if err := c.ensureClient(); err != nil {
		return err
	}
	sc := c.sftpClient()
	if sc != nil {
		if err := mkdirRemote(sc, path.Dir(dst)); err != nil {
			return fmt.Errorf("failed to create remote directory: %w", err)
		}
		tmp := fmt.Sprintf("%s/.wdp.upload.%s", path.Dir(dst), randHex())
		f, err := sc.Create(tmp)
		if err != nil {
			return fmt.Errorf("failed to create remote temp file: %w", err)
		}
		if err := c.sftpCopy(ctx, f, r); err != nil {
			f.Close()
			_ = sc.Remove(tmp)
			return fmt.Errorf("write failed: %w", err)
		}
		if err := f.Chmod(mode.Perm()); err != nil {
			f.Close()
			_ = sc.Remove(tmp)
			return fmt.Errorf("failed to set permissions: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = sc.Remove(tmp) // 关闭失败时清掉远端临时文件
			return err
		}
		if err := sc.PosixRename(tmp, dst); err != nil {
			if err2 := sc.Rename(tmp, dst); err2 != nil {
				_ = sc.Remove(tmp) // 两种改名均失败，清掉临时文件
				return fmt.Errorf("rename failed: %w", err)
			}
		}
		return nil
	}
	// 降级：无 SFTP 时用专用会话流式写入（cat 接 stdin）
	return c.streamUpload(ctx, dst, r, mode)
}

// streamUpload 通过会话 stdin 流式上传：写目标目录下临时文件，成功后
// 原子改名（mv 同目录），失败清理临时文件——不留下部分写入的目标文件。
func (c *Conn) streamUpload(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	sess, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer sess.Close()
	sess.Stdin = r
	var stderr bytes.Buffer
	sess.Stderr = &stderr

	cmd := fmt.Sprintf(`D=%s && mkdir -p -- "$D" && T=$(mktemp "$D/.wdp.upload.XXXXXX") &&
cat > "$T" && chmod %o "$T" && mv -f "$T" %s ||
{ rc=$?; rm -f "$T" 2>/dev/null; exit $rc; }`,
		shellquote.Quote(path.Dir(dst)), mode.Perm(), shellquote.Quote(dst))
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("stream upload failed: %w: %s", err, stderr.String())
		}
		return nil
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return ctx.Err()
	}
}

// DownloadFile 下载文件（SFTP 优先，降级 exec cat）。
func (c *Conn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	if err := c.ensureClient(); err != nil {
		return err
	}
	sc := c.sftpClient()
	if sc != nil {
		f, err := sc.Open(src)
		if err != nil {
			return fmt.Errorf("failed to open remote file: %w", err)
		}
		defer f.Close()
		// SFTP 读方向的 copy 包一层 ctx 看护（读阻塞同样可无限挂起）
		type res struct{ err error }
		done := make(chan res, 1)
		go func() {
			_, err := io.Copy(w, f)
			done <- res{err}
		}()
		select {
		case r := <-done:
			return r.err
		case <-ctx.Done():
			_ = f.Close()
			c.killSftp()
			<-done
			return ctx.Err()
		}
	}
	res, err := c.Exec(ctx, conn.ExecRequest{Script: fmt.Sprintf("cat -- %s", shellquote.Quote(src))})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("cat failed: %s", res.Stderr)
	}
	_, err = io.WriteString(w, res.Stdout)
	return err
}

// mkdirRemote 逐级创建远端目录（已存在则忽略）。
func mkdirRemote(c *sftp.Client, dir string) error {
	if dir == "" || dir == "/" || dir == "." {
		return nil
	}
	if _, err := c.Stat(dir); err == nil {
		return nil
	}
	if err := mkdirRemote(c, path.Dir(dir)); err != nil {
		return err
	}
	if err := c.Mkdir(dir); err != nil && !os.IsExist(err) {
		// sftp 的存在性错误码映射不总是到位，二次探测
		if _, err2 := c.Stat(dir); err2 != nil {
			return err
		}
	}
	return nil
}

// UploadFileAs 以 becomeUser 身份写入 dst（提权分发）。
//
// 机制：先把数据经 SFTP/流式写到登录用户自己的临时文件（登录用户可写
// /tmp），再用 `sudo -n -u <user> -- cp` 搬到目标路径并设权限——sudo 以
// 目标身份读取登录用户的临时文件，因此不要求临时文件对他人可读。
// 结束后无论成败都清理临时文件。
func (c *Conn) UploadFileAs(ctx context.Context, dst string, r io.Reader, mode fs.FileMode, becomeUser string) error {
	if becomeUser == "" {
		return c.UploadFile(ctx, dst, r, mode)
	}
	if mode == 0 {
		mode = 0o644
	}
	tmp := "/tmp/.wdp.upload." + randHex()
	if err := c.UploadFile(ctx, tmp, r, 0o600); err != nil {
		return err
	}
	cleanup := func() {
		_, _ = c.Exec(ctx, conn.ExecRequest{Script: "rm -f -- " + shellquote.Quote(tmp), TimeoutMs: 30_000})
	}
	defer cleanup()

	script := fmt.Sprintf("install -m %04o -- %s %s",
		mode.Perm(), shellquote.Quote(tmp), shellquote.Quote(dst))
	out, err := c.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 120_000, BecomeUser: becomeUser})
	if err != nil {
		return fmt.Errorf("become upload failed: %w", err)
	}
	if out.Code != 0 {
		return fmt.Errorf("become upload failed rc=%d: %s", out.Code, strings.TrimSpace(out.Stderr))
	}
	return nil
}
