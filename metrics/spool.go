package metrics

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
)

// Spool limits: a day of one-minute samples fits well within MaximumBytes.
const (
	MaximumSpoolAge   = 24 * time.Hour
	MaximumSpoolBytes = 8 << 20
)

// Spool keeps samples on disk until the gateway stored them, so neither an
// outage nor a restart loses them. Each sample is a file in samples/; the
// batch being delivered is batch.json, which is resent unchanged until the
// gateway answers definitely, so a retry after an ambiguous failure is
// deduplicated by the gateway.
type Spool struct {
	Directory string
	Now       func() time.Time

	mutex sync.Mutex
}

// Prepared is a batch ready to send.
type Prepared struct {
	ID      string
	Body    []byte
	Samples int
}

func (spool *Spool) samplesDirectory() string { return filepath.Join(spool.Directory, "samples") }
func (spool *Spool) batchPath() string        { return filepath.Join(spool.Directory, "batch.json") }

// Add stores a sample and drops what is older than a day or beyond the size
// cap, oldest first.
func (spool *Spool) Add(sample Sample) error {
	spool.mutex.Lock()
	defer spool.mutex.Unlock()

	data, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	name := filepath.Join(spool.samplesDirectory(), fmt.Sprintf("%020d.json", sample.At))
	if err := inventory.WriteFileAtomic(name, data, 0o600); err != nil {
		return err
	}
	return spool.prune()
}

// Next returns the batch in flight, or builds one from the oldest samples.
// It returns nil when nothing waits.
func (spool *Spool) Next() (*Prepared, error) {
	spool.mutex.Lock()
	defer spool.mutex.Unlock()

	if body, err := os.ReadFile(spool.batchPath()); err == nil {
		var batch Batch
		if err := json.Unmarshal(body, &batch); err == nil && validBatchID(batch.BatchID) && len(batch.Samples) > 0 {
			return &Prepared{ID: batch.BatchID, Body: body, Samples: len(batch.Samples)}, nil
		}
		// A damaged batch file can never be delivered.
		if err := os.Remove(spool.batchPath()); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	names, err := spool.sampleFiles()
	if err != nil || len(names) == 0 {
		return nil, err
	}
	names = names[:min(len(names), MaximumBatchSamples)]
	batch := Batch{ProtocolVersion: ProtocolVersion, BatchID: newBatchID(), Samples: make([]Sample, 0, len(names))}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(spool.samplesDirectory(), name))
		if err != nil {
			return nil, err
		}
		var sample Sample
		if json.Unmarshal(data, &sample) == nil && len(sample.Points) > 0 {
			batch.Samples = append(batch.Samples, sample)
		}
	}
	var prepared *Prepared
	if len(batch.Samples) > 0 {
		body, err := json.Marshal(batch)
		if err != nil {
			return nil, err
		}
		if err := inventory.WriteFileAtomic(spool.batchPath(), body, 0o600); err != nil {
			return nil, err
		}
		prepared = &Prepared{ID: batch.BatchID, Body: body, Samples: len(batch.Samples)}
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(spool.samplesDirectory(), name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return prepared, nil
}

// Done forgets the batch in flight, after it was stored or rejected for good.
func (spool *Spool) Done() error {
	spool.mutex.Lock()
	defer spool.mutex.Unlock()

	if err := os.Remove(spool.batchPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Clear drops every sample, for when metrics are turned off.
func (spool *Spool) Clear() error {
	spool.mutex.Lock()
	defer spool.mutex.Unlock()

	if err := os.RemoveAll(spool.samplesDirectory()); err != nil {
		return err
	}
	if err := os.Remove(spool.batchPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Bytes is the size of everything waiting.
func (spool *Spool) Bytes() int64 {
	spool.mutex.Lock()
	defer spool.mutex.Unlock()

	total, _ := spool.size()
	return total
}

func (spool *Spool) size() (int64, map[string]int64) {
	var total int64
	sizes := make(map[string]int64)
	if info, err := os.Stat(spool.batchPath()); err == nil {
		total += info.Size()
	}
	entries, _ := os.ReadDir(spool.samplesDirectory())
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && strings.HasSuffix(entry.Name(), ".json") {
			total += info.Size()
			sizes[entry.Name()] = info.Size()
		}
	}
	return total, sizes
}

// sampleFiles lists sample files oldest first.
func (spool *Spool) sampleFiles() ([]string, error) {
	entries, err := os.ReadDir(spool.samplesDirectory())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

func (spool *Spool) prune() error {
	names, err := spool.sampleFiles()
	if err != nil {
		return err
	}
	cutoff := spool.now().Add(-MaximumSpoolAge).Unix()
	total, sizes := spool.size()
	for _, name := range names {
		at, err := strconv.ParseInt(strings.TrimSuffix(name, ".json"), 10, 64)
		if err == nil && at >= cutoff && total <= MaximumSpoolBytes {
			break
		}
		if err := os.Remove(filepath.Join(spool.samplesDirectory(), name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		total -= sizes[name]
	}
	return nil
}

func (spool *Spool) now() time.Time {
	if spool.Now != nil {
		return spool.Now()
	}
	return time.Now()
}

func newBatchID() string {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return hex.EncodeToString(id)
}

func validBatchID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == id
}
