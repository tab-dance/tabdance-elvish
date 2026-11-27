package eval

import (
	"fmt"
	"unsafe"

	"src.elv.sh/pkg/eval/vars"
	"src.elv.sh/pkg/persistent/hash"
)

// Ns is the runtime representation of a namespace. The zero value of Ns is an
// empty namespace. To create a non-empty Ns, use either NsBuilder or CombineNs.
//
// An Ns is immutable after its associated code chunk has finished execution.
type Ns struct {
	// All variables in the namespace. Static variable accesses are compiled
	// into indexed accesses into this slice.
	slots []vars.Var
	// Static information for each variable, reflecting the state when the
	// associated code chunk has finished execution.
	//
	// This is only used for introspection and seeding the compilation of a new
	// code chunk. Normal static variable accesses are compiled into indexed
	// accesses into the slots slice.
	//
	// This is a slice instead of a map with the names of variables as keys,
	// because most namespaces are small enough for linear lookup to be faster
	// than map access.
	infos []staticVarInfo
	// hasTombstones is true when infos may contain tombstones of rolled-back
	// declarations (hidden, deleted, never activated) that the pre-compile
	// sweep (withoutTombstones) should remove. It lets the sweep skip its
	// O(n) scan in the common case; a false positive only costs one scan.
	hasTombstones bool
}

// Nser is anything that can be converted to an *Ns.
type Nser interface {
	Ns() *Ns
}

// Static information known about a variable.
type staticVarInfo struct {
	name     string
	readOnly bool
	// Deleted variables can still be kept in the Ns since there might be a
	// reference to them in a closure. Shadowed variables are also considered
	// deleted. A chunk publishes the compile-time shadow deletions of its
	// template; rollback reverts the deletions projected by forms that never
	// executed. An executed del marks the entry hidden as well, so that
	// rollback can tell it apart from such a projection.
	deleted bool
	// Hidden variables exist in the namespace slot layout but are not visible
	// to Elvish name lookup. Declarations start hidden in function scopes and
	// become visible when their declaration op executes. Rollback of a failed
	// chunk hides entries that lost to an executed op or never executed; such
	// entries are also deleted, making them tombstones that the next
	// compilation sweeps away (see Ns.withoutTombstones).
	hidden bool
	// Activated means the declaration, assignment, or deletion for this slot has
	// executed in the current chunk evaluation. Rollback preserves activated
	// slots and restores pre-existing hidden/deleted metadata for the rest.
	activated bool
	// Captured marks a declaration compiled inside an exception-capture chunk
	// (?(...)). When the capture fails, such declarations are tombstoned by
	// the capture's rollback; declarations of enclosing forms are left for
	// their own ops.
	captured bool
}

// CombineNs returns an *Ns that contains all the bindings from both ns1 and
// ns2. Names in ns2 takes precedence over those in ns1.
func CombineNs(ns1, ns2 *Ns) *Ns {
	ns := ns2.clone()

	hasName := map[string]bool{}
	for _, info := range ns.infos {
		if !info.deleted {
			hasName[info.name] = true
		}
	}
	for i, info := range ns1.infos {
		if !info.deleted && !hasName[info.name] {
			ns.slots = append(ns.slots, ns1.slots[i])
			ns.infos = append(ns.infos, info)
		}
	}
	return ns
}

func (ns *Ns) clone() *Ns {
	return &Ns{
		slots:         append([]vars.Var(nil), ns.slots...),
		infos:         append([]staticVarInfo(nil), ns.infos...),
		hasTombstones: ns.hasTombstones}
}

// Ns returns ns itself.
func (ns *Ns) Ns() *Ns {
	return ns
}

// Kind returns "ns".
func (ns *Ns) Kind() string {
	return "ns"
}

// Hash returns a hash of the address of ns.
func (ns *Ns) Hash() uint32 {
	return hash.Pointer(unsafe.Pointer(ns))
}

// Equal returns whether rhs has the same identity as ns.
func (ns *Ns) Equal(rhs any) bool {
	if ns2, ok := rhs.(*Ns); ok {
		return ns == ns2
	}
	return false
}

