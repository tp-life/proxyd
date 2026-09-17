//go:build darwin

package tunhelper

// macOS root 特权 helper 服务端：launchd 系统域常驻，
// 监听 /var/run/com.proxyd.tun.sock（0600 + LOCAL_PEERCRED 属主校验），
// 只执行白名单指令集（ping、tun.create）。utun 创建逻辑参考
// sing-tun v0.4.22 的 tun_darwin.go create/addRoute，用 x/sys/unix 与
// x/net/route 重新实现（不 import sing-tun）。
// helper 不读业务配置、不连网；日志只含操作类别，不含地址/路由明细。

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

const (
	// helperLabel 是 helper 的 launchd 服务标识。
	helperLabel = "com.proxyd.tun-helper"
	// helperPlistPath 是 helper 的 LaunchDaemon plist 路径。
	helperPlistPath = "/Library/LaunchDaemons/com.proxyd.tun-helper.plist"
	// helperOwnerUIDEnv 是安装链路经 plist 环境变量记录的属主 UID。
	helperOwnerUIDEnv = "PROXYD_TUN_OWNER_UID"
)

// utun 控制与 ioctl 常量（x/sys/unix 未导出，值同 sing-tun 与 XNU 头文件）。
const (
	utunControlName = "com.apple.net.utun_control"
	// sysprotoControl 是 bsd/sys/socket.h 的 SYSPROTO_CONTROL。
	sysprotoControl = 2
	// utunOptIfName 是 bsd/net/if_utun.h 的 UTUN_OPT_IFNAME。
	utunOptIfName = 2
	// siocAddAddrIn6 是 netinet6/in6_var.h 的 SIOCAIFADDR_IN6。
	siocAddAddrIn6 = 2155899162
	// in6IFFNODAD / in6IFFSecured 是 netinet6/in6_var.h 的地址标志。
	in6IFFNODAD   = 0x0020
	in6IFFSecured = 0x0400
	// nd6InfiniteLifetime 是 netinet6/nd6.h 的 ND6_INFINITE_LIFETIME。
	nd6InfiniteLifetime = 0xFFFFFFFF
)

// ifAliasReq 是 SIOCAIFADDR 的请求体（bsd/net/if.h ifaliasreq 的 v4 形态）。
type ifAliasReq struct {
	Name    [unix.IFNAMSIZ]byte
	Addr    unix.RawSockaddrInet4
	Dstaddr unix.RawSockaddrInet4
	Mask    unix.RawSockaddrInet4
}

// ifAliasReq6 是 SIOCAIFADDR_IN6 的请求体（netinet6/in6_var.h in6_aliasreq）。
type ifAliasReq6 struct {
	Name     [16]byte
	Addr     unix.RawSockaddrInet6
	Dstaddr  unix.RawSockaddrInet6
	Mask     unix.RawSockaddrInet6
	Flags    uint32
	Lifetime addrLifetime6
}

// addrLifetime6 是 in6_aliasreq 的地址生命周期（inet6 用 double 字段对齐内核 ABI）。
type addrLifetime6 struct {
	Expire    float64
	Preferred float64
	Vltime    uint32
	Pltime    uint32
}

// mask4 由前缀长度生成 IPv4 掩码字节。
func mask4(bits int) [4]byte {
	var mask [4]byte
	for i := 0; i < bits && i < 32; i++ {
		mask[i/8] |= 1 << (7 - uint(i%8))
	}
	return mask
}

// mask16 由前缀长度生成 IPv6 掩码字节。
func mask16(bits int) [16]byte {
	var mask [16]byte
	for i := 0; i < bits && i < 128; i++ {
		mask[i/8] |= 1 << (7 - uint(i%8))
	}
	return mask
}

// darwinHelperOps 是 helperOps 的 macOS 实现；syscall/ioctl/route 发送全部封装为
// 可注入字段（仿 gateway 的 run/writeFile 注入），便于单测 fake 调用序列。
type darwinHelperOps struct {
	logf func(format string, args ...any)

	socket       func(domain, typ, proto int) (int, error)
	closeFD      func(fd int)
	ioctlCtlInfo func(fd int, name string) (uint32, error)
	connectCtl   func(fd int, ctlID uint32, unit uint32) error
	utunIfName   func(fd int) (string, error)
	setMTU       func(socketFd int, ifName string, mtu int) error
	addAddr4     func(socketFd int, ifName string, prefix netip.Prefix) error
	addAddr6     func(socketFd int, ifName string, prefix netip.Prefix) error
	routeOp      func(rtmType int, destination netip.Prefix, gateway netip.Addr) error
}

