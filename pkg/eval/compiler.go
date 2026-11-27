package eval

import (
	"fmt"
	"io"
	"strings"

	"src.elv.sh/pkg/diag"
	"src.elv.sh/pkg/eval/vars"
	"src.elv.sh/pkg/parse"
	"src.elv.sh/pkg/prog"
)

// compiler maintains the set of states needed when compiling a single source
// file.
type compiler struct {
	// Builtin namespace.
	builtin *staticNs
	// Lexical namespaces.
	scopes []*staticNs
	// Sources of captured variables.
	captures []*staticUpNs
	// Pragmas tied to scopes.
	pragmas []*scopePragma
	// Names of internal modules.
	modules []string
	// Destination of warning messages. This is currently only used for
	// deprecation messages.
	warn io.Writer
	// Deprecation registry.
	deprecations deprecationRegistry
	// Information about the source.
	src parse.Source
	// Whether declarations being compiled belong to an exception-capture chunk
	// (?(...)); their rollback is handled by the capture, not the enclosing
	// chunk.
	inCapture bool
	// Compilation errors.
	errors []*CompilationError
	// Suggested code to fix potential issues found during compilation.
	autofixes []string
}

type scopePragma struct {
	unknownCommandIsExternal bool
}

func compile(b, g *staticNs, modules []string, tree parse.Tree, w io.Writer) (nsOp, []string, error) {
	g = g.clone()
	cp := &compiler{
		b, []*staticNs{g}, []*staticUpNs{new(staticUpNs)},
		[]*scopePragma{{unknownCommandIsExternal: true}},
		modules,
		w, newDeprecationRegistry(), tree.Source, false, nil, nil}
	chunkOp := cp.chunkOp(tree.Root)
	return nsOp{chunkOp, g, g.mutated}, cp.autofixes, diag.PackErrors(cp.errors)
}

// rollbackAfterChunkException restores the namespace state after an exception:
// effects of ops that executed before the failure are kept, declarations that
// never executed are undone (tab-dance/tabdance-elvish#47, upstream #1445).
//
// Per variable name, the entry of the last op that executed wins: an executed
// declaration or assignment makes that entry the visible binding, reverting
// any shadow-deletion projected by later unexecuted forms, and an executed del
// leaves the name deleted (delLocalVarOp marks its entry hidden to distinguish
// an executed del from compile-time shadow-deletion metadata). Entries not
// touched by an executed op are restored to their pre-chunk state (below the
// chunk's rollback boundary) or tombstoned (at or beyond it), so rolled-back
// declarations are unresolvable both at runtime and to later compilations.
// Tombstones preserve the slot layout: forms still running in background
// pipelines hold the same namespace object, and truncating it in place would
// race or panic. The next compilation sweeps tombstones away (see
// Ns.withoutTombstones), so they do not accumulate.
//
// When capture is false (chunk-level rollback), execution of the code that
// failed has stopped, and all unexecuted declarations of the chunk are
// tombstoned. When capture is true (exception captured by ?(...)), execution
// continues in the enclosing chunk: only declarations belonging to the capture
// (marked captured at compile time) are tombstoned, while pending declarations
// of enclosing forms are left for their own ops.
func rollbackAfterChunkException(fm *Frame, capture bool) {
	// The rollback may create tombstones (via markEntryGone, or by flipping
	// losers to hidden+deleted); flag them for the pre-compile sweep. Setting
	// this conservatively is fine: a false positive costs one O(n) scan.
	fm.local.hasTombstones = true
	boundary := fm.rollbackBoundary
	infos := fm.local.infos
	for i := 0; i < boundary && i < len(fm.rollbackOrig); i++ {
		if infos[i].activated {
			continue
		}
		infos[i].hidden = fm.rollbackOrig[i].hidden
		infos[i].deleted = fm.rollbackOrig[i].deleted
	}
	byName := map[string][]int{}
	for i, info := range infos {
		byName[info.name] = append(byName[info.name], i)
	}
	for _, idxs := range byName {
		winner := -1
		for _, i := range idxs {
			if infos[i].activated {
				winner = i
			}
		}
		if winner == -1 {
			// Nothing executed for this name: pre-existing entries were
			// restored above; the chunk's own pending declarations are
			// tombstoned (when inside a failed capture, only those of the
			// capture itself).
			for _, i := range idxs {
				if i >= boundary {
					markEntryGone(infos, i, capture)
				}
			}
			continue
		}
		var finalHidden, finalDeleted bool
		if infos[winner].deleted && infos[winner].hidden {
			// Executed del: the name stays deleted.
			finalHidden, finalDeleted = true, true
		}
		for _, i := range idxs {
			if i == winner {
				infos[i].hidden = finalHidden
				infos[i].deleted = finalDeleted
				continue
			}
			infos[i].hidden = true
			if i >= boundary && !infos[i].activated {
				markEntryGone(infos, i, capture)
			} else {
				infos[i].deleted = true
			}
		}
	}
}

