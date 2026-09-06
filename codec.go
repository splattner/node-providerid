package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

var magic = []byte{'k', '8', 's', 0}

// field returns a singular length-delimited field. Ambiguous duplicate fields
// are rejected rather than relying on protobuf merge semantics during an edit.
func field(b []byte, want protowire.Number) ([]byte, error) {
	var value []byte
	found := false
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		m := protowire.ConsumeFieldValue(num, typ, b[n:])
		if m < 0 {
			return nil, protowire.ParseError(m)
		}
		if num == want {
			if found || typ != protowire.BytesType {
				return nil, fmt.Errorf("invalid or duplicate protobuf field %d", want)
			}
			value, _ = protowire.ConsumeBytes(b[n:])
			found = true
		}
		b = b[n+m:]
	}
	return value, nil
}

// replaceField copies all other wire bytes verbatim, including unknown fields.
func replaceField(b []byte, want protowire.Number, value []byte) ([]byte, error) {
	if _, err := field(b, want); err != nil {
		return nil, err
	}
	var out []byte
	found := false
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		m := protowire.ConsumeFieldValue(num, typ, b[n:])
		if num == want {
			out = protowire.AppendTag(out, want, protowire.BytesType)
			out = protowire.AppendBytes(out, value)
			found = true
		} else {
			out = append(out, b[:n+m]...)
		}
		b = b[n+m:]
	}
	if !found {
		out = protowire.AppendTag(out, want, protowire.BytesType)
		out = protowire.AppendBytes(out, value)
	}
	return out, nil
}

// nodeValue reads or changes spec.providerID without typed Node unmarshalling,
// which could drop fields introduced by a newer Kubernetes version.
func nodeValue(data []byte, name string, next *string) (string, []byte, error) {
	if next != nil && !utf8.ValidString(*next) {
		return "", nil, fmt.Errorf("providerID must be UTF-8")
	}
	if bytes.HasPrefix(data, []byte("k8s:enc:")) {
		return "", nil, fmt.Errorf("encrypted Node storage is unsupported")
	}
	if !bytes.HasPrefix(data, magic) {
		return jsonNode(data, name, next)
	}
	envelope := data[len(magic):]
	meta, err := field(envelope, 1)
	if err != nil {
		return "", nil, err
	}
	version, err := field(meta, 1)
	if err != nil {
		return "", nil, err
	}
	kind, err := field(meta, 2)
	if err != nil {
		return "", nil, err
	}
	if string(version) != "v1" || string(kind) != "Node" {
		return "", nil, fmt.Errorf("expected v1/Node, got %q/%q", version, kind)
	}
	encoding, err := field(envelope, 3)
	if err != nil {
		return "", nil, err
	}
	contentType, err := field(envelope, 4)
	if err != nil {
		return "", nil, err
	}
	if len(encoding) != 0 || (len(contentType) != 0 && string(contentType) != "application/vnd.kubernetes.protobuf") {
		return "", nil, fmt.Errorf("unsupported protobuf envelope encoding/content type")
	}
	node, err := field(envelope, 2)
	if err != nil {
		return "", nil, err
	}
	objectMeta, err := field(node, 1)
	if err != nil {
		return "", nil, err
	}
	storedName, err := field(objectMeta, 1)
	if err != nil {
		return "", nil, err
	}
	if string(storedName) != name {
		return "", nil, fmt.Errorf("Node name mismatch: got %q, want %q", storedName, name)
	}
	spec, err := field(node, 2)
	if err != nil {
		return "", nil, err
	}
	current, err := field(spec, 3) // core/v1 NodeSpec.providerID
	if err != nil {
		return "", nil, err
	}
	if !utf8.Valid(current) {
		return "", nil, fmt.Errorf("stored providerID is not UTF-8")
	}
	if next == nil || *next == string(current) {
		return string(current), data, nil
	}
	spec, err = replaceField(spec, 3, []byte(*next))
	if err != nil {
		return "", nil, err
	}
	node, err = replaceField(node, 2, spec)
	if err != nil {
		return "", nil, err
	}
	envelope, err = replaceField(envelope, 2, node)
	if err != nil {
		return "", nil, err
	}
	return string(current), append(append([]byte{}, magic...), envelope...), nil
}

func object(data []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	return obj, nil
}

func jsonString(data json.RawMessage) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return "", fmt.Errorf("expected string, got null")
	}
	var value string
	err := json.Unmarshal(data, &value)
	return value, err
}

func jsonNode(data []byte, name string, next *string) (string, []byte, error) {
	obj, err := object(data)
	if err != nil {
		return "", nil, fmt.Errorf("unsupported or malformed Node storage: %w", err)
	}
	version, err := jsonString(obj["apiVersion"])
	if err != nil {
		return "", nil, err
	}
	kind, err := jsonString(obj["kind"])
	if err != nil {
		return "", nil, err
	}
	if version != "v1" || kind != "Node" {
		return "", nil, fmt.Errorf("expected JSON v1/Node")
	}
	meta, err := object(obj["metadata"])
	if err != nil {
		return "", nil, err
	}
	storedName, err := jsonString(meta["name"])
	if err != nil {
		return "", nil, err
	}
	if storedName != name {
		return "", nil, fmt.Errorf("Node name mismatch: got %q, want %q", storedName, name)
	}
	spec := map[string]json.RawMessage{}
	if raw, ok := obj["spec"]; ok {
		spec, err = object(raw)
		if err != nil {
			return "", nil, err
		}
	}
	current, err := jsonString(spec["providerID"])
	if err != nil {
		return "", nil, err
	}
	if next == nil || current == *next {
		return current, data, nil
	}
	spec["providerID"], err = json.Marshal(*next)
	if err != nil {
		return "", nil, err
	}
	obj["spec"], err = json.Marshal(spec)
	if err != nil {
		return "", nil, err
	}
	updated, err := json.Marshal(obj)
	return current, updated, err
}
