package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func wire(num protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), value)
}

func fixture(id *string) []byte {
	meta := append(wire(1, []byte("worker-1")), wire(100, []byte("unknown metadata"))...)
	spec := wire(1, []byte("10.42.0.0/24"))
	spec = append(spec, protowire.AppendVarint(protowire.AppendTag(nil, 4, protowire.VarintType), 1)...)
	if id != nil {
		spec = append(spec, wire(3, []byte(*id))...)
	}
	spec = append(spec, wire(101, []byte("unknown spec"))...)
	node := append(wire(1, meta), wire(2, spec)...)
	node = append(node, wire(3, wire(100, []byte("status untouched")))...)
	node = append(node, wire(102, []byte("unknown node"))...)
	typeMeta := append(wire(1, []byte("v1")), wire(2, []byte("Node"))...)
	env := append(wire(1, typeMeta), wire(2, node)...)
	env = append(env, wire(103, []byte("unknown envelope"))...)
	return append(append([]byte{}, magic...), env...)
}

func TestProtobufPreservation(t *testing.T) {
	old, next := "k3s://worker-1", "aws:///eu-central-1a/i-example"
	before := fixture(&old)
	current, after, err := nodeValue(before, "worker-1", &next)
	if err != nil || current != old {
		t.Fatalf("read: %q %v", current, err)
	}
	if !bytes.Equal(after, fixture(&next)) {
		t.Fatal("changed bytes outside providerID or its containing length prefixes")
	}
	current, _, err = nodeValue(after, "worker-1", nil)
	if err != nil || current != next {
		t.Fatalf("read updated: %q %v", current, err)
	}
	_, same, err := nodeValue(after, "worker-1", &next)
	if err != nil || !bytes.Equal(same, after) {
		t.Fatal("no-op changed bytes")
	}
}

func TestMissingAndEmptyProvider(t *testing.T) {
	next := "example://123"
	current, after, err := nodeValue(fixture(nil), "worker-1", &next)
	if err != nil || current != "" {
		t.Fatalf("set missing: %q %v", current, err)
	}
	empty := ""
	current, cleared, err := nodeValue(after, "worker-1", &empty)
	if err != nil || current != next {
		t.Fatalf("clear: %q %v", current, err)
	}
	current, _, err = nodeValue(cleared, "worker-1", nil)
	if err != nil || current != "" {
		t.Fatalf("read cleared: %q %v", current, err)
	}
}

func TestJSONPreservesUnknownAndNumbers(t *testing.T) {
	before := []byte(`{"apiVersion":"v1","kind":"Node","metadata":{"name":"worker-1","unknown":{"big":9007199254740993}},"spec":{"providerID":"old","future":[1,2,3]},"status":{"future":true}}`)
	next := "new"
	old, after, err := nodeValue(before, "worker-1", &next)
	if err != nil || old != "old" {
		t.Fatalf("%q %v", old, err)
	}
	if !bytes.Contains(after, []byte("9007199254740993")) {
		t.Fatal("large number lost precision")
	}
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &b)
	for _, key := range []string{"metadata", "status"} {
		if !bytes.Equal(a[key], b[key]) {
			t.Fatalf("changed %s", key)
		}
	}
	read, _, err := nodeValue(after, "worker-1", nil)
	if err != nil || read != next {
		t.Fatalf("%q %v", read, err)
	}
}

func TestRejectInvalid(t *testing.T) {
	id := "old"
	valid := fixture(&id)
	for _, data := range [][]byte{
		[]byte("k8s:enc:aescbc:v1:key:garbage"),
		[]byte("garbage"),
		valid[:len(valid)-1],
		append(append([]byte{}, valid...), wire(2, nil)...),
		[]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"worker-1"}}`),
		[]byte(`{"apiVersion":"v1","kind":"Node","metadata":{"name":"worker-1"},"spec":{"providerID":42}}`),
	} {
		if _, _, err := nodeValue(data, "worker-1", &id); err == nil {
			t.Fatalf("accepted invalid data: %x", data)
		}
	}
	if _, _, err := nodeValue(valid, "wrong-name", &id); err == nil {
		t.Fatal("accepted wrong Node name")
	}
	if _, err := field([]byte{0x18, 0x01}, 3); err == nil {
		t.Fatal("accepted wrong wire type")
	}
}

func TestBackupDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	if err := saveBackup(path, map[string]any{"value_base64": []byte{0, 1, 2}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := saveBackup(path, map[string]string{"replacement": "bad"}); err == nil {
		t.Fatal("overwrote existing backup")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("backup changed")
	}
}

func FuzzNodeValue(f *testing.F) {
	id := "example://123"
	f.Add(fixture(&id))
	f.Add([]byte(`{"apiVersion":"v1","kind":"Node","metadata":{"name":"worker-1"},"spec":{}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, after, err := nodeValue(data, "worker-1", &id)
		if err != nil {
			return
		}
		value, _, err := nodeValue(after, "worker-1", nil)
		if err != nil || value != id {
			t.Fatalf("invalid round trip: %q %v", value, err)
		}
	})
}
