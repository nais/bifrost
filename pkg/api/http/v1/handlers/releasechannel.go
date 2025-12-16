package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	"github.com/sirupsen/logrus"
)

type ReleaseChannelHandler struct {
	repository releasechannel.Repository
	logger     *logrus.Logger
}

func NewReleaseChannelHandler(repository releasechannel.Repository, logger *logrus.Logger) *ReleaseChannelHandler {
	return &ReleaseChannelHandler{
		repository: repository,
		logger:     logger,
	}
}

type ReleaseChannelResponse struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Type           string `json:"type"`
	Schedule       string `json:"schedule,omitempty"`
	Description    string `json:"description,omitempty"`
	CurrentVersion string `json:"current_version"`
	LastUpdated    string `json:"last_updated"`
	CreatedAt      string `json:"created_at"`
}

// ListChannels godoc
//
//	@Summary		List all release channels
//	@Description	Returns a list of all available release channels for Unleash version management
//	@Tags			release-channels
//	@Produce		json
//	@Success		200	{array}		ReleaseChannelResponse
//	@Failure		500	{object}	ErrorResponse	"Internal server error"
//	@Router			/v1/releasechannels [get]
func (h *ReleaseChannelHandler) ListChannels(c *gin.Context) {
	ctx := c.Request.Context()

	channels, err := h.repository.List(ctx)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).Error("Failed to list release channels")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "failed_to_list",
			Message:    "Failed to retrieve release channels",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	response := make([]ReleaseChannelResponse, 0, len(channels))
	for _, channel := range channels {
		response = append(response, toReleaseChannelResponse(channel))
	}

	c.JSON(http.StatusOK, response)
}

// GetChannel godoc
//
//	@Summary		Get a release channel by name
//	@Description	Returns details of a specific release channel
//	@Tags			release-channels
//	@Produce		json
//	@Param			name	path		string	true	"Release channel name"
//	@Success		200		{object}	ReleaseChannelResponse
//	@Failure		404		{object}	ErrorResponse	"Release channel not found"
//	@Router			/v1/releasechannels/{name} [get]
func (h *ReleaseChannelHandler) GetChannel(c *gin.Context) {
	ctx := c.Request.Context()
	name := c.Param("name")

	channel, err := h.repository.Get(ctx, name)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("channel", name).Warn("Release channel not found")
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:      "not_found",
			Message:    "Release channel not found",
			Details:    map[string]string{"channel": name},
			StatusCode: http.StatusNotFound,
		})
		return
	}

	c.JSON(http.StatusOK, toReleaseChannelResponse(channel))
}

func toReleaseChannelResponse(channel *releasechannel.Channel) ReleaseChannelResponse {
	var lastUpdated string
	if !channel.Status.LastUpdated.IsZero() {
		lastUpdated = channel.Status.LastUpdated.Format("2006-01-02T15:04:05Z07:00")
	}

	return ReleaseChannelResponse{
		Name:           channel.Name,
		Version:        channel.Version,
		Type:           channel.Spec.Type,
		Schedule:       channel.Spec.Schedule,
		Description:    channel.Spec.Description,
		CurrentVersion: channel.Status.CurrentVersion,
		LastUpdated:    lastUpdated,
		CreatedAt:      channel.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
