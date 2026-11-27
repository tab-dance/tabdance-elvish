// Package eval handles evaluation of parsed Elvish code and provides runtime
// facilities.
package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"

	"src.elv.sh/pkg/env"
	"src.elv.sh/pkg/eval/vals"
	"src.elv.sh/pkg/eval/vars"
	"src.elv.sh/pkg/logutil"
	"src.elv.sh/pkg/parse"
)

var logger = logutil.GetLogger("[eval] ")

const (
	// FnSuffix is the suffix for the variable names of functions. Defining a
	// function "foo" is equivalent to setting a variable named "foo~", and vice
	// versa.
	FnSuffix = "~"
	// NsSuffix is the suffix for the variable names of namespaces. Defining a
	// namespace foo is equivalent to setting a variable named "foo:", and vice
	// versa.
	NsSuffix = ":"
)

const (
	defaultValuePrefix        = "▶ "
	defaultNotifyBgJobSuccess = true
)

// Evaler provides methods for evaluating code, and maintains state that is
// persisted between evaluation of different pieces of code. An Evaler is safe
// to use concurrently.
type Evaler struct {
	// The following fields must only be set before the Evaler is used to
	// evaluate any code; mutating them afterwards may cause race conditions.

	// Command-line arguments, exposed as $args.
	Args vals.List
	// Hooks to run before exit or exec.
	PreExitHooks []func()
	preExitOnce  sync.Once
	// Chdir hooks, exposed indirectly as $before-chdir and $after-chdir.
	BeforeChdir, AfterChdir []func(string)
	// Directories to search libraries.
	LibDirs []string
	// Source code of internal bundled modules indexed by use specs.
	BundledModules map[string]string
	// Callback to notify the success or failure of background jobs. Must not be
	// mutated once the Evaler is used to evaluate any code.
	BgJobNotify func(string)
	// Path to the rc file, and path to the rc file actually evaluated. These
	// are not used by the Evaler itself right now; they are here so that they
	// can be exposed to the runtime: module.
	RcPath, EffectiveRcPath string

	mu sync.RWMutex
	// Mutations to fields below must be guarded by mutex.
	//
	// Note that this is *not* a GIL; most state mutations when executing Elvish
	// code is localized and do not need to hold this mutex.
	//
	// TODO: Actually guard all mutations by this mutex.

	global, builtin *Ns

	deprecations deprecationRegistry

	// Internal modules are indexed by use specs. External modules are indexed by
	// absolute paths.
	modules map[string]*Ns

	// Various states and configs exposed to Elvish code.
	//
	// The prefix to prepend to value outputs when writing them to terminal,
	// exposed as $value-out-prefix.
	valuePrefix string
	// Whether to notify the success of background jobs, exposed as
	// $notify-bg-job-sucess.
	notifyBgJobSuccess bool
	// The current number of background jobs, exposed as $num-bg-jobs.
	numBgJobs int

	// Default-global namespace concurrency contract (fix/issue-1445-decl-after-fail).
	//
	// Problem: one chunk must roll back unexecuted post-failure declarations
	// (elves/elvish#1445) while preserving executed pre-failure effects. The
	// rollback mutates ns transaction metadata (hidden/activated/deleted) and
	// publishes global ns updates. Concurrent default-global Eval raced on that
	// shared state; serializing Eval fixed races but deadlocked when editor
	// addons (edit:instant) nested Eval on the same goroutine.
	//
	// Minimal/safe contract:
	//   - Eval(default global), ExtendGlobal, DeleteFromGlobal, and
	//     CallInDefaultGlobal take globalEvalMu. They serialize namespace
	//     transaction boundaries, global publication, and structural ns mutation.
	//   - globalEvalMu is reentrant per goroutine (owner+depth+cond) so nested
	//     default-global Eval works. Goroutine identity is recorded only by
	//     long-running holders (Eval, CallInDefaultGlobal) and consulted only
	//     while a transaction is active, so uncontended acquisitions — the
	//     common case for editor paths like highlighting — never parse it.
	//   - Plain Call with nil Global snapshots ev.global under globalEvalMu,
	//     then executes outside the lock. This avoids races with concurrent
	//     transaction-metadata mutation while keeping callbacks from deadlocking
	//     prompt/editor paths. It is not a serialized transaction with concurrent
	//     Eval/ExtendGlobal/DeleteFromGlobal; callers needing serialized callback
	//     execution must use CallInDefaultGlobal and must not block on work that
	//     needs globalEvalMu.
	//   - Custom cfg.Global bypasses globalEvalMu; caller synchronizes shared ns.
	//
	// Rollback of unexecuted declarations never restructures the published
	// namespace in place: failed-chunk declarations become tombstones (hidden and
	// deleted entries), which the next Eval sweeps away before compiling
	// (Ns.withoutTombstones). Forms dispatched to pipeline goroutines get a private
	// metadata copy of the namespace (shared variable objects), so their ops can
	// neither race nor panic against the chunk's rollback bookkeeping.
	//
	// Risks: per-Evaler serialization/contention, goroutine-identity parsing on
	// nested and contended paths only (with runtime stack-format sensitivity
	// there), and plain Call remaining outside the transaction boundary.
	//
	// Alternatives considered: per-namespace/COW transaction snapshots (cleaner
	// isolation, large semantic change), fine-grained ns metadata locking (high
	// deadlock/race risk), explicit eval-transaction token threaded through
	// EvalCfg/Frame/callbacks (cleaner ownership, invasive API change), or
	// narrower lock scope around snapshot/commit only (unsafe while execution can
	// observe/mutate shared ns state).
	globalEvalMu         sync.Mutex
	globalEvalCond       *sync.Cond
	globalEvalDepth      int
	globalEvalOwnerKnown bool
	globalEvalOwnerID    uint64
}

