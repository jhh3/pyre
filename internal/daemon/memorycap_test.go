package daemon

import (
	"context"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// A node whose Hive answers advertises memory.hive at hello, so the hub can
// route other nodes' memory searches to it; one without Hive does not.
func TestHelloAdvertisesHiveMemory(t *testing.T) {
	for _, has := range []bool{true, false} {
		n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()},
			MemoryAvailable: func(context.Context) bool { return has }})
		hello, err := n.d.Hello(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := contracts.HasCap(hello.Capabilities, contracts.NodeCapMemoryHive); got != has {
			t.Fatalf("hive %v: capabilities %v", has, hello.Capabilities)
		}
	}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}})
	if hello, _ := n.d.Hello(context.Background()); contracts.HasCap(hello.Capabilities, contracts.NodeCapMemoryHive) {
		t.Fatal("advertised without a memory hook")
	}
}
