package lpp

import (
	"sort"
	"sync"
	"time"
)

// SnapshotInfo is the metadata the controller keeps for each snapshot. It is
// also serialized into a JSON sidecar on the node (<basePath>/.snapshots/<id>.json)
// so the snapshot is self-describing from the node alone.
type SnapshotInfo struct {
	SnapshotID     string    `json:"snapshotID"`
	Name           string    `json:"name"`
	SourceVolumeID string    `json:"sourceVolumeID"`
	Node           string    `json:"node"`
	SourcePath     string    `json:"sourcePath"`
	SnapshotPath   string    `json:"snapshotPath"`
	FSType         string    `json:"fsType"`
	QuotaType      string    `json:"quotaType,omitempty"`
	SizeBytes      int64     `json:"sizeBytes"`
	CreationTime   time.Time `json:"creationTime"`
	ReadyToUse     bool      `json:"readyToUse"`
}

// SnapshotTracker is an in-memory index of snapshots, keyed by snapshot ID.
// It is authoritative for the controller's lifetime; on restart it starts
// empty and ListSnapshots will only see snapshots created since (a known
// limitation — most callers Create then immediately List/Delete within one
// controller lifetime). CreateVolume-from-snapshot and DeleteSnapshot remain
// correct after restart because they re-derive node/path from the
// VolumeSnapshotContent the csi-snapshotter passes in the request.
type SnapshotTracker struct {
	mu     sync.RWMutex
	all    map[string]SnapshotInfo // snapshotID -> info
	byName map[string]string       // snapshot name -> snapshotID (for the CSI "names are globally unique" rule)
}

func NewSnapshotTracker() *SnapshotTracker {
	return &SnapshotTracker{all: map[string]SnapshotInfo{}, byName: map[string]string{}}
}

func (t *SnapshotTracker) Put(s SnapshotInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.all[s.SnapshotID] = s
	if s.Name != "" {
		t.byName[s.Name] = s.SnapshotID
	}
}

func (t *SnapshotTracker) Get(id string) (SnapshotInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.all[id]
	return s, ok
}

// GetByName returns the snapshot currently registered under the given name.
// Used to enforce the CSI rule that snapshot names are globally unique.
func (t *SnapshotTracker) GetByName(name string) (SnapshotInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.byName[name]
	if !ok {
		return SnapshotInfo{}, false
	}
	s, ok := t.all[id]
	return s, ok
}

func (t *SnapshotTracker) Delete(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.all[id]; ok && s.Name != "" && t.byName[s.Name] == id {
		delete(t.byName, s.Name)
	}
	delete(t.all, id)
}

// List returns snapshots sorted by ID, optionally filtered by exact snapshot
// ID and/or source volume ID. Either filter may be "".
func (t *SnapshotTracker) List(snapshotID, sourceVolumeID string) []SnapshotInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]SnapshotInfo, 0, len(t.all))
	for _, s := range t.all {
		if snapshotID != "" && s.SnapshotID != snapshotID {
			continue
		}
		if sourceVolumeID != "" && s.SourceVolumeID != sourceVolumeID {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SnapshotID < out[j].SnapshotID })
	return out
}
