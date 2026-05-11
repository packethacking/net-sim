package router

import (
	"reflect"
	"testing"

	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/events"
)

// TestSummariseSourcesStableOrder: the (key, list) pair returned must
// be deterministic across reorderings of the input — the rxFeeder uses
// the key for "did the source-set change" dedup, so two ticks with the
// same sources in different orders must not look like a change.
func TestSummariseSourcesStableOrder(t *testing.T) {
	qa := &linkQueue{src: config.PortRef{NodeID: "a", PortID: "vhf"}}
	qb := &linkQueue{src: config.PortRef{NodeID: "b", PortID: "vhf"}}
	qc := &linkQueue{src: config.PortRef{NodeID: "c", PortID: "vhf"}}

	k1, l1 := summariseSources([]*linkQueue{qa, qb, qc})
	k2, l2 := summariseSources([]*linkQueue{qc, qa, qb})
	k3, l3 := summariseSources([]*linkQueue{qb, qc, qa})

	if k1 != k2 || k2 != k3 {
		t.Errorf("keys differ across permutations: %q, %q, %q", k1, k2, k3)
	}
	if !reflect.DeepEqual(l1, l2) || !reflect.DeepEqual(l2, l3) {
		t.Errorf("lists differ across permutations: %v / %v / %v", l1, l2, l3)
	}
	want := []string{"a.vhf", "b.vhf", "c.vhf"}
	if !reflect.DeepEqual(l1, want) {
		t.Errorf("list = %v, want %v", l1, want)
	}
	if k1 != "a.vhf|b.vhf|c.vhf" {
		t.Errorf("key = %q, want a.vhf|b.vhf|c.vhf", k1)
	}
}

func TestSummariseSourcesEmpty(t *testing.T) {
	k, l := summariseSources(nil)
	if k != "" || l != nil {
		t.Errorf("empty input → got key=%q list=%v, want empty/nil", k, l)
	}
}

func TestSummariseSourcesSingle(t *testing.T) {
	q := &linkQueue{src: config.PortRef{NodeID: "x", PortID: "y"}}
	k, l := summariseSources([]*linkQueue{q})
	if k != "x.y" || !reflect.DeepEqual(l, []string{"x.y"}) {
		t.Errorf("got key=%q list=%v", k, l)
	}
}

// TestEventBusOnRouterIsOptional makes sure that constructing a Router
// without an EventBus doesn't break anything (existing setups). We
// can't easily exercise the full audio path in a unit test, but this
// confirms the type wiring compiles and the field is honoured as nil.
func TestEventBusOnRouterIsOptional(t *testing.T) {
	r := &Router{opts: Options{}, logger: quietLogger()}
	if r.opts.EventBus != nil {
		t.Errorf("EventBus default = %v, want nil", r.opts.EventBus)
	}
	// And that storing one is fine.
	b := events.NewBus()
	r.opts.EventBus = b
	if r.opts.EventBus != b {
		t.Error("EventBus assignment didn't stick")
	}
}
