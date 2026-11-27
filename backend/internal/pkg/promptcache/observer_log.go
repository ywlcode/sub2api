package promptcache

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sys/unix"
)

const maxLogEventBytes = 16 << 10

// LogOptions are local diagnostic limits, independent of ordinary application
// logs and billing. Zero values select the documented defaults.
type LogOptions struct {
	Dir, InstanceID                string
	Retention                      time.Duration
	MaxTotalBytes, MaxSegmentBytes int64
	QueueSize                      int
	StatsInterval                  time.Duration
	Warn                           func(error)
}

func (o LogOptions) defaults() LogOptions {
	if o.Retention <= 0 {
		o.Retention = 7 * 24 * time.Hour
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = 5120 << 20
	}
	if o.MaxSegmentBytes <= 0 {
		o.MaxSegmentBytes = 64 << 20
	}
	// Reserve space for a segment and its temporary compressed copy.
	o.MaxSegmentBytes = min(o.MaxSegmentBytes, o.MaxTotalBytes/4)
	if o.QueueSize <= 0 {
		o.QueueSize = 1024
	}
	if o.StatsInterval <= 0 {
		o.StatsInterval = time.Minute
	}
	return o
}

// LogSnapshot counters are cumulative for this process, not for an arbitrary
// selected log window or account. A missing snapshot never implies zero loss.
type LogSnapshot struct {
	SubmittedEvents       uint64 `json:"submitted_events"`
	WrittenEvents         uint64 `json:"written_events"`
	QueueDroppedEvents    uint64 `json:"queue_dropped_events"`
	OversizeDroppedEvents uint64 `json:"oversize_dropped_events"`
	WriteFailedEvents     uint64 `json:"write_failed_events"`
	MaintenanceErrors     uint64 `json:"maintenance_errors"`
	CapacityDeletedFiles  uint64 `json:"capacity_deleted_files"`
	AgeDeletedFiles       uint64 `json:"age_deleted_files"`
	RetentionShortened    bool   `json:"retention_shortened"`
	RetainedBytes         int64  `json:"retained_bytes"`
	OldestSegmentTime     string `json:"oldest_segment_time"`
}

type queuedLogEvent struct {
	at     time.Time
	fields []zap.Field
}
type observerLogStore interface {
	append([]byte, time.Time) error
	maintain(time.Time) error
	close() error
	snapshot() LogSnapshot
}

// ObserverLog owns one worker. Producers only copy bounded scalar fields into
// a bounded queue; file I/O, compression and retention never run on producers.
type ObserverLog struct {
	opts                          LogOptions
	store                         observerLogStore
	queue                         chan queuedLogEvent
	done                          chan struct{}
	mu                            sync.RWMutex
	closed                        bool
	submitted, dropped, oversized atomic.Uint64
	snapshotValue                 atomic.Pointer[LogSnapshot]
}

func NewObserverLog(options LogOptions) (*ObserverLog, error) {
	opts := options.defaults()
	store, err := newSegmentLog(opts)
	if err != nil {
		return nil, err
	}
	return newObserverLog(opts, store), nil
}

func newObserverLog(opts LogOptions, store observerLogStore) *ObserverLog {
	l := &ObserverLog{opts: opts, store: store, queue: make(chan queuedLogEvent, opts.QueueSize), done: make(chan struct{})}
	l.snapshotValue.Store(&LogSnapshot{})
	go l.run()
	return l
}

func (l *ObserverLog) Emit(fields []zap.Field) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return false
	}
	l.submitted.Add(1)
	size := len(fields) * 64
	for _, f := range fields {
		size += len(f.String) + len(f.Key)
	}
	if size > maxLogEventBytes {
		l.oversized.Add(1)
		return false
	}
	// Do not allocate/copy when the worker is already saturated.
	if len(l.queue) == cap(l.queue) {
		l.dropped.Add(1)
		return false
	}
	event := queuedLogEvent{time.Now(), append([]zap.Field(nil), fields...)}
	select {
	case l.queue <- event:
		return true
	default:
		l.dropped.Add(1)
		return false
	}
}

func (l *ObserverLog) Snapshot() LogSnapshot {
	s := *l.snapshotValue.Load()
	s.SubmittedEvents = l.submitted.Load()
	s.QueueDroppedEvents = l.dropped.Load()
	s.OversizeDroppedEvents = l.oversized.Load()
	return s
}

func (l *ObserverLog) Close() {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.queue)
	}
	l.mu.Unlock()
	<-l.done
}

