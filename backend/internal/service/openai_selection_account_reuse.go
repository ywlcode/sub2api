package service

import (
	"context"
	"fmt"
)

type openAISelectionAccountReuseKey struct{}

// Owned by one synchronous legacy selection call. Never attach this state to
// the inbound request: retries, fallback passes and queue re-entry need fresh
// reads. Only an exact account returned by the full DB recheck may be reused.
type openAISelectionAccountReuse struct{ accounts map[*Account]struct{} }

func withOpenAISelectionAccountReuse(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAISelectionAccountReuseKey{}, &openAISelectionAccountReuse{})
}

func rememberRecheckedOpenAIAccount(ctx context.Context, account *Account) {
	if reuse, ok := ctx.Value(openAISelectionAccountReuseKey{}).(*openAISelectionAccountReuse); ok {
		if reuse.accounts == nil {
			reuse.accounts = make(map[*Account]struct{})
		}
		reuse.accounts[account] = struct{}{}
	}
}

func (s *OpenAIGatewayService) reuseRecheckedOpenAIAccount(ctx context.Context, account *Account) (*Account, bool, error) {
	reuse, ok := ctx.Value(openAISelectionAccountReuseKey{}).(*openAISelectionAccountReuse)
	if !ok || account == nil || s.schedulerSnapshot.localSnapshotReader != nil {
		return nil, false, nil
	}
	if _, verified := reuse.accounts[account]; !verified {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	// GetAccount normally reapplies this live authority check after decoding
	// Redis. Preserve it while using the complete, newer DB account instead.
	accounts, err := s.schedulerSnapshot.markPerformanceAccounts(ctx, map[int64]*Account{account.ID: account})
	if err != nil {
		return nil, true, err
	}
	hydrated := accounts[account.ID]
	if hydrated == nil || !hydrated.IsSchedulable() {
		return nil, true, fmt.Errorf("%w: selected account %d is no longer schedulable", ErrNoAvailableAccounts, account.ID)
	}
	// A non-batch path can hydrate twice in this same selection. The owned
	// copy still contains the verified DB configuration; recheck pause again.
	reuse.accounts[hydrated] = struct{}{}
	return hydrated, true, nil
}