// Marks a never-executed entry at or beyond the chunk's rollback boundary as
// gone, by making it a tombstone: hidden, so that it is swept away by the next
// compilation (see Ns.withoutTombstones), and deleted, so that it is
// unresolvable by runtime and compile-time lookups until then. When the
// exception was captured by ?(...), only entries declared inside the capture
// are tombstoned; pending declarations of enclosing forms are left untouched,
// as their own ops may still run.
func markEntryGone(infos []staticVarInfo, i int, capture bool) {
	if !capture || infos[i].captured {
		infos[i].hidden = true
		infos[i].deleted = true
	}
}

type nsOp struct {
	inner    effectOp
	template *staticNs
	// layoutMutated records whether the compiled chunk declares, redeclares
	// or imports anything at the chunk level. Chunks that don't can neither
	// create pending declarations nor shadow existing ones, so preparing them
	// needs no rollback bookkeeping.
	layoutMutated bool
}

// Prepares the local namespace, and returns the namespace and a function for
// executing the inner effectOp. Mutates fm.local.
func (op nsOp) prepare(fm *Frame) (*Ns, func() Exception) {
	if !op.layoutMutated {
		// The chunk cannot change the namespace layout: it has no pending
		// declarations to roll back and no shadowed entries to restore, so
		// the existing namespace is shared as-is and execution skips the
		// rollback. Runtime effects on existing variables - set and del -
		// publish directly, which is what the rollback would keep anyway.
		// The rollback boundary sits above every entry, so a failed capture
		// inside such a chunk can only re-resolve entries to their current
		// state and tombstones nothing.
		fm.rollbackBoundary = len(fm.local.infos)
		fm.rollbackOrig = nil
		return fm.local, func() Exception { return op.inner.exec(fm) }
	}
	origInfos := append([]staticVarInfo(nil), fm.local.infos...)

	n := len(op.template.infos)
	if n < len(origInfos) {
		n = len(origInfos)
	}
	newLocal := &Ns{slots: make([]vars.Var, n), infos: make([]staticVarInfo, n)}
	copy(newLocal.slots, fm.local.slots)
	copy(newLocal.infos, origInfos)
	if len(op.template.infos) >= len(origInfos) {
		copy(newLocal.infos[:len(op.template.infos)], op.template.infos)
	}
	for i := 0; i < len(origInfos) && i < len(newLocal.infos); i++ {
		// Activation tracks execution in this chunk evaluation for rollback.
		// Pre-existing slots start unactivated; executed ops set it.
		//
		// Resetting activation can turn an executed del (hidden, deleted,
		// activated) into a sweepable tombstone, so keep the tombstone flag
		// accurate for the pre-compile sweep.
		if newLocal.infos[i].hidden && newLocal.infos[i].deleted {
			newLocal.hasTombstones = true
		}
		newLocal.infos[i].activated = false
	}
	for i := len(origInfos); i < len(op.template.infos); i++ {
		// Appended entries are published with the chunk's compile-time
		// projection, including shadow-deletions of earlier same-name entries
		// (visible only once their declaration op executes; rollback undoes
		// projections of forms that never ran).
		newLocal.infos[i].hidden = false
		newLocal.infos[i].activated = false
		// TODO: Take readOnly into account too
		newLocal.slots[i] = MakeVarFromName(newLocal.infos[i].name)
	}
	fm.local = newLocal
	fm.rollbackBoundary = len(origInfos)
	fm.rollbackOrig = origInfos
	exec := func() Exception {
		exc := op.inner.exec(fm)
		if exc != nil {
			rollbackAfterChunkException(fm, false)
		}
		return exc
	}
	return fm.local, exec
}

