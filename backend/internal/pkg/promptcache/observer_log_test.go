package promptcache

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func observerOptions(t *testing.T) LogOptions {
	t.Helper()
	return (LogOptions{Dir: t.TempDir(), InstanceID: "0123456789abcdef", MaxTotalBytes: 128 << 10, MaxSegmentBytes: 16 << 10}).defaults()
}

func readObserverFiles(t *testing.T, dir string) []byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "shadow-*.jsonl*"))
	require.NoError(t, err)
	var all bytes.Buffer
	for _, name := range files {
		f, err := os.Open(name)
		require.NoError(t, err)
		var reader io.Reader = f
		if strings.HasSuffix(name, ".gz") {
			gz, err := gzip.NewReader(f)
			require.NoError(t, err)
			defer func() { _ = gz.Close() }()
			reader = gz
		}
		_, err = io.Copy(&all, reader)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	return all.Bytes()
}

func TestShadowLogIndependentJSONAndFinalCounters(t *testing.T) {
	opts := observerOptions(t)
	l, err := NewObserverLog(opts)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		require.True(t, l.Emit([]zap.Field{zap.String("component", "service.prompt_cache_shadow"), zap.String("instance_id", opts.InstanceID), zap.Int("schema_version", 1), zap.Int("input_tokens", 2048)}))
	}
	l.Close()
	l.Close()
	require.False(t, l.Emit(nil))
	s := l.Snapshot()
	require.EqualValues(t, 10, s.SubmittedEvents)
	require.EqualValues(t, 10, s.WrittenEvents)
	require.Zero(t, s.QueueDroppedEvents)
	require.Zero(t, s.WriteFailedEvents)
	lines := bufio.NewScanner(bytes.NewReader(readObserverFiles(t, opts.Dir)))
	events, snapshots := 0, 0
	for lines.Scan() {
		var row map[string]any
		require.NoError(t, json.Unmarshal(lines.Bytes(), &row))
		switch row["msg"] {
		case "prompt_cache.shadow":
			events++
		case "prompt_cache.shadow_stats":
			snapshots++
			require.Equal(t, true, row["final"])
			logging, ok := row["logging"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, float64(10), logging["written_events"])
		default:
			t.Fatalf("unexpected event %v", row["msg"])
		}
	}
	require.NoError(t, lines.Err())
	require.Equal(t, 10, events)
	require.Equal(t, 1, snapshots)
	files, err := os.ReadDir(opts.Dir)
	require.NoError(t, err)
	for _, entry := range files {
		info, err := entry.Info()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		if entry.Name() != ".writer.lock" {
			require.True(t, strings.HasSuffix(entry.Name(), ".jsonl.gz"))
		}
	}
}

// This fake blocks disk I/O to prove that a full logging queue cannot make a
// gateway producer wait, and that the final snapshot reports every loss.
type blockedObserverStore struct {
	entered, release chan struct{}
	once             sync.Once
	fail             bool
}

func (s *blockedObserverStore) append(raw []byte, _ time.Time) error {
	if !bytes.Contains(raw, []byte("prompt_cache.shadow_stats")) {
		s.once.Do(func() { close(s.entered); <-s.release })
		if s.fail {
			return errors.New("simulated full disk")
		}
	}
	return nil
}
func (*blockedObserverStore) maintain(time.Time) error { return nil }
func (*blockedObserverStore) close() error             { return nil }
func (*blockedObserverStore) snapshot() LogSnapshot    { return LogSnapshot{} }

func TestShadowLogQueueDoesNotBlockAndLossIsCounted(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(string(rune('0'+map[bool]int{false: 0, true: 1}[fail])), func(t *testing.T) {
			opts := observerOptions(t)
			opts.QueueSize = 2
			store := &blockedObserverStore{entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
			l := newObserverLog(opts, store)
			require.True(t, l.Emit(nil))
			select {
			case <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("worker did not enter store")
			}
			require.True(t, l.Emit(nil))
			require.True(t, l.Emit(nil))
			returned := make(chan bool, 1)
			go func() { returned <- l.Emit(nil) }()
			select {
			case accepted := <-returned:
				require.False(t, accepted)
			case <-time.After(time.Second):
				t.Fatal("producer waited on disk")
			}
			require.False(t, l.Emit([]zap.Field{zap.String("model", strings.Repeat("x", maxLogEventBytes))}))
			close(store.release)
			l.Close()
			s := l.Snapshot()
			require.EqualValues(t, 5, s.SubmittedEvents)
			require.EqualValues(t, 1, s.QueueDroppedEvents)
			require.EqualValues(t, 1, s.OversizeDroppedEvents)
			if fail {
				require.EqualValues(t, 3, s.WriteFailedEvents)
			} else {
				require.EqualValues(t, 3, s.WrittenEvents)
			}
			require.Equal(t, s.SubmittedEvents, s.WrittenEvents+s.QueueDroppedEvents+s.OversizeDroppedEvents+s.WriteFailedEvents)
		})
	}
}

