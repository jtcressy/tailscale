// Copyright (c) 2022 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tka

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/fxamacker/cbor/v2"
	"tailscale.com/atomicfile"
)

// Chonk implementations provide durable storage for AUMs and other
// TKA state.
//
// All methods must be thread-safe.
//
// The name 'tailchonk' was coined by @catzkorn.
type Chonk interface {
	// AUM returns the AUM with the specified digest.
	//
	// If the AUM does not exist, then os.ErrNotExist is returned.
	AUM(hash AUMHash) (AUM, error)

	// ChildAUMs returns all AUMs with a specified previous
	// AUM hash.
	ChildAUMs(prevAUMHash AUMHash) ([]AUM, error)

	// CommitVerifiedAUMs durably stores the provided AUMs.
	// Callers MUST ONLY provide AUMs which are verified (specifically,
	// a call to aumVerify() must return a nil error).
	// as the implementation assumes that only verified AUMs are stored.
	CommitVerifiedAUMs(updates []AUM) error

	// Heads returns AUMs for which there are no children. In other
	// words, the latest AUM in all possible chains (the 'leaves').
	Heads() ([]AUM, error)

	// SetLastActiveAncestor is called to record the oldest-known AUM
	// that contributed to the current state. This value is used as
	// a hint on next startup to determine which chain to pick when computing
	// the current state, if there are multiple distinct chains.
	SetLastActiveAncestor(hash AUMHash) error

	// LastActiveAncestor returns the oldest-known AUM that was (in a
	// previous run) an ancestor of the current state. This is used
	// as a hint to pick the correct chain in the event that the Chonk stores
	// multiple distinct chains.
	LastActiveAncestor() (*AUMHash, error)
}

// Mem implements in-memory storage of TKA state, suitable for
// tests.
//
// Mem implements the Chonk interface.
type Mem struct {
	l           sync.RWMutex
	aums        map[AUMHash]AUM
	parentIndex map[AUMHash][]AUMHash

	lastActiveAncestor *AUMHash
}

func (c *Mem) SetLastActiveAncestor(hash AUMHash) error {
	c.l.Lock()
	defer c.l.Unlock()
	c.lastActiveAncestor = &hash
	return nil
}

func (c *Mem) LastActiveAncestor() (*AUMHash, error) {
	c.l.RLock()
	defer c.l.RUnlock()
	return c.lastActiveAncestor, nil
}

// Heads returns AUMs for which there are no children. In other
// words, the latest AUM in all chains (the 'leaf').
func (c *Mem) Heads() ([]AUM, error) {
	c.l.RLock()
	defer c.l.RUnlock()
	out := make([]AUM, 0, 6)

	// An AUM is a 'head' if there are no nodes for which it is the parent.
	for _, a := range c.aums {
		if len(c.parentIndex[a.Hash()]) == 0 {
			out = append(out, a)
		}
	}
	return out, nil
}

// AUM returns the AUM with the specified digest.
func (c *Mem) AUM(hash AUMHash) (AUM, error) {
	c.l.RLock()
	defer c.l.RUnlock()
	aum, ok := c.aums[hash]
	if !ok {
		return AUM{}, os.ErrNotExist
	}
	return aum, nil
}