func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine 123 [...]"
	b := buf[:n]
	const prefix = "goroutine "
	if len(b) < len(prefix) || string(b[:len(prefix)]) != prefix {
		panic("eval: cannot parse goroutine id")
	}
	b = b[len(prefix):]
	i := 0
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		i++
	}
	if i == 0 {
		panic("eval: cannot parse goroutine id")
	}
	id, err := strconv.ParseUint(string(b[:i]), 10, 64)
	if err != nil {
		panic("eval: cannot parse goroutine id: " + err.Error())
	}
	return id
}

func (ev *Evaler) lockGlobalEval() {
	ev.globalEvalMu.Lock()
	for ev.globalEvalDepth > 0 && !ev.sameGoroutineAsOwner() {
		if ev.globalEvalCond == nil {
			ev.globalEvalCond = sync.NewCond(&ev.globalEvalMu)
		}
		ev.globalEvalCond.Wait()
	}
	ev.globalEvalDepth++
	ev.globalEvalMu.Unlock()
}

// sameGoroutineAsOwner reports whether the calling goroutine is the one
// recorded as the owner of the active transaction. It must be called with
// globalEvalMu held, while a transaction is active.
//
// The owner is recorded once per long-running transaction by
// noteGlobalEvalOwner. While the owner is not recorded — which only happens
// for short sections that run no code which could reenter — every caller is
// conservatively treated as a different goroutine and blocks.
func (ev *Evaler) sameGoroutineAsOwner() bool {
	return ev.globalEvalOwnerKnown &&
		ev.globalEvalOwnerID == currentGoroutineID()
}

// noteGlobalEvalOwner records the calling goroutine as the owner of the
// current transaction, allowing reentrant acquisition from code that runs
// during it, such as chdir hooks or Go functions calling back into the
// Evaler. Long-running holders (Eval, CallInDefaultGlobal) must call it
// immediately after acquiring the transaction, before running any code.
func (ev *Evaler) noteGlobalEvalOwner() {
	ev.globalEvalMu.Lock()
	if !ev.globalEvalOwnerKnown {
		ev.globalEvalOwnerID = currentGoroutineID()
		ev.globalEvalOwnerKnown = true
	}
	ev.globalEvalMu.Unlock()
}

