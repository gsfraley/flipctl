package plugin

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Tier is the trust tier a registry entry was admitted under.
type Tier string

const (
	// TierSystem entries come from the daemon's -plugins catalog. They are
	// reviewed with the image and cannot be changed through the API.
	TierSystem Tier = "system"
	// TierInstalled entries were promoted from ephemeral and persist across
	// daemon restarts.
	TierInstalled Tier = "installed"
	// TierEphemeral entries were registered at runtime. They are deactivated
	// when the daemon starts unless it is told to keep them.
	TierEphemeral Tier = "ephemeral"
)

// Entry is one active app in the registry.
type Entry struct {
	App      App       `json:"app"`
	Tier     Tier      `json:"tier"`
	Source   string    `json:"source"`
	Manifest []byte    `json:"-"`
	Updated  time.Time `json:"updated"`
	// Granted is the capability set confirmed for this entry. For system
	// entries it equals the declared capabilities.
	Granted Capabilities `json:"granted"`
}

// Snapshot is an immutable, versioned view of the registry. Readers take a
// snapshot once per request and never observe a half-applied change.
type Snapshot struct {
	Version uint64
	Entries []Entry
}

// ByID returns the active entry with id.
func (s *Snapshot) ByID(id string) (Entry, bool) {
	for _, e := range s.Entries {
		if e.App.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Apps returns the active apps in registry order.
func (s *Snapshot) Apps() []App {
	out := make([]App, 0, len(s.Entries))
	for _, e := range s.Entries {
		out = append(out, e.App)
	}
	return out
}

var (
	ErrNotFound  = errors.New("plugin not found")
	ErrProtected = errors.New("plugin tier cannot be changed at runtime")
)

// Registry holds the authoritative set of active apps. Writes are serialized
// and published by atomically swapping in a new Snapshot; reads are lock-free.
type Registry struct {
	mu  sync.Mutex
	cur atomic.Pointer[Snapshot]
}

// NewRegistry returns a registry seeded with entries at version 1.
func NewRegistry(entries []Entry) *Registry {
	r := &Registry{}
	es := append([]Entry(nil), entries...)
	sortEntries(es)
	r.cur.Store(&Snapshot{Version: 1, Entries: es})
	return r
}

// SystemEntries wraps catalog apps as system-tier entries.
func SystemEntries(apps []App, source string) []Entry {
	out := make([]Entry, 0, len(apps))
	for _, a := range apps {
		out = append(out, Entry{App: a, Tier: TierSystem, Source: source, Granted: a.Capabilities})
	}
	return out
}

// Snapshot returns the current registry snapshot.
func (r *Registry) Snapshot() *Snapshot { return r.cur.Load() }

// Put adds or replaces e. System entries cannot be replaced, and a runtime
// entry cannot take over an id owned by a higher tier. It returns the previous
// entry (if any) and the new snapshot.
func (r *Registry) Put(e Entry) (prev *Entry, snap *Snapshot, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur.Load()
	next := make([]Entry, 0, len(cur.Entries)+1)
	for _, x := range cur.Entries {
		if x.App.ID != e.App.ID {
			next = append(next, x)
			continue
		}
		if x.Tier == TierSystem || (x.Tier == TierInstalled && e.Tier == TierEphemeral) {
			return nil, cur, fmt.Errorf("%w: %s is %s", ErrProtected, x.App.ID, x.Tier)
		}
		old := x
		prev = &old
	}
	if e.Updated.IsZero() {
		e.Updated = time.Now()
	}
	next = append(next, e)
	sortEntries(next)
	snap = &Snapshot{Version: cur.Version + 1, Entries: next}
	r.cur.Store(snap)
	return prev, snap, nil
}

// Remove deletes the runtime entry id.
func (r *Registry) Remove(id string) (Entry, *Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur.Load()
	next := make([]Entry, 0, len(cur.Entries))
	var removed *Entry
	for _, x := range cur.Entries {
		if x.App.ID == id {
			if x.Tier == TierSystem {
				return Entry{}, cur, fmt.Errorf("%w: %s is %s", ErrProtected, id, x.Tier)
			}
			old := x
			removed = &old
			continue
		}
		next = append(next, x)
	}
	if removed == nil {
		return Entry{}, cur, ErrNotFound
	}
	snap := &Snapshot{Version: cur.Version + 1, Entries: next}
	r.cur.Store(snap)
	return *removed, snap, nil
}

// Retier moves an existing runtime entry to tier (ephemeral -> installed).
func (r *Registry) Retier(id string, tier Tier, source string) (Entry, *Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur.Load()
	next := append([]Entry(nil), cur.Entries...)
	for i, x := range next {
		if x.App.ID != id {
			continue
		}
		if x.Tier == TierSystem {
			return Entry{}, cur, fmt.Errorf("%w: %s is %s", ErrProtected, id, x.Tier)
		}
		x.Tier, x.Source, x.Updated = tier, source, time.Now()
		next[i] = x
		sortEntries(next)
		snap := &Snapshot{Version: cur.Version + 1, Entries: next}
		r.cur.Store(snap)
		return x, snap, nil
	}
	return Entry{}, cur, ErrNotFound
}

var tierOrder = map[Tier]int{TierSystem: 0, TierInstalled: 1, TierEphemeral: 2}

// sortEntries keeps system apps first in catalog order, then installed and
// ephemeral apps by id, so projections are deterministic.
func sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		ti, tj := tierOrder[es[i].Tier], tierOrder[es[j].Tier]
		if ti != tj {
			return ti < tj
		}
		if es[i].Tier == TierSystem {
			return false
		}
		return es[i].App.ID < es[j].App.ID
	})
}
