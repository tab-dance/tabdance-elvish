package eval

import (
	"testing"

	"src.elv.sh/pkg/parse"
)

func mustEval(t *testing.T, ev *Evaler, code string) {
	t.Helper()
	if err := ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{}); err != nil {
		t.Fatalf("eval %q: %v", code, err)
	}
}

func evalExpectError(t *testing.T, ev *Evaler, code string) {
	t.Helper()
	if err := ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{}); err == nil {
		t.Fatalf("eval %q: expected exception, got nil", code)
	}
}

// Tombstones of declarations rolled back after a chunk failure must be swept
// away by the next compilation instead of accumulating.
func TestFailedChunksDoNotGrowGlobal(t *testing.T) {
	ev := NewEvaler()
	for i := 0; i < 50; i++ {
		evalExpectError(t, ev, "fail x; var y = 1")
	}
	if n := len(ev.global.infos); n > 2 {
		t.Errorf("global ns has %d entries after 50 failed chunks; tombstones accumulate", n)
	}
	if ev.Global().HasKeyString("y") {
		t.Errorf("y is visible after failed chunks")
	}
}

// Tombstones of declarations rolled back inside a failed exception capture
// must be swept away by the next compilation instead of accumulating.
func TestFailedCapturesDoNotGrowGlobal(t *testing.T) {
	ev := NewEvaler()
	mustEval(t, ev, "var e = $nil")
	for i := 0; i < 50; i++ {
		mustEval(t, ev, "set e = ?(fail bad; var y = 1)")
	}
	if n := len(ev.global.infos); n > 3 {
		t.Errorf("global ns has %d entries after 50 failed captures; tombstones accumulate", n)
	}
	if ev.Global().HasKeyString("y") {
		t.Errorf("y is visible after failed captures")
	}
}

// A declaration or a failing exception capture in a background pipeline must
// not race or panic against the failing chunk's rollback, which happens while
// the background form may still be running. Run with -race.
func TestBgDeclarationWithFailingChunk(t *testing.T) {
	ev := NewEvaler()
	for i := 0; i < 10; i++ {
		evalExpectError(t, ev, "var w = 1 &\nfail e")
	}
	if ev.Global().HasKeyString("w") {
		t.Errorf("w is visible after failed chunks")
	}
}

func TestBgCaptureOutlivesChunkFailure(t *testing.T) {
	ev := NewEvaler()
	blocked := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	ev.ExtendGlobal(BuildNs().
		AddGoFn("block-in-capture", func() { close(blocked); <-release }).
		AddGoFn("sink", func(any) { close(finished) }).
		Ns())
	evalExpectError(t, ev,
		"sink ?(block-in-capture; var y = 1; fail e) &\nfail main; var z = 1")
	// The background form is still blocked inside its capture; the chunk has
	// already failed and rolled back. Let the background form finish its
	// capture (and its rollback) only now.
	<-blocked
	close(release)
	<-finished
	if ev.Global().HasKeyString("y") {
		t.Errorf("y is visible after failed background capture")
	}
	if ev.Global().HasKeyString("z") {
		t.Errorf("z is visible after failed chunk")
	}
}

// A failed exception capture inside a function must not restore metadata of
// the function's arguments from the caller's namespace.
func TestCaptureInFunctionLeavesArgsClean(t *testing.T) {
	ev := NewEvaler()
	mustEval(t, ev, "var g0 = 1; var g1 = 1")
	done := make(chan struct{})
	var argInfos []staticVarInfo
	ev.ExtendGlobal(BuildNs().AddGoFn("probe", func(fm *Frame) {
		argInfos = append([]staticVarInfo(nil), fm.local.infos...)
		close(done)
	}).Ns())
	// Leaves a deleted, hidden entry among the first entries of the caller's
	// namespace, which a contaminated rollback would copy onto arguments.
	mustEval(t, ev, "del g0")
	mustEval(t, ev, "fn f {|a b| put ?(fail boom); probe }")
	mustEval(t, ev, "f 1 2")
	<-done
	if len(argInfos) != 2 {
		t.Fatalf("probe saw %d local entries, want 2", len(argInfos))
	}
	for _, info := range argInfos {
		if info.hidden || info.deleted {
			t.Errorf("argument %q contaminated after failed capture: hidden=%v deleted=%v",
				info.name, info.hidden, info.deleted)
		}
	}
}