func (ev *Evaler) unlockGlobalEval() {
	ev.globalEvalMu.Lock()
	if ev.globalEvalDepth == 0 {
		ev.globalEvalMu.Unlock()
		panic("eval: global eval lock released without being held")
	}
	ev.globalEvalDepth--
	if ev.globalEvalDepth == 0 {
		ev.globalEvalOwnerKnown = false
		if ev.globalEvalCond != nil {
			ev.globalEvalCond.Signal()
		}
	}
	ev.globalEvalMu.Unlock()
}

// NewEvaler creates a new Evaler.
func NewEvaler() *Evaler {
	builtin := builtinNs.Ns()

	newListVar := func(l vals.List) vars.PtrVar { return vars.FromPtr(&l) }
	beforeExitHookElvish := newListVar(vals.EmptyList)
	beforeChdirElvish := newListVar(vals.EmptyList)
	afterChdirElvish := newListVar(vals.EmptyList)

	ev := &Evaler{
		global:  new(Ns),
		builtin: builtin,

		deprecations: newDeprecationRegistry(),

		modules:        make(map[string]*Ns),
		BundledModules: make(map[string]string),

		valuePrefix:        defaultValuePrefix,
		notifyBgJobSuccess: defaultNotifyBgJobSuccess,
		numBgJobs:          0,
		Args:               vals.EmptyList,
	}

	ev.PreExitHooks = []func(){func() {
		CallHook(ev, nil, "before-exit", beforeExitHookElvish.Get().(vals.List))
	}}
	ev.BeforeChdir = []func(string){func(path string) {
		CallHook(ev, nil, "before-chdir", beforeChdirElvish.Get().(vals.List), path)
	}}
	ev.AfterChdir = []func(string){func(path string) {
		CallHook(ev, nil, "after-chdir", afterChdirElvish.Get().(vals.List), path)
	}}

	ev.ExtendBuiltin(BuildNs().
		AddVar("pwd", NewPwdVar(ev)).
		AddVar("before-exit", beforeExitHookElvish).
		AddVar("before-chdir", beforeChdirElvish).
		AddVar("after-chdir", afterChdirElvish).
		AddVar("value-out-indicator",
			vars.FromPtrWithMutex(&ev.valuePrefix, &ev.mu)).
		AddVar("notify-bg-job-success",
			vars.FromPtrWithMutex(&ev.notifyBgJobSuccess, &ev.mu)).
		AddVar("num-bg-jobs",
			vars.FromGet(func() any { return strconv.Itoa(ev.getNumBgJobs()) })).
		AddVar("args", vars.FromGet(func() any { return ev.Args })))

	// Install the "builtin" module after extension is complete.
	ev.modules["builtin"] = ev.builtin

	return ev
}

// PreExit runs all pre-exit hooks.
func (ev *Evaler) PreExit() {
	ev.preExitOnce.Do(func() {
		ev.mu.RLock()
		hooks := append([]func(){}, ev.PreExitHooks...)
		ev.mu.RUnlock()
		for _, hook := range hooks {
			hook()
		}
	})
}

// Access methods.

// Global returns the current default global Ns.
//
// The returned *Ns is the live namespace object and may be replaced by
// Eval/ExtendGlobal/DeleteFromGlobal. Its variable slots are safe for
// concurrent Get/Set through vars.Var, but namespace metadata may be mutated
// during a default-global Eval transaction. Callers that need a metadata-
// consistent snapshot should use GlobalSnapshot; callers that need serialized
// mutation/transaction semantics should use Eval/ExtendGlobal/
// DeleteFromGlobal/CallInDefaultGlobal.
func (ev *Evaler) Global() *Ns {
	ev.mu.RLock()
	defer ev.mu.RUnlock()
	return ev.global
}

