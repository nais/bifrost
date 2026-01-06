package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/api/dto"
	"github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
)

// Unleash type alias for swagger documentation
type Unleash = unleashv1.Unleash

type UnleashHandler struct {
	service            unleash.IService
	config             *config.Config
	logger             *logrus.Logger
	releaseChannelRepo releasechannel.Repository
}

func NewUnleashHandler(service unleash.IService, config *config.Config, logger *logrus.Logger, releaseChannelRepo releasechannel.Repository) *UnleashHandler {
	return &UnleashHandler{
		service:            service,
		config:             config,
		logger:             logger,
		releaseChannelRepo: releaseChannelRepo,
	}
}

type ErrorResponse struct {
	Error      string            `json:"error"`
	Message    string            `json:"message,omitempty"`
	Details    map[string]string `json:"details,omitempty"`
	StatusCode int               `json:"status_code"`
}

// ListInstances godoc
//
//	@Summary		List all Unleash instances
//	@Description	Returns a list of all Unleash feature flag server instances as Kubernetes CRDs
//	@Tags			unleash-v1
//	@Produce		json
//	@Success		200	{array}		Unleash
//	@Failure		500	{object}	ErrorResponse	"Internal server error"
//	@Router			/v1/unleash [get]
func (h *UnleashHandler) ListInstances(c *gin.Context) {
	ctx := c.Request.Context()

	crds, err := h.service.ListCRDs(ctx, false)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).Error("Failed to list instances")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "list_failed",
			Message:    "Could not retrieve instances",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	c.JSON(http.StatusOK, crds)
}

// GetInstance godoc
//
//	@Summary		Get Unleash instance by name
//	@Description	Returns details of a specific Unleash instance as Kubernetes CRD
//	@Tags			unleash-v1
//	@Produce		json
//	@Param			name	path		string	true	"Instance name"
//	@Success		200		{object}	Unleash
//	@Failure		404		{object}	ErrorResponse	"Instance not found"
//	@Router			/v1/unleash/{name} [get]
func (h *UnleashHandler) GetInstance(c *gin.Context) {
	ctx := c.Request.Context()
	name := c.Param("name")

	crd, err := h.service.GetCRD(ctx, name)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("instance", name).Warn("Instance not found")
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:      "not_found",
			Message:    "Instance not found",
			Details:    map[string]string{"name": name},
			StatusCode: http.StatusNotFound,
		})
		return
	}

	c.JSON(http.StatusOK, crd)
}

// CreateInstance godoc
//
//	@Summary		Create a new Unleash instance
//	@Description	Creates a new Unleash feature flag server instance with database and credentials. Returns the Kubernetes CRD.
//	@Tags			unleash-v1
//	@Accept			json
//	@Produce		json
//	@Param			request	body		dto.UnleashConfigRequest	true	"Unleash instance configuration"
//	@Success		201		{object}	Unleash
//	@Failure		400		{object}	ErrorResponse	"Invalid request or validation error"
//	@Failure		500		{object}	ErrorResponse	"Internal server error"
//	@Router			/v1/unleash [post]
func (h *UnleashHandler) CreateInstance(c *gin.Context) {
	ctx := c.Request.Context()
	var req dto.UnleashConfigRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		h.logger.WithContext(ctx).WithError(err).Error("Invalid request body")
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:      "invalid_request",
			Message:    "Invalid request body",
			Details:    map[string]string{"error": err.Error()},
			StatusCode: http.StatusBadRequest,
		})
		return
	}

	// Enable federation by default for new instances
	req.EnableFederation = true

	builder := req.ToConfigBuilder()
	//lint:ignore SA1019 CustomVersion is deprecated but still supported for backwards compatibility
	if req.CustomVersion == "" && req.ReleaseChannelName == "" {
		if h.config.Unleash.DefaultReleaseChannel != "" {
			builder.WithReleaseChannel(h.config.Unleash.DefaultReleaseChannel)
		} else {
			// No default channel configured and no explicit version provided
			h.logger.WithContext(ctx).WithField("name", req.Name).Warn("Instance creation rejected: must specify custom-version, release-channel-name, or configure a default release channel")
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error:      "no_version_source",
				Message:    "Must specify custom-version or release-channel-name",
				StatusCode: http.StatusBadRequest,
			})
			return
		}
	}

	builder.MergeTeamsAndNamespaces()

	config, err := builder.Build()
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", req.Name).Error("Validation failed")
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:      "validation_failed",
			Details:    map[string]string{"reason": err.Error()},
			StatusCode: http.StatusBadRequest,
		})
		return
	}

	crd, err := h.service.Create(ctx, config)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", config.Name).Error("Failed to create instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "creation_failed",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	c.JSON(http.StatusCreated, crd)
}

