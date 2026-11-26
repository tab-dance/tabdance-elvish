//go:build unix

package eval

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
	"src.elv.sh/pkg/env"
	"src.elv.sh/pkg/eval/errs"
	"src.elv.sh/pkg/eval/vals"
	"src.elv.sh/pkg/sys/eunix"
)

// ErrNotInSameProcessGroup is thrown when the process IDs passed to fg are not
// in the same process group.
var ErrNotInSameProcessGroup = errors.New("not in the same process group")

// Reference to syscall.Exec. Can be overridden in tests.
var syscallExec = syscall.Exec

var (
	unixClose    = unix.Close
	unixDup      = unix.Dup
	unixDup2     = unix.Dup2
	unixFcntlInt = unix.FcntlInt
)

// Reference to redirection application. Can be overridden in tests.
var execApplyRedirs = defaultApplyExecRedirs

func execFn(fm *Frame, args ...any) error {
	if len(args) == 0 {
		if fm.background {
			return fmt.Errorf("exec with no command does not support background execution")
		}
		fm.persistExecRedirs = true
		return nil
	}

	argstrings := make([]string, len(args))
	for i, a := range args {
		argstrings[i] = vals.ToString(a)
	}

	var err error
	argstrings[0], err = exec.LookPath(argstrings[0])
	if err != nil {
		return err
	}

	restoreRedirs, err := execApplyRedirs(fm.ports)
	if err != nil {
		return err
	}

	// PreExit hooks are one-shot by contract. A successful exec never returns,
	// so hooks must run before syscall.Exec. If exec fails, hooks may have
	// already run; fd and SHLVL state are rolled back below.
	fm.Evaler.PreExit()
	oldSHLVL, hadSHLVL := os.LookupEnv(env.SHLVL)
	decSHLVL()

	err = syscallExec(argstrings[0], argstrings, os.Environ())
	if restoreRedirs != nil {
		restoreRedirs()
	}
	if hadSHLVL {
		_ = os.Setenv(env.SHLVL, oldSHLVL)
	} else {
		_ = os.Unsetenv(env.SHLVL)
	}
	return err
}

type execFDState struct {
	fd      int
	savedFD int
	flags   int
	closed  bool
}

func defaultApplyExecRedirs(ports []*Port) (func(), error) {
	saved := make(map[int]int)
	closeSaved := func() {
		for _, dup := range saved {
			_ = unixClose(dup)
		}
	}

	targets := make(map[int]execFDState)
	saveTarget := func(fd int) error {
		if _, ok := targets[fd]; ok {
			return nil
		}
		flags, err := unixFcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			if errors.Is(err, unix.EBADF) {
				targets[fd] = execFDState{fd: fd, savedFD: -1, closed: true}
				return nil
			}
			return err
		}
		dup, err := unixDup(fd)
		if err != nil {
			if errors.Is(err, unix.EBADF) {
				targets[fd] = execFDState{fd: fd, savedFD: -1, closed: true}
				return nil
			}
			return err
		}
		if _, err := unixFcntlInt(uintptr(dup), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
			_ = unixClose(dup)
			return err
		}
		targets[fd] = execFDState{fd: fd, savedFD: dup, flags: flags}
		return nil
	}
	restoreTargets := func() {
		fds := make([]int, 0, len(targets))
		for fd := range targets {
			fds = append(fds, fd)
		}
		sort.Ints(fds)
		for i := len(fds) - 1; i >= 0; i-- {
			state := targets[fds[i]]
			if state.closed {
				_ = unixClose(state.fd)
				continue
			}
			if state.savedFD >= 0 {
				_ = unixDup2(state.savedFD, state.fd)
				_, _ = unixFcntlInt(uintptr(state.fd), unix.F_SETFD, state.flags)
			}
		}
		for _, state := range targets {
			if !state.closed && state.savedFD >= 0 {
				_ = unixClose(state.savedFD)
			}
		}
	}
	rollback := func() {
		restoreTargets()
		closeSaved()
	}

	for fd, port := range ports {
		if port == nil || port.File == nil {
			continue
		}
		src := int(port.File.Fd())
		if src == fd || src < 0 || src >= len(ports) {
			continue
		}
		if _, ok := saved[src]; ok {
			continue
		}
		dup, err := unixDup(src)
		if err != nil {
			rollback()
			return nil, err
		}
		saved[src] = dup
	}

	for fd, port := range ports {
		if port == nil {
			continue
		}
		if err := saveTarget(fd); err != nil {
			rollback()
			return nil, err
		}
		if port.File == nil {
			if err := unixClose(fd); err != nil && !errors.Is(err, unix.EBADF) {
				rollback()
				return nil, err
			}
			continue
		}
		src := int(port.File.Fd())
		if dup, ok := saved[src]; ok {
			src = dup
		}
		if src == fd {
			if _, err := unixFcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
				rollback()
				return nil, err
			}
			continue
		}
		if err := unixDup2(src, fd); err != nil {
			rollback()
			return nil, err
		}
	}

	closeSaved()
	return restoreTargets, nil
}

// Decrements $E:SHLVL. Called from execFn to ensure that $E:SHLVL remains the
// same in the new command.
func decSHLVL() {
	i, err := strconv.Atoi(os.Getenv(env.SHLVL))
	if err != nil {
		return
	}
	os.Setenv(env.SHLVL, strconv.Itoa(i-1))
}

func fg(pids ...int) error {
	if len(pids) == 0 {
		return errs.ArityMismatch{What: "arguments", ValidLow: 1, ValidHigh: -1, Actual: len(pids)}
	}
	var thepgid int
	for i, pid := range pids {
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			return err
		}
		if i == 0 {
			thepgid = pgid
		} else if pgid != thepgid {
			return ErrNotInSameProcessGroup
		}
	}

	err := eunix.Tcsetpgrp(0, thepgid)
	if err != nil {
		return err
	}

	errors := make([]Exception, len(pids))

	for i, pid := range pids {
		err := syscall.Kill(pid, syscall.SIGCONT)
		if err != nil {
			errors[i] = &exception{err, nil}
		}
	}

	for i, pid := range pids {
		if errors[i] != nil {
			continue
		}
		var ws syscall.WaitStatus
		_, err = syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil)
		if err != nil {
			errors[i] = &exception{err, nil}
		} else {
			// TODO find command name
			errors[i] = &exception{NewExternalCmdExit(
				"[pid "+strconv.Itoa(pid)+"]", ws, pid), nil}
		}
	}

	return MakePipelineError(errors)
}