// GlobalSnapshot returns a metadata-consistent snapshot of the default global
// namespace. It clones namespace metadata under globalEvalMu while preserving
// shared variable slot objects, so reads observe a stable visibility view and
// variable Get/Set still operate on liveVars. Callers that only need to look
// up a single name should use GlobalVar instead, which does not clone.
func (ev *Evaler) GlobalSnapshot() *Ns {
	ev.lockGlobalEval()
	defer ev.unlockGlobalEval()
	return ev.global.clone()
}

// GlobalVar returns the variable with the given name in the default global
// namespace, or nil if no such variable exists. It is safe to call
// concurrently with default-global Eval; like GlobalSnapshot it serializes on
// the global evaluation transaction, but without cloning the namespace, making
// it suitable for point lookups on hot paths such as per-keystroke syntax
// highlighting.
func (ev *Evaler) GlobalVar(name string) vars.Var {
	ev.lockGlobalEval()
	defer ev.unlockGlobalEval()
	return ev.global.IndexString(name)
}

// ExtendGlobal extends the global namespace with the given namespace.
func (ev *Evaler) ExtendGlobal(ns Nser) {
	ev.lockGlobalEval()
	defer ev.unlockGlobalEval()
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.global = CombineNs(ev.global, ns.Ns())
}

// DeleteFromGlobal deletes names from the global namespace.
func (ev *Evaler) DeleteFromGlobal(names map[string]struct{}) {
	ev.lockGlobalEval()
	defer ev.unlockGlobalEval()
	ev.mu.Lock()
	defer ev.mu.Unlock()
	g := ev.global.clone()
	for i := range g.infos {
		if _, ok := names[g.infos[i].name]; ok {
			g.infos[i].deleted = true
		}
	}
	ev.global = g
}

// Builtin returns the builtin Ns.
func (ev *Evaler) Builtin() *Ns {
	ev.mu.RLock()
	defer ev.mu.RUnlock()
	return ev.builtin
}

// ExtendBuiltin extends the builtin namespace with the given namespace.
func (ev *Evaler) ExtendBuiltin(ns Nser) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.builtin = CombineNs(ev.builtin, ns.Ns())
}

// ReplaceBuiltin replaces the builtin namespace. It should only be used in
// tests.
func (ev *Evaler) ReplaceBuiltin(ns *Ns) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.builtin = ns
}

func (ev *Evaler) registerDeprecation(d deprecation) bool {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return ev.deprecations.register(d)
}

// AddModule add an internal module so that it can be used with "use $name" from
// script.
func (ev *Evaler) AddModule(name string, mod *Ns) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.modules[name] = mod
}

// ValuePrefix returns the prefix to prepend to value outputs when writing them
// to terminal.
func (ev *Evaler) ValuePrefix() string {
	ev.mu.RLock()
	defer ev.mu.RUnlock()
	return ev.valuePrefix
}

func (ev *Evaler) getNotifyBgJobSuccess() bool {
	ev.mu.RLock()
	defer ev.mu.RUnlock()
	return ev.notifyBgJobSuccess
}

func (ev *Evaler) getNumBgJobs() int {
	ev.mu.RLock()
	defer ev.mu.RUnlock()
	return ev.numBgJobs
}

func (ev *Evaler) addNumBgJobs(delta int) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.numBgJobs += delta
}

// Chdir changes the current directory, and updates $E:PWD on success
//
// It runs the functions in beforeChdir immediately before changing the
// directory, and the functions in afterChdir immediately after (if chdir was
// successful). It returns nil as long as the directory changing part succeeds.
func (ev *Evaler) Chdir(path string) error {
	for _, hook := range ev.BeforeChdir {
		hook(path)
	}

	err := os.Chdir(path)
	if err != nil {
		return err
	}

	for _, hook := range ev.AfterChdir {
		hook(path)
	}

	pwd, err := os.Getwd()
	if err != nil {
		logger.Println("getwd after cd:", err)
		return nil
	}
	os.Setenv(env.PWD, pwd)

	return nil
}

