//go:build linux || darwin || windows

package remote

// 本文件实现远程文件传输领域服务：在 tailcat 隧道内建立 SSH/SFTP 客户端，
// 并向 application/API 层暴露稳定的目录、上传和下载语义。tailcat 与 pkg/sftp
// 依赖均被封装在 remote bounded context 内，外层不接触第三方协议类型。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// fileTransferHandshakeTimeout 同时约束 SSH 握手和 SFTP 子系统版本协商，
// 防止不完整服务长期占用管理 API 请求。
const fileTransferHandshakeTimeout = 30 * time.Second

// FileCredentials 是一次 SFTP 连接使用的 SSH 凭据值对象。
// 私钥与口令只存在于单次 HTTP 请求内存，不进入配置、状态、审计或日志。
type FileCredentials struct {
	Username   string // Username 供系统 sshd 选择账户；proxyd 内嵌 SSH 忽略客户端用户名。
	PrivateKey []byte // PrivateKey 是本次请求提供的 OpenSSH/PEM 私钥原文，可为空。
	Passphrase []byte // Passphrase 是加密私钥口令，只在解析 PrivateKey 时使用。
}

// FileEntry 是远端目录条目的稳定领域表示，不暴露 os.FileInfo 或 SFTP 类型。
type FileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
}

// FileListing 是目录浏览结果，Path 为服务端规范化后的绝对路径。
type FileListing struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// FileDownload 描述已经打开的下载流及其可信元数据。
// Reader 的所有权交给调用方，必须调用 Close；关闭它不会自动关闭 FileClient。
type FileDownload struct {
	Name    string
	Size    int64
	ModTime time.Time
	Reader  io.ReadCloser
}

// FileClient 是一条 tailcat → SSH → SFTP 会话。
// 零值不可用；调用方必须 Close，以同时回收 SFTP、SSH 和 tailcat 数据面资源。
type FileClient struct {
	sftp      *sftp.Client
	ssh       *ssh.Client
	transport net.Conn
	stopWatch func() bool
	closeOnce sync.Once
	closeErr  error
}

// OpenFileClient 使用管理器持久客户端身份连接远端的 22 端口并启动 SFTP。
//
// 参数说明：
//   - ctx: context.Context，控制 DERP、SSH 握手和整个文件请求的取消。
//   - token: string，应用层已经解析出的完整 tailcat token。
//   - credentials: FileCredentials，可选 SSH 用户名、私钥和私钥口令。
//
// 返回值说明：*FileClient 和 error；成功对象由调用方负责 Close。
//
// 错误情况：remote 模块关闭、tailcat 拨号、SSH 私钥解析/认证或 SFTP 子系统启动失败时
// 返回错误。SSH host key 只在已由 token 锁定身份的 WireGuard 隧道内跳过 known_hosts，
// 不会对普通 TCP SSH 连接放宽校验。
func (m *Manager) OpenFileClient(ctx context.Context, token string, credentials FileCredentials) (*FileClient, error) {
	m.mu.Lock()
	if m.cfg.Disabled {
		m.mu.Unlock()
		return nil, fmt.Errorf("远程访问模块已禁用")
	}
	clientKey := m.clientKeyLocked()
	m.mu.Unlock()

	transport, err := Dial(ctx, token, 22, clientKey)
	if err != nil {
		return nil, fmt.Errorf("建立 tailcat 文件隧道失败: %w", err)
	}
	client, err := openFileClientOnConn(ctx, transport, credentials)
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	return client, nil
}

