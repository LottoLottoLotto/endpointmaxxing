package placement

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func flat(mib int64) func(int64) int64 { return func(int64) int64 { return mib } }

func TestPicksGPUWithRoomWithoutEvicting(t *testing.T) {
	gpus := []GPU{{0, 24000, 20000}, {1, 24000, 2000}}
	holders := []Holder{{ID: "a", Evictable: true, Idle: true, MiB: map[int]int64{0: 19000}}}
	d, err := Decide(gpus, nil, holders, Need{Count: 1, PerGPU: flat(8000)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.GPUs, []int{1}) || len(d.Evict) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestEvictsIdleLRUFirstAndOnlyAsMuchAsNeeded(t *testing.T) {
	now := time.Now()
	gpus := []GPU{{0, 24000, 23000}}
	holders := []Holder{
		{ID: "busy-old", Evictable: true, Idle: false, LastUsed: now.Add(-time.Hour), MiB: map[int]int64{0: 6000}},
		{ID: "idle-new", Evictable: true, Idle: true, LastUsed: now, MiB: map[int]int64{0: 6000}},
		{ID: "idle-old", Evictable: true, Idle: true, LastUsed: now.Add(-time.Hour), MiB: map[int]int64{0: 6000}},
		{ID: "pinned", Evictable: false, Idle: true, MiB: map[int]int64{0: 5000}},
	}
	d, err := Decide(gpus, nil, holders, Need{Count: 1, PerGPU: flat(6000)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Evict, []string{"idle-old"}) {
		t.Fatalf("evicted %v, want the least recently used idle instance only", d.Evict)
	}
}

func TestReservationsCountAndBlockersAreNamed(t *testing.T) {
	gpus := []GPU{{0, 24000, 4000}}
	holders := []Holder{{ID: "cli-model", Evictable: false, MiB: map[int]int64{0: 4000}}}
	// 20000 free, but a loading instance has 15000 reserved.
	_, err := Decide(gpus, map[int]int64{0: 15000}, holders, Need{Count: 1, PerGPU: flat(8000)})
	pe, ok := err.(*Error)
	if !ok || !strings.Contains(pe.Msg, "GPU 0 needs 8000 MiB, 5000 MiB free") || pe.Blockers[0] != "cli-model" {
		t.Fatalf("got %v", err)
	}
}

func TestFractionScalesWithGPUSizeAndTensorParallelSpansGPUs(t *testing.T) {
	gpus := []GPU{{0, 24000, 0}, {1, 48000, 30000}, {2, 24000, 1000}}
	frac := func(total int64) int64 { return total * 9 / 10 }
	d, err := Decide(gpus, nil, nil, Need{Count: 2, PerGPU: frac})
	if err != nil {
		t.Fatal(err)
	}
	// GPU 1 has 18000 free of 48000 and needs 43200: only {0,2} fits.
	if !reflect.DeepEqual(d.GPUs, []int{0, 2}) || d.PerGPU[0] != 21600 {
		t.Fatalf("got %+v", d)
	}
	if _, err := Decide(gpus, nil, nil, Need{Count: 4, PerGPU: frac}); err == nil {
		t.Fatal("4 GPUs placed on 3")
	}
}

func TestExclusiveEvictsEverythingEvictableOnTheGPU(t *testing.T) {
	gpus := []GPU{{0, 24000, 3000}}
	holders := []Holder{{ID: "small", Evictable: true, Idle: true, MiB: map[int]int64{0: 2000}}}
	d, err := Decide(gpus, nil, holders, Need{Count: 1, Exclusive: true})
	if err == nil {
		// 1000 MiB of non-emx memory remains; the slack absorbs it.
		if !reflect.DeepEqual(d.Evict, []string{"small"}) {
			t.Fatalf("got %+v", d)
		}
		return
	}
	t.Fatal(err)
}

func TestPinnedGPUsAreRespected(t *testing.T) {
	gpus := []GPU{{0, 24000, 0}, {1, 24000, 23000}}
	if _, err := Decide(gpus, nil, nil, Need{Count: 1, PerGPU: flat(4000), Pinned: []int{1}}); err == nil {
		t.Fatal("placed on a pinned GPU without room")
	}
	if _, err := Decide(gpus, nil, nil, Need{Count: 1, PerGPU: flat(1), Pinned: []int{7}}); err == nil {
		t.Fatal("accepted a GPU that does not exist")
	}
}
