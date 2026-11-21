package eval_test

import (
	"testing"

	. "src.elv.sh/pkg/eval"
	"src.elv.sh/pkg/must"
	"src.elv.sh/pkg/parse"
)

func TestExitRunsDefersAndPropagates(t *testing.T) {
	ev := NewEvaler()
	port1, collect1 := must.OK2(CapturePort())
	port2, collect2 := must.OK2(CapturePort())
	err := ev.Eval(parse.Source{Name: "[test]", Code: `
		fn f { defer { echo inner }; exit 7; echo after-exit }
		fn g { defer { echo outer }; f; echo after-f }
		g
		echo after-g
	`}, EvalCfg{Ports: []*Port{DummyInputPort, port1, port2}})
	code, ok := AsExit(err)
	if !ok {
		t.Fatalf("want Exit error, got %v", err)
	}
	if code != 7 {
		t.Fatalf("want exit code 7, got %d", code)
	}
	_, stdout := collect1()
	_, stderr := collect2()
	if string(stderr) != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if string(stdout) != "inner\nouter\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "inner\nouter\n")
	}
}

func TestExitNotCaughtByTry(t *testing.T) {
	ev := NewEvaler()
	port1, collect1 := must.OK2(CapturePort())
	port2, collect2 := must.OK2(CapturePort())
	err := ev.Eval(parse.Source{Name: "[test]", Code: `
		try {
			defer { echo deferred }
			exit 3
		} catch e {
			echo caught
		} finally {
			echo finally
		}
		echo after
	`}, EvalCfg{Ports: []*Port{DummyInputPort, port1, port2}})
	code, ok := AsExit(err)
	if !ok {
		t.Fatalf("want Exit error, got %v", err)
	}
	if code != 3 {
		t.Fatalf("want exit code 3, got %d", code)
	}
	_, stdout := collect1()
	_, stderr := collect2()
	if string(stderr) != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if string(stdout) != "deferred\nfinally\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "deferred\nfinally\n")
	}
}