// Repr returns an opaque representation of the Ns showing its address.
func (ns *Ns) Repr(int) string {
	return fmt.Sprintf("<ns %p>", ns)
}

// Index looks up a variable with the given name, and returns its value if it
// exists. This is only used for introspection from Elvish code; for
// introspection from Go code, use IndexString.
func (ns *Ns) Index(k any) (any, bool) {
	if ks, ok := k.(string); ok {
		variable := ns.IndexString(ks)
		if variable == nil {
			return nil, false
		}
		return variable.Get(), true
	}
	return nil, false
}

// IndexString looks up a variable with the given name, and returns its value if
// it exists, or nil if it does not. This is the type-safe version of Index and
// is useful for introspection from Go code.
func (ns *Ns) IndexString(k string) vars.Var {
	_, i := ns.lookup(k)
	if i != -1 {
		return ns.slots[i]
	}
	return nil
}

func (ns *Ns) lookup(k string) (staticVarInfo, int) {
	for i, info := range ns.infos {
		if info.name == k && !info.deleted && !info.hidden {
			return info, i
		}
	}
	return staticVarInfo{}, -1
}

// IterateKeys produces the names of all the variables in this Ns.
func (ns *Ns) IterateKeys(f func(any) bool) {
	for _, info := range ns.infos {
		if info.deleted || info.hidden {
			continue
		}
		if !f(info.name) {
			break
		}
	}
}

// IterateKeysString produces the names of all variables in the Ns. It is the
// type-safe version of IterateKeys and is useful for introspection from Go
// code. It doesn't support breaking early.
func (ns *Ns) IterateKeysString(f func(string)) {
	for _, info := range ns.infos {
		if !info.deleted && !info.hidden {
			f(info.name)
		}
	}
}

// HasKeyString reports whether the Ns has a variable with the given name.
func (ns *Ns) HasKeyString(k string) bool {
	for _, info := range ns.infos {
		if info.name == k && !info.deleted && !info.hidden {
			return true
		}
	}
	return false
}

func (ns *Ns) static() *staticNs {
	return &staticNs{infos: ns.infos}
}

// withoutTombstones returns a namespace without tombstones left by rolled-back
// declarations: entries that are hidden, deleted and never activated. Such
// entries were never visible to running code, so no closure can reference
// them; live closures hold variable objects directly, not namespace entries.
// If there is nothing to sweep, ns itself is returned (and its tombstone flag
// is cleared, so the next call skips the scan).
func (ns *Ns) withoutTombstones() *Ns {
	if !ns.hasTombstones {
		return ns
	}
	sweepable := false
	for _, info := range ns.infos {
		if info.hidden && info.deleted && !info.activated {
			sweepable = true
			break
		}
	}
	if !sweepable {
		ns.hasTombstones = false
		return ns
	}
	kept := 0
	swept := &Ns{slots: make([]vars.Var, len(ns.infos)), infos: make([]staticVarInfo, len(ns.infos))}
	for i, info := range ns.infos {
		if info.hidden && info.deleted && !info.activated {
			continue
		}
		swept.slots[kept] = ns.slots[i]
		swept.infos[kept] = info
		kept++
	}
	swept.slots = swept.slots[:kept:kept]
	swept.infos = swept.infos[:kept:kept]
	return swept
}

// NsBuilder is a helper type used for building an Ns.
type NsBuilder struct {
	prefix string
	m      map[string]vars.Var
}

// BuildNs returns a helper for building an Ns.
func BuildNs() NsBuilder {
	return BuildNsNamed("")
}

// BuildNsNamed returns a helper for building an Ns with the given name. The name is
// only used for the names of Go functions.
func BuildNsNamed(name string) NsBuilder {
	prefix := ""
	if name != "" {
		prefix = "<" + name + ">:"
	}
	return NsBuilder{prefix, make(map[string]vars.Var)}
}

