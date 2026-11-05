package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type ConnectionTraceBackfillService struct {
	repo   UsageLogRepository
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (s *ConnectionTraceBackfillService) Start() {
	reader, ok := s.repo.(interface {
		BackfillConnectionTraceBodyBytes(context.Context, int) (bool, error)
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				batchCtx, batchCancel := context.WithTimeout(ctx, 3*time.Second)
				done, err := reader.BackfillConnectionTraceBodyBytes(batchCtx, 500)
				batchCancel()
				if err != nil {
					logger.LegacyPrintf("connection_diagnostics", "body byte backfill batch failed: %v", err)
					timer.Reset(time.Minute)
				} else if done {
					return
				} else {
					timer.Reset(time.Second)
				}
			}
		}
	}()
}

func (s *ConnectionTraceBackfillService) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}