// openFileClientOnConn 在已连接的隧道流上完成 SSH 与 SFTP 握手。
//
// 参数说明：ctx 控制取消；transport 为调用方移交所有权的 net.Conn；credentials 为
// 单次 SSH 凭据。该函数抽离网络层，便于用 net.Pipe 验证协议和认证行为。
// 返回值说明：*FileClient 和 error；失败时调用方仍负责关闭 transport。
// 错误情况：私钥格式/口令错误、握手超时、SSH 认证拒绝或 sftp 子系统缺失时返回错误。
func openFileClientOnConn(ctx context.Context, transport net.Conn, credentials FileCredentials) (*FileClient, error) {
	authMethods := make([]ssh.AuthMethod, 0, 1)
	if len(credentials.PrivateKey) > 0 {
		var signer ssh.Signer
		var err error
		if len(credentials.Passphrase) > 0 {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(credentials.PrivateKey, credentials.Passphrase)
		} else {
			signer, err = ssh.ParsePrivateKey(credentials.PrivateKey)
		}
		if err != nil {
			return nil, fmt.Errorf("SSH 私钥解析失败: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	username := strings.TrimSpace(credentials.Username)
	if username == "" {
		username = "proxyd"
	}
	deadline := time.Now().Add(fileTransferHandshakeTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = transport.SetDeadline(deadline)
	stopHandshake := context.AfterFunc(ctx, func() { _ = transport.Close() })
	connection, channels, requests, err := ssh.NewClientConn(transport, "proxyd-file-transfer", &ssh.ClientConfig{
		User:            username,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		stopHandshake()
		return nil, fmt.Errorf("SSH 握手或认证失败: %w", err)
	}
	sshClient := ssh.NewClient(connection, channels, requests)
	// SSH 握手完成不代表 SFTP 子系统一定响应；保持同一个 deadline 与取消 watcher
	// 直到版本协商结束，避免不完整/恶意 SSH 服务让管理 API 永久阻塞。
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		stopHandshake()
		_ = sshClient.Close()
		return nil, fmt.Errorf("远端未提供可用的 SFTP 子系统: %w", err)
	}
	stopHandshake()
	_ = transport.SetDeadline(time.Time{})
	result := &FileClient{sftp: sftpClient, ssh: sshClient, transport: transport}
	result.stopWatch = context.AfterFunc(ctx, func() { _ = result.Close() })
	return result, nil
}

// List 列出远端目录，并把目录放在普通文件之前、同类按名称排序。
// 参数说明：ctx 控制目录读取取消；directory 为空时使用当前用户主目录（"."）。
// 返回值说明：FileListing，包含规范化绝对路径和稳定排序的条目。
// 错误情况：路径不存在、权限不足、目标不是目录或连接中断时返回错误。
func (c *FileClient) List(ctx context.Context, directory string) (FileListing, error) {
	directory = normalizeRemotePath(directory)
	canonical, err := c.sftp.RealPath(directory)
	if err != nil {
		return FileListing{}, fmt.Errorf("解析远端目录失败: %w", err)
	}
	infos, err := c.sftp.ReadDirContext(ctx, canonical)
	if err != nil {
		return FileListing{}, fmt.Errorf("读取远端目录失败: %w", err)
	}
	entries := make([]FileEntry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, FileEntry{
			Name: info.Name(), Path: path.Join(canonical, info.Name()), Size: info.Size(),
			Mode: info.Mode().String(), ModTime: info.ModTime(), IsDir: info.IsDir(),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return FileListing{Path: canonical, Entries: entries}, nil
}

// OpenDownload 打开一个远端普通文件用于流式下载。
// 参数说明：filePath 为 SFTP 路径，支持相对主目录或绝对路径。
// 返回值说明：FileDownload 和 error；调用方必须关闭成功返回的 Reader。
// 错误情况：路径为空、不存在、权限不足、目标为目录或连接中断时返回错误。
func (c *FileClient) OpenDownload(filePath string) (FileDownload, error) {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return FileDownload{}, fmt.Errorf("远端文件路径不能为空")
	}
	file, err := c.sftp.Open(filePath)
	if err != nil {
		return FileDownload{}, fmt.Errorf("打开远端文件失败: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return FileDownload{}, fmt.Errorf("读取远端文件信息失败: %w", err)
	}
	if info.IsDir() {
		_ = file.Close()
		return FileDownload{}, fmt.Errorf("远端路径是目录，不能直接下载")
	}
	return FileDownload{Name: info.Name(), Size: info.Size(), ModTime: info.ModTime(), Reader: file}, nil
}

// Upload 把输入流写入远端文件，并通过同目录临时文件 + POSIX rename 原子替换目标。
//
// 参数说明：filePath 为目标文件路径；source 为浏览器上传流，所有权仍归调用方。
// 返回值说明：成功写入的字节数和 error。
// 错误情况：路径为空、父目录不存在、权限不足、传输中断、关闭或原子替换失败时返回错误。
// 临时文件在任意失败路径都会 best-effort 删除，避免把半文件暴露为最终名称。
func (c *FileClient) Upload(filePath string, source io.Reader) (int64, error) {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return 0, fmt.Errorf("远端文件路径不能为空")
	}
	temporaryPath := fmt.Sprintf("%s.proxyd-upload-%d", filePath, time.Now().UnixNano())
	file, err := c.sftp.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return 0, fmt.Errorf("创建远端临时文件失败: %w", err)
	}
	keepTemporary := true
	defer func() {
		_ = file.Close()
		if keepTemporary {
			_ = c.sftp.Remove(temporaryPath)
		}
	}()
	written, copyErr := io.Copy(file, source)
	closeErr := file.Close()
	if copyErr != nil {
		return written, fmt.Errorf("上传远端文件失败: %w", copyErr)
	}
	if closeErr != nil {
		return written, fmt.Errorf("提交远端文件内容失败: %w", closeErr)
	}
	if err := c.sftp.PosixRename(temporaryPath, filePath); err != nil {
		return written, fmt.Errorf("原子替换远端文件失败: %w", err)
	}
	keepTemporary = false
	return written, nil
}

// Close 按从上层协议到底层传输的顺序回收文件连接。
// 参数说明：无。
// 返回值说明：最先出现的关闭错误；重复调用允许底层返回已关闭错误。
// 错误情况：关闭 SFTP、SSH 或 tailcat 连接失败时返回其中第一个错误，其余资源仍会尝试关闭。
func (c *FileClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.stopWatch != nil {
			c.stopWatch()
		}
		var errs []error
		if c.sftp != nil {
			errs = append(errs, c.sftp.Close())
		}
		if c.ssh != nil {
			errs = append(errs, c.ssh.Close())
		}
		if c.transport != nil {
			errs = append(errs, c.transport.Close())
		}
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}

// normalizeRemotePath 统一目录浏览的空路径语义。
// 参数说明：value 为用户提交的 SFTP 目录路径。
// 返回值说明：去除首尾空白后的路径；空值返回 "."，由服务端解析为 shell-user 主目录。
// 错误情况：无；路径存在性与权限由 SFTP 服务端判定。
func normalizeRemotePath(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "."
}
