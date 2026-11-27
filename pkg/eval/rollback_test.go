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

// A chunk that doesn't change the namespace layout takes the no-copy fast
// path: it shares the namespace instead of copying it, and a failure keeps
// the effects that executed before it, like the rollback would.
func TestReadOnlyChunkSharesNamespaceAndKeepsEffects(t *testing.T) {
	ev := NewEvaler()
	mustEval(t, ev, "var a = one; var b = two")
	shared := ev.global
	evalExpectError(t, ev, "set a = five; del b; fail x")
	if ev.global != shared {
		t.Errorf("read-only chunk copied the namespace")
	}
	if v := ev.GlobalVar("a"); v == nil || v.Get() != "five" {
		t.Errorf("executed set before failure not kept: got %v, want five", v)
	}
	if v := ev.GlobalVar("b"); v != nil {
		t.Errorf("executed del before failure not kept: b still visible")
	}
	// A declaring chunk still copies for rollback bookkeeping.
	mustEval(t, ev, "var c = 3")
	if ev.global == shared {
		t.Errorf("declaring chunk shared the namespace instead of copying")
	}
}

// A failed exception capture inside a read-only chunk must not disturb
// existing bindings: with no capture-internal declarations and the rollback
// boundary above every entry, its rollback is inert.
func TestReadOnlyChunkCaptureFailureIsInert(t *testing.T) {
	ev := NewEvaler()
	mustEval(t, ev, "var a = one; var r = $nil")
	shared := ev.global
	mustEval(t, ev, "set r = ?(fail x)")
	if ev.global != shared {
		t.Errorf("read-only chunk with capture copied the namespace")
	}
	if v := ev.GlobalVar("a"); v == nil || v.Get() != "one" {
		t.Errorf("existing binding disturbed by failed capture: got %v, want one", v)
	}
	if v := ev.GlobalVar("r"); v == nil {
		t.Errorf("r missing after failed capture")
	}
}

// Entries deleted by an executed del become sweepable once a later declaring
// chunk resets their activation; the tombstone flag set by that reset must
// make the next compilation sweep them away.
func TestDeletedEntriesAreSweptAfterNextDeclaringChunk(t *testing.T) {
	ev := NewEvaler()
	mustEval(t, ev, "var x = 1")
	mustEval(t, ev, "del x")
	mustEval(t, ev, "var y = 2")
	mustEval(t, ev, "put ok")
	for _, info := range ev.global.infos {
		if info.name == "x" {
			t.Errorf("deleted entry x not swept: hidden=%v deleted=%v activated=%v",
				info.hidden, info.deleted, info.activated)
		}
	}
	if ev.global.hasTombstones {
		t.Errorf("tombstone flag not cleared by sweep")
	}
}