func (l *ObserverLog) run() {
	defer close(l.done)
	ticker := time.NewTicker(l.opts.StatsInterval)
	defer ticker.Stop()
	enc := zapcore.NewJSONEncoder(zapcore.EncoderConfig{TimeKey: "time", LevelKey: "level", MessageKey: "msg", EncodeTime: zapcore.RFC3339NanoTimeEncoder, EncodeLevel: zapcore.LowercaseLevelEncoder, LineEnding: "\n"})
	var written, failed, maintenance, sequence uint64
	var warned time.Time
	warn := func(err error) {
		if err != nil && l.opts.Warn != nil && (warned.IsZero() || time.Since(warned) >= time.Minute) {
			warned = time.Now()
			l.opts.Warn(err)
		}
	}
	publish := func() LogSnapshot {
		s := l.store.snapshot()
		s.WrittenEvents, s.WriteFailedEvents, s.MaintenanceErrors = written, failed, maintenance
		l.snapshotValue.Store(&s)
		return l.Snapshot()
	}
	status := func(final bool) {
		sequence++
		now := time.Now()
		if err := l.store.maintain(now); err != nil {
			maintenance++
			warn(err)
		}
		event := struct {
			Time      string      `json:"time"`
			Message   string      `json:"msg"`
			Component string      `json:"component"`
			Schema    int         `json:"schema_version"`
			Instance  string      `json:"instance_id"`
			Scope     string      `json:"state_scope"`
			Sequence  uint64      `json:"sequence"`
			Final     bool        `json:"final"`
			Counters  LogSnapshot `json:"logging"`
		}{now.UTC().Format(time.RFC3339Nano), "prompt_cache.shadow_stats", "service.prompt_cache_shadow", 1, l.opts.InstanceID, "process", sequence, final, publish()}
		raw, err := json.Marshal(event)
		if err == nil {
			err = l.store.append(append(raw, '\n'), now)
		}
		if err != nil {
			maintenance++
			warn(err)
		}
		publish()
	}
	for {
		select {
		case e, ok := <-l.queue:
			if !ok {
				status(true)
				if err := l.store.close(); err != nil {
					maintenance++
					warn(err)
				}
				publish()
				return
			}
			buf, err := enc.EncodeEntry(zapcore.Entry{Time: e.at, Level: zapcore.InfoLevel, Message: "prompt_cache.shadow"}, e.fields)
			if err == nil {
				if buf.Len() > maxLogEventBytes {
					l.oversized.Add(1)
				} else {
					err = l.store.append(buf.Bytes(), time.Now())
					if err == nil {
						written++
					}
				}
				buf.Free()
			}
			if err != nil {
				failed++
				warn(err)
			}
			publish()
		case <-ticker.C:
			status(false)
		}
	}
}

// Only files with our exact generated names are eligible for cleanup. A lock
// prevents two application processes sharing a directory from racing retention.
var segmentName = regexp.MustCompile(`^shadow-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[a-f0-9]{16}-[0-9]+\.jsonl(?:\.gz)?$`)

type segmentFile struct {
	path     string
	size     int64
	modified time.Time
}
type segmentLog struct {
	opts                   LogOptions
	lock, file             *os.File
	path, day, compressing string
	size, total            int64
	sequence               uint64
	stats                  LogSnapshot
}

