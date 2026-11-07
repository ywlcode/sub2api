package repository

import (
	"context"
	"database/sql"
)

// GetRequestPolicy reads both selection controls from the same row/snapshot.
func (r *userGroupRateRepository) GetRequestPolicy(ctx context.Context, userID, groupID int64) (*int, []string, error) {
	var rpm sql.NullInt32
	var denied []byte
	err := scanSingleRow(ctx, r.sql, `SELECT rpm_override, denied_models FROM user_group_rate_multipliers WHERE user_id = $1 AND group_id = $2`, []any{userID, groupID}, &rpm, &denied)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	models, err := decodeUserGroupDeniedModels(denied)
	if err != nil {
		return nil, nil, err
	}
	var limit *int
	if rpm.Valid {
		value := int(rpm.Int32)
		limit = &value
	}
	return limit, models, nil
}
