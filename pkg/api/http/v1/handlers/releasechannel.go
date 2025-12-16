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
	// Core fields
	Name      string `json:"name"`
	Image     string `json:"image"`
	CreatedAt string `json:"created_at"`

	// Status fields from CRD
	Phase             string `json:"phase"`
	Instances         int    `json:"instances"`
	InstancesUpToDate int    `json:"instances_up_to_date"`
	Progress          int    `json:"progress"`
	Completed         bool   `json:"completed"`
	FailureReason     string `json:"failure_reason,omitempty"`
	LastReconciled    string `json:"last_reconciled,omitempty"`

	// Strategy fields from CRD
	MaxParallel   int  `json:"max_parallel"`
	CanaryEnabled bool `json:"canary_enabled"`

	// Legacy fields - kept for backwards compatibility
	// Deprecated: Use 'image' instead. This field returns the same value as 'image'.
	Version string `json:"version"`
	// Deprecated: This field is reserved for future use and always returns an empty string.
	Type string `json:"type,omitempty"`
	// Deprecated: This field is reserved for future use and always returns an empty string.
	Schedule string `json:"schedule,omitempty"`
	// Deprecated: This field is reserved for future use and always returns an empty string.
	Description string `json:"description,omitempty"`
	// Deprecated: Use 'image' instead. This field returns the same value as 'image'.
	CurrentVersion string `json:"current_version"`
	// Deprecated: Use 'last_reconciled' instead.
	LastUpdated string `json:"last_updated,omitempty"`
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
	var lastReconciled string
	if !channel.Status.LastReconcileTime.IsZero() {
		lastReconciled = channel.Status.LastReconcileTime.Format("2006-01-02T15:04:05Z07:00")
	}

	return ReleaseChannelResponse{
		// Core fields
		Name:      channel.Name,
		Image:     channel.Image,
		CreatedAt: channel.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),

		// Status fields from CRD
		Phase:             channel.Status.Phase,
		Instances:         channel.Status.Instances,
		InstancesUpToDate: channel.Status.InstancesUpToDate,
		Progress:          channel.Status.Progress,
		Completed:         channel.Status.Completed,
		FailureReason:     channel.Status.FailureReason,
		LastReconciled:    lastReconciled,

		// Strategy fields from CRD
		MaxParallel:   channel.Spec.MaxParallel,
		CanaryEnabled: channel.Spec.CanaryEnabled,

		// Legacy fields - kept for backwards compatibility
		Version:        channel.Image,  // Deprecated: same as image
		CurrentVersion: channel.Image,  // Deprecated: same as image
		LastUpdated:    lastReconciled, // Deprecated: same as last_reconciled
		// Type, Schedule, Description left empty - reserved for future use
	}
}
