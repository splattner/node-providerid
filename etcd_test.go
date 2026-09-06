package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func testEtcdClient(t *testing.T) *clientv3.Client {
	t.Helper()
	endpoint := os.Getenv("ETCD_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set ETCD_TEST_ENDPOINT to a disposable local etcd endpoint")
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func TestListNodes(t *testing.T) {
	cli := testEtcdClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("/node-providerid-test-list-%d/", time.Now().UnixNano())
	defer func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = cli.Delete(c, prefix, clientv3.WithPrefix())
	}()
	nodes := map[string]string{"node-a": "k3s://node-a", "node-b": "", "node-c": "aws:///z/i-1"}
	for name, id := range nodes {
		body := fmt.Sprintf(`{"apiVersion":"v1","kind":"Node","metadata":{"name":%q},"spec":{"providerID":%q}}`, name, id)
		if _, err := cli.Put(ctx, prefix+name, body); err != nil {
			t.Fatal(err)
		}
	}
	// A nested key (not a direct Node) must be ignored, not decoded.
	if _, err := cli.Put(ctx, prefix+"node-a/extra", "junk"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := listNodes(cli, prefix, 10*time.Second, &buf); err != nil {
		t.Fatalf("listNodes: %v", err)
	}
	want := "node-a\tk3s://node-a\nnode-b\t\nnode-c\taws:///z/i-1\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

// Run only against a disposable, local etcd instance. This test uses a random
// key outside /registry and removes it after testing successful and stale writes.
func TestEtcdCompareAndPut(t *testing.T) {
	cli := testEtcdClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	key := fmt.Sprintf("/node-providerid-test/%d", time.Now().UnixNano())
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = cli.Delete(cleanup, key)
	}()
	lease, err := cli.Grant(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := cli.Put(ctx, key, "old", clientv3.WithLease(lease.ID))
	if err != nil {
		t.Fatal(err)
	}
	first, err := compareAndPut(ctx, cli, key, initial.Header.Revision, int64(lease.ID), []byte("new"))
	if err != nil || !first.Succeeded {
		t.Fatalf("first write: %v %v", first, err)
	}
	stale, err := compareAndPut(ctx, cli, key, initial.Header.Revision, int64(lease.ID), []byte("stale"))
	if err != nil || stale.Succeeded {
		t.Fatalf("stale write: %v %v", stale, err)
	}
	read, err := cli.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Kvs) != 1 || string(read.Kvs[0].Value) != "new" || read.Kvs[0].Lease != int64(lease.ID) {
		t.Fatalf("lost data or lease: %v", read)
	}
	_, err = cli.Delete(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := compareAndPut(ctx, cli, key, first.Header.Revision, 0, []byte("resurrected"))
	if err != nil || deleted.Succeeded {
		t.Fatalf("recreated deleted key: %v %v", deleted, err)
	}
}