func TestShadowLogConcurrentCloseAndProducers(t *testing.T) {
	opts := observerOptions(t)
	l, err := NewObserverLog(opts)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Emit([]zap.Field{zap.Int("input_tokens", 1)})
			}
		}()
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.Close() }()
	}
	wg.Wait()
	s := l.Snapshot()
	require.Equal(t, s.SubmittedEvents, s.WrittenEvents+s.QueueDroppedEvents+s.OversizeDroppedEvents+s.WriteFailedEvents)
}

func TestShadowLogAgeRetentionAndDirectoryIsolation(t *testing.T) {
	opts := observerOptions(t)
	now := time.Now()
	old := filepath.Join(opts.Dir, "shadow-20260101T000000.000000000Z-0123456789abcdef-000001.jsonl")
	require.NoError(t, os.WriteFile(old, []byte("{}\n"), 0600))
	require.NoError(t, os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)))
	unrelated := filepath.Join(opts.Dir, "sub2api.log")
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0600))
	link := filepath.Join(opts.Dir, "shadow-20260101T000000.000000000Z-0123456789abcdef-000002.jsonl")
	require.NoError(t, os.Symlink(unrelated, link))
	s, err := newSegmentLog(opts)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.NoFileExists(t, old)
	require.EqualValues(t, 1, s.stats.AgeDeletedFiles)
	require.False(t, s.stats.RetentionShortened)
	b, err := os.ReadFile(unrelated)
	require.NoError(t, err)
	require.Equal(t, "keep", string(b))
	_, err = os.Lstat(link)
	require.NoError(t, err)
}

func TestShadowLogCapacityAndCompression(t *testing.T) {
	opts := observerOptions(t)
	opts.MaxTotalBytes, opts.MaxSegmentBytes = 64<<10, 8<<10
	s, err := newSegmentLog(opts)
	require.NoError(t, err)
	now := time.Now()
	random := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		var entropy [3000]byte
		_, _ = random.Read(entropy[:])
		raw := []byte(`{"value":"` + base64.StdEncoding.EncodeToString(entropy[:]) + `"}` + "\n")
		require.NoError(t, s.append(raw, now))
		files, err := os.ReadDir(opts.Dir)
		require.NoError(t, err)
		var total int64
		for _, f := range files {
			if f.Type().IsRegular() {
				info, err := f.Info()
				require.NoError(t, err)
				total += info.Size()
			}
		}
		require.LessOrEqual(t, total, opts.MaxTotalBytes)
	}
	require.NoError(t, s.close())
	require.Positive(t, s.stats.CapacityDeletedFiles)
	require.True(t, s.stats.RetentionShortened)
	scanner := bufio.NewScanner(bytes.NewReader(readObserverFiles(t, opts.Dir)))
	count := 0
	for scanner.Scan() {
		require.True(t, json.Valid(scanner.Bytes()))
		count++
	}
	require.NoError(t, scanner.Err())
	require.Positive(t, count)
	require.Less(t, count, 100)
}

func TestShadowLogDailyRotationCrashRecoveryAndLock(t *testing.T) {
	opts := observerOptions(t)
	s, err := newSegmentLog(opts)
	require.NoError(t, err)
	_, err = newSegmentLog(opts)
	require.Error(t, err)
	now := time.Now().UTC()
	require.NoError(t, s.append([]byte("{\"first\":1}\n"), now))
	first := s.path
	require.NoError(t, s.append([]byte("{\"second\":1}\n"), now.Add(24*time.Hour)))
	require.FileExists(t, first+".gz")
	require.NoFileExists(t, first)
	// Simulate a crash after writing the active source and compression temp.
	require.NoError(t, s.file.Close())
	s.file = nil
	crashed := s.path
	s.path = ""
	require.NoError(t, os.WriteFile(crashed+".gz.tmp", []byte("incomplete"), 0600))
	require.NoError(t, s.close())
	recovered, err := newSegmentLog(opts)
	require.NoError(t, err)
	require.NoFileExists(t, crashed+".gz.tmp")
	require.NoError(t, recovered.append([]byte("{\"third\":1}\n"), now))
	active := recovered.path
	require.NoError(t, recovered.maintain(now))
	require.FileExists(t, crashed+".gz")
	require.FileExists(t, active)
	require.NoError(t, recovered.append([]byte("{\"fourth\":1}\n"), now))
	require.NoError(t, recovered.close())
	all := readObserverFiles(t, opts.Dir)
	for _, name := range []string{"first", "second", "third", "fourth"} {
		require.Contains(t, string(all), `"`+name+`":1`)
	}
}

func TestShadowLogInvalidDirectory(t *testing.T) {
	opts := observerOptions(t)
	file := filepath.Join(opts.Dir, "regular-file")
	require.NoError(t, os.WriteFile(file, nil, 0600))
	opts.Dir = filepath.Join(file, "logs")
	_, err := NewObserverLog(opts)
	require.Error(t, err)
}
