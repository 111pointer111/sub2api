package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) GetOpenAIPlatformUsage(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	usage, err := h.accountUsageService.GetOpenAIPlatformUsage(c.Request.Context(), accountID, c.Query("force") == "true")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, usage)
}
