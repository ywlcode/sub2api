package service

import (
	"context"
	"errors"
	"time"
)

const (
	authLookupMaxWaiting  = 512
	authLookupWaitTimeout = 5 * time.Second
)

func (s *APIKeyService) beginAuthLookup(ctx context.Context) (func(), error) {
	if s.authLookupSlots == nil {
		return func() {}, nil
	}
	s.authLookupTotal.Add(1)
	if err := s.acquireAuthLookupSlot(ctx); err != nil {
		if errors.Is(err, ErrAPIKeyAuthOverloaded) {
			s.authLookupRejected.Add(1)
		}
		return nil, err
	}
	s.authLookupInFlight.Add(1)
	return func() { s.authLookupInFlight.Add(-1); <-s.authLookupSlots }, nil
}

func (s *APIKeyService) acquireAuthLookupSlot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.authLookupSlots <- struct{}{}:
		return nil
	default:
	}
	// A cold cache can receive many distinct valid Keys at once. Queue short
	// bursts on every site without increasing simultaneous database queries.
	// The queue and deadline also bound work from invalid credentials.
	if s.authLookupWaiters == nil {
		return ErrAPIKeyAuthOverloaded
	}
	select {
	case s.authLookupWaiters <- struct{}{}:
		defer func() { <-s.authLookupWaiters }()
	default:
		return ErrAPIKeyAuthOverloaded
	}
	timer := time.NewTimer(authLookupWaitTimeout)
	defer timer.Stop()
	select {
	case s.authLookupSlots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.authLookupSlots
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrAPIKeyAuthOverloaded
	}
}
