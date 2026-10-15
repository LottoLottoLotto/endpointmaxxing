package placement

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestChipsAlignToTraysOnAGalaxy(t *testing.T) {
	chips := make([]int, 32)
	for i := range chips {
		chips[i] = i
	}
	// Tray 0 is partly used by another job, tray 1 holds an evictable model: tray 2 is free.
	ext := map[int]bool{3: true}
	holders := []Holder{{ID: "a", Evictable: true, Idle: true, MiB: map[int]int64{8: 1, 9: 1}}}
	d, err := DecideChips(chips, ext, holders, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.GPUs, []int{16, 17, 18, 19, 20, 21, 22, 23}) || len(d.Evict) != 0 {
		t.Fatalf("got %v evict %v, want the free tray 16-23", d.GPUs, d.Evict)
	}
}

func TestChipsEvictIdleLRUFirst(t *testing.T) {
	now := time.Now()
	holders := []Holder{
		{ID: "busy", Evictable: true, Idle: false, LastUsed: now.Add(-time.Hour), MiB: map[int]int64{0: 1}},
		{ID: "idle-new", Evictable: true, Idle: true, LastUsed: now, MiB: map[int]int64{1: 1}},
		{ID: "idle-old", Evictable: true, Idle: true, LastUsed: now.Add(-time.Hour), MiB: map[int]int64{2: 1}},
	}
	d, err := DecideChips([]int{0, 1, 2}, nil, holders, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Evict, []string{"idle-old"}) {
		t.Fatalf("evicted %v", d.Evict)
	}
}

func TestChipsSingleCardBlockedByOtherJobOrPinnedModel(t *testing.T) {
	_, err := DecideChips([]int{0}, map[int]bool{0: true}, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "in use by another job") {
		t.Fatalf("got %v", err)
	}
	_, err = DecideChips([]int{0}, nil, []Holder{{ID: "cli-model", MiB: map[int]int64{0: 1}}}, 1)
	pe, ok := err.(*Error)
	if !ok || pe.Blockers[0] != "cli-model" {
		t.Fatalf("got %v", err)
	}
	if _, err := DecideChips([]int{0}, nil, nil, 4); err == nil {
		t.Fatal("4 chips placed on 1")
	}
}
