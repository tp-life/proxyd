//go:build windows

package remote

// 本文件是 Web Terminal 内嵌 shell 服务的 Windows 会话实现：PowerShell + ConPTY。
// 移植自 tailcat tailcat_ssh_windows.go（BSD-3-Clause）：
// Portions Copyright (c) Tailscale Inc & contributors.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	ssh "github.com/tailscale/gliderssh"
	"golang.org/x/sys/windows"
)

// newShellSessionCommand 返回运行 PowerShell 的未启动命令：
// rawCmd 为空时进入交互会话，否则用 -Command 执行。
//
// 参数说明：
//   - u: *user.User，shell 归属的本机用户（proxyd 进程用户）。
//   - rawCmd: string，客户端携带的可选远端命令。
//
// 返回值说明：*exec.Cmd，已设置 Args、Dir 与环境；Windows 进程整体继承父进程环境。
//
// 错误情况：无。
func newShellSessionCommand(u *user.User, rawCmd string) *exec.Cmd {
	clearInheritedCtrlCIgnore()
	shell := powerShellPath()
	var args []string
	if rawCmd == "" {
		args = []string{shell, "-NoLogo"}
	} else {
		args = []string{shell, "-Command", rawCmd}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = u.HomeDir
	// Windows 进程需要大部分父进程环境（SystemRoot、TEMP 等）才能正常工作，
	// 与 Unix 的最小环境不同，这里整体继承。
	cmd.Env = os.Environ()
	return cmd
}

// powerShellPath 返回 Windows PowerShell 可执行文件路径。
func powerShellPath() string {
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

var (
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole       = kernel32.NewProc("CreatePseudoConsole")
	procUpdateProcThreadAttribute = kernel32.NewProc("UpdateProcThreadAttribute")
	procSetConsoleCtrlHandler     = kernel32.NewProc("SetConsoleCtrlHandler")
)

// clearInheritedCtrlCIgnore 清除本进程的「忽略 Ctrl-C」控制台标志，该标志会被
// 会话子进程继承；继承它的子进程永远收不到控制台 Ctrl-C 事件，交互会话中的
// ^C 会被静默丢弃。SetConsoleCtrlHandler(NULL, FALSE) 只清标志，不动已装处理器。
var clearInheritedCtrlCIgnore = sync.OnceFunc(func() {
	procSetConsoleCtrlHandler.Call(0, 0)
})

// conPTYAvailable 报告当前 Windows 版本是否有伪控制台 API（Windows 10 1809+）。
// 不做检查直接调用 x/sys/windows 的伪控制台函数会在旧系统上 panic。
var conPTYAvailable = sync.OnceValue(func() bool {
	return procCreatePseudoConsole.Find() == nil
})

// runShellWithPTY 把命令挂到 Windows 伪控制台（ConPTY）上运行。cmd 只作为
// Args/Env/Dir 的载体：进程由 startConPTY 直接创建，不经 exec 包启动。
//
// 参数说明：
//   - sess: ssh.Session，已申请 PTY 的会话。
//   - cmd: *exec.Cmd，尚未启动的 shell 命令。
//   - ptyReq: ssh.Pty，客户端请求的首个终端尺寸。
//   - winCh: <-chan ssh.Window，后续 window-change 事件。
//
// 返回值说明：无；命令退出码经 sess.Exit 回传。
//
// 错误情况：无 ConPTY 时降级为管道模式；创建失败时向会话写错误并以退出码 1 结束。
func runShellWithPTY(sess ssh.Session, cmd *exec.Cmd, ptyReq ssh.Pty, winCh <-chan ssh.Window) {
	if !conPTYAvailable() {
		fmt.Fprintf(sess.Stderr(), "当前 Windows 版本无 ConPTY，降级为无 PTY 模式\r\n")
		runShellWithPipes(sess, cmd)
		return
	}

	if ptyReq.Term != "" {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
	}

	cp, err := startConPTY(cmd, ptyReq.Window.Width, ptyReq.Window.Height)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "conpty: %v\r\n", err)
		sess.Exit(1)
		return
	}
	defer cp.Close()

	go io.Copy(cp.inWrite, sess) // stdin

	// 窗口尺寸协程在会话关闭、winCh 关闭前一直运行，可能晚于本函数返回；
	// 伪控制台关闭后 Resize 自动变为 no-op。
	go func() {
		for win := range winCh {
			_ = cp.Resize(win.Width, win.Height)
		}
	}()

	// 输出排空协程必须先于下面的 CloseConsole 运行：关闭伪控制台可能阻塞，
	// 直到其未读输出被消费。
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		_, _ = io.Copy(sess, cp.outRead)
	}()

	code, err := cp.WaitExitCode()
	// 关闭伪控制台让 conhost 释放输出管道句柄，上面的读取协程才能看到 EOF。
	cp.CloseConsole()
	<-outDone

	if err != nil {
		fmt.Fprintf(sess.Stderr(), "wait: %v\r\n", err)
		sess.Exit(1)
		return
	}
	sess.Exit(code)
}

