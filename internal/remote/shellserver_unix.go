//go:build linux || darwin

package remote

// 本文件是 Web Terminal 内嵌 shell 服务的 Unix 会话实现：登录 shell 选择与真实 PTY。
// 移植自 tailcat tailcat_ssh_unix.go（BSD-3-Clause）：
// Portions Copyright (c) Tailscale Inc & contributors.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/creack/pty"
	ssh "github.com/tailscale/gliderssh"
	"github.com/u-root/u-root/pkg/termios"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// newShellSessionCommand 返回运行用户登录 shell 的未启动命令：
// rawCmd 为空时进入交互登录 shell，否则用 shell -c 执行。
//
// 参数说明：
//   - u: *user.User，shell 归属的本机用户（proxyd 进程用户）。
//   - rawCmd: string，客户端携带的可选远端命令。
//
// 返回值说明：*exec.Cmd，已设置 Args、Dir 与基础环境，调用方可追加客户端环境。
//
// 错误情况：无。
func newShellSessionCommand(u *user.User, rawCmd string) *exec.Cmd {
	shell := loginShell(u)
	var args []string
	if rawCmd == "" {
		args = []string{shell, "-l"}
	} else {
		args = []string{shell, "-c", rawCmd}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = u.HomeDir
	cmd.Env = []string{
		"SHELL=" + shell,
		"USER=" + u.Username,
		"HOME=" + u.HomeDir,
		"PATH=" + defaultShellPath(u),
	}
	return cmd
}

// runShellWithPTY 把命令挂到伪终端上运行，并桥接 SSH 会话与窗口尺寸变化。
//
// 参数说明：
//   - sess: ssh.Session，已申请 PTY 的会话。
//   - cmd: *exec.Cmd，尚未启动的 shell 命令。
//   - ptyReq: ssh.Pty，客户端请求的首个终端尺寸与模式。
//   - winCh: <-chan ssh.Window，后续 window-change 事件。
//
// 返回值说明：无；命令退出码经 sess.Exit 回传。
//
// 错误情况：PTY 打开或进程启动失败时向会话写错误并以退出码 1 结束。
func runShellWithPTY(sess ssh.Session, cmd *exec.Cmd, ptyReq ssh.Pty, winCh <-chan ssh.Window) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "pty open: %v\r\n", err)
		sess.Exit(1)
		return
	}
	defer ptmx.Close()
	defer tty.Close()

	// 按 SSH 请求配置终端模式与初始尺寸。
	if rc, err := tty.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) {
			tios, err := termios.GTTY(int(fd))
			if err != nil {
				return
			}
			tios.Row = int(ptyReq.Window.Height)
			tios.Col = int(ptyReq.Window.Width)
			for c, v := range ptyReq.Modes {
				if c == gossh.TTY_OP_ISPEED {
					tios.Ispeed = int(v)
					continue
				}
				if c == gossh.TTY_OP_OSPEED {
					tios.Ospeed = int(v)
					continue
				}
				k, ok := shellOpcodeShortName[c]
				if !ok {
					continue
				}
				if _, ok := tios.CC[k]; ok {
					tios.CC[k] = uint8(v)
					continue
				}
				if _, ok := tios.Opts[k]; ok {
					tios.Opts[k] = v > 0
					continue
				}
			}
			tios.STTY(int(fd))
		})
	}

	if ptyReq.Term != "" {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setctty: true, Setsid: true}
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(sess.Stderr(), "start: %v\r\n", err)
		sess.Exit(1)
		return
	}
	_ = tty.Close() // 子进程已持有 tty

	// 窗口尺寸协程在会话关闭、winCh 关闭前一直运行，可能晚于本函数返回，
	// 因此使用独立 dup 出的 fd，避免与 defer 的 ptmx.Close 竞争复用。
	if winchFd, err := unix.Dup(int(ptmx.Fd())); err == nil {
		go func() {
			defer unix.Close(winchFd)
			for win := range winCh {
				_ = unix.IoctlSetWinsize(winchFd, syscall.TIOCSWINSZ, &unix.Winsize{
					Row:    uint16(win.Height),
					Col:    uint16(win.Width),
					Xpixel: uint16(win.WidthPixels),
					Ypixel: uint16(win.HeightPixels),
				})
			}
		}()
	}

	go func() {
		_, _ = io.Copy(ptmx, sess) // stdin
	}()
	_, _ = io.Copy(sess, ptmx) // stdout，阻塞到 PTY 关闭

	if err := cmd.Wait(); err != nil {
		sess.Exit(shellExitCode(err))
		return
	}
	sess.Exit(0)
}