// EvalCfg keeps configuration for the (*Evaler).Eval method.
type EvalCfg struct {
	// Context that can be used to cancel the evaluation.
	Interrupts context.Context
	// Ports to use in evaluation. The first 3 elements, if not specified
	// (either being nil or Ports containing fewer than 3 elements),
	// will be filled with DummyInputPort, DummyOutputPort and
	// DummyOutputPort respectively.
	Ports []*Port
	// Whether the Eval method should try to put the Elvish in the foreground
	// after the code is executed.
	PutInFg bool
	// If not nil, used the given global namespace, instead of Evaler's own.
	//
	// Value updates to variables that already exist in this namespace are
	// visible to the caller (variable objects are shared), but declarations
	// made by the evaluated code are not persisted into it; module loading
	// uses the namespace returned by Frame.PrepareEval instead. Callers that
	// need declarations to persist should use the Evaler's default global
	// namespace.
	Global *Ns
}

func (cfg *EvalCfg) fillDefaults() {
	if len(cfg.Ports) < 3 {
		cfg.Ports = append(cfg.Ports, make([]*Port, 3-len(cfg.Ports))...)
	}
	if cfg.Ports[0] == nil {
		cfg.Ports[0] = DummyInputPort
	}
	if cfg.Ports[1] == nil {
		cfg.Ports[1] = DummyOutputPort
	}
	if cfg.Ports[2] == nil {
		cfg.Ports[2] = DummyOutputPort
	}
}

// Eval evaluates a piece of source code with the given configuration. The
// returned error may be a parse error, compilation error or exception.
//
// Evaluations using the Evaler's default global namespace are serialized to
// protect shared namespace runtime metadata. Evaluations with a custom
// cfg.Global bypass this serialization; callers must synchronize concurrent
// use of a shared *Ns. Tombstones of rolled-back declarations are swept
// before compiling, so repeated failures don't grow the namespace.
func (ev *Evaler) Eval(src parse.Source, cfg EvalCfg) error {
	cfg.fillDefaults()
	errFile := cfg.Ports[2].File

	tree, err := parse.Parse(src, parse.Config{WarningWriter: errFile})
	if err != nil {
		return err
	}

	defaultGlobal := cfg.Global == nil
	if defaultGlobal {
		ev.lockGlobalEval()
		defer ev.unlockGlobalEval()
		ev.noteGlobalEvalOwner()
	}
	ev.mu.RLock()
	b := ev.builtin
	if defaultGlobal {
		// Sweep tombstones left by rolled-back declarations of earlier
		// chunks, so that they don't accumulate. The swept namespace is
		// observably equivalent and becomes the base of this compilation.
		cfg.Global = ev.global.withoutTombstones()
	}
	g := cfg.Global
	ev.mu.RUnlock()

	op, _, err := compile(b.static(), g.static(), nil, tree, errFile)
	if err != nil {
		return err
	}

	fm, cleanup := ev.prepareFrame(src, cfg)
	defer cleanup()

	newLocal, exec := op.prepare(fm)
	if defaultGlobal {
		ev.mu.Lock()
		ev.global = newLocal
		ev.mu.Unlock()
	}
	return exec()
}

// CallCfg keeps configuration for the (*Evaler).Call method.
type CallCfg struct {
	// Arguments to pass to the function.
	Args []any
	// Options to pass to the function.
	Opts map[string]any
	// The name of the internal source that is calling the function.
	From string
}

func (cfg *CallCfg) fillDefaults() {
	if cfg.Opts == nil {
		cfg.Opts = NoOpts
	}
	if cfg.From == "" {
		cfg.From = "[internal]"
	}
}

