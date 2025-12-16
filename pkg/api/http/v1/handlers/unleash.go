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
	"github.com/nais/bifrost/pkg/infrastructure/github"
	"github.com/sirupsen/logrus"
)

type UnleashHandler struct {
	service            *unleash.Service
	config             *config.Config
	logger             *logrus.Logger
	releaseChannelRepo releasechannel.Repository
}

func NewUnleashHandler(service *unleash.Service, config *config.Config, logger *logrus.Logger, releaseChannelRepo releasechannel.Repository) *UnleashHandler {
	return &UnleashHandler{
		service:            service,
		config:             config,
		logger:             logger,
		releaseChannelRepo: releaseChannelRepo,
	}
}

type ErrorResponse struct {
	Error      string            `json:"error"`
	Message    string            `json:"message"`
	Details    map[string]string `json:"details,omitempty"`
	StatusCode int               `json:"status_code"`
}

func (h *UnleashHandler) ListInstances(c *gin.Context) {
	ctx := c.Request.Context()

	instances, err := h.service.List(ctx, false)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).Error("Failed to list instances")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "failed_to_list",
			Message:    "Failed to retrieve Unleash instances",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	c.JSON(http.StatusOK, dto.ToV1ListResponse(instances))
}

func (h *UnleashHandler) GetInstance(c *gin.Context) {
	ctx := c.Request.Context()
	name := c.Param("name")

	instance, err := h.service.Get(ctx, name)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("instance", name).Warn("Instance not found")
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:      "not_found",
			Message:    "Unleash instance not found",
			Details:    map[string]string{"instance": name},
			StatusCode: http.StatusNotFound,
		})
		return
	}

	c.JSON(http.StatusOK, dto.ToV1Response(instance))
}

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

	builder := req.ToConfigBuilder()
	if req.CustomVersion == "" && req.ReleaseChannelName == "" {
		unleashVersions, err := github.UnleashVersions()
		if err != nil {
			h.logger.WithContext(ctx).WithError(err).Warn("Failed to fetch Unleash versions")
		}
		builder.SetDefaultVersionIfNeeded(unleashVersions)
	}

	builder.MergeTeamsAndNamespaces()

	config, err := builder.Build()
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", req.Name).Error("Validation failed")
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:      "validation_failed",
			Message:    "Configuration validation failed",
			Details:    map[string]string{"validation": err.Error()},
			StatusCode: http.StatusBadRequest,
		})
		return
	}

	_, err = h.service.Create(ctx, config)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", config.Name).Error("Failed to create instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "creation_failed",
			Message:    "Failed to create Unleash instance",
			Details:    map[string]string{"error": err.Error()},
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	instance, err := h.service.Get(ctx, config.Name)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", config.Name).Error("Failed to retrieve created instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "retrieval_failed",
			Message:    "Instance created but failed to retrieve details",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	c.JSON(http.StatusCreated, dto.ToV1Response(instance))
}

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

	// Check if switching release channels and validate major version
	if req.ReleaseChannelName != "" {
		existing, err := h.service.Get(ctx, name)
		if err != nil {
			h.logger.WithContext(ctx).WithError(err).WithField("instance", name).Warn("Instance not found")
			c.JSON(http.StatusNotFound, ErrorResponse{
				Error:      "not_found",
				Message:    "Unleash instance not found",
				Details:    map[string]string{"instance": name},
				StatusCode: http.StatusNotFound,
			})
			return
		}

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
					Message: "Cannot switch to release channel with lower major version",
					Details: map[string]string{
						"old_channel": existing.ReleaseChannelName,
						"new_channel": req.ReleaseChannelName,
						"error":       err.Error(),
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
			Message:    "Configuration validation failed",
			Details:    map[string]string{"validation": err.Error()},
			StatusCode: http.StatusBadRequest,
		})
		return
	}

	_, err = h.service.Update(ctx, config)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Error("Failed to update instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "update_failed",
			Message:    "Failed to update Unleash instance",
			Details:    map[string]string{"error": err.Error()},
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	instance, err := h.service.Get(ctx, name)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Error("Failed to retrieve updated instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "retrieval_failed",
			Message:    "Instance updated but failed to retrieve details",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	c.JSON(http.StatusOK, dto.ToV1Response(instance))
}

func (h *UnleashHandler) DeleteInstance(c *gin.Context) {
	ctx := c.Request.Context()
	name := c.Param("name")

	_, err := h.service.Get(ctx, name)
	if err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Warn("Instance not found")
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:      "not_found",
			Message:    "Unleash instance not found",
			Details:    map[string]string{"instance": name},
			StatusCode: http.StatusNotFound,
		})
		return
	}

	if err := h.service.Delete(ctx, name); err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Error("Failed to delete instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "deletion_failed",
			Message:    "Failed to delete Unleash instance",
			Details:    map[string]string{"error": err.Error()},
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

	// Parse old version
	oldVersion := oldChannel.Version
	if oldVersion == "" {
		oldVersion = oldChannel.Status.CurrentVersion
	}
	oldVersion = strings.TrimPrefix(oldVersion, "v")
	oldSemver, err := semver.NewVersion(oldVersion)
	if err != nil {
		return fmt.Errorf("failed to parse old channel version %s: %w", oldVersion, err)
	}

	// Parse new version
	newVersion := newChannel.Version
	if newVersion == "" {
		newVersion = newChannel.Status.CurrentVersion
	}
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
