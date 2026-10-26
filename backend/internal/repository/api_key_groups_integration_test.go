//go:build integration

package repository

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
)

func (s *APIKeyRepoSuite) TestAdditionalGroupReplacementAndPrimaryRemoval() {
	user := s.mustCreateUser("replace-multi@test.invalid")
	first := s.mustCreateGroup("multi-first")
	second := s.mustCreateGroup("multi-second")
	third := s.mustCreateGroup("multi-third")
	key := &service.APIKey{UserID: user.ID, Key: "sk-multi-ctes", Name: "multi", Status: service.StatusActive, GroupID: &first.ID, GroupIDs: []int64{first.ID, second.ID}}
	s.Require().NoError(s.repo.Create(s.ctx, key))
	affected, err := s.repo.UpdateGroupIDByUserAndGroup(s.ctx, user.ID, second.ID, third.ID)
	s.Require().NoError(err)
	s.Equal(int64(1), affected)
	got, err := s.repo.GetByKeyForAuth(s.ctx, key.Key)
	s.Require().NoError(err)
	s.Equal([]int64{first.ID, third.ID}, got.GroupIDs)
	_, err = s.repo.ClearGroupIDByGroupID(s.ctx, first.ID)
	s.Require().NoError(err)
	got, err = s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.GroupID)
	s.Equal(third.ID, *got.GroupID)
	s.Equal([]int64{third.ID}, got.GroupIDs)
}

func (s *APIKeyRepoSuite) TestGroupPrioritySurvivesReplacementAndPrimaryRemoval() {
	user := s.mustCreateUser("priority-multi@test.invalid")
	first := s.mustCreateGroup("priority-first")
	second := s.mustCreateGroup("priority-second")
	third := s.mustCreateGroup("priority-third")
	fourth := s.mustCreateGroup("priority-fourth")
	replacement := s.mustCreateGroup("priority-replacement")
	key := &service.APIKey{UserID: user.ID, Key: "sk-priority-multi", Name: "priority", Status: service.StatusActive,
		GroupID: &first.ID, GroupIDs: []int64{first.ID, third.ID, second.ID, fourth.ID}}
	s.Require().NoError(s.repo.Create(s.ctx, key))
	_, err := s.repo.UpdateGroupIDByUserAndGroup(s.ctx, user.ID, third.ID, replacement.ID)
	s.Require().NoError(err)
	got, err := s.repo.GetByKeyForAuth(s.ctx, key.Key)
	s.Require().NoError(err)
	s.Equal([]int64{first.ID, replacement.ID, second.ID, fourth.ID}, got.GroupIDs)
	// Merging with a group already on the key keeps the earlier priority.
	_, err = s.repo.UpdateGroupIDByUserAndGroup(s.ctx, user.ID, fourth.ID, replacement.ID)
	s.Require().NoError(err)
	_, err = s.repo.ClearGroupIDByGroupID(s.ctx, first.ID)
	s.Require().NoError(err)
	got, err = s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.GroupID)
	s.Equal(replacement.ID, *got.GroupID)
	s.Equal([]int64{replacement.ID, second.ID}, got.GroupIDs)
}

func (s *APIKeyRepoSuite) TestGroupPriorityMigrationPreservesLegacyOrderAndIsIdempotent() {
	user := s.mustCreateUser("legacy-priority@test.invalid")
	first := s.mustCreateGroup("legacy-first")
	second := s.mustCreateGroup("legacy-second")
	third := s.mustCreateGroup("legacy-third")
	key := &service.APIKey{UserID: user.ID, Key: "sk-legacy-priority", Name: "legacy", Status: service.StatusActive, GroupID: &first.ID}
	s.Require().NoError(s.repo.Create(s.ctx, key))
	_, err := s.client.ExecContext(s.ctx, `INSERT INTO api_key_groups (api_key_id, group_id) VALUES ($1, $2), ($1, $3)`, key.ID, third.ID, second.ID)
	s.Require().NoError(err)
	sql, err := migrations.FS.ReadFile("308_api_key_group_priority.sql")
	s.Require().NoError(err)
	_, err = s.client.ExecContext(s.ctx, string(sql))
	s.Require().NoError(err)
	got, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Equal([]int64{first.ID, second.ID, third.ID}, got.GroupIDs)
	got.GroupIDs = []int64{first.ID, third.ID, second.ID}
	s.Require().NoError(s.repo.Update(s.ctx, got, service.APIKeyUpdateFields{GroupID: true}))
	_, err = s.client.ExecContext(s.ctx, string(sql))
	s.Require().NoError(err)
	got, err = s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Equal([]int64{first.ID, third.ID, second.ID}, got.GroupIDs)
}
