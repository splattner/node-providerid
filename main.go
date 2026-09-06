package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: node-providerid get|set|list [flags]; use <command> -h for flags")
	}
	command := args[0]
	switch command {
	case "-h", "--help":
		fmt.Println("Usage: node-providerid get|set|list [flags]\nUse get -h, set -h or list -h for flags.")
		return nil
	case "-v", "--version", "version":
		fmt.Println(version)
		return nil
	case "get", "set", "list":
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	endpoints := f.String("endpoints", "https://127.0.0.1:2379", "comma-separated etcd client endpoints")
	prefix := f.String("prefix", "/registry", "Kubernetes etcd storage prefix")
	ca := f.String("cacert", "/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt", "trusted etcd CA certificate")
	cert := f.String("cert", "/var/lib/rancher/k3s/server/tls/etcd/server-client.crt", "etcd client certificate")
	key := f.String("key", "/var/lib/rancher/k3s/server/tls/etcd/server-client.key", "etcd client private key")
	timeout := f.Duration("timeout", 10*time.Second, "timeout for each etcd operation")
	var node, providerID, expected, backup string
	var dryRun bool
	if command != "list" {
		f.StringVar(&node, "node", "", "Kubernetes Node name (required)")
	}
	if command == "set" {
		f.StringVar(&providerID, "provider-id", "", "new providerID; explicitly pass an empty string to clear")
		f.StringVar(&expected, "expect", "", "optional expected current providerID")
		f.StringVar(&backup, "backup", "", "required output JSON backup file; must not already exist")
		f.BoolVar(&dryRun, "dry-run", false, "show proposed change without writing or creating a backup")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", f.Args())
	}
	seen := map[string]bool{}
	f.Visit(func(v *flag.Flag) { seen[v.Name] = true })
	if command != "list" {
		validName := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
		if len(node) > 253 || !validName.MatchString(node) {
			return errors.New("--node must be a nonempty DNS subdomain Node name")
		}
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if command == "set" {
		if !seen["provider-id"] {
			return errors.New("--provider-id is required (empty string is allowed)")
		}
		if !dryRun && backup == "" {
			return errors.New("--backup is required for set without --dry-run")
		}
	}
	if !strings.HasPrefix(*prefix, "/") || strings.Contains(*prefix, "//") {
		return errors.New("--prefix must be an absolute etcd prefix without //")
	}
	// Kubernetes retains the historical 'minions' storage resource name.
	keyPrefix := strings.TrimRight(*prefix, "/") + "/minions/"
	cli, eps, err := dialEtcd(*ca, *cert, *key, *endpoints, *timeout)
	if err != nil {
		return err
	}
	defer cli.Close()

	if command == "list" {
		return listNodes(cli, keyPrefix, *timeout, os.Stdout)
	}

	etcdKey := keyPrefix + node
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	resp, err := cli.Get(ctx, etcdKey)
	cancel()
	if err != nil {
		return fmt.Errorf("read etcd: %w", err)
	}
	if len(resp.Kvs) != 1 {
		return fmt.Errorf("Node not found at %s (check name, endpoints and --prefix)", etcdKey)
	}
	kv := resp.Kvs[0]
	var next *string
	if command == "set" {
		next = &providerID
	}
	current, updated, err := nodeValue(kv.Value, node, next)
	if err != nil {
		return err
	}
	if command == "get" {
		fmt.Println(current)
		return nil
	}
	if seen["expect"] && current != expected {
		return fmt.Errorf("providerID mismatch: current %q, expected %q", current, expected)
	}
	if current == providerID {
		fmt.Printf("unchanged: %s providerID=%q\n", node, current)
		return nil
	}
	if dryRun {
		fmt.Printf("dry-run: %s providerID %q -> %q (mod_revision=%d)\n", etcdKey, current, providerID, kv.ModRevision)
		return nil
	}
	// Durable per-key backup is created before the conditional write.
	if err := saveBackup(backup, map[string]any{
		"key": etcdKey, "node": node, "providerID": current,
		"value_base64": kv.Value, "mod_revision": kv.ModRevision,
		"create_revision": kv.CreateRevision, "lease": kv.Lease,
		"cluster_id":   fmt.Sprint(resp.Header.ClusterId),
		"endpoints":    eps,
		"tool_version": version,
		"saved_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	txn, err := compareAndPut(ctx, cli, etcdKey, kv.ModRevision, kv.Lease, updated)
	if err != nil {
		return fmt.Errorf("etcd transaction failed (outcome may be unknown; run get before retrying): %w", err)
	}
	if !txn.Succeeded {
		return errors.New("Node changed concurrently; nothing was written by this attempt; read again and retry with a new backup path")
	}
	fmt.Printf("updated: %s providerID %q -> %q (revision=%d, backup=%s)\n", node, current, providerID, txn.Header.Revision, backup)
	return nil
}

// listNodes prints "<name>\t<providerID>" for every stored Node, sorted by name.
// It is read-only. Entries that cannot be decoded are reported on stderr and
// cause a non-zero exit without stopping the listing.
func listNodes(cli *clientv3.Client, keyPrefix string, timeout time.Duration, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	resp, err := cli.Get(ctx, keyPrefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	cancel()
	if err != nil {
		return fmt.Errorf("read etcd: %w", err)
	}
	failed := false
	for _, kv := range resp.Kvs {
		name := strings.TrimPrefix(string(kv.Key), keyPrefix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		current, _, err := nodeValue(kv.Value, name, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", name, err)
			failed = true
			continue
		}
		fmt.Fprintf(out, "%s\t%s\n", name, current)
	}
	if failed {
		return errors.New("one or more Node entries could not be decoded")
	}
	return nil
}

func dialEtcd(caFile, certFile, keyFile, endpoints string, timeout time.Duration) (*clientv3.Client, []string, error) {
	tlsConfig, err := loadTLS(caFile, certFile, keyFile)
	if err != nil {
		return nil, nil, err
	}
	eps := strings.Split(endpoints, ",")
	for i, endpoint := range eps {
		eps[i] = strings.TrimSpace(endpoint)
		u, err := url.Parse(eps[i])
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, nil, fmt.Errorf("invalid HTTPS etcd endpoint %q", eps[i])
		}
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: eps, TLS: tlsConfig, DialTimeout: timeout})
	if err != nil {
		return nil, nil, err
	}
	return cli, eps, nil
}

func compareAndPut(ctx context.Context, cli *clientv3.Client, key string, revision, lease int64, value []byte) (*clientv3.TxnResponse, error) {
	return cli.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", revision)).
		Then(clientv3.OpPut(key, string(value), clientv3.WithLease(clientv3.LeaseID(lease)))).Commit()
}

func loadTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA file contains no valid certificates")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client certificate/key: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{cert}}, nil
}

func saveBackup(path string, record any) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	e := json.NewEncoder(f)
	e.SetIndent("", "  ")
	if err := e.Encode(record); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
