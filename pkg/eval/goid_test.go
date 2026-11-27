package eval

import (
	"testing"
)

// The chdir hook chain (cd -> chdir -> callHookGoid -> callGoid ->
// globalSnapshotGoid) must reenter the running default-global transaction
// through the frame's cached goroutine identity instead of a runtime.Stack
// goroutine-ID lookup, whose cost grows with stack depth (#57).
func TestChdirHookSkipsGoroutineIDLookup(t *testing.T) {
	ev := NewEvaler()
	mustEval(t, ev, "var hook-ran = $false")
	mustEval(t, ev, "set @before-chdir = {|dst| set hook-ran = $true }")

	lookups := 0
	realGoroutineID := goroutineID
	goroutineID = func() uint64 {
		lookups++
		return realGoroutineID()
	}
	defer func() { goroutineID = realGoroutineID }()

	mustEval(t, ev, "cd .")

	// The only permitted lookup is the transaction owner note of the
	// top-level Eval itself; the hook chain must not add more.
	if lookups > 1 {
		t.Errorf("goroutine-ID lookup ran %d times during a hooked cd; want at most 1",
			lookups)
	}
	if v := ev.GlobalVar("hook-ran"); v == nil || v.Get() != true {
		t.Errorf("before-chdir hook did not run")
	}
}
