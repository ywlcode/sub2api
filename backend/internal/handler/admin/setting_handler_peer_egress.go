package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/site"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) ListPeerEgress(c *gin.Context) {
	out, err := h.settingService.ListPeerEgress(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, out)
}
func (h *SettingHandler) UpdatePeerEgress(c *gin.Context) {
	var req struct {
		Processing *string `json:"processing_proxy_url"`
		Forward    *string `json:"forward_proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Processing == nil || req.Forward == nil {
		response.BadRequest(c, "Both proxy fields are required; use an empty string for the node default.")
		return
	}
	settings := site.EgressSettings{Processing: *req.Processing, Forward: *req.Forward}
	if err := h.settingService.SetPeerEgress(c.Request.Context(), c.Param("node"), settings); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}
