package handlers

import (
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/unleash"
	"github.com/sirupsen/logrus"
)

type Handler struct {
	config         *config.Config
	logger         *logrus.Logger
	unleashService unleash.IUnleashService
}

func NewHandler(config *config.Config, logger *logrus.Logger, unleashService unleash.IUnleashService) *Handler {
	return &Handler{
		config:         config,
		logger:         logger,
		unleashService: unleashService,
	}
}
