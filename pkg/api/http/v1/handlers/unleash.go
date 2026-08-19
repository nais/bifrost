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
	domainunleash "github.com/nais/bifrost/pkg/domain/unleash"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// Refuse to provision over an instance that already exists. Without this a
	// duplicate POST — a client retry, a double submit — runs the whole create
	// path against live resources, and any failure along the way triggers a
	// rollback that tears them down. Create must never touch an existing
	// instance; updates go through PUT.
	//
	// Fail closed: only a definite NotFound means it is safe to provision. Any
	// other read error is inconclusive, and treating it as "does not exist"
	// would let a duplicate through during an API-server blip.
	if _, err := h.service.Get(ctx, config.Name); err == nil {
		h.logger.WithContext(ctx).WithField("name", config.Name).Warn("Instance already exists, refusing to create")
		c.JSON(http.StatusConflict, ErrorResponse{
			Error:      "already_exists",
			Message:    "Instance already exists",
			Details:    map[string]string{"name": config.Name},
			StatusCode: http.StatusConflict,
		})
		return
	} else if !apierrors.IsNotFound(err) {
		h.logger.WithContext(ctx).WithError(err).WithField("name", config.Name).
			Error("Could not determine whether the instance already exists")
		c.JSON(http.StatusServiceUnavailable, ErrorResponse{
			Error:      "existence_check_failed",
			Message:    "Could not verify whether the instance already exists",
			StatusCode: http.StatusServiceUnavailable,
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
//	@Failure		409		{object}	ErrorResponse	"Instance was modified concurrently"
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

	// allowed_teams is authoritative; allowed_namespaces is its deprecated
	// alias. Either one, when supplied, states the complete desired set and
	// replaces the list — that is what makes revocation possible. Supplying
	// neither is not a statement about access, so the existing list is kept.
	//
	// Preserving takes the union of both stored fields: instances written
	// before the two were kept in sync may hold different lists, and an
	// unrelated update must never silently drop an entry.
	desiredTeams := req.AllowedTeams
	if desiredTeams == nil {
		//lint:ignore SA1019 AllowedNamespaces is deprecated but still supported for backwards compatibility
		desiredTeams = req.AllowedNamespaces
	}
	if desiredTeams == nil {
		// existing is the domain Instance, whose namespaces field is not
		// deprecated, so no SA1019 suppression is needed here.
		preserved := domainunleash.MergeLists(existing.AllowedTeams, existing.AllowedNamespaces)
		desiredTeams = &preserved
	}
	req.AllowedTeams = desiredTeams
	//lint:ignore SA1019 AllowedNamespaces is deprecated but still supported for backwards compatibility
	req.AllowedNamespaces = desiredTeams

	if req.AllowedClusters == nil {
		req.AllowedClusters = &existing.AllowedClusters
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
	// Replace rather than union: unioning here would resurrect every entry the
	// caller just asked to remove.
	builder.SetAllowedTeams(*desiredTeams)

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

	// Log the access change explicitly. A request that leaves the allowed set
	// untouched is indistinguishable from one that never arrived unless the
	// before/after sets are recorded here.
	before := domainunleash.MergeLists(existing.AllowedTeams)
	accessLog := h.logger.WithContext(ctx).WithFields(logrus.Fields{
		"instance":             name,
		"allowed_teams_before": before,
		"allowed_teams_after":  config.AllowedTeams,
	})
	if before == config.AllowedTeams {
		accessLog.Info("Allowed teams unchanged by update")
	} else {
		accessLog.Info("Changing allowed teams")
	}

	// The request body is merged onto what `existing` said, so the write must be
	// conditional on that read. Otherwise a reconciler or another client writing
	// in between is overwritten by preserved values that are already stale.
	crd, err := h.service.Update(ctx, config, domainunleash.UpdateOptions{
		ExpectedResourceVersion: existing.ResourceVersion,
	})
	if err != nil {
		// A conflict means somebody else wrote first, so the request is not
		// wrong and the server is not broken: the caller has to re-read and
		// decide again. Reporting it as a 500 hides a retryable outcome.
		if apierrors.IsConflict(err) {
			h.logger.WithContext(ctx).WithError(err).WithField("name", name).Warn("Update conflicted with a concurrent write")
			c.JSON(http.StatusConflict, ErrorResponse{
				Error:      "conflict",
				Message:    "Instance was modified by another writer, re-read it and retry",
				StatusCode: http.StatusConflict,
			})
			return
		}
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

	_, getErr := h.service.Get(ctx, name)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		h.logger.WithContext(ctx).WithError(getErr).WithField("name", name).Error("Failed to look up instance for deletion")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "deletion_failed",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	// Run best-effort teardown even when the CRD is already gone, so a database,
	// user, or secret orphaned by a prior partial failure is still reaped.
	if err := h.service.Delete(ctx, name); err != nil {
		h.logger.WithContext(ctx).WithError(err).WithField("name", name).Error("Failed to delete instance")
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:      "deletion_failed",
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	// Preserve the existing 404 contract when nothing was there to begin with.
	if apierrors.IsNotFound(getErr) {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error:      "not_found",
			Message:    "Instance not found",
			Details:    map[string]string{"name": name},
			StatusCode: http.StatusNotFound,
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