// Call calls a given function.
//
// When evalCfg.Global is nil, Call snapshots the Evaler's default global
// namespace under globalEvalMu, then executes the callback without holding the
// lock. The snapshot avoids races with concurrent default-global transaction
// metadata mutation while preserving shared variable slot objects. Keeping
// callback execution outside globalEvalMu preserves compatibility with
// UI/editor callbacks that may block or reenter eval-sensitive paths.
//
// Safety: default-global Call is not serialized with concurrent Eval/
// ExtendGlobal/DeleteFromGlobal. Use it for normal/editor callbacks that can
// tolerate seeing committed global state before/after concurrent transactions.
// Use CallInDefaultGlobal when callback execution must be serialized with those
// transactions, and only when the callback cannot block on work that needs
// globalEvalMu.
func (ev *Evaler) Call(f Callable, callCfg CallCfg, evalCfg EvalCfg) error {
	callCfg.fillDefaults()
	evalCfg.fillDefaults()
	if evalCfg.Global == nil {
		evalCfg.Global = ev.GlobalSnapshot()
	}
	fm, cleanup := ev.prepareFrame(parse.Source{Name: callCfg.From}, evalCfg)
	defer cleanup()
	return f.Call(fm, callCfg.Args, callCfg.Opts)
}

// CallInDefaultGlobal calls a function with the Evaler's default global
// namespace serialized with default-global Eval/ExtendGlobal/DeleteFromGlobal.
// Use it for concurrent or background callers that must not race with global
// namespace transactions. Do not use it from callbacks that may already be
// executing while holding or depending on prompt/editor paths that cannot block
// on global eval progress.
func (ev *Evaler) CallInDefaultGlobal(f Callable, callCfg CallCfg, evalCfg EvalCfg) error {
	callCfg.fillDefaults()
	evalCfg.fillDefaults()
	ev.lockGlobalEval()
	defer ev.unlockGlobalEval()
	ev.noteGlobalEvalOwner()
	evalCfg.Global = ev.global
	fm, cleanup := ev.prepareFrame(parse.Source{Name: callCfg.From}, evalCfg)
	defer cleanup()
	return f.Call(fm, callCfg.Args, callCfg.Opts)
}

func (ev *Evaler) prepareFrame(src parse.Source, cfg EvalCfg) (*Frame, func()) {
	intCtx := cfg.Interrupts
	if intCtx == nil {
		intCtx = context.Background()
	}

	ports := fillDefaultDummyPorts(cfg.Ports)

	fm := &Frame{ev, src, cfg.Global, new(Ns), nil, nil, intCtx, ports, nil, false, false, 0, nil}
	return fm, func() {
		if cfg.PutInFg {
			err := putSelfInFg()
			if err != nil {
				fmt.Fprintln(ports[2].File,
					"failed to put myself in foreground:", err)
			}
		}
	}
}

func fillDefaultDummyPorts(ports []*Port) []*Port {
	growPorts(&ports, 3)
	if ports[0] == nil {
		ports[0] = DummyInputPort
	}
	if ports[1] == nil {
		ports[1] = DummyOutputPort
	}
	if ports[2] == nil {
		ports[2] = DummyOutputPort
	}
	return ports
}

// Check checks the given source code for any parse error, autofixes, and
// compilation error. It always tries to compile the code even if there is a
// parse error. If w is not nil, deprecation messages are written to it.
func (ev *Evaler) Check(src parse.Source, w io.Writer) (error, []string, error) {
	tree, parseErr := parse.Parse(src, parse.Config{WarningWriter: w})
	autofixes, compileErr := ev.CheckTree(tree, w)
	return parseErr, autofixes, compileErr
}

// CheckTree checks the given parsed source tree for autofixes and compilation
// errors. If w is not nil, deprecation messages are written to it.
//
// The default global namespace is read through GlobalSnapshot so that the
// compile-time view is metadata-consistent with concurrent default-global
// Eval transactions.
func (ev *Evaler) CheckTree(tree parse.Tree, w io.Writer) ([]string, error) {
	ev.mu.RLock()
	b, m := ev.builtin, ev.modules
	ev.mu.RUnlock()
	g := ev.GlobalSnapshot()
	_, autofixes, compileErr := compile(b.static(), g.static(), mapKeys(m), tree, w)
	return autofixes, compileErr
}
