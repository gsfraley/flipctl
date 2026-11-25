package plugin

import (
	"errors"
	"sync"
	"testing"
)

func testEntry(id string, tier Tier, version string) Entry {
	return Entry{App: App{ID: id, Title: id, Version: version, Command: Command{Executable: "./x"}}, Tier: tier}
}

func TestRegistryReplaceAndRemoveSwapSnapshotsAtomically(t *testing.T) {
	r := NewRegistry([]Entry{testEntry("network.ping", TierSystem, "1")})
	before := r.Snapshot()
	prev, s2, err := r.Put(testEntry("field.lora", TierEphemeral, "0.1.0"))
	if err != nil || prev != nil || s2.Version != before.Version+1 {
		t.Fatalf("put: prev=%v version=%d err=%v", prev, s2.Version, err)
	}
	prev, s3, err := r.Put(testEntry("field.lora", TierEphemeral, "0.2.0"))
	if err != nil || prev == nil || prev.App.Version != "0.1.0" {
		t.Fatalf("replace: prev=%v err=%v", prev, err)
	}
	if e, _ := s3.ByID("field.lora"); e.App.Version != "0.2.0" || len(s3.Entries) != 2 {
		t.Fatalf("replace not applied: %#v", s3.Entries)
	}
	// Older snapshots are immutable: a reader holding one is unaffected.
	if _, ok := before.ByID("field.lora"); ok || len(s2.Entries) != 2 {
		t.Fatal("published snapshot was mutated")
	}
	if e, _ := s2.ByID("field.lora"); e.App.Version != "0.1.0" {
		t.Fatal("previous snapshot observed the replacement")
	}
	removed, s4, err := r.Remove("field.lora")
	if err != nil || removed.App.Version != "0.2.0" || len(s4.Entries) != 1 {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if _, _, err := r.Remove("field.lora"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove err=%v", err)
	}
}

func TestRegistryProtectsSystemAndInstalledTiers(t *testing.T) {
	r := NewRegistry([]Entry{testEntry("network.ping", TierSystem, "1"), testEntry("field.cal", TierInstalled, "1")})
	if _, _, err := r.Put(testEntry("network.ping", TierEphemeral, "2")); !errors.Is(err, ErrProtected) {
		t.Fatalf("ephemeral replaced system: %v", err)
	}
	if _, _, err := r.Put(testEntry("field.cal", TierEphemeral, "2")); !errors.Is(err, ErrProtected) {
		t.Fatalf("ephemeral replaced installed: %v", err)
	}
	if _, _, err := r.Remove("network.ping"); !errors.Is(err, ErrProtected) {
		t.Fatalf("system removed: %v", err)
	}
	if r.Snapshot().Version != 1 {
		t.Fatal("rejected writes bumped the version")
	}
}

func TestRegistryPromoteRetiersEntry(t *testing.T) {
	r := NewRegistry(nil)
	if _, _, err := r.Put(testEntry("field.lora", TierEphemeral, "1")); err != nil {
		t.Fatal(err)
	}
	e, s, err := r.Retier("field.lora", TierInstalled, "installed/field.lora.yaml")
	if err != nil || e.Tier != TierInstalled || s.Entries[0].Tier != TierInstalled {
		t.Fatalf("retier: %#v %v", e, err)
	}
}

func TestRegistryConcurrentWritersAndReaders(t *testing.T) {
	r := NewRegistry(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _, _ = r.Put(testEntry("field.x", TierEphemeral, "v"))
				_, _, _ = r.Remove("field.x")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s := r.Snapshot()
				if len(s.Entries) > 1 {
					t.Error("duplicate entries observed")
				}
			}
		}()
	}
	wg.Wait()
}
