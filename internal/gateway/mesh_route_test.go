package gateway

import "testing"

// Routing between machines: with measured speeds, the machine that would finish the request sooner wins,
// even when it is busier; without them, the one with fewer requests ahead per slot.
func TestRouteCandidatesPreferTheSoonerMachine(t *testing.T) {
	cpu := routeCand{name: "cpu", busy: 0, serves: 1, speed: 30}
	gpu := routeCand{name: "gpu", busy: 3, serves: 1, speed: 400} // three ahead, still sooner: 4/400 < 1/30
	cs := []routeCand{cpu, gpu}
	sortCands(cs)
	if cs[0].name != "gpu" {
		t.Fatalf("by speed: %v first", cs[0].name)
	}
	slowGPU := routeCand{name: "gpu", busy: 20, serves: 1, speed: 400} // 21/400 > 1/30
	cs = []routeCand{slowGPU, cpu}
	sortCands(cs)
	if cs[0].name != "cpu" {
		t.Fatalf("a long queue must lose to an idle machine: %v first", cs[0].name)
	}
	unmeasured := routeCand{name: "new", busy: 0, serves: 1}
	cs = []routeCand{gpu, unmeasured}
	sortCands(cs)
	if cs[0].name != "new" {
		t.Fatalf("without every speed, requests ahead decide: %v first", cs[0].name)
	}
	shared := routeCand{name: "shared", busy: 7, serves: 8, speed: 100} // a free slot: no round to wait
	single := routeCand{name: "single", busy: 1, serves: 1, speed: 100}
	cs = []routeCand{single, shared}
	sortCands(cs)
	if cs[0].name != "shared" {
		t.Fatalf("a free slot beats a queue at the same speed: %v first", cs[0].name)
	}
}

func TestSharedSettingPaths(t *testing.T) {
	for path, want := range map[string]bool{
		"/emx/v1/keys": true, "/emx/v1/keys/alice/rotate": true, "/emx/v1/teams/ml": true, "/emx/v1/prices/q*": true,
		"/emx/v1/aliases/chat": true, "/emx/v1/models": false, "/emx/v1/clients/bob/data": false,
	} {
		if sharedSetting(path) != want {
			t.Errorf("%s: %v", path, !want)
		}
	}
}
