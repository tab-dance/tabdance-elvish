//go:build unix

package eval

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"src.elv.sh/pkg/env"
	"src.elv.sh/pkg/parse"
	"src.elv.sh/pkg/testutil"
)

func TestExec_Argv0Argv(t *testing.T) {
	dir := testutil.InTempDir(t)
	testutil.ApplyDir(testutil.Dir{
		"bin": testutil.Dir{
			"elvish": testutil.File{Perm: 0755},
			"cat":    testutil.File{Perm: 0755},
		},
	})

	testutil.Setenv(t, "PATH", dir+"/bin")
	testutil.Setenv(t, env.SHLVL, "1")

	var tests = []struct {
		name      string
		code      string
		wantArgv0 string
		wantArgv  []string
		wantError bool
	}{
		{
			name:      "absolute path command",
			code:      "exec /bin/sh foo bar",
			wantArgv0: "/bin/sh",
			wantArgv:  []string{"/bin/sh", "foo", "bar"},
		},
		{
			name:      "relative path command",
			code:      "exec cat foo bar",
			wantArgv0: dir + "/bin/cat",
			wantArgv:  []string{dir + "/bin/cat", "foo", "bar"},
		},
		{
			name:      "no command",
			code:      "exec",
			wantArgv0: dir + "/bin/elvish",
			wantArgv:  []string{dir + "/bin/elvish"},
		},
		{
			name:      "bad command",
			code:      "exec bad",
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Helper()
			var (
				gotArgv0 string
				gotArgv  []string
			)
			syscallExec = func(argv0 string, argv []string, envv []string) error {
				gotArgv0 = argv0
				gotArgv = argv
				return nil
			}
			execApplyRedirs = func([]*Port) (func(), error) { return nil, nil }
			defer func() {
				syscallExec = syscall.Exec
				execApplyRedirs = defaultApplyExecRedirs
			}()

			ev := NewEvaler()
			err := ev.Eval(parse.Source{Name: "[test]", Code: test.code}, EvalCfg{})

			if gotArgv0 != test.wantArgv0 {
				t.Errorf("got argv0 %q, want %q", gotArgv0, test.wantArgv0)
			}
			if !reflect.DeepEqual(gotArgv, test.wantArgv) {
				t.Errorf("got argv %q, want %q", gotArgv, test.wantArgv)
			}
			hasError := err != nil
			if hasError != test.wantError {
				t.Errorf("has error %v, want %v", hasError, test.wantError)
			}
		})
	}
}

func TestDecSHLVL(t *testing.T) {
	// Valid integers are decremented, regardless of sign
	testDecSHLVL(t, "-2", "-3")
	testDecSHLVL(t, "-1", "-2")
	testDecSHLVL(t, "0", "-1")
	testDecSHLVL(t, "1", "0")
	testDecSHLVL(t, "2", "1")
	testDecSHLVL(t, "3", "2")

	// Non-integers are kept unchanged
	testDecSHLVL(t, "", "")
	testDecSHLVL(t, "x", "x")
}

func testDecSHLVL(t *testing.T, oldValue, newValue string) {
	t.Helper()
	testutil.Setenv(t, env.SHLVL, oldValue)

	decSHLVL()
	if gotValue := os.Getenv(env.SHLVL); gotValue != newValue {
		t.Errorf("got new value %q, want %q", gotValue, newValue)
	}
}

func TestExec_AppliesRedirections(t *testing.T) {
	if os.Getenv("ELVISH_EXEC_REDIR_HELPER") == "1" {
		out := os.Getenv("ELVISH_EXEC_REDIR_OUT")
		code := "exec /bin/sh -c \"echo hi\" > " + parse.Quote(out)
		ev := NewEvaler()
		_ = ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{})
		return
	}

	dir := testutil.InTempDir(t)
	outFile := filepath.Join(dir, "out.txt")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, "-test.run=^TestExec_AppliesRedirections$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"ELVISH_EXEC_REDIR_HELPER=1",
		"ELVISH_EXEC_REDIR_OUT="+outFile,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading redirected output: %v\nhelper output:\n%s", err, output)
	}
	if got := string(data); got != "hi\n" {
		t.Fatalf("redirected output = %q, want %q\nhelper output:\n%s", got, "hi\n", output)
	}
}

func TestExec_AppliesFdCloseRedirection(t *testing.T) {
	if os.Getenv("ELVISH_EXEC_FD_CLOSE_HELPER") == "1" {
		out := os.Getenv("ELVISH_EXEC_FD_CLOSE_OUT")
		code := "exec /bin/sh -c 'if echo hi >&2; then exit 1; else echo ok; fi' > " +
			parse.Quote(out) + " 2>&-"
		ev := NewEvaler()
		_ = ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{})
		return
	}

	dir := testutil.InTempDir(t)
	outFile := filepath.Join(dir, "out.txt")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, "-test.run=^TestExec_AppliesFdCloseRedirection$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"ELVISH_EXEC_FD_CLOSE_HELPER=1",
		"ELVISH_EXEC_FD_CLOSE_OUT="+outFile,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading redirected output: %v\nhelper output:\n%s", err, output)
	}
	if got := string(data); got != "ok\n" {
		t.Fatalf("redirected output = %q, want %q\nhelper output:\n%s", got, "ok\n", output)
	}
}

func TestExec_RestoresStateOnExecFailure(t *testing.T) {
	dir := testutil.InTempDir(t)
	target, err := os.Create(filepath.Join(dir, "target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	dest, err := os.Create(filepath.Join(dir, "dest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()

	targetFD := int(target.Fd())
	ports := make([]*Port, targetFD+1)
	ports[targetFD] = &Port{File: dest}

	testutil.Setenv(t, env.SHLVL, "2")

	oldExec := syscallExec
	syscallExec = func(argv0 string, argv []string, envv []string) error {
		return errors.New("exec failed")
	}
	defer func() { syscallExec = oldExec }()

	fm := &Frame{Evaler: NewEvaler(), ports: ports}
	err = execFn(fm, "/bin/sh", "-c", "true")
	if err == nil || err.Error() != "exec failed" {
		t.Fatalf("execFn error = %v, want exec failed", err)
	}

	if got := os.Getenv(env.SHLVL); got != "2" {
		t.Fatalf("SHLVL = %q, want %q", got, "2")
	}

	if _, err := target.WriteString("ok"); err != nil {
		t.Fatalf("writing to original target fd: %v", err)
	}
	data, err := os.ReadFile(target.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "ok" {
		t.Fatalf("original target content = %q, want %q", got, "ok")
	}
	destData, err := os.ReadFile(dest.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(destData) != 0 {
		t.Fatalf("dest should be untouched by rollback target, got %q", string(destData))
	}
}