// newDarwinHelperOps 创建生产实现的执行能力。
func newDarwinHelperOps(logf func(format string, args ...any)) *darwinHelperOps {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &darwinHelperOps{
		logf:    logf,
		socket:  unix.Socket,
		closeFD: func(fd int) { _ = unix.Close(fd) },
		ioctlCtlInfo: func(fd int, name string) (uint32, error) {
			ctlInfo := &unix.CtlInfo{}
			copy(ctlInfo.Name[:], name)
			if err := unix.IoctlCtlInfo(fd, ctlInfo); err != nil {
				return 0, os.NewSyscallError("IoctlCtlInfo", err)
			}
			return ctlInfo.Id, nil
		},
		connectCtl: func(fd int, ctlID uint32, unit uint32) error {
			return os.NewSyscallError("Connect", unix.Connect(fd, &unix.SockaddrCtl{ID: ctlID, Unit: unit}))
		},
		utunIfName: func(fd int) (string, error) {
			return unix.GetsockoptString(fd, sysprotoControl, utunOptIfName)
		},
		setMTU: func(socketFd int, ifName string, mtu int) error {
			var ifr unix.IfreqMTU
			copy(ifr.Name[:], ifName)
			ifr.MTU = int32(mtu)
			return os.NewSyscallError("IoctlSetIfreqMTU", unix.IoctlSetIfreqMTU(socketFd, &ifr))
		},
		addAddr4: func(socketFd int, ifName string, prefix netip.Prefix) error {
			// 点对点接口：Dstaddr 设为自身（按 sing-tun 做法）。
			addr := unix.RawSockaddrInet4{
				Len:    unix.SizeofSockaddrInet4,
				Family: unix.AF_INET,
				Addr:   prefix.Addr().As4(),
			}
			ifReq := ifAliasReq{
				Addr:    addr,
				Dstaddr: addr,
				Mask: unix.RawSockaddrInet4{
					Len:    unix.SizeofSockaddrInet4,
					Family: unix.AF_INET,
					Addr:   mask4(prefix.Bits()),
				},
			}
			copy(ifReq.Name[:], ifName)
			if _, _, errno := unix.Syscall(
				unix.SYS_IOCTL,
				uintptr(socketFd),
				uintptr(unix.SIOCAIFADDR),
				uintptr(unsafe.Pointer(&ifReq)),
			); errno != 0 {
				return os.NewSyscallError("SIOCAIFADDR", errno)
			}
			return nil
		},
		addAddr6: func(socketFd int, ifName string, prefix netip.Prefix) error {
			ifReq := ifAliasReq6{
				Addr: unix.RawSockaddrInet6{
					Len:    unix.SizeofSockaddrInet6,
					Family: unix.AF_INET6,
					Addr:   prefix.Addr().As16(),
				},
				Mask: unix.RawSockaddrInet6{
					Len:    unix.SizeofSockaddrInet6,
					Family: unix.AF_INET6,
					Addr:   mask16(prefix.Bits()),
				},
				Flags: in6IFFNODAD | in6IFFSecured,
				Lifetime: addrLifetime6{
					Vltime: nd6InfiniteLifetime,
					Pltime: nd6InfiniteLifetime,
				},
			}
			if prefix.Bits() == 128 {
				ifReq.Dstaddr = unix.RawSockaddrInet6{
					Len:    unix.SizeofSockaddrInet6,
					Family: unix.AF_INET6,
					Addr:   prefix.Addr().Next().As16(),
				}
			}
			copy(ifReq.Name[:], ifName)
			if _, _, errno := unix.Syscall(
				unix.SYS_IOCTL,
				uintptr(socketFd),
				uintptr(siocAddAddrIn6),
				uintptr(unsafe.Pointer(&ifReq)),
			); errno != 0 {
				return os.NewSyscallError("SIOCAIFADDR_IN6", errno)
			}
			return nil
		},
		routeOp: func(rtmType int, destination netip.Prefix, gateway netip.Addr) error {
			routeMessage := route.RouteMessage{
				Type:    rtmType,
				Flags:   unix.RTF_UP | unix.RTF_STATIC | unix.RTF_GATEWAY,
				Version: unix.RTM_VERSION,
				Seq:     1,
				Addrs: []route.Addr{
					syscall.RTAX_DST:     &route.Inet4Addr{IP: destination.Addr().As4()},
					syscall.RTAX_NETMASK: &route.Inet4Addr{IP: mask4(destination.Bits())},
					syscall.RTAX_GATEWAY: &route.Inet4Addr{IP: gateway.As4()},
				},
			}
			request, err := routeMessage.Marshal()
			if err != nil {
				return err
			}
			routeFd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, 0)
			if err != nil {
				return err
			}
			defer unix.Close(routeFd)
			_, err = unix.Write(routeFd, request)
			return err
		},
	}
}

