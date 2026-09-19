// 动态符号 trampoline：Go 无法直接调用 libSystem 中的 C 函数，只能取本 trampoline 的
// 地址再交给 runtime.syscall_syscall6 间接调用。写法与 x/sys/unix 生成的代码一致。
#include "textflag.h"

TEXT libc_proc_pid_rusage_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_proc_pid_rusage(SB)
GLOBL	·libc_proc_pid_rusage_trampoline_addr(SB), RODATA, $8
DATA	·libc_proc_pid_rusage_trampoline_addr(SB)/8, $libc_proc_pid_rusage_trampoline<>(SB)
