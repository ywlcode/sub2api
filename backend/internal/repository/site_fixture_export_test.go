//go:build integration

package repository

import (
	"database/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"testing"
)

func SiteTestDatabase(t *testing.T) (*sql.DB, *ent.Client) { return newBillingAuditDatabase(t) }
func SiteTestUser(t *testing.T, c *ent.Client, u *service.User) *service.User {
	return mustCreateUser(t, c, u)
}
func SiteTestGroup(t *testing.T, c *ent.Client, g *service.Group) *service.Group {
	return mustCreateGroup(t, c, g)
}
func SiteTestAccount(t *testing.T, c *ent.Client, a *service.Account) *service.Account {
	return mustCreateAccount(t, c, a)
}
func SiteTestKey(t *testing.T, c *ent.Client, k *service.APIKey) *service.APIKey {
	return mustCreateApiKey(t, c, k)
}