// Ping 实现握手探活：无特权操作，恒成功。
func (o *darwinHelperOps) Ping() error { return nil }

// useSocket 开临时 socket 执行一段操作后关闭。
func (o *darwinHelperOps) useSocket(domain, typ, proto int, block func(socketFd int) error) error {
	socketFd, err := o.socket(domain, typ, proto)
	if err != nil {
		return err
	}
	defer o.closeFD(socketFd)
	return block(socketFd)
}

// CreateTUN 创建 utun 设备并配置 MTU/地址/路由（抄 sing-tun tun_darwin.go create/addRoute）。
//
// 参数：
//   - params: CreateTUNParams，客户端提交的创建参数；ops 内重新校验，不信任客户端。
//
// 返回值：
//   - fd int：utun 设备描述符；调用方负责经 SCM_RIGHTS 发送后关闭 helper 侧副本。
//   - ifName string：内核分配的接口名（如 "utun5"）。
//   - err error：任一步失败时返回中文错误。
//
// 错误情况：任一步失败即关闭 fd，已添加的路由按逆序 RTM_DEL best-effort 回滚。
func (o *darwinHelperOps) CreateTUN(params CreateTUNParams) (int, string, error) {
	cfg, err := params.parse()
	if err != nil {
		return -1, "", err
	}
	fd, err := o.socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, sysprotoControl)
	if err != nil {
		return -1, "", fmt.Errorf("创建 utun 控制 socket 失败: %w", err)
	}
	fail := func(err error) (int, string, error) {
		o.closeFD(fd)
		return -1, "", err
	}
	ctlID, err := o.ioctlCtlInfo(fd, utunControlName)
	if err != nil {
		return fail(fmt.Errorf("查询 utun 控制 ID 失败: %w", err))
	}
	// Unit=0：内核分配接口序号。
	if err := o.connectCtl(fd, ctlID, 0); err != nil {
		return fail(fmt.Errorf("连接 utun 控制失败: %w", err))
	}
	ifName, err := o.utunIfName(fd)
	if err != nil {
		return fail(fmt.Errorf("读取 utun 接口名失败: %w", err))
	}
	err = o.useSocket(unix.AF_INET, unix.SOCK_DGRAM, 0, func(socketFd int) error {
		if err := o.setMTU(socketFd, ifName, cfg.mtu); err != nil {
			return fmt.Errorf("设置 MTU 失败: %w", err)
		}
		if err := o.addAddr4(socketFd, ifName, cfg.inet4); err != nil {
			return fmt.Errorf("配置 IPv4 地址失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	if cfg.inet6.IsValid() {
		err = o.useSocket(unix.AF_INET6, unix.SOCK_DGRAM, 0, func(socketFd int) error {
			if err := o.addAddr6(socketFd, ifName, cfg.inet6); err != nil {
				return fmt.Errorf("配置 IPv6 地址失败: %w", err)
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
	}
	if cfg.autoRoute {
		gateway := cfg.inet4.Addr() // 点对点接口，下一跳即接口自身地址
		var added []netip.Prefix
		for _, routeRange := range cfg.routes {
			if err := o.routeOp(unix.RTM_ADD, routeRange, gateway); err != nil {
				for i := len(added) - 1; i >= 0; i-- {
					if delErr := o.routeOp(unix.RTM_DELETE, added[i], gateway); delErr != nil {
						o.logf("[tun-helper] 回滚路由 %s 失败: %v", added[i], delErr)
					}
				}
				return fail(fmt.Errorf("添加路由 %s 失败: %w", routeRange, err))
			}
			added = append(added, routeRange)
		}
	}
	o.logf("[tun-helper] utun 已创建：%s（mtu=%d，路由 %d 条）", ifName, cfg.mtu, len(cfg.routes))
	return fd, ifName, nil
}

// helperOwnerUID 读取安装链路记录的属主 UID（plist 环境变量）。
//
// 参数：无。
//
// 返回值：
//   - int：允许连接的非 root 属主 UID。
//   - error：环境变量缺失或非法时返回；helper 拒绝在无属主记录的情况下运行。
//
// 错误情况：属主缺失意味着 helper 被手工/异常启动，直接拒绝服务。
func helperOwnerUID() (int, error) {
	raw := strings.TrimSpace(os.Getenv(helperOwnerUIDEnv))
	if raw == "" {
		return -1, fmt.Errorf("缺少属主记录（环境变量 %s）；请通过 proxyd tun helper install 安装", helperOwnerUIDEnv)
	}
	uid, err := strconv.Atoi(raw)
	if err != nil || uid < 0 {
		return -1, fmt.Errorf("属主 UID %q 非法", raw)
	}
	return uid, nil
}

// listenHelperSocket 创建 helper 监听 socket：清掉残留文件、0600、chown 给属主。
func listenHelperSocket(ownerUID int) (net.Listener, error) {
	_ = os.Remove(helperSocketPath)
	ln, err := net.Listen("unix", helperSocketPath)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", helperSocketPath, err)
	}
	if err := os.Chmod(helperSocketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("设置 socket 权限失败: %w", err)
	}
	if err := os.Chown(helperSocketPath, ownerUID, 0); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("设置 socket 属主失败: %w", err)
	}
	return ln, nil
}

// peerUID 读取 unix socket 对端进程的 UID（LOCAL_PEERCRED）。
//
// 参数：
//   - conn: *net.UnixConn，已接受的连接。
//
// 返回值：
//   - uint32：对端进程有效 UID。
//   - error：内核凭证不可读时返回；调用方必须按拒绝处理。
//
// 错误情况：无法读取对端身份时绝不放行。
func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

// authorizedHelperUID 判定对端 UID 是否允许下发指令：root 或安装时记录的属主。
func authorizedHelperUID(uid uint32, ownerUID int) bool {
	return uid == 0 || uid == uint32(ownerUID)
}

// RunHelperServer 以 root 运行 helper 服务端（由 launchd 系统域托管，
// `proxyd tun-helper` 内部子命令入口，不应手工调用）。
//
// 参数：
//   - ctx: context.Context，取消后停止监听。
//   - logf: func(string, ...any)，操作日志（不含地址/路由明细等敏感信息）。
//
// 返回值：
//   - error：非 root、缺属主记录或监听失败时返回；正常取消返回 nil。
//
// 错误情况：无看门狗（与 gateway helper 的差异）：helper 发出 fd 即关闭自己的副本，
// 主进程退出时内核随最后一个 fd 关闭自动销毁 utun 与相关路由，无需 helper 兜底清理。
func RunHelperServer(ctx context.Context, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("tun helper 必须以 root 运行（经 launchd 系统域托管）")
	}
	ownerUID, err := helperOwnerUID()
	if err != nil {
		return err
	}
	ops := newDarwinHelperOps(logf)

	ln, err := listenHelperSocket(ownerUID)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	logf("[tun-helper] 已监听 %s（属主 UID %d）", helperSocketPath, ownerUID)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("helper 接受连接失败: %w", err)
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			_ = conn.Close()
			continue
		}
		uid, err := peerUID(unixConn)
		if err != nil || !authorizedHelperUID(uid, ownerUID) {
			logf("[tun-helper] 拒绝未授权连接（uid 校验失败）")
			_ = conn.Close()
			continue
		}
		go func() {
			defer conn.Close()
			serveHelperConn(conn, ops)
		}()
	}
}
