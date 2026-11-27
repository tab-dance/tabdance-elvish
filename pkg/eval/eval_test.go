package eval_test

import (
	"context"
	"sync"
	"testing"

	. "src.elv.sh/pkg/eval"

	"src.elv.sh/pkg/eval/vars"
	"src.elv.sh/pkg/parse"
)

func TestEval_AlternativeGlobal(t *testing.T) {
	ev := NewEvaler()
	g := BuildNs().AddVar("a", vars.NewReadOnly("")).Ns()
	err := ev.Eval(parse.Source{Name: "[test]", Code: "nop $a"}, EvalCfg{Global: g})
	if err != nil {
		t.Errorf("got error %v, want nil", err)
	}
	// Regression test for #1223
	if ev.Global().HasKeyString("a") {
		t.Errorf("$a from alternative global leaked into Evaler global")
	}
}

func TestEval_Concurrent(t *testing.T) {
	ev := NewEvaler()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		ev.Eval(parse.Source{Name: "[test]", Code: "var a"}, EvalCfg{})
		wg.Done()
	}()
	go func() {
		ev.Eval(parse.Source{Name: "[test]", Code: "var b"}, EvalCfg{})
		wg.Done()
	}()
	wg.Wait()
	g := ev.Global()
	if !g.HasKeyString("a") {
		t.Errorf("variable $a not created")
	}
	if !g.HasKeyString("b") {
		t.Errorf("variable $b not created")
	}
}

func TestEval_ConcurrentDefaultGlobalStress(t *testing.T) {
	ev := NewEvaler()
	const n = 50

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			codes := []string{
				"try { fail e; var x = after } catch e { nop }; var ok = yes",
				"try { fail e; fn f { put after } } catch e { nop }; var ok = yes",
				"try { fail e; use str } catch e { nop }; var ok = yes",
				"try { fail e; var x = (fail e) } catch e { nop }; var ok = yes",
			}
			code := codes[i%len(codes)]
			if err := ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{}); err != nil {
				t.Errorf("eval %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	g := ev.Global()
	if !g.HasKeyString("ok") {
		t.Errorf("variable $ok not created")
	}
	if g.HasKeyString("x") {
		t.Errorf("variable $x leaked")
	}
	if g.HasKeyString("f~") {
		t.Errorf("function f leaked")
	}
	if g.HasKeyString("str:") {
		t.Errorf("namespace str: leaked")
	}
}

func TestEvalAndCall_ConcurrentDefaultGlobal(t *testing.T) {
	ev := NewEvaler()
	if err := ev.Eval(parse.Source{Name: "[test]", Code: "fn id {|x| put $x }"}, EvalCfg{}); err != nil {
		t.Fatal(err)
	}
	fn := ev.Global().IndexString("id~").Get().(Callable)
	const n = 50

	var wg sync.WaitGroup
	wg.Add(2 * n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			code := "try { fail e; var x = after } catch e { nop }; var ok = yes"
			if i%2 == 0 {
				code = "try { fail e; fn f { put after } } catch e { nop }; var ok = yes"
			}
			if err := ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{}); err != nil {
				t.Errorf("eval %d: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			outPort, collect, err := ValueCapturePort()
			if err != nil {
				t.Errorf("capture port %d: %v", i, err)
				return
			}
			err = ev.Call(fn, CallCfg{Args: []any{"ok"}, From: "[test]"}, EvalCfg{Ports: []*Port{nil, outPort, nil}})
			vs := collect()
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			if len(vs) != 1 || vs[0] != "ok" {
				t.Errorf("call %d: got %v", i, vs)
			}
		}()
	}
	wg.Wait()
	g := ev.Global()
	if !g.HasKeyString("ok") {
		t.Errorf("variable $ok not created")
	}
	if g.HasKeyString("x") {
		t.Errorf("variable $x leaked")
	}
	if g.HasKeyString("f~") {
		t.Errorf("function f leaked")
	}
}

func TestCall_DefaultGlobalSnapshotConcurrentWithEval(t *testing.T) {
	ev := NewEvaler()
	if err := ev.Eval(parse.Source{Name: "[test]", Code: "fn id {|x| put $x }; var ok = yes"}, EvalCfg{}); err != nil {
		t.Fatal(err)
	}
	fn := ev.Global().IndexString("id~").Get().(Callable)
	const n = 50

	var wg sync.WaitGroup
	wg.Add(2 * n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			code := "try { fail e; var x = after } catch e { nop }; set ok = yes"
			if i%2 == 0 {
				code = "try { fail e; fn f { put after } } catch e { nop }; set ok = yes"
			}
			if err := ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{}); err != nil {
				t.Errorf("eval %d: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			outPort, collect, err := ValueCapturePort()
			if err != nil {
				t.Errorf("capture port %d: %v", i, err)
				return
			}
			err = ev.Call(fn, CallCfg{Args: []any{"ok"}, From: "[test]"}, EvalCfg{Ports: []*Port{nil, outPort, nil}})
			vs := collect()
			if err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			if len(vs) != 1 || vs[0] != "ok" {
				t.Errorf("call %d: got %v", i, vs)
			}
		}()
	}
	wg.Wait()
}

// TestEval_FailedDeclarationsDoNotLinger verifies that declarations rolled back
// by a failed chunk are neither visible nor resolvable in later chunks, and
// that repeated failures do not corrupt later redeclarations
// (tab-dance/tabdance-elvish#47, upstream #1445).
func TestEval_FailedDeclarationsDoNotLinger(t *testing.T) {
	ev := NewEvaler()
	evalCode := func(code string) error {
		return ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{})
	}
	if err := evalCode("var r = 1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := evalCode("fail e; var r = 2"); err == nil {
			t.Fatalf("eval %d: expected exception", i)
		}
	}
	g := ev.Global()
	if got := g.IndexString("r").Get(); got != "1" {
		t.Errorf("after failed redeclarations, $r = %v, want 1", got)
	}
	if err := evalCode("fail e; var z = after"); err == nil {
		t.Fatal("expected exception")
	}
	if ev.Global().HasKeyString("z") {
		t.Errorf("failed declaration $z still visible in global namespace")
	}
	if err := evalCode("var r = 5"); err != nil {
		t.Fatal(err)
	}
	if got := ev.Global().IndexString("r").Get(); got != "5" {
		t.Errorf("after successful redeclaration, $r = %v, want 5", got)
	}
	if ev.Global().HasKeyString("z") {
		t.Errorf("failed declaration $z still visible in global namespace")
	}
	if err := evalCode("put $z"); err == nil {
		t.Errorf("compiling $z after failed declaration should fail")
	}
}

