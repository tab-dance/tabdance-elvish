package eval

import (
	"fmt"
	"testing"

	"src.elv.sh/pkg/eval/vars"
	"src.elv.sh/pkg/parse"
)

var benchmarks = []struct {
	name string
	code string
}{
	{"empty", ""},
	{"nop", "nop"},
	{"nop-nop", "nop | nop"},
	{"put-x", "put x"},
	{"for-100", "for x [(range 100)] { }"},
	{"range-100", "range 100 | each {|_| }"},
	{"read-local", "var x = val; nop $x"},
	{"read-upval", "var x = val; { nop $x }"},
}

func BenchmarkEval(b *testing.B) {
	for _, bench := range benchmarks {
		b.Run(bench.name, func(b *testing.B) {
			ev := NewEvaler()
			src := parse.Source{Name: "[benchmark]", Code: bench.code}

			tree, err := parse.Parse(src, parse.Config{})
			if err != nil {
				panic(err)
			}
			op, _, err := compile(ev.builtin.static(), ev.global.static(), nil, tree, nil)
			if err != nil {
				panic(err)
			}

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				fm, cleanup := ev.prepareFrame(src, EvalCfg{Global: ev.Global()})
				_, exec := op.prepare(fm)
				_ = exec()
				cleanup()
			}
		})
	}
}

func BenchmarkEvalDefaultGlobal(b *testing.B) {
	for _, bench := range benchmarks {
		bench := bench
		b.Run(bench.name, func(b *testing.B) {
			ev := NewEvaler()
			src := parse.Source{Name: "[benchmark]", Code: bench.code}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ev.Eval(src, EvalCfg{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkEvalDefaultGlobalParallel measures default-global Eval under
// contention. Default-global evals are serialized by Evaler.globalEvalMu, so
// this benchmark intentionally captures serialized throughput rather than
// parallel speedup.
func BenchmarkEvalDefaultGlobalParallel(b *testing.B) {
	src := parse.Source{Name: "[benchmark]", Code: "nop"}
	ev := NewEvaler()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := ev.Eval(src, EvalCfg{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkEvalDefaultGlobalRollback measures a failing chunk, whose rollback
// tombstones a pending declaration, together with the sweep of the previous
// iteration's tombstone.
func BenchmarkEvalDefaultGlobalRollback(b *testing.B) {
	ev := NewEvaler()
	src := parse.Source{Name: "[benchmark]", Code: "fail e; var x = 1"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ev.Eval(src, EvalCfg{}); err == nil {
			b.Fatal("expected exception")
		}
	}
}

// BenchmarkGlobalVar measures the locked point-lookup path used by editor
// highlighting and namespace completion. Compare with a GlobalSnapshot call
// to guard against accidentally reintroducing a clone on this path.
func BenchmarkGlobalVar(b *testing.B) {
	b.Run("small", func(b *testing.B) {
		ev := NewEvaler()
		err := ev.Eval(parse.Source{Name: "[benchmark]", Code: "var x = 1"}, EvalCfg{})
		if err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = ev.GlobalVar("x")
		}
	})
	b.Run("1000-vars", func(b *testing.B) {
		ev := NewEvaler()
		nb := BuildNs()
		for i := 0; i < 1000; i++ {
			nb.AddVar(fmt.Sprintf("v%d", i), vars.FromInit(nil))
		}
		ev.ExtendGlobal(nb.Ns())
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = ev.GlobalVar("v500")
		}
	})
}
