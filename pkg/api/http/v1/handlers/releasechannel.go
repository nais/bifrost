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
	// Name is the unique identifier of the release channel (e.g., "stable", "rapid")
	Name string `json:"name" example:"stable"`

	// Image is the full container image reference including tag (e.g., "quay.io/unleash/unleash-server:6.3.0")
	Image string `json:"image" example:"quay.io/unleash/unleash-server:6.3.0"`

	// CreatedAt is the timestamp when the release channel was created (RFC3339 format)
	CreatedAt string `json:"created_at" example:"2024-01-01T00:00:00Z"`

	// CurrentVersion is the current version tracked by the release channel status
	CurrentVersion string `json:"current_version" example:"6.3.0"`

	// LastUpdated is the timestamp when the release channel was last reconciled (RFC3339 format)
	LastUpdated string `json:"last_updated,omitempty" example:"2024-03-15T10:30:00Z"`

	// Legacy fields - kept for backwards compatibility
	// Deprecated: Use 'image' instead. This field returns the same value as 'image'.
	Version string `json:"version" example:"quay.io/unleash/unleash-server:6.3.0"`
	// Deprecated: This field is reserved for future use and always returns an empty string.
	Type string `json:"type,omitempty"`
	// Deprecated: This field is reserved for future use and always returns an empty string.
	Schedule string `json:"schedule,omitempty"`
	// Deprecated: This field is reserved for future use and always returns an empty string.
	Description string `json:"description,omitempty"`
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
	lastUpdated := ""
	if !channel.Status.LastReconcileTime.IsZero() {
		lastUpdated = channel.Status.LastReconcileTime.Time.Format("2006-01-02T15:04:05Z07:00")
	}

	return ReleaseChannelResponse{
		// Core fields
		Name:      channel.Name,
		Image:     channel.Image,
		CreatedAt: channel.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),

		// Current version from release channel status
		CurrentVersion: channel.Status.Version,

		// Last reconcile time from status
		LastUpdated: lastUpdated,

		// Legacy fields - kept for backwards compatibility
		Version: channel.Image, // Deprecated: same as image
		// Type, Schedule, Description left empty - reserved for future use
	}
}