// AddVar adds a variable.
func (nb NsBuilder) AddVar(name string, v vars.Var) NsBuilder {
	nb.m[name] = v
	return nb
}

// AddVars adds all the variables given in the map.
func (nb NsBuilder) AddVars(m map[string]vars.Var) NsBuilder {
	for name, v := range m {
		nb.AddVar(name, v)
	}
	return nb
}

// AddFn adds a function. The resulting variable will be read-only.
func (nb NsBuilder) AddFn(name string, v Callable) NsBuilder {
	return nb.AddVar(name+FnSuffix, vars.NewReadOnly(v))
}

// AddNs adds a sub-namespace. The resulting variable will be read-only.
func (nb NsBuilder) AddNs(name string, v Nser) NsBuilder {
	return nb.AddVar(name+NsSuffix, vars.NewReadOnly(v.Ns()))
}

// AddGoFn adds a Go function. The resulting variable will be read-only.
func (nb NsBuilder) AddGoFn(name string, impl any) NsBuilder {
	return nb.AddFn(name, NewGoFn(nb.prefix+name, impl))
}

// AddGoFns adds Go functions. The resulting variables will be read-only.
func (nb NsBuilder) AddGoFns(fns map[string]any) NsBuilder {
	for name, impl := range fns {
		nb.AddGoFn(name, impl)
	}
	return nb
}

// Ns builds a namespace.
func (nb NsBuilder) Ns() *Ns {
	n := len(nb.m)
	ns := &Ns{slots: make([]vars.Var, n), infos: make([]staticVarInfo, n)}
	i := 0
	for name, variable := range nb.m {
		ns.slots[i] = variable
		ns.infos[i] = staticVarInfo{name, vars.IsReadOnly(variable), false, false, true, false}
		i++
	}
	return ns
}

// The compile-time representation of a namespace. Called "static" namespace
// since it contains information that are known without executing the code.
// The data structure itself, however, is not static, and gets mutated as the
// compiler gains more information about the namespace. The zero value of
// staticNs is an empty namespace.
type staticNs struct {
	infos []staticVarInfo
	// mutated records whether compilation has added or shadowed any entry in
	// this scope. Chunk-level scopes start unmutated, and nsOp.prepare uses
	// the flag to skip rollback bookkeeping for chunks that cannot change
	// the namespace layout.
	mutated bool
}

func (ns *staticNs) clone() *staticNs {
	return &staticNs{infos: append([]staticVarInfo(nil), ns.infos...)}
}

func (ns *staticNs) del(k string) {
	if _, i := ns.lookup(k); i != -1 {
		ns.infos[i].deleted = true
		ns.infos[i].hidden = false
		ns.mutated = true
	}
}

// Adds a name, shadowing any existing one, and returns the index for the new
// name.
func (ns *staticNs) add(k string) int {
	return ns.addWith(k, false, false)
}

func (ns *staticNs) addWith(k string, hidden, captured bool) int {
	ns.del(k)
	ns.mutated = true
	ns.infos = append(ns.infos, staticVarInfo{k, false, false, hidden, !hidden, captured})
	return len(ns.infos) - 1
}

func (ns *staticNs) hasVisible(k string) bool {
	_, i := ns.lookup(k)
	return i != -1
}

func (ns *staticNs) lookup(k string) (staticVarInfo, int) {
	for i, info := range ns.infos {
		if info.name == k && !info.deleted {
			return info, i
		}
	}
	return staticVarInfo{}, -1
}

type staticUpNs struct {
	infos []upvalInfo
}

type upvalInfo struct {
	name string
	// Whether the upvalue comes from the immediate outer scope, i.e. the local
	// scope a lambda is evaluated in.
	local bool
	// Index of the upvalue variable. If local is true, this is an index into
	// the local scope. If local is false, this is an index into the up scope.
	index int
}

func (up *staticUpNs) add(k string, local bool, index int) int {
	for i, info := range up.infos {
		if info.name == k {
			return i
		}
	}
	up.infos = append(up.infos, upvalInfo{k, local, index})
	return len(up.infos) - 1
}
