//go:build linux

package supervisor

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// siginfo mirrors the parts of the kernel's siginfo_t that waitid fills for
// a child event. On 64-bit Linux the union starts at offset 16 (after three
// int32 fields and padding); for SIGCHLD it holds si_pid, si_uid, si_status.
type siginfo struct {
	Signo  int32
	Errno  int32
	Code   int32
	_      int32
	Pid    int32
	Uid    uint32
	Status int32
	_      [128 - 28]byte
}

// StartReaper makes the process a child subreaper and reaps orphaned
// descendants on SIGCHLD. Children the Manager waits on itself are left
// alone: the reaper peeks with WNOWAIT and only collects pids that are not
// managed. isManaged reports whether a pid belongs to a tracked child.
func StartReaper(logger *slog.Logger, isManaged func(pid int) bool) error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		for range ch {
			reapOrphans(logger, isManaged)
		}
	}()
	return nil
}

func waitidPeek() (pid int, err error) {
	var info siginfo
	for {
		_, _, errno := unix.Syscall6(unix.SYS_WAITID, unix.P_ALL, 0,
			uintptr(unsafe.Pointer(&info)), unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, 0, 0)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return 0, errno
		}
		return int(info.Pid), nil
	}
}

func reapOrphans(logger *slog.Logger, isManaged func(pid int) bool) {
	for {
		pid, err := waitidPeek()
		if err != nil {
			// ECHILD: no children at all.
			return
		}
		if pid == 0 {
			// Children exist but none has exited.
			return
		}
		if isManaged(pid) {
			// Leave it for the Manager's Wait; its own SIGCHLD already fired.
			return
		}
		var ws unix.WaitStatus
		if _, err := unix.Wait4(pid, &ws, 0, nil); err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		logger.Debug("reaped orphan", "pid", pid, "status", ws)
	}
}