// Orphans returns all AUMs which do not have a parent.
func (c *Mem) Orphans() ([]AUM, error) {
	c.l.RLock()
	defer c.l.RUnlock()
	out := make([]AUM, 0, 6)
	for _, a := range c.aums {
		if _, ok := a.Parent(); !ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// ChildAUMs returns all AUMs with a specified previous
// AUM hash.
func (c *Mem) ChildAUMs(prevAUMHash AUMHash) ([]AUM, error) {
	c.l.RLock()
	defer c.l.RUnlock()
	out := make([]AUM, 0, 6)
	for _, entry := range c.parentIndex[prevAUMHash] {
		out = append(out, c.aums[entry])
	}

	return out, nil
}

// CommitVerifiedAUMs durably stores the provided AUMs.
// Callers MUST ONLY provide well-formed and verified AUMs,
// as the rest of the TKA implementation assumes that only
// verified AUMs are stored.
func (c *Mem) CommitVerifiedAUMs(updates []AUM) error {
	c.l.Lock()
	defer c.l.Unlock()
	if c.aums == nil {
		c.parentIndex = make(map[AUMHash][]AUMHash, 64)
		c.aums = make(map[AUMHash]AUM, 64)
	}

updateLoop:
	for _, aum := range updates {
		aumHash := aum.Hash()
		c.aums[aumHash] = aum

		parent, ok := aum.Parent()
		if ok {
			for _, exists := range c.parentIndex[parent] {
				if exists == aumHash {
					continue updateLoop
				}
			}
			c.parentIndex[parent] = append(c.parentIndex[parent], aumHash)
		}
	}

	return nil
}

// FS implements filesystem storage of TKA state.
//
// FS implements the Chonk interface.
type FS struct {
	base string
	mu   sync.RWMutex
}

// ChonkDir returns an implementation of Chonk which uses the
// given directory to store TKA state.
func ChonkDir(dir string) (*FS, error) {
	stat, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("chonk directory %q is a file", dir)
	}
	return &FS{base: dir}, nil
}

// fsHashInfo describes how information about an AUMHash is represented
// on disk.
//
// The CBOR-serialization of this struct is stored to base/__/base32(hash)
// where __ are the first two characters of base32(hash).
//
// CBOR was chosen because we are already using it and it serializes
// much smaller than JSON for AUMs. The 'keyasint' thing isn't essential
// but again it saves a bunch of bytes.
type fsHashInfo struct {
	Children []AUMHash `cbor:"1,keyasint"`
	AUM      *AUM      `cbor:"2,keyasint"`
}

// aumDir returns the directory an AUM is stored in, and its filename
// within the directory.
func (c *FS) aumDir(h AUMHash) (dir, base string) {
	s := h.String()
	return filepath.Join(c.base, s[:2]), s
}

// AUM returns the AUM with the specified digest.
//
// If the AUM does not exist, then os.ErrNotExist is returned.
func (c *FS) AUM(hash AUMHash) (AUM, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	info, err := c.get(hash)
	if err != nil {
		return AUM{}, err
	}
	if info.AUM == nil {
		return AUM{}, os.ErrNotExist
	}
	return *info.AUM, nil
}

// AUM returns any known AUMs with a specific parent hash.
func (c *FS) ChildAUMs(prevAUMHash AUMHash) ([]AUM, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	info, err := c.get(prevAUMHash)
	if err != nil {
		if os.IsNotExist(err) {
			// not knowing about this hash is not an error
			return nil, nil
		}
		return nil, err
	}

	out := make([]AUM, len(info.Children))
	for i, h := range info.Children {
		c, err := c.get(h)
		if err != nil {
			// We expect any AUM recorded as a child on its parent to exist.
			return nil, fmt.Errorf("reading child %d of %x: %v", i, h, err)
		}
		if c.AUM == nil {
			return nil, fmt.Errorf("child %d of %x: AUM not stored", i, h)
		}
		out[i] = *c.AUM
	}

	return out, nil
}

func (c *FS) get(h AUMHash) (*fsHashInfo, error) {
	dir, base := c.aumDir(h)
	f, err := os.Open(filepath.Join(dir, base))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m, err := cborDecOpts.DecMode()
	if err != nil {
		return nil, err
	}

	var out fsHashInfo
	if err := m.NewDecoder(f).Decode(&out); err != nil {
		return nil, err
	}
	if out.AUM != nil && out.AUM.Hash() != h {
		return nil, fmt.Errorf("%s: AUM does not match file name hash %s", f.Name(), out.AUM.Hash())
	}
	return &out, nil
}

// Heads returns AUMs for which there are no children. In other
// words, the latest AUM in all possible chains (the 'leaves').
//
// Heads is expected to be called infrequently compared to AUM() or
// ChildAUMs(), so we haven't put any work into maintaining an index.
// Instead, the full set of AUMs is scanned.
func (c *FS) Heads() ([]AUM, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]AUM, 0, 6) // 6 is arbitrary.
	err := c.scanHashes(func(info *fsHashInfo) {
		if len(info.Children) == 0 && info.AUM != nil {
			out = append(out, *info.AUM)
		}
	})
	return out, err
}