// loginShell 返回用户的登录 shell。
//
// 参数说明：
//   - u: *user.User，目标本机用户。
//
// 返回值说明：string，优先目录服务记录，其次 SHELL 环境变量，兜底 /bin/sh。
//
// 错误情况：无；所有查询失败都回退到默认值。
func loginShell(u *user.User) string {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("dscl", ".", "-read", filepath.Join("/Users", u.Username), "UserShell").Output()
		if err == nil {
			if s, ok := strings.CutPrefix(string(out), "UserShell: "); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	if e := os.Getenv("SHELL"); e != "" {
		return e
	}
	return "/bin/sh"
}

// defaultShellPath 返回该用户会话的默认 PATH。
//
// 参数说明：
//   - u: *user.User，用于区分 root 与普通用户。
//
// 返回值说明：string，root 含 sbin 目录。
//
// 错误情况：无。
func defaultShellPath(u *user.User) string {
	if u.Uid == "0" {
		return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return "/usr/local/bin:/usr/bin:/bin"
}

// shellOpcodeShortName 把 SSH 终端模式操作码映射为 termios 包使用的助记名。
var shellOpcodeShortName = map[uint8]string{
	gossh.VINTR:         "intr",
	gossh.VQUIT:         "quit",
	gossh.VERASE:        "erase",
	gossh.VKILL:         "kill",
	gossh.VEOF:          "eof",
	gossh.VEOL:          "eol",
	gossh.VEOL2:         "eol2",
	gossh.VSTART:        "start",
	gossh.VSTOP:         "stop",
	gossh.VSUSP:         "susp",
	gossh.VDSUSP:        "dsusp",
	gossh.VREPRINT:      "rprnt",
	gossh.VWERASE:       "werase",
	gossh.VLNEXT:        "lnext",
	gossh.VFLUSH:        "flush",
	gossh.VSWTCH:        "swtch",
	gossh.VSTATUS:       "status",
	gossh.VDISCARD:      "discard",
	gossh.IGNPAR:        "ignpar",
	gossh.PARMRK:        "parmrk",
	gossh.INPCK:         "inpck",
	gossh.ISTRIP:        "istrip",
	gossh.INLCR:         "inlcr",
	gossh.IGNCR:         "igncr",
	gossh.ICRNL:         "icrnl",
	gossh.IUCLC:         "iuclc",
	gossh.IXON:          "ixon",
	gossh.IXANY:         "ixany",
	gossh.IXOFF:         "ixoff",
	gossh.IMAXBEL:       "imaxbel",
	gossh.IUTF8:         "iutf8",
	gossh.ISIG:          "isig",
	gossh.ICANON:        "icanon",
	gossh.XCASE:         "xcase",
	gossh.ECHO:          "echo",
	gossh.ECHOE:         "echoe",
	gossh.ECHOK:         "echok",
	gossh.ECHONL:        "echonl",
	gossh.NOFLSH:        "noflsh",
	gossh.TOSTOP:        "tostop",
	gossh.IEXTEN:        "iexten",
	gossh.ECHOCTL:       "echoctl",
	gossh.ECHOKE:        "echoke",
	gossh.PENDIN:        "pendin",
	gossh.OPOST:         "opost",
	gossh.OLCUC:         "olcuc",
	gossh.ONLCR:         "onlcr",
	gossh.OCRNL:         "ocrnl",
	gossh.ONOCR:         "onocr",
	gossh.ONLRET:        "onlret",
	gossh.CS7:           "cs7",
	gossh.CS8:           "cs8",
	gossh.PARENB:        "parenb",
	gossh.PARODD:        "parodd",
	gossh.TTY_OP_ISPEED: "tty_op_ispeed",
	gossh.TTY_OP_OSPEED: "tty_op_ospeed",
}
