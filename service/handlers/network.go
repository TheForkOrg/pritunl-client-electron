package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/pritunl/pritunl-client-electron/service/profile"
	"github.com/pritunl/pritunl-client-electron/service/utils"
)

func networkDnsReset(c *gin.Context) {
	utils.ResetDns()
	utils.ClearDNSCache()

	c.JSON(200, nil)
}

func networkAllReset(c *gin.Context) {
	utils.ResetDns()
	utils.ClearDns()
	utils.ResetNetworking()
	utils.ClearDNSCache()

	_ = profile.RestartProfiles(false)

	c.JSON(200, nil)
}
