//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type selfRecordsTestRepo struct{ ContentModerationRepository }

func (selfRecordsTestRepo) MyModerationRecords(context.Context, int64, int, int, time.Time, bool) (*MyModerationRecordsPage, error) {
	return &MyModerationRecordsPage{AutoBanEnabled: true, Items: []MyModerationRecord{
		{InputExcerpt: "saved text password=fixturelongsecret"},
		{InputExcerpt: "[Recovered context withheld]"},
	}}, nil
}

func TestMyModerationRecordsSanitizesRetainedContent(t *testing.T) {
	s := &ContentModerationService{
		settingRepo: &contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: `{"ban_threshold":7,"auto_ban_enabled":true}`,
		}},
		repo: selfRecordsTestRepo{},
	}
	result, err := s.MyModerationRecords(context.Background(), 27, 1, 10)
	require.NoError(t, err)
	require.Equal(t, 7, result.Threshold)
	require.True(t, result.AutoBanEnabled)
	require.Contains(t, result.Items[0].InputExcerpt, "saved text")
	require.NotContains(t, result.Items[0].InputExcerpt, "fixturelongsecret")
	require.Empty(t, result.Items[1].InputExcerpt)
}