func newSegmentLog(opts LogOptions) (*segmentLog, error) {
	if !regexp.MustCompile(`^[a-f0-9]{16}$`).MatchString(opts.InstanceID) || opts.MaxSegmentBytes < 1024 {
		return nil, errors.New("invalid observer log instance or storage limits")
	}
	if err := os.MkdirAll(opts.Dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(opts.Dir, ".writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("observer log directory already in use: %w", err)
	}
	s := &segmentLog{opts: opts, lock: lock}
	entries, err := os.ReadDir(opts.Dir)
	if err == nil {
		for _, entry := range entries {
			if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".gz.tmp") && segmentName.MatchString(strings.TrimSuffix(entry.Name(), ".tmp")) {
				err = os.Remove(filepath.Join(opts.Dir, entry.Name()))
				if err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		_ = s.close()
		return nil, err
	}
	if err = s.prune(time.Now(), 0); err != nil {
		_ = s.close()
		return nil, err
	}
	return s, nil
}

func (s *segmentLog) files() ([]segmentFile, error) {
	entries, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		return nil, err
	}
	files := make([]segmentFile, 0, len(entries))
	for _, entry := range entries {
		if !segmentName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		files = append(files, segmentFile{filepath.Join(s.opts.Dir, entry.Name()), info.Size(), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modified.Equal(files[j].modified) {
			return files[i].path < files[j].path
		}
		return files[i].modified.Before(files[j].modified)
	})
	return files, nil
}

func (s *segmentLog) prune(now time.Time, reserve int64) error {
	files, err := s.files()
	if err != nil {
		return err
	}
	s.total = 0
	for _, f := range files {
		s.total += f.size
	}
	s.stats.OldestSegmentTime = ""
	for _, f := range files {
		if f.path == s.path || f.path == s.compressing {
			continue
		}
		aged := now.Sub(f.modified) > s.opts.Retention
		capacity := s.total+reserve > s.opts.MaxTotalBytes
		if aged || capacity {
			if err := os.Remove(f.path); err != nil {
				return err
			}
			s.total -= f.size
			if aged {
				s.stats.AgeDeletedFiles++
			} else {
				s.stats.CapacityDeletedFiles++
				s.stats.RetentionShortened = true
			}
		} else if s.stats.OldestSegmentTime == "" {
			s.stats.OldestSegmentTime = f.modified.UTC().Format(time.RFC3339Nano)
		}
	}
	if s.total+reserve > s.opts.MaxTotalBytes {
		return errors.New("observer log capacity exhausted")
	}
	return nil
}

func (s *segmentLog) open(now time.Time) error {
	for {
		s.sequence++
		path := filepath.Join(s.opts.Dir, fmt.Sprintf("shadow-%s-%s-%06d.jsonl", now.UTC().Format("20060102T150405.000000000Z"), s.opts.InstanceID, s.sequence))
		// Even a repeated clock/instance must not overwrite an older archive.
		if _, err := os.Lstat(path + ".gz"); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		s.file, s.path, s.size, s.day = f, path, 0, now.UTC().Format("20060102")
		return nil
	}
}

func (s *segmentLog) append(raw []byte, now time.Time) error {
	if int64(len(raw)) > s.opts.MaxSegmentBytes {
		return errors.New("observer event exceeds segment limit")
	}
	if s.file != nil && (s.size+int64(len(raw)) > s.opts.MaxSegmentBytes || s.day != now.UTC().Format("20060102")) {
		if err := s.rotate(now); err != nil {
			return err
		}
	}
	if s.file == nil {
		if err := s.open(now); err != nil {
			return err
		}
	}
	if s.total+int64(len(raw)) > s.opts.MaxTotalBytes {
		if err := s.prune(now, int64(len(raw))); err != nil {
			return err
		}
	}
	n, err := s.file.Write(raw)
	if err != nil || n != len(raw) {
		// Do not leave a partial JSON line in the middle of a segment.
		rollback := s.file.Truncate(s.size)
		_, seek := s.file.Seek(s.size, io.SeekStart)
		if rollback != nil || seek != nil {
			_ = s.file.Close()
			s.file = nil
			s.path = ""
			s.size = 0
		}
		return errors.Join(err, rollback, seek, io.ErrShortWrite)
	}
	s.size += int64(n)
	s.total += int64(n)
	return nil
}

func (s *segmentLog) rotate(now time.Time) error {
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	path := s.path
	// Keep the source protected from capacity cleanup during compression.
	defer func() { s.path = ""; s.size = 0 }()
	if err != nil {
		return err
	}
	return s.compress(path, now)
}

func (s *segmentLog) compress(path string, now time.Time) error {
	s.compressing = path
	defer func() { s.compressing = "" }()
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	// DEFLATE can slightly expand incompressible data. Reserve more than its
	// worst-case overhead, counting both source and temporary destination.
	reserve := info.Size() + info.Size()/100 + 1024
	if err = s.prune(now, reserve); err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp := path + ".gz.tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	zw, err := gzip.NewWriterLevel(dst, gzip.BestSpeed)
	if err != nil {
		_ = dst.Close()
		return err
	}
	_, copied := io.Copy(zw, src)
	compressed := zw.Close()
	synced := dst.Sync()
	closed := dst.Close()
	if err = errors.Join(copied, compressed, synced, closed); err != nil {
		return err
	}
	if err = os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	if err = os.Rename(tmp, path+".gz"); err != nil {
		return err
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return s.prune(now, 0)
}

func (s *segmentLog) maintain(now time.Time) error {
	if s.file != nil && s.day != now.UTC().Format("20060102") {
		if err := s.rotate(now); err != nil {
			return err
		}
	}
	if err := s.prune(now, 0); err != nil {
		return err
	}
	// Recover one uncompressed segment left by a crash per tick. Only this
	// worker touches the directory while holding the exclusive process lock.
	files, err := s.files()
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.path != s.path && strings.HasSuffix(f.path, ".jsonl") {
			err = s.compress(f.path, now)
			return err
		}
	}
	return nil
}

func (s *segmentLog) snapshot() LogSnapshot { out := s.stats; out.RetainedBytes = s.total; return out }
func (s *segmentLog) close() error {
	err := s.rotate(time.Now())
	if s.lock != nil {
		err = errors.Join(err, unix.Flock(int(s.lock.Fd()), unix.LOCK_UN), s.lock.Close())
		s.lock = nil
	}
	return err
}
