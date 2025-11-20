package handler

import (
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/github"
	"github.com/nais/bifrost/pkg/unleash"
	"github.com/nais/bifrost/pkg/utils"

	unleashv1 "github.com/nais/unleasherator/api/v1"
)

// ErrorResponse represents a standardized error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// UnleashResponse represents the response when creating or updating an Unleash instance
type UnleashResponse struct {
	*unleashv1.Unleash
}

// UnleashListResponse represents a list of Unleash instances
type UnleashListResponse struct {
	Instances []*unleash.UnleashInstance `json:"instances"`
	Count     int                        `json:"count"`
}

// HealthHandler godoc
//
//	@Summary		Health check
//	@Description	Check if the service is healthy
//	@Tags			health
//	@Produce		plain
//	@Success		200	{string}	string	"OK"
//	@Router			/healthz [get]
func (h *Handler) HealthHandler(c *gin.Context) {
	c.String(200, "OK")
}

// UnleashInstanceMiddleware loads the Unleash instance by ID
func (h *Handler) UnleashInstanceMiddleware(c *gin.Context) {
	teamName := c.Param("id")
	ctx := c.Request.Context()

	instance, err := h.unleashService.Get(ctx, teamName)
	if err != nil {
		h.logger.Info(err)
		c.JSON(404, ErrorResponse{
			Error:   "Instance not found",
			Details: fmt.Sprintf("Unleash instance '%s' does not exist", teamName),
		})
		c.Abort()
		return
	}

	c.Set("unleashInstance", instance)
	c.Next()
}

// UnleashInstancePost godoc
//
//	@Summary		Create or update Unleash instance
//	@Description	Create a new Unleash instance or update an existing one
//	@Tags			unleash
//	@Accept			json
//	@Produce		json
//	@Param			request	body		unleash.UnleashConfig	true	"Unleash configuration"
//	@Success		200		{object}	UnleashResponse			"Successfully created or updated"
//	@Failure		400		{object}	ErrorResponse			"Invalid request or validation error"
//	@Failure		500		{object}	ErrorResponse			"Internal server error"
//	@Router			/unleash/new [post]
//	@Router			/unleash/{id}/edit [post]
func (h *Handler) UnleashInstancePost(c *gin.Context) {
	var err error

	ctx := c.Request.Context()
	log := h.logger.WithContext(ctx)
	uc := &unleash.UnleashConfig{}

	instance, exists := c.Get("unleashInstance")
	if exists {
		instance, ok := instance.(*unleash.UnleashInstance)
		if !ok {
			c.JSON(500, ErrorResponse{
				Error:   "Internal server error",
				Details: "Error parsing existing Unleash instance",
			})
			return
		}
		uc = unleash.UnleashVariables(instance.ServerInstance, true)
	}

	unleashVersions, err := github.UnleashVersions()
	if err != nil {
		log.WithError(err).Error("Error getting Unleash versions from Github")
		unleashVersions = []github.UnleashVersion{
			{
				GitTag:        "v5.10.2-20240329-070801-0180a96",
				ReleaseTime:   time.Date(2024, 3, 29, 7, 8, 1, 0, time.UTC),
				CommitHash:    "0180a96",
				VersionNumber: "5.10.2",
			},
		}
	}

	if err = c.ShouldBindJSON(uc); err != nil {
		log.WithError(err).Error("Error binding post data to Unleash config")
		c.JSON(400, ErrorResponse{
			Error:   "Invalid request body",
			Details: err.Error(),
		})
		return
	}

	if exists {
		uc.Name = instance.(*unleash.UnleashInstance).ServerInstance.GetName()
		uc.FederationNonce = instance.(*unleash.UnleashInstance).ServerInstance.Spec.Federation.SecretNonce
	} else {
		uc.FederationNonce = utils.RandomString(8)
		uc.SetDefaultValues(unleashVersions)
	}

	// We are removing the differentiating between teams and namespaces, and merging them into one field
	uc.MergeTeamsAndNamespaces()

	if validationErr := uc.Validate(); validationErr != nil {
		log.WithError(validationErr).Error("Error validating Unleash config")
		c.JSON(400, ErrorResponse{
			Error:   "Validation failed",
			Details: validationErr.Error(),
		})
		return
	}

	var unleashInstance *unleashv1.Unleash

	if exists {
		unleashInstance, err = h.unleashService.Update(ctx, uc)
	} else {
		unleashInstance, err = h.unleashService.Create(ctx, uc)
	}

	if err != nil {
		var unleashErr *unleash.UnleashError

		if errors.As(err, &unleashErr) {
			c.JSON(500, ErrorResponse{
				Error:   "Error persisting Unleash instance",
				Details: unleashErr.Reason,
			})
		} else {
			c.JSON(500, ErrorResponse{
				Error:   "Error persisting Unleash instance",
				Details: err.Error(),
			})
		}
		return
	}

	c.JSON(200, UnleashResponse{Unleash: unleashInstance})
}

// UnleashInstanceDelete godoc
//
//	@Summary		Delete Unleash instance
//	@Description	Delete an existing Unleash instance by name
//	@Tags			unleash
//	@Produce		json
//	@Param			id	path	string	true	"Instance name"
//	@Success		204	"Successfully deleted"
//	@Failure		404	{object}	ErrorResponse	"Instance not found"
//	@Failure		500	{object}	ErrorResponse	"Internal server error"
//	@Router			/unleash/{id} [delete]
func (h *Handler) UnleashInstanceDelete(c *gin.Context) {
	instance := c.MustGet("unleashInstance").(*unleash.UnleashInstance)
	ctx := c.Request.Context()

	if err := h.unleashService.Delete(ctx, instance.Name); err != nil {
		c.JSON(500, ErrorResponse{
			Error:   "Error deleting Unleash instance",
			Details: err.Error(),
		})
		return
	}

	c.Status(204) // No content on successful delete
}
