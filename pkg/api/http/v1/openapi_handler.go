package v1

import (
	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/api/generated"
	"github.com/nais/bifrost/pkg/api/http/v1/handlers"
	"github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	"github.com/sirupsen/logrus"
)

// OpenAPIHandler implements the generated ServerInterface by delegating to existing handlers
type OpenAPIHandler struct {
	unleashHandler        *handlers.UnleashHandler
	releaseChannelHandler *handlers.ReleaseChannelHandler
}

// NewOpenAPIHandler creates a new OpenAPI handler adapter
func NewOpenAPIHandler(
	service unleash.IService,
	config *config.Config,
	logger *logrus.Logger,
	releaseChannelRepo releasechannel.Repository,
) *OpenAPIHandler {
	return &OpenAPIHandler{
		unleashHandler:        handlers.NewUnleashHandler(service, config, logger, releaseChannelRepo),
		releaseChannelHandler: handlers.NewReleaseChannelHandler(releaseChannelRepo, logger),
	}
}

// Verify that OpenAPIHandler implements the generated.ServerInterface
var _ generated.ServerInterface = (*OpenAPIHandler)(nil)

// ListChannels implements GET /releasechannels
func (h *OpenAPIHandler) ListChannels(c *gin.Context) {
	h.releaseChannelHandler.ListChannels(c)
}

// GetChannel implements GET /releasechannels/{name}
func (h *OpenAPIHandler) GetChannel(c *gin.Context, name string) {
	// The generated code doesn't set the path param in the context,
	// so we need to set it manually for the existing handler
	c.Params = append(c.Params, gin.Param{Key: "name", Value: name})
	h.releaseChannelHandler.GetChannel(c)
}

// ListInstances implements GET /unleash
func (h *OpenAPIHandler) ListInstances(c *gin.Context) {
	h.unleashHandler.ListInstances(c)
}

// CreateInstance implements POST /unleash
func (h *OpenAPIHandler) CreateInstance(c *gin.Context) {
	h.unleashHandler.CreateInstance(c)
}

// DeleteInstance implements DELETE /unleash/{name}
func (h *OpenAPIHandler) DeleteInstance(c *gin.Context, name string) {
	c.Params = append(c.Params, gin.Param{Key: "name", Value: name})
	h.unleashHandler.DeleteInstance(c)
}

// GetInstance implements GET /unleash/{name}
func (h *OpenAPIHandler) GetInstance(c *gin.Context, name string) {
	c.Params = append(c.Params, gin.Param{Key: "name", Value: name})
	h.unleashHandler.GetInstance(c)
}

// UpdateInstance implements PUT /unleash/{name}
func (h *OpenAPIHandler) UpdateInstance(c *gin.Context, name string) {
	c.Params = append(c.Params, gin.Param{Key: "name", Value: name})
	h.unleashHandler.UpdateInstance(c)
}
