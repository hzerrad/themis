// Package digest computes the canonical hash that binds an approval and an
// execution to exactly one action. The core and the executor both use it.
package digest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// TargetIdentity is the target as bound by an action. It has no
// resourceVersion, which changes on every status update.
type TargetIdentity struct {
	Group      string `json:"group"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`

	// SpecHash covers the spec fields the capability touches.
	SpecHash string `json:"specHash"`

	// Manager is the field manager that owns the touched fields.
	Manager string `json:"manager"`
}

// ActionFields are every field that matters to an execution.
type ActionFields struct {
	IncidentID        string         `json:"incidentID"`
	Capability        string         `json:"capability"`
	CapabilityVersion string         `json:"capabilityVersion"`
	Target            TargetIdentity `json:"target"`
	Params            map[string]any `json:"params"`
	PolicyID          string         `json:"policyID"`
	PolicyDigest      string         `json:"policyDigest"`
	Constraints       map[string]any `json:"constraints"`
}

// Compute returns "sha256:<hex>" over the RFC 8785 canonical JSON of a. Its
// output is stored with approvals, so it must not change.
func Compute(a ActionFields) (string, error) {
	return hash(a)
}

// SpecHash hashes only the dot-separated paths of obj, so changes elsewhere,
// such as to status, leave it unchanged. A missing path hashes as null.
func SpecHash(obj map[string]any, paths []string) (string, error) {
	picked := make(map[string]any, len(paths))
	for _, p := range paths {
		v, err := lookup(obj, strings.Split(p, "."))
		if err != nil {
			return "", fmt.Errorf("path %s: %w", p, err)
		}
		picked[p] = v
	}
	return hash(picked)
}

func lookup(obj map[string]any, path []string) (any, error) {
	var v any = obj
	for i, key := range path {
		if v == nil {
			return nil, nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s is not an object", strings.Join(path[:i], "."))
		}
		v = m[key]
	}
	return v, nil
}

func hash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode: %w", err)
	}
	if err := exactNumbers(data); err != nil {
		return "", err
	}
	canonical := jsontext.Value(data)
	if err := canonical.Canonicalize(); err != nil {
		return "", fmt.Errorf("canonicalize: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// maxExact bounds the integers a double holds exactly; 2^53+1 rounds to it.
const maxExact = 1 << 53

// exactNumbers rejects numbers that canonicalization would round.
func exactNumbers(data []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if tok.Kind() != '0' {
			continue
		}
		if f, err := tok.Float(); err != nil || math.Abs(f) >= maxExact {
			return fmt.Errorf("number %s is beyond the exact range of canonical JSON", tok.String())
		}
	}
}
