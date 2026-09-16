#!/bin/sh

# 本脚本是本地 Makefile 与 GoReleaser 共用的 UPX 压缩边界。
# 参数说明：
#   1. binary_path：已经完成 Go 编译、等待原地压缩的二进制文件路径。
#   2. target：目标操作系统或 GoReleaser target，例如 linux、linux_amd64、windows_amd64。
# 环境变量：
#   UPX：可选的 UPX 命令名或绝对路径，默认使用 PATH 中的 upx。
# 返回值说明：Linux/Windows 压缩并通过 UPX 完整性检测后返回 0；macOS 与未知平台
# 安全跳过并返回 0，避免使用 --force-macos 生成会被现代 macOS 直接终止的产物。
# 错误情况：参数缺失、二进制不存在、受支持平台缺少 UPX、压缩失败或完整性检测
# 失败时返回非零状态，使构建流水线不会继续归档损坏或未压缩的发布文件。

set -eu

if [ "$#" -ne 2 ]; then
	echo "用法: $0 <二进制路径> <目标平台>" >&2
	exit 2
fi

binary_path=$1
target=$2
upx_command=${UPX:-upx}

if [ ! -f "$binary_path" ]; then
	echo "错误：待压缩二进制不存在：$binary_path" >&2
	exit 1
fi

# UPX 5.x 仍明确禁用现代 macOS。--force-macos 虽能改写 Mach-O，但在 Apple Silicon
# 实测会被系统以退出状态 137 终止，因此这里必须按目标格式跳过，而不能按构建主机判断。
case "$target" in
	darwin | darwin_*)
		echo "跳过 UPX：目标 $target 为 macOS，UPX 上游当前不支持现代 Mach-O。"
		exit 0
		;;
	linux | linux_* | windows | windows_*)
		;;
	*)
		echo "跳过 UPX：目标 $target 不在已验证的平台范围内。"
		exit 0
		;;
esac

if ! command -v "$upx_command" >/dev/null 2>&1; then
	echo "错误：目标 $target 需要 UPX 压缩，但未找到命令 $upx_command。" >&2
	echo "请安装 UPX，或通过 UPX=/绝对路径/upx 指定命令。" >&2
	exit 1
fi

# -6 在压缩率和构建耗时之间取平衡；--best 对当前约 66 MiB 的单文件程序耗时明显
# 更长。压缩后立即执行 UPX 自检，确保归档阶段只接收结构完整的 ELF/PE 文件。
"$upx_command" -6 "$binary_path"
"$upx_command" -t "$binary_path"