// TestCheckTree_ConcurrentWithEval exercises metadata reads (used by the
// editor's syntax checker) against concurrent default-global Eval
// transactions; run with -race.
func TestCheckTree_ConcurrentWithEval(t *testing.T) {
	ev := NewEvaler()
	if err := ev.Eval(parse.Source{Name: "[test]", Code: "fn id {|x| put $x }"}, EvalCfg{}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			code := "try { fail e; var x = after } catch e { nop }; var ok = yes"
			if i%2 == 0 {
				code = "try { fail e; fn f { put after } } catch e { nop }; var ok = yes"
			}
			if err := ev.Eval(parse.Source{Name: "[test]", Code: code}, EvalCfg{}); err != nil {
				t.Errorf("eval %d: %v", i, err)
				return
			}
		}
		close(stop)
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			tree, err := parse.Parse(parse.Source{Name: "[check]", Code: "id x"}, parse.Config{})
			if err != nil {
				t.Errorf("parse: %v", err)
				return
			}
			if _, err := ev.CheckTree(tree, nil); err != nil {
				t.Errorf("check: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

// TestEval_ReentrantNestedEval verifies that a default-global Eval whose code
// calls a Go function that runs another default-global Eval does not deadlock
// or corrupt namespace state (the global eval lock is reentrant).
func TestEval_ReentrantNestedEval(t *testing.T) {
	ev := NewEvaler()
	ev.ExtendGlobal(BuildNs().AddGoFn("nested-eval", func() {
		err := ev.Eval(parse.Source{Name: "[nested]", Code: "var inner = ok"}, EvalCfg{})
		if err != nil {
			t.Errorf("nested eval: %v", err)
		}
	}).Ns())
	err := ev.Eval(parse.Source{Name: "[test]", Code: "nested-eval; var outer = yes"}, EvalCfg{})
	if err != nil {
		t.Fatalf("outer eval: %v", err)
	}
	g := ev.Global()
	if !g.HasKeyString("inner") || !g.HasKeyString("outer") {
		t.Errorf("nested or outer declaration missing from global namespace")
	}
}

// TestEval_ReentrantSnapshotDuringEval verifies that short global accesses,
// like the ones chdir hooks make through ev.Call, reenter a running
// transaction instead of deadlocking on it.
func TestEval_ReentrantSnapshotDuringEval(t *testing.T) {
	ev := NewEvaler()
	ev.ExtendGlobal(BuildNs().AddGoFn("nested-access", func() {
		ev.GlobalSnapshot()
		if v := ev.GlobalVar("before"); v == nil || v.Get() != "yes" {
			t.Errorf("GlobalVar during eval: got %v, want yes", v)
		}
		err := ev.Eval(parse.Source{Name: "[nested]", Code: "var inner = ok"}, EvalCfg{})
		if err != nil {
			t.Errorf("nested eval: %v", err)
		}
	}).Ns())
	err := ev.Eval(parse.Source{Name: "[test]", Code: "var before = yes; nested-access"},
		EvalCfg{})
	if err != nil {
		t.Fatalf("outer eval: %v", err)
	}
}

func TestGlobalVar(t *testing.T) {
	ev := NewEvaler()
	if v := ev.GlobalVar("x"); v != nil {
		t.Errorf("GlobalVar on missing name: got %v, want nil", v)
	}
	err := ev.Eval(parse.Source{Name: "[test]", Code: "var x = foo"}, EvalCfg{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if v := ev.GlobalVar("x"); v == nil || v.Get() != "foo" {
		t.Errorf("GlobalVar(x): got %v, want foo", v)
	}
	err = ev.Eval(parse.Source{Name: "[test]", Code: "del x"}, EvalCfg{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if v := ev.GlobalVar("x"); v != nil {
		t.Errorf("GlobalVar after del: got %v, want nil", v)
	}
}

// TestEval_InterruptRollsBackDeclarations verifies that an interrupt raised
// between pipelines of a chunk rolls back declarations that follow the
// interrupt point, like any other chunk exception.
func TestEval_InterruptRollsBackDeclarations(t *testing.T) {
	ev := NewEvaler()
	block := make(chan struct{})
	started := make(chan struct{})
	ev.ExtendGlobal(BuildNs().AddGoFn("block", func() {
		close(started)
		<-block
	}).Ns())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
		close(block)
	}()
	err := ev.Eval(parse.Source{Name: "[test]", Code: "var x = 1; block; var y = 2"},
		EvalCfg{Interrupts: ctx})
	if err == nil {
		t.Fatal("expected interrupt exception")
	}
	g := ev.Global()
	if !g.HasKeyString("x") {
		t.Errorf("$x missing after interrupt")
	}
	if g.HasKeyString("y") {
		t.Errorf("$y leaked after interrupt")
	}
}

type fooOpts struct{ Opt string }

func (*fooOpts) SetDefaultOptions() {}

func TestCall(t *testing.T) {
	ev := NewEvaler()
	var gotOpt, gotArg string
	fn := NewGoFn("foo", func(fm *Frame, opts fooOpts, arg string) {
		gotOpt = opts.Opt
		gotArg = arg
	})

	passedArg := "arg value"
	passedOpt := "opt value"
	ev.Call(fn,
		CallCfg{
			Args: []any{passedArg},
			Opts: map[string]any{"opt": passedOpt},
			From: "[TestCall]"},
		EvalCfg{})

	if gotArg != passedArg {
		t.Errorf("got arg %q, want %q", gotArg, passedArg)
	}
	if gotOpt != passedOpt {
		t.Errorf("got opt %q, want %q", gotOpt, passedOpt)
	}
}

var checkTests = []struct {
	name           string
	code           string
	wantParseErr   bool
	wantCompileErr bool
}{
	{name: "no error", code: "put $nil"},
	{name: "parse error only", code: "put [",
		wantParseErr: true},
	{name: "compile error only", code: "put $x",
		wantCompileErr: true},
	{name: "both parse and compile error", code: "put [$x",
		wantParseErr: true, wantCompileErr: true},
}

func TestCheck(t *testing.T) {
	ev := NewEvaler()
	for _, test := range checkTests {
		t.Run(test.name, func(t *testing.T) {
			parseErr, _, compileErr := ev.Check(parse.Source{Name: "[test]", Code: test.code}, nil)
			if (parseErr != nil) != test.wantParseErr {
				t.Errorf("got parse error %v, when wantParseErr = %v",
					parseErr, test.wantParseErr)
			}
			if (compileErr != nil) != test.wantCompileErr {
				t.Errorf("got compile error %v, when wantCompileErr = %v",
					compileErr, test.wantCompileErr)
			}
		})
	}
}
