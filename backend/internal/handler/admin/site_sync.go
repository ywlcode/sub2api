package admin

import "context"

func (h *TLSFingerprintProfileHandler) RefreshSiteReplica(ctx context.Context) error {
	if h == nil || h.service == nil {
		return nil
	}
	return h.service.RefreshSiteReplica(ctx)
}
