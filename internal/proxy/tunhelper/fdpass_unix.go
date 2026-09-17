//go:build darwin || linux

package tunhelper

// fd 回传机制（本包相对 gateway 模板的唯一新机制）：tun.create 成功时，
// 服务端经同一 unix 连接用 SCM_RIGHTS 把 utun fd 发给客户端；
// 客户端接收后 dup 到高位 fd（mihomo 的 tun.file-descriptor 用 0 作「未启用」哨兵，
// 须避开低位）并关闭原始接收 fd。仅 unix 系有意义；windows 存根见 fdpass_other.go。

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// helperFDDupFloor 是客户端 dup fd 的下界：避开 0 哨兵与常见低位描述符。
const helperFDDupFloor = 10

// closeHelperFD 关闭 helper 侧/接收侧的原始 fd。
func closeHelperFD(fd int) {
	_ = unix.Close(fd)
}

// CloseFD 关闭一个由本包返回的 fd（如 RequestTUN 拿到的 utun fd）。
// 供应用层在 fd 尚未移交 mihomo 的失败回滚路径上使用；fd 一旦随配置成功应用，
// 所有权归 mihomo，不得再经本函数关闭。
//
// 参数：
//   - fd: int，待关闭的文件描述符。
//
// 返回值：无；关闭失败静默忽略（回滚路径 best-effort）。
//
// 错误情况：无；fd 非法时 unix.Close 错误被丢弃。
func CloseFD(fd int) {
	_ = unix.Close(fd)
}

// sendHelperFD 经 unix 连接以 SCM_RIGHTS 发送一个 fd（附带 1 字节载荷，
// 供 recvmsg 对齐接收）。调用方负责发送成功后关闭自己的 fd 副本。
//
// 参数：
//   - conn: net.Conn，必须是 *net.UnixConn（stream）。
//   - fd: int，待发送的文件描述符。
//
// 返回值：error，连接类型不符或 sendmsg 失败时返回。
//
// 错误情况：非 unix 连接返回中文错误（net.Pipe 等仅出现在测试中）。
func sendHelperFD(conn net.Conn, fd int) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("fd 回传需要 unix socket 连接")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	rights := unix.UnixRights(fd)
	var sendErr error
	if err := raw.Write(func(rawFD uintptr) bool {
		sendErr = unix.Sendmsg(int(rawFD), []byte{0}, rights, nil, 0)
		return sendErr != unix.EAGAIN
	}); err != nil {
		return err
	}
	if sendErr != nil {
		return fmt.Errorf("SCM_RIGHTS 发送 fd 失败: %w", sendErr)
	}
	return nil
}

// recvHelperFD 从 unix 连接接收一个 SCM_RIGHTS fd，dup 到 >=helperFDDupFloor 的
// 高位 fd 后关闭原始接收 fd，返回 dup 后的 fd。
//
// 参数：
//   - conn: net.Conn，必须是 *net.UnixConn（stream）。
//
// 返回值：
//   - int：dup 后的 fd，调用方持有并在用完（mihomo 接管）前不得关闭。
//   - error：连接类型不符、recvmsg 失败或报文未携带 fd 时返回。
//
// 错误情况：dup 失败时原始接收 fd 一并关闭，不泄漏。
func recvHelperFD(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, fmt.Errorf("fd 回传需要 unix socket 连接")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return -1, err
	}
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	var n, oobn int
	var recvErr error
	if err := raw.Read(func(rawFD uintptr) bool {
		n, oobn, _, _, recvErr = unix.Recvmsg(int(rawFD), buf, oob, 0)
		return recvErr != unix.EAGAIN
	}); err != nil {
		return -1, err
	}
	if recvErr != nil {
		return -1, fmt.Errorf("SCM_RIGHTS 接收 fd 失败: %w", recvErr)
	}
	if n == 0 {
		return -1, fmt.Errorf("对端未发送 fd（连接已关闭）")
	}
	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("解析控制消息失败: %w", err)
	}
	received := -1
	for _, message := range messages {
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			continue
		}
		if len(fds) > 0 {
			received = fds[0]
			break
		}
	}
	if received < 0 {
		return -1, fmt.Errorf("应答报文未携带 fd")
	}
	dupFd, err := unix.FcntlInt(uintptr(received), unix.F_DUPFD_CLOEXEC, helperFDDupFloor)
	_ = unix.Close(received)
	if err != nil {
		return -1, fmt.Errorf("dup 接收 fd 到高位失败: %w", err)
	}
	return dupFd, nil
}
