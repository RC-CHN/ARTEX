package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// Checkpoint fragments remain opaque bytes, never JSONB: results may contain
// escaped NUL and integers that cannot survive a float64 round trip. Separate
// lease metadata from immutable command receipts so heartbeats don't rewrite
// the full execution history. The root and all fragments commit in one CAS.
type checkpointPart struct {
	key  string
	data json.RawMessage
	hash [32]byte
}

func splitCheckpoint(raw json.RawMessage) ([]checkpointPart, error) {
	var parts []checkpointPart
	var split func(gjson.Result, []string) error
	split = func(value gjson.Result, path []string) error {
		var raw json.RawMessage
		container := len(path) == 0 || len(path) == 1 && (path[0] == "nodes" || path[0] == "sessions") || len(path) == 2 && path[0] == "sessions" || len(path) == 3 && path[0] == "sessions" && path[2] == "commands"
		if container && value.IsObject() {
			// Keep raw slices into one validated document. Repeatedly decoding
			// nested RawMessage maps copies/scans the full history at every level.
			obj := make(map[string]json.RawMessage)
			changed := false
			for k, v := range value.Map() {
				move := len(path) == 0 && (k == "nodes" || k == "sessions") || len(path) == 1 || len(path) == 3
				if len(path) == 2 {
					switch k {
					case "spec", "commands", "call_ids", "mcp_tools", "notifications", "notification_ids":
						move = true
					}
				}
				if move {
					child := append(append([]string{}, path...), k)
					if err := split(v, child); err != nil {
						return err
					}
					changed = true
				} else {
					obj[k] = json.RawMessage(v.Raw)
				}
			}
			if changed {
				var err error
				raw, err = json.Marshal(obj)
				if err != nil {
					return err
				}
			}
		}
		if raw == nil {
			raw = json.RawMessage(value.Raw)
		}
		key, _ := json.Marshal(path)
		// Bound even a single large command result: acknowledged chunks provide
		// real progress signals and prevent a large packet from monopolizing I/O.
		for offset, chunk := 0, 0; offset < len(raw); chunk++ {
			end := min(len(raw), offset+checkpointBatchBytes)
			data := raw[offset:end]
			parts = append(parts, checkpointPart{string(key) + "/" + strconv.Itoa(chunk), data, sha256.Sum256(data)})
			offset = end
		}
		return nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid runner checkpoint")
	}
	if err := split(gjson.ParseBytes(raw), []string{}); err != nil {
		return nil, err
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].key < parts[j].key })
	return parts, nil
}

func joinCheckpoint(parts []checkpointPart) (json.RawMessage, error) {
	type node struct {
		raw      json.RawMessage
		children map[string]*node
	}
	root := &node{}
	groups := map[string]map[int]json.RawMessage{}
	for _, p := range parts {
		sep := strings.LastIndexByte(p.key, '/')
		if sep < 0 || sha256.Sum256(p.data) != p.hash {
			return nil, errors.New("invalid runner checkpoint chunk")
		}
		index, err := strconv.Atoi(p.key[sep+1:])
		if err != nil || index < 0 || index > len(parts) || len(p.data) == 0 || len(p.data) > checkpointBatchBytes {
			return nil, errors.New("invalid runner checkpoint chunk index")
		}
		key := p.key[:sep]
		if groups[key] == nil {
			groups[key] = map[int]json.RawMessage{}
		}
		if _, exists := groups[key][index]; exists {
			return nil, errors.New("duplicate runner checkpoint chunk")
		}
		groups[key][index] = p.data
	}
	for key, chunks := range groups {
		var data json.RawMessage
		for i := 0; i < len(chunks); i++ {
			chunk, ok := chunks[i]
			if !ok {
				return nil, errors.New("missing runner checkpoint chunk")
			}
			data = append(data, chunk...)
		}
		var path []string
		if err := json.Unmarshal([]byte(key), &path); err != nil || len(path) > 4 || !json.Valid(data) {
			return nil, errors.New("invalid runner checkpoint fragment")
		}
		n := root
		for _, k := range path {
			if n.children == nil {
				n.children = map[string]*node{}
			}
			if n.children[k] == nil {
				n.children[k] = &node{}
			}
			n = n.children[k]
		}
		if n.raw != nil {
			return nil, errors.New("duplicate runner checkpoint fragment")
		}
		n.raw = data
	}
	var fold func(*node) (json.RawMessage, error)
	fold = func(n *node) (json.RawMessage, error) {
		if n.raw == nil {
			return nil, errors.New("missing runner checkpoint parent")
		}
		if len(n.children) == 0 {
			return n.raw, nil
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(n.raw, &obj); err != nil || obj == nil {
			return nil, errors.New("invalid runner checkpoint parent")
		}
		for k, child := range n.children {
			if _, ok := obj[k]; ok {
				return nil, errors.New("overlapping runner checkpoint fragments")
			}
			raw, err := fold(child)
			if err != nil {
				return nil, err
			}
			obj[k] = raw
		}
		// Children are already validated. Avoid json.Marshal revalidating the
		// same large raw result at every level of the reconstructed tree.
		keys := make([]string, 0, len(obj))
		size := 2
		for k, v := range obj {
			keys = append(keys, k)
			size += len(k) + len(v) + 4
		}
		sort.Strings(keys)
		raw := make(json.RawMessage, 0, size)
		raw = append(raw, '{')
		for i, k := range keys {
			if i > 0 {
				raw = append(raw, ',')
			}
			name, _ := json.Marshal(k)
			raw = append(raw, name...)
			raw = append(raw, ':')
			raw = append(raw, obj[k]...)
		}
		raw = append(raw, '}')
		return raw, nil
	}
	return fold(root)
}
