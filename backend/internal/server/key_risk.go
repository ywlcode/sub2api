package server

import (
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type keyRiskStarter interface {
	SetKeyRiskAPIKeys(*service.APIKeyService)
	StartKeyRisk(string) (func(), error)
}

func configureKeyRisk(cfg *config.Config, starter keyRiskStarter, keys *service.APIKeyService, lifecycle *Lifecycle) {
	if !cfg.RunsBackgroundJobs() {
		return
	}
	starter.SetKeyRiskAPIKeys(keys)
	stop, err := starter.StartKeyRisk(cfg.Database.DSN())
	if err != nil {
		slog.Warn("key risk initialization failed")
		return
	}
	lifecycle.onDrain = append(lifecycle.onDrain, stop)
}