type CompilationError = diag.Error[CompilationErrorTag]

// CompilationErrorTag parameterizes [diag.Error] to define [CompilationError].
type CompilationErrorTag struct{}

func (CompilationErrorTag) ErrorTag() string { return "compilation error" }

// Reports a compilation error.
func (cp *compiler) errorpf(r diag.Ranger, format string, args ...any) {
	cp.errorpfInner(r, fmt.Sprintf(format, args...), false)
}

// Reports a compilation error, and mark it as partial iff the end of r happens
// to coincide with the end of the source code.
func (cp *compiler) errorpfPartial(r diag.Ranger, format string, args ...any) {
	cp.errorpfInner(r, fmt.Sprintf(format, args...), r.Range().To == len(cp.src.Code))
}

func (cp *compiler) errorpfInner(r diag.Ranger, msg string, partial bool) {
	cp.errors = append(cp.errors, &CompilationError{
		Message: msg,
		Context: *diag.NewContext(cp.src.Name, cp.src.Code, r),
		// TODO: This criteria is too strict and only captures a small subset of
		// partial compilation errors.
		Partial: partial,
	})
}

// UnpackCompilationErrors returns the constituent compilation errors if the
// given error contains one or more compilation errors. Otherwise it returns
// nil.
func UnpackCompilationErrors(e error) []*CompilationError {
	if errs := diag.UnpackErrors[CompilationErrorTag](e); len(errs) > 0 {
		return errs
	}
	return nil
}

func (cp *compiler) thisScope() *staticNs {
	return cp.scopes[len(cp.scopes)-1]
}

func (cp *compiler) currentPragma() *scopePragma {
	return cp.pragmas[len(cp.pragmas)-1]
}

func (cp *compiler) pushScope() (*staticNs, *staticUpNs) {
	sc := new(staticNs)
	up := new(staticUpNs)
	cp.scopes = append(cp.scopes, sc)
	cp.captures = append(cp.captures, up)
	currentPragmaCopy := *cp.currentPragma()
	cp.pragmas = append(cp.pragmas, &currentPragmaCopy)
	return sc, up
}

func (cp *compiler) popScope() {
	cp.scopes[len(cp.scopes)-1] = nil
	cp.scopes = cp.scopes[:len(cp.scopes)-1]
	cp.captures[len(cp.captures)-1] = nil
	cp.captures = cp.captures[:len(cp.captures)-1]
	cp.pragmas[len(cp.pragmas)-1] = nil
	cp.pragmas = cp.pragmas[:len(cp.pragmas)-1]
}

func (cp *compiler) checkDeprecatedBuiltin(name string, r diag.Ranger) {
	msg := ""
	minLevel := 22
	switch name {
	// We don't have any deprecated builtins targeted for 0.22 yet, but keep
	// this code here so that the code doesn't get stale. This function is only
	// called for symbols that actually resolve to builtins, so having a fake
	// one here is harmless.
	case "foo~":
		msg = `the "foo" command is deprecated; use "bar" instead`
	default:
		return
	}
	cp.deprecate(r, msg, minLevel)
}

type deprecationTag struct{}

func (deprecationTag) ErrorTag() string { return "deprecation" }

func (cp *compiler) deprecate(r diag.Ranger, msg string, minLevel int) {
	if cp.warn == nil || r == nil {
		return
	}
	dep := deprecation{cp.src.Name, r.Range(), msg}
	if prog.DeprecationLevel >= minLevel && cp.deprecations.register(dep) {
		err := diag.Error[deprecationTag]{
			Message: msg,
			Context: *diag.NewContext(cp.src.Name, cp.src.Code, r.Range())}
		fmt.Fprintln(cp.warn, err.Show(""))
	}
}

// Given a variable that doesn't resolve, add any applicable autofixes.
func (cp *compiler) autofixUnresolvedVar(qname string) {
	if len(cp.modules) == 0 {
		return
	}
	first, _ := SplitQName(qname)
	mod := strings.TrimSuffix(first, ":")
	if mod != first && sliceContains(cp.modules, mod) {
		cp.autofixes = append(cp.autofixes, "use "+mod)
	}
}
