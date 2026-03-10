package router

import (
	"embed"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-contrib/gzip"
	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

func isLikelyMissingV1ProxyRoute(path string) bool {
	if path == "/responses" || path == "/responses/compact" ||
		path == "/chat/completions" || path == "/completions" ||
		path == "/embeddings" || path == "/messages" ||
		path == "/moderations" || path == "/rerank" {
		return true
	}
	return strings.HasPrefix(path, "/audio/") || strings.HasPrefix(path, "/images/") || path == "/models"
}

func SetWebRouter(router *gin.Engine, buildFS embed.FS, indexPage []byte) {
	router.Use(gzip.Gzip(gzip.DefaultCompression))
	router.Use(middleware.GlobalWebRateLimit())
	router.Use(middleware.Cache())
	router.Use(static.Serve("/", common.EmbedFolder(buildFS, "web/dist")))
	router.NoRoute(func(c *gin.Context) {
		c.Set(middleware.RouteTagKey, "web")
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/v1") || strings.HasPrefix(path, "/api") || strings.HasPrefix(path, "/assets") {
			controller.RelayNotFound(c)
			return
		}
		if isLikelyMissingV1ProxyRoute(path) {
			common.SysLog("proxy request hit web router, likely missing /v1 prefix: method=" + c.Request.Method + " path=" + path + " remote=" + c.ClientIP())
			controller.RelayNotFound(c)
			return
		}
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexPage)
	})
}