func (c *FS) scanHashes(eachHashInfo func(*fsHashInfo)) error {
	prefixDirs, err := os.ReadDir(c.base)
	if err != nil {
		return fmt.Errorf("reading prefix dirs: %v", err)
	}
	for _, prefix := range prefixDirs {
		if !prefix.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(c.base, prefix.Name()))
		if err != nil {
			return fmt.Errorf("reading prefix dir: %v", err)
		}
		for _, file := range files {
			var h AUMHash
			if err := h.UnmarshalText([]byte(file.Name())); err != nil {
				return fmt.Errorf("invalid aum file: %s: %w", file.Name(), err)
			}
			info, err := c.get(h)
			if err != nil {
				return fmt.Errorf("reading %x: %v", h, err)
			}

			eachHashInfo(info)
		}
	}

	return nil
}

// SetLastActiveAncestor is called to record the oldest-known AUM
// that contributed to the current state. This value is used as
// a hint on next startup to determine which chain to pick when computing
// the current state, if there are multiple distinct chains.
func (c *FS) SetLastActiveAncestor(hash AUMHash) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return atomicfile.WriteFile(filepath.Join(c.base, "last_active_ancestor"), hash[:], 0644)
}

// LastActiveAncestor returns the oldest-known AUM that was (in a
// previous run) an ancestor of the current state. This is used
// as a hint to pick the correct chain in the event that the Chonk stores
// multiple distinct chains.
//
// Nil is returned if no last-active ancestor is set.
func (c *FS) LastActiveAncestor() (*AUMHash, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	hash, err := ioutil.ReadFile(filepath.Join(c.base, "last_active_ancestor"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Not exist == none set.
		}
		return nil, err
	}

	var out AUMHash
	if len(hash) != len(out) {
		return nil, fmt.Errorf("stored hash is of wrong length: %d != %d", len(hash), len(out))
	}
	copy(out[:], hash)
	return &out, nil
}

// CommitVerifiedAUMs durably stores the provided AUMs.
// Callers MUST ONLY provide AUMs which are verified (specifically,
// a call to aumVerify must return a nil error), as the
// implementation assumes that only verified AUMs are stored.
func (c *FS) CommitVerifiedAUMs(updates []AUM) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, aum := range updates {
		h := aum.Hash()
		// We keep track of children against their parent so that
		// ChildAUMs() do not need to scan all AUMs.
		parent, hasParent := aum.Parent()
		if hasParent {
			err := c.commit(parent, func(info *fsHashInfo) {
				// Only add it if its not already there.
				for i := range info.Children {
					if info.Children[i] == h {
						return
					}
				}
				info.Children = append(info.Children, h)
			})
			if err != nil {
				return fmt.Errorf("committing update[%d] to parent %x: %v", i, parent, err)
			}
		}

		err := c.commit(h, func(info *fsHashInfo) {
			info.AUM = &aum
		})
		if err != nil {
			return fmt.Errorf("committing update[%d] (%x): %v", i, h, err)
		}
	}

	return nil
}

// commit calls the provided updater function to record changes relevant
// to the given hash. The caller is expected to update the AUM and
// Children fields, as relevant.
func (c *FS) commit(h AUMHash, updater func(*fsHashInfo)) error {
	toCommit := fsHashInfo{}

	existing, err := c.get(h)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return err
	default:
		toCommit = *existing
	}

	updater(&toCommit)
	if toCommit.AUM != nil && toCommit.AUM.Hash() != h {
		return fmt.Errorf("cannot commit AUM with hash %x to %x", toCommit.AUM.Hash(), h)
	}

	dir, base := c.aumDir(h)
	if err := os.MkdirAll(dir, 0755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("creating directory: %v", err)
	}

	m, err := cbor.CTAP2EncOptions().EncMode()
	if err != nil {
		return fmt.Errorf("cbor EncMode: %v", err)
	}

	var buff bytes.Buffer
	if err := m.NewEncoder(&buff).Encode(toCommit); err != nil {
		return fmt.Errorf("encoding: %v", err)
	}
	return atomicfile.WriteFile(filepath.Join(dir, base), buff.Bytes(), 0644)
}