// UpdateInstance godoc
//
//	@Summary		Update an existing Unleash instance
//	@Description	Updates configuration of an existing Unleash instance. Preserves federation settings if not specified. Returns the Kubernetes CRD.
//	@Tags			unleash-v1
//	@Accept			json
//	@Produce		json
//	@Param			name	path		string						true	"Instance name"
//	@Param			request	body		dto.UnleashConfigRequest	true	"Updated Unleash configuration"
//	@Success		200		{object}	Unleash
//	@Failure		400		{object}	ErrorResponse	"Invalid request or validation error"
//	@Failure		404		{object}	ErrorResponse	"Instance not found"
//	@Failure		500		{object}	ErrorResponse	"Internal server error"
//	@Router			/v1/unleash/{name} [put]
func (h *UnleashHandler) UpdateInstance(c *gin.Context) {
	ctx := c.Request.Context()
	name := c.Param("name")
	var req dto.UnleashConfigRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		h.logger.WithContext(ctx).WithError(err).Error("Invalid request body")
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:      "invalid_request",
			Message:    "Invalid request body",
			Details:    map[string]string{"error": err.Error()},
			StatusCode: http.StatusBadRequest,
		})
		return
	}

	req.Name = name

	// Get existing instance to validate changes and preserve version source if not specified
	existing, err := h.service.Get(ctx, name)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("instance", name).Warn("Instance not found")
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:      "not_found",
			Message:    "Instance not found",
			Details:    map[string]string{"name": name},
			StatusCode: http.StatusNotFound,
		})
		return
	}

	// Preserve existing version source if neither custom version nor release channel is specified
	//lint:ignore SA1019 CustomVersion is deprecated but still supported for backwards compatibility
	if req.CustomVersion == "" && req.ReleaseChannelName == "" {
		//lint:ignore SA1019 CustomVersion is deprecated but still supported for backwards compatibility
		req.CustomVersion = existing.CustomVersion
		req.ReleaseChannelName = existing.ReleaseChannelName
	}

	// Preserve federation settings from existing instance
	// Federation is always enabled for managed instances
	req.EnableFederation = true
	req.FederationNonce = existing.FederationNonce
	if req.AllowedTeams == "" {
		req.AllowedTeams = existing.AllowedTeams
	}
	//lint:ignore SA1019 AllowedNamespaces is deprecated but still supported for backwards compatibility
	if req.AllowedNamespaces == "" {
		//lint:ignore SA1019 AllowedNamespaces is deprecated but still supported for backwards compatibility
		req.AllowedNamespaces = existing.AllowedNamespaces
	}
	if req.AllowedClusters == "" {
		req.AllowedClusters = existing.AllowedClusters
	}

	// Check if switching release channels and validate major version
	if req.ReleaseChannelName != "" {
		// If instance has a release channel and switching to a different one, validate major version
		if existing.ReleaseChannelName != "" && existing.ReleaseChannelName != req.ReleaseChannelName {
			if err := h.validateReleaseChannelSwitch(ctx, existing.ReleaseChannelName, req.ReleaseChannelName); err != nil {
				h.logger.WithContext(ctx).WithError(err).WithFields(logrus.Fields{
					"instance":    name,
					"old_channel": existing.ReleaseChannelName,
					"new_channel": req.ReleaseChannelName,
				}).Warn("Release channel switch validation failed")
				c.JSON(http.StatusBadRequest, ErrorResponse{
					Error:   "invalid_channel_switch",
					Message: "Cannot downgrade major version",
					Details: map[string]string{
						"from": existing.ReleaseChannelName,
						"to":   req.ReleaseChannelName,
					},
					StatusCode: http.StatusBadRequest,
				})
				return
			}
		}
	}

	builder := req.ToConfigBuilder()
	builder.MergeTeamsAndNamespaces()

	config, err := builder.Build()
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Error("Validation failed")
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:      "validation_failed",
			Details:    map[string]string{"reason": err.Error()},
			StatusCode: http.StatusBadRequest,
		})
		return
	}

	crd, err := h.service.Update(ctx, config)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Error("Failed to update instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "update_failed",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	c.JSON(http.StatusOK, crd)
}

