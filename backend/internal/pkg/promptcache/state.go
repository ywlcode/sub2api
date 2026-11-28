package promptcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const stateMagic = "S2PCV3\x00\x01"
const stateEntrySize = 56
const maxStateEntries = 1010000 // Validated configuration cap + warm-state cap.

// Save atomically stores only keyed hashes, measured counters, timestamps and
// the local HMAC key. The file is private and is not part of observer logs.
func (e *Engine) Save(path string, now time.Time) error {
	e.mu.Lock()
	data := make([]byte, 0, 104+(len(e.entries)+len(e.shared.entries)+len(e.warm.entries))*stateEntrySize)
	data = append(data, stateMagic...)
	data = append(data, e.secret[:]...)
	data = binary.BigEndian.AppendUint64(data, uint64(now.UnixNano()))
	for _, cache := range []*prefixCache{&e.prefixCache, &e.shared, &e.warm} {
		pos, count := len(data), uint64(0)
		data = binary.BigEndian.AppendUint64(data, 0)
		for elem := cache.lru.Front(); elem != nil; elem = elem.Next() {
			item, ok := elem.Value.(entry)
			if !ok || !item.expires.IsZero() && !now.Before(item.expires) {
				continue
			}
			data = append(data, item.key[:]...)
			data = binary.BigEndian.AppendUint64(data, uint64(item.written.UnixNano()))
			expires := int64(0)
			if !item.expires.IsZero() {
				expires = item.expires.UnixNano()
			}
			data = binary.BigEndian.AppendUint64(data, uint64(expires))
			data = binary.BigEndian.AppendUint64(data, uint64(item.inputTokens))
			count++
		}
		binary.BigEndian.PutUint64(data[pos:pos+8], count)
	}
	e.mu.Unlock()
	checksum := sha256.Sum256(data)
	data = append(data, checksum[:]...)
	f, err := os.CreateTemp(filepath.Dir(path), ".cache-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Load validates the entire bounded snapshot before replacing an empty engine.
// A bad/truncated file never leaves partially restored state behind.
func (e *Engine) Load(path string, now time.Time) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > int64(104+maxStateEntries*stateEntrySize) {
		return errors.New("unsafe or oversized cache state")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return errors.New("cache state changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(105+maxStateEntries*stateEntrySize)))
	if err != nil {
		return err
	}
	if len(data) < 104 || len(data) > 104+maxStateEntries*stateEntrySize || string(data[:8]) != stateMagic {
		return errors.New("invalid cache state header")
	}
	checksum := sha256.Sum256(data[:len(data)-32])
	if !bytes.Equal(checksum[:], data[len(data)-32:]) {
		return errors.New("invalid cache state checksum")
	}
	read := data[40 : len(data)-32]
	stamp := int64(binary.BigEndian.Uint64(read[:8]))
	read = read[8:]
	if stamp > now.Add(time.Minute).UnixNano() {
		return errors.New("cache state timestamp is in the future")
	}
	limits := []int{e.limit, e.shared.limit, e.warm.limit}
	caches := make([]prefixCache, 3)
	for i, limit := range limits {
		caches[i] = newPrefixCache(limit)
		if len(read) < 8 {
			return errors.New("truncated cache state")
		}
		count := binary.BigEndian.Uint64(read[:8])
		read = read[8:]
		if count > maxStateEntries || count > uint64(len(read)/stateEntrySize) {
			return errors.New("invalid cache state count")
		}
		for n := uint64(0); n < count; n++ {
			var item entry
			copy(item.key[:], read[:32])
			written := int64(binary.BigEndian.Uint64(read[32:40]))
			expires := int64(binary.BigEndian.Uint64(read[40:48]))
			measured := binary.BigEndian.Uint64(read[48:56])
			read = read[56:]
			if measured > uint64(^uint(0)>>1) || written > now.UnixNano() || written <= 0 {
				return errors.New("invalid cache state counter or time")
			}
			item.written, item.inputTokens = time.Unix(0, written), int(measured)
			if i < 2 {
				if expires < written {
					return errors.New("invalid cache state expiry")
				}
				item.expires = minTime(time.Unix(0, expires), item.written.Add(e.options.TTL))
				if !now.Before(item.expires) {
					continue
				}
			} else if expires != 0 {
				return errors.New("invalid warm-state expiry")
			}
			if _, exists := caches[i].entries[item.key]; exists {
				return errors.New("duplicate cache state entry")
			}
			if len(caches[i].entries) < limit {
				caches[i].entries[item.key] = caches[i].lru.PushBack(item)
			}
		}
	}
	if len(read) != 0 {
		return errors.New("trailing cache state data")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.entries)+len(e.shared.entries)+len(e.warm.entries)+e.retainedPoints != 0 {
		return errors.New("cannot replace an active cache")
	}
	copy(e.secret[:], data[8:40])
	e.prefixCache, e.shared, e.warm = caches[0], caches[1], caches[2]
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
