package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Secondary auth deliberately reads current local balances and permissions.
// Fetch the same projection as GetByKeyForAuth, including denied models, in one
// statement instead of issuing an eager-load query for each edge. PostgreSQL's
// statement snapshot also prevents mixing rows from two replica installations.
var secondaryAuthQuery = func() string {
	projection := func(alias string, columns []string) string {
		return alias + "." + strings.Join(columns, ", "+alias+".")
	}
	return `SELECT row_to_json(kp),
		CASE WHEN up.id IS NOT NULL THEN row_to_json(up) END,
		CASE WHEN gp.id IS NOT NULL THEN row_to_json(gp) END,
		(SELECT json_agg(ag.id ORDER BY ag.id)
		 FROM user_allowed_groups b JOIN groups ag ON ag.id = b.group_id AND ag.deleted_at IS NULL
		 WHERE b.user_id = k.user_id),
		(SELECT json_agg(b.group_id ORDER BY b.priority, b.group_id)
		 FROM api_key_groups b JOIN groups eg ON eg.id = b.group_id AND eg.deleted_at IS NULL
		 WHERE b.api_key_id = k.id),
		(SELECT denied_models FROM user_group_rate_multipliers
		 WHERE user_id = k.user_id AND group_id = k.group_id)
	FROM api_keys k
	CROSS JOIN LATERAL (SELECT ` + projection("k", authKeyColumns) + `) kp
	LEFT JOIN LATERAL (SELECT ` + projection("u", authUserColumns) + `
		FROM users u WHERE u.id = k.user_id AND u.deleted_at IS NULL) up ON true
	LEFT JOIN LATERAL (SELECT ` + projection("g", authGroupColumns) + `
		FROM groups g WHERE g.id = k.group_id AND g.deleted_at IS NULL) gp ON true
	WHERE k.key = $1 AND k.deleted_at IS NULL`
}()

func (r *apiKeyRepository) GetByKeyForSecondaryAuth(ctx context.Context, key string) (*service.APIKey, error) {
	queryer, err := r.secondaryAuthQueryer(ctx)
	if err != nil {
		return nil, err
	}
	var keyJSON, userJSON, groupJSON, allowedJSON, extraJSON, deniedJSON []byte
	err = scanSingleRow(ctx, queryer, secondaryAuthQuery, []any{key},
		&keyJSON, &userJSON, &groupJSON, &allowedJSON, &extraJSON, &deniedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	var entity dbent.APIKey
	var allowed, extra []int64
	for _, part := range []struct {
		raw []byte
		dst any
	}{
		{keyJSON, &entity}, {userJSON, &entity.Edges.User}, {groupJSON, &entity.Edges.Group},
		{allowedJSON, &allowed}, {extraJSON, &extra},
	} {
		if len(part.raw) > 0 {
			if err := json.Unmarshal(part.raw, part.dst); err != nil {
				return nil, fmt.Errorf("decode secondary auth projection: %w", err)
			}
		}
	}
	denied, err := decodeUserGroupDeniedModels(deniedJSON)
	if err != nil {
		return nil, fmt.Errorf("decode secondary auth denied models: %w", err)
	}
	keyState := apiKeyEntityToService(&entity)
	if keyState.User != nil {
		keyState.User.AllowedGroups = allowed
		keyState.User.UserGroupDeniedModels = denied
	}
	// Preserve the legacy primary binding first, followed by ordered extras.
	if keyState.GroupID != nil {
		keyState.GroupIDs = []int64{*keyState.GroupID}
	}
	for _, id := range extra {
		if keyState.GroupID == nil || id != *keyState.GroupID {
			keyState.GroupIDs = append(keyState.GroupIDs, id)
		}
	}
	return keyState, nil
}

type preparedSecondaryAuth struct{ stmt *sql.Stmt }

// Small, always-fresh read. Config/cache synchronization is fenced by the
// replica installer; this query also observes local debits and status changes.
func (r *apiKeyRepository) GetSecondaryAuthState(ctx context.Context, key string) (*service.SecondaryAuthState, error) {
	state := new(service.SecondaryAuthState)
	err := scanSingleRow(ctx, r.sql, `SELECT k.id,k.user_id,k.group_id,k.status,
		COALESCE(u.status,''),COALESCE(g.status,''),u.id IS NOT NULL,g.id IS NOT NULL,
		COALESCE(u.balance,0),k.quota,k.quota_used,k.expires_at
		FROM api_keys k
		LEFT JOIN users u ON u.id=k.user_id AND u.deleted_at IS NULL
		LEFT JOIN groups g ON g.id=k.group_id AND g.deleted_at IS NULL
		WHERE k.key=$1 AND k.deleted_at IS NULL`, []any{key},
		&state.KeyID, &state.UserID, &state.GroupID, &state.KeyStatus, &state.UserStatus, &state.GroupStatus,
		&state.UserExists, &state.GroupExists, &state.Balance, &state.Quota, &state.QuotaUsed, &state.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (q preparedSecondaryAuth) QueryContext(ctx context.Context, _ string, args ...any) (*sql.Rows, error) {
	return q.stmt.QueryContext(ctx, args...)
}

// Reuse a prepared statement across the bounded connection pool. Without this,
// parsing and planning the full auth projection on every admission dominates
// execution time under a burst. Only the SQL plan is reused, never auth data.
// database/sql closes its per-connection statements when the owning DB closes.
func (r *apiKeyRepository) secondaryAuthQueryer(ctx context.Context) (sqlQueryer, error) {
	db, ok := r.sql.(*sql.DB)
	if !ok {
		// Transaction-bound repositories retain that transaction's snapshot.
		return r.sql, nil
	}
	if stmt := r.secondaryAuthStmt.Load(); stmt != nil {
		return preparedSecondaryAuth{stmt}, nil
	}
	ready := r.secondaryAuthPrepare.DoChan("prepare", func() (any, error) {
		if stmt := r.secondaryAuthStmt.Load(); stmt != nil {
			return stmt, nil
		}
		// One cancelled client must not poison shared initialization. A failed
		// prepare is not cached; subsequent requests can retry after recovery.
		prepareCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stmt, err := db.PrepareContext(prepareCtx, secondaryAuthQuery)
		if err != nil {
			return nil, err
		}
		r.secondaryAuthStmt.Store(stmt)
		return stmt, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ready:
		if result.Err != nil {
			return nil, result.Err
		}
		stmt, ok := result.Val.(*sql.Stmt)
		if !ok || stmt == nil {
			return nil, errors.New("secondary auth prepared statement unavailable")
		}
		return preparedSecondaryAuth{stmt}, nil
	}
}