// DeleteInstance godoc
//
//	@Summary		Delete an Unleash instance
//	@Description	Deletes an existing Unleash instance and its associated database and credentials
//	@Tags			unleash-v1
//	@Param			name	path	string	true	"Instance name"
//	@Success		204	"Successfully deleted"
//	@Failure		404	{object}	ErrorResponse	"Instance not found"
//	@Failure		500	{object}	ErrorResponse	"Internal server error"
//	@Router			/v1/unleash/{name} [delete]
func (h *UnleashHandler) DeleteInstance(c *gin.Context) {
	ctx := c.Request.Context()
	name := c.Param("name")

	_, err := h.service.Get(ctx, name)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Warn("Instance not found")
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:      "not_found",
			Message:    "Instance not found",
			Details:    map[string]string{"name": name},
			StatusCode: http.StatusNotFound,
		})
		return
	}

	if err := h.service.Delete(ctx, name); err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Error("Failed to delete instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "deletion_failed",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// validateReleaseChannelSwitch validates that switching from one release channel to another
// doesn't result in a major version downgrade
func (h *UnleashHandler) validateReleaseChannelSwitch(ctx context.Context, oldChannelName, newChannelName string) error {
	// Get old channel
	oldChannel, err := h.releaseChannelRepo.Get(ctx, oldChannelName)
	if err != nil {
		return fmt.Errorf("failed to get old release channel %s: %w", oldChannelName, err)
	}

	// Get new channel
	newChannel, err := h.releaseChannelRepo.Get(ctx, newChannelName)
	if err != nil {
		return fmt.Errorf("failed to get new release channel %s: %w", newChannelName, err)
	}

	// Extract version from image (e.g., "quay.io/unleash/unleash-server:6.3.0" -> "6.3.0")
	oldVersion := extractVersionFromImage(oldChannel.Image)
	oldVersion = strings.TrimPrefix(oldVersion, "v")
	oldSemver, err := semver.NewVersion(oldVersion)
	if err != nil {
		return fmt.Errorf("failed to parse old channel version %s: %w", oldVersion, err)
	}

	// Extract version from image
	newVersion := extractVersionFromImage(newChannel.Image)
	newVersion = strings.TrimPrefix(newVersion, "v")
	newSemver, err := semver.NewVersion(newVersion)
	if err != nil {
		return fmt.Errorf("failed to parse new channel version %s: %w", newVersion, err)
	}

	// Compare major versions
	if newSemver.Major() < oldSemver.Major() {
		return fmt.Errorf("cannot downgrade from major version %d to %d", oldSemver.Major(), newSemver.Major())
	}

	h.logger.WithContext(ctx).WithFields(logrus.Fields{
		"old_channel":       oldChannelName,
		"old_version":       oldVersion,
		"old_major":         oldSemver.Major(),
		"new_channel":       newChannelName,
		"new_version":       newVersion,
		"new_major":         newSemver.Major(),
		"version_permitted": true,
	}).Debug("Release channel switch validation passed")

	return nil
}

// extractVersionFromImage extracts the version tag from a container image reference.
// e.g., "quay.io/unleash/unleash-server:6.3.0" -> "6.3.0"
func extractVersionFromImage(image string) string {
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		return image[idx+1:]
	}
	return ""
}
