//go:build unix

package eval

import (
	"errors"
	"os"
	"os/exec"
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

// Reference to redirection application. Can be overridden in tests.
var execApplyRedirs = defaultApplyExecRedirs

func execFn(fm *Frame, args ...any) error {
	var argstrings []string
	if len(args) == 0 {
		argstrings = []string{"elvish"}
	} else {
		argstrings = make([]string, len(args))
		for i, a := range args {
			argstrings[i] = vals.ToString(a)
		}
	}

	var err error
	argstrings[0], err = exec.LookPath(argstrings[0])
	if err != nil {
		return err
	}

	if err := execApplyRedirs(fm.ports); err != nil {
		return err
	}

	fm.Evaler.PreExit()
	decSHLVL()

	return syscallExec(argstrings[0], argstrings, os.Environ())
}

func defaultApplyExecRedirs(ports []*Port) error {
	saved := make(map[int]int)
	closeSaved := func() {
		for _, dup := range saved {
			_ = unix.Close(dup)
		}
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
		dup, err := unix.Dup(src)
		if err != nil {
			closeSaved()
			return err
		}
		saved[src] = dup
	}

	for fd, port := range ports {
		if port == nil {
			continue
		}
		if port.File == nil {
			_ = unix.Close(fd)
			continue
		}
		src := int(port.File.Fd())
		if dup, ok := saved[src]; ok {
			src = dup
		}
		if src == fd {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
				closeSaved()
				return err
			}
			continue
		}
		if err := unix.Dup2(src, fd); err != nil {
			closeSaved()
			return err
		}
	}

	closeSaved()
	return nil
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
