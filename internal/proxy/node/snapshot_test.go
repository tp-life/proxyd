package node

import (
	"os"
	"path/filepath"
	"testing"
)

func testNodes() []*Node {
	return []*Node{
		{
			Name:         "节点A",
			Subscription: "sub1",
			Mapping:      map[string]any{"name": "节点A", "type": "ss", "server": "1.2.3.4", "port": 8388, "cipher": "aes-128-gcm", "password": "pw"},
			Alive:        true,
			Delay:        123,
		},
		{
			Name:         "节点B",
			Subscription: "manual",
			Mapping:      map[string]any{"name": "节点B", "type": "http", "server": "5.6.7.8", "port": 8080, "username": "u", "password": "p"},
			Alive:        false,
			FailReason:   "connect error: timeout",
		},
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.json")
	nodes := testNodes()
	if err := SaveSnapshot(path, nodes); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	snap, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap == nil || len(snap.Nodes) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Version != SnapshotVersion {
		t.Errorf("version = %d, want %d", snap.Version, SnapshotVersion)
	}
	a := snap.Nodes[0]
	if a.Name != "节点A" || a.Subscription != "sub1" || !a.Alive || a.Delay != 123 {
		t.Errorf("node A: %+v", a)
	}
	if a.Mapping["type"] != "ss" || a.Mapping["name"] != "节点A" {
		t.Errorf("node A mapping: %+v", a.Mapping)
	}
	if a.Key() != nodes[0].Key() {
		t.Errorf("Key 不稳定: %q vs %q", a.Key(), nodes[0].Key())
	}
	b := snap.Nodes[1]
	if b.Alive || b.FailReason != "connect error: timeout" {
		t.Errorf("node B: %+v", b)
	}
}

func TestLoadSnapshotMissing(t *testing.T) {
	snap, err := LoadSnapshot(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil || snap != nil {
		t.Fatalf("missing file should return (nil, nil), got %+v, %v", snap, err)
	}
}

func TestLoadSnapshotCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSnapshot(path)
	if err == nil || snap != nil {
		t.Fatalf("corrupt file should return error, got %+v, %v", snap, err)
	}
}

func TestLoadSnapshotFutureVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.json")
	if err := os.WriteFile(path, []byte(`{"version":999,"nodes":[{"name":"x","mapping":{"type":"direct"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSnapshot(path)
	if err == nil || snap != nil {
		t.Fatalf("future version should return error, got %+v, %v", snap, err)
	}
}

func TestLoadSnapshotSkipsInvalidNodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.json")
	body := `{"version":1,"nodes":[{"name":"","mapping":{"type":"direct"}},{"name":"ok","mapping":null},{"name":"ok","mapping":{"type":"direct"}}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].Name != "ok" {
		t.Fatalf("invalid nodes should be skipped: %+v", snap.Nodes)
	}
	if snap.Nodes[0].Mapping["name"] != "ok" {
		t.Errorf("Mapping[name] 未同步: %+v", snap.Nodes[0].Mapping)
	}
}
