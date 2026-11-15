package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	auditLogQueueCapacity = 4096
	auditLogBatchSize     = 100
	auditLogFlushInterval = time.Second

	auditRetentionBatchSize = 5000
)

// AuditLogService 管理面操作审计日志服务。
// 写入端为非阻塞异步批量落库（不拖慢管理请求）；
// 读取端提供分页查询；清空端点由 handler 层做 TOTP 强校验后调用 ClearAll。
type AuditLogService struct {
	repo           AuditLogRepository
	settingService *SettingService

	queue chan *AuditLog

	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	writerOnce sync.Once

	droppedCount uint64
	writeFailed  uint64
	writtenCount uint64
}

func NewAuditLogService(repo AuditLogRepository, settingService *SettingService) *AuditLogService {
	ctx, cancel := context.WithCancel(context.Background())
	return &AuditLogService{
		repo:           repo,
		settingService: settingService,
		queue:          make(chan *AuditLog, auditLogQueueCapacity),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Start starts the local writer. OpsCleanupService owns shared retention.
func (s *AuditLogService) Start() {
	s.StartWriter()
}

// StartWriter drains this process's audit queue, including on gateway replicas.
func (s *AuditLogService) StartWriter() {
	if s == nil || s.repo == nil {
		return
	}
	s.writerOnce.Do(func() {
		s.wg.Add(1)
		go s.runWriter()
	})
}

// Stop 停止服务并尽量落盘队列中剩余记录。
func (s *AuditLogService) Stop() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// Record 非阻塞入队一条审计记录；队列打满时丢弃并计数（管理面流量下几乎不可能发生）。
func (s *AuditLogService) Record(entry *AuditLog) {
	if s == nil || entry == nil {
		return
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	select {
	case <-s.ctx.Done():
		return
	default:
	}
	select {
	case s.queue <- entry:
	default:
		atomic.AddUint64(&s.droppedCount, 1)
	}
}

// List 分页查询审计日志。
func (s *AuditLogService) List(ctx context.Context, filter *AuditLogFilter) (*AuditLogList, error) {
	return s.repo.List(ctx, filter)
}

// GetByID 查询单条详情。
func (s *AuditLogService) GetByID(ctx context.Context, id int64) (*AuditLog, error) {
	return s.repo.GetByID(ctx, id)
}

// ClearAll 全量清空审计日志并写入留痕记录。
// 调用方（handler）必须先完成 TOTP 验证；本方法负责：
//  1. 统计并清空全表
//  2. 同步写入一条 "audit_log.clear" 留痕记录（绕过异步队列，保证落库）
func (s *AuditLogService) ClearAll(ctx context.Context, trace *AuditLog) (int64, error) {
	deleted, err := s.repo.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("count audit logs: %w", err)
	}
	if err := s.repo.TruncateAll(ctx); err != nil {
		return 0, fmt.Errorf("truncate audit logs: %w", err)
	}

	if trace != nil {
		trace.Action = AuditActionAuditLogClear
		if trace.CreatedAt.IsZero() {
			trace.CreatedAt = time.Now().UTC()
		}
		if trace.Extra == nil {
			trace.Extra = map[string]any{}
		}
		trace.Extra["deleted_rows"] = deleted
		if err := s.repo.Insert(ctx, trace); err != nil {
			// 留痕失败必须显式暴露：清空已发生，但审计链断裂。
			return deleted, fmt.Errorf("audit logs cleared (%d rows) but failed to persist clear-trace record: %w", deleted, err)
		}
	}
	return deleted, nil
}

func (s *AuditLogService) runWriter() {
	defer s.wg.Done()

	ticker := time.NewTicker(auditLogFlushInterval)
	defer ticker.Stop()

	batch := make([]*AuditLog, 0, auditLogBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		inserted, err := s.repo.BatchInsert(ctx, batch)
		cancel()
		if err != nil {
			atomic.AddUint64(&s.writeFailed, uint64(len(batch)))
			_, _ = fmt.Fprintf(os.Stderr, "time=%s level=WARN msg=\"audit log flush failed\" err=%v batch=%d\n",
				time.Now().Format(time.RFC3339Nano), err, len(batch))
		} else {
			atomic.AddUint64(&s.writtenCount, uint64(inserted))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-s.ctx.Done():
			// 停机前排空队列。
			for {
				select {
				case item := <-s.queue:
					if item == nil {
						continue
					}
					batch = append(batch, item)
					if len(batch) >= auditLogBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case item := <-s.queue:
			if item == nil {
				continue
			}
			batch = append(batch, item)
			if len(batch) >= auditLogBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// RunRetention is invoked by OpsCleanupService under its shared schedule/lock.
func (s *AuditLogService) RunRetention(ctx context.Context, now time.Time) error {
	if s == nil || s.repo == nil || s.settingService == nil || s.settingService.settingRepo == nil {
		return nil
	}
	raw, err := s.settingService.settingRepo.GetValue(ctx, SettingKeyAuditLogRetentionDays)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return fmt.Errorf("read audit retention: %w", err)
	}
	days := defaultAuditLogRetentionDays
	if raw = strings.TrimSpace(raw); raw != "" {
		days, err = strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("parse audit retention: %w", err)
		}
	}
	if days <= 0 {
		return nil // Explicit permanent retention.
	}
	cutoff := now.UTC().AddDate(0, 0, -days)
	for {
		deleted, err := s.repo.DeleteBefore(ctx, cutoff, auditRetentionBatchSize)
		if err != nil {
			return err
		}
		if deleted < auditRetentionBatchSize {
			return nil
		}
	}
}