// conPTY 是挂接了单个进程的 Windows 伪控制台。
type conPTY struct {
	inWrite *os.File // 被挂接进程从这里读输入
	outRead *os.File // 被挂接进程的输出从这里读
	proc    windows.Handle

	mu     sync.Mutex // 保护 hpc 与 closed：Resize 与 CloseConsole 存在竞争
	hpc    windows.Handle
	closed bool
}

// startConPTY 创建指定尺寸的伪控制台并把进程挂接进去，程序参数、环境与
// 工作目录取自 cmd。伪控制台挂接要求直接创建进程，cmd 不经 exec 包启动。
func startConPTY(cmd *exec.Cmd, width, height int) (*conPTY, error) {
	// CreatePseudoConsole 拒绝零尺寸，而没有真实终端的 SSH 客户端可能发来零值。
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		inRead.Close()
		inWrite.Close()
		return nil, err
	}

	var hpc windows.Handle
	err = windows.CreatePseudoConsole(
		windows.Coord{X: int16(width), Y: int16(height)},
		windows.Handle(inRead.Fd()),
		windows.Handle(outWrite.Fd()),
		0, &hpc)
	// 伪控制台会复制所需句柄，无论成败我们手里的两个管道端都在这里关闭。
	inRead.Close()
	outWrite.Close()
	if err != nil {
		inWrite.Close()
		outRead.Close()
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	cleanup := func() {
		windows.ClosePseudoConsole(hpc)
		inWrite.Close()
		outRead.Close()
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		cleanup()
		return nil, err
	}
	defer attrs.Delete()
	if err := updatePseudoConsoleAttr(attrs.List(), hpc); err != nil {
		cleanup()
		return nil, err
	}

	cmdLine, err := windows.UTF16FromString(windows.ComposeCommandLine(cmd.Args))
	if err != nil {
		cleanup()
		return nil, err
	}
	var dir *uint16
	if cmd.Dir != "" {
		dir, err = windows.UTF16PtrFromString(cmd.Dir)
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	env, err := envBlock(cmd.Env)
	if err != nil {
		cleanup()
		return nil, err
	}

	si := &windows.StartupInfoEx{
		// STARTF_USESTDHANDLES 配合 NULL 标准句柄，避免子进程继承本进程的
		// 控制台句柄；否则子进程输出会绕过伪控制台落到父进程控制台。
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attrs.List(),
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(nil, &cmdLine[0], nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT,
		env, dir, &si.StartupInfo, &pi)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}
	windows.CloseHandle(pi.Thread)

	return &conPTY{inWrite: inWrite, outRead: outRead, proc: pi.Process, hpc: hpc}, nil
}

// updatePseudoConsoleAttr 在属性列表上设置伪控制台属性。直接调用
// UpdateProcThreadAttribute 而不走 x/sys/windows 封装：该属性的值是伪控制台
// 句柄本身而非指针，句柄转 unsafe.Pointer 会触发 go vet 的 unsafeptr 检查。
func updatePseudoConsoleAttr(list *windows.ProcThreadAttributeList, hpc windows.Handle) error {
	r1, _, e1 := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(list)),
		0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(hpc),
		unsafe.Sizeof(hpc),
		0, 0)
	if r1 == 0 {
		return e1
	}
	return nil
}

// Resize 调整伪控制台尺寸；CloseConsole 之后为 no-op。
func (c *conPTY) Resize(width, height int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	return windows.ResizePseudoConsole(c.hpc, windows.Coord{X: int16(width), Y: int16(height)})
}

// WaitExitCode 等待被挂接进程退出并返回退出码。
func (c *conPTY) WaitExitCode() (int, error) {
	if _, err := windows.WaitForSingleObject(c.proc, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.proc, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}

// CloseConsole 只关闭伪控制台本身，不动管道与进程句柄。
func (c *conPTY) CloseConsole() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		windows.ClosePseudoConsole(c.hpc)
		c.closed = true
	}
}

// Close 释放全部资源：伪控制台（若未关）、两个管道端与进程句柄。
func (c *conPTY) Close() {
	c.CloseConsole()
	c.inWrite.Close()
	c.outRead.Close()
	windows.CloseHandle(c.proc)
}

// envBlock 把 key=value 列表转换为 CreateProcess 期望的 UTF-16、NUL 分隔、
// 双 NUL 结尾的环境块。Windows 环境键大小写不敏感且块内不允许重复，后出现的
// 条目（如追加在 os.Environ 之后的客户端 TERM/LANG）覆盖先出现的。
func envBlock(env []string) (*uint16, error) {
	seen := make(map[string]int) // 大写键 → 列表下标
	var list []string
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		uk := strings.ToUpper(k)
		if i, dup := seen[uk]; dup {
			list[i] = kv
		} else {
			seen[uk] = len(list)
			list = append(list, kv)
		}
	}
	var block []uint16
	for _, kv := range list {
		u, err := windows.UTF16FromString(kv) // NUL 结尾
		if err != nil {
			return nil, err
		}
		block = append(block, u...)
	}
	block = append(block, 0)
	return &block[0], nil
}
