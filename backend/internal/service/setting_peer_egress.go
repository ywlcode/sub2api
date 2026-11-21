package service

import (
	"context"
	"encoding/json"
	"errors"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/site"
)

type PeerEgressView struct {
	Node string `json:"node"`
	site.EgressSettings
}

func (s *SettingService) peerEgressKey(node string) (string, error) {
	if s.cfg != nil && s.cfg.Site.EffectiveMode() == site.Primary {
		for _, p := range s.cfg.Site.Peers {
			if p.ID == node {
				return "site_peer_egress." + node, nil
			}
		}
	}
	return "", infraerrors.NotFound("SITE_PEER_NOT_FOUND", "Unknown secondary site")
}

func (s *SettingService) GetPeerEgress(ctx context.Context, node string) (p site.EgressSettings, err error) {
	key, err := s.peerEgressKey(node)
	if err != nil {
		return p, err
	}
	values, err := s.settingRepo.GetMultiple(ctx, []string{key})
	if err != nil {
		return p, err
	}
	if raw := values[key]; raw != "" {
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return p, errors.New("invalid stored secondary egress settings")
		}
	}
	return p, p.Validate()
}

func (s *SettingService) ListPeerEgress(ctx context.Context) ([]PeerEgressView, error) {
	out := []PeerEgressView{}
	if s.cfg == nil || s.cfg.Site.EffectiveMode() != site.Primary {
		return out, nil
	}
	for _, peer := range s.cfg.Site.Peers {
		p, err := s.GetPeerEgress(ctx, peer.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, PeerEgressView{peer.ID, p})
	}
	return out, nil
}
func (s *SettingService) SetPeerEgress(ctx context.Context, node string, p site.EgressSettings) error {
	key, err := s.peerEgressKey(node)
	if err != nil {
		return err
	}
	if err = p.Validate(); err != nil {
		return infraerrors.BadRequest("INVALID_SITE_PROXY", err.Error())
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, key, string(raw))
}
