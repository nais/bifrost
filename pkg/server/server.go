package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/api/generated"
	v1 "github.com/nais/bifrost/pkg/api/http/v1"
	"github.com/nais/bifrost/pkg/application/migration"
	unleashapp "github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	"github.com/nais/bifrost/pkg/infrastructure/cloudsql"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	fqdnV1alpha3 "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/sqladmin/v1beta4"
	"k8s.io/apimachinery/pkg/runtime"
	client_go_scheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

func initGoogleClients(ctx context.Context) (*admin.InstancesService, *admin.DatabasesService, *admin.UsersService, error) {
	googleClient, err := admin.NewService(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	return googleClient.Instances, googleClient.Databases, googleClient.Users, nil
}

func initKubernetesClient() (ctrl.Client, error) {
	var kubeClient ctrl.Client
	scheme := runtime.NewScheme()
	if err := fqdnV1alpha3.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add fqdnV1alpha3 to scheme: %w", err)
	}
	if err := unleashv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add unleashv1 to scheme: %w", err)
	}
	if err := client_go_scheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add client_go_scheme to scheme: %w", err)
	}
	opts := ctrl.Options{
		Scheme: scheme,
	}
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build config from kubeconfig: %w", err)
		}

		kubeClient, err = ctrl.New(config, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
		}
	} else {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
		}
		kubeClient, err = ctrl.New(config, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
		}
	}

	return kubeClient, nil
}

func initLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: "2006-01-02 15:04:05",
	})

	return logger
}

// apiVersionMiddleware adds X-API-Version header to responses
func apiVersionMiddleware(version string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-API-Version", version)
		c.Next()
	}
}

// jsonLoggerMiddleware returns a gin middleware that logs requests as JSON using logrus.
// It skips logging for specified paths (like health checks) to reduce noise.
func jsonLoggerMiddleware(logger *logrus.Logger, skipPaths []string) gin.HandlerFunc {
	skipPathSet := make(map[string]struct{}, len(skipPaths))
	for _, path := range skipPaths {
		skipPathSet[path] = struct{}{}
	}

	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		c.Next()

		// Skip logging for specified paths
		if _, skip := skipPathSet[path]; skip {
			return
		}

		latency := time.Since(start)
		clientIP := c.ClientIP()
		method := c.Request.Method
		statusCode := c.Writer.Status()
		bodySize := c.Writer.Size()

		if raw != "" {
			path = path + "?" + raw
		}

		entry := logger.WithFields(logrus.Fields{
			"status":     statusCode,
			"method":     method,
			"path":       path,
			"latency":    latency.String(),
			"latency_ms": latency.Milliseconds(),
			"client_ip":  clientIP,
			"body_size":  bodySize,
		})

		if statusCode >= 500 {
			entry.Error("Server error")
		} else if statusCode >= 400 {
			entry.Warn("Client error")
		} else {
			entry.Info("Request completed")
		}
	}
}

func setupRouter(config *config.Config, logger *logrus.Logger, v1Service *unleashapp.Service, releaseChannelRepo releasechannel.Repository) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(jsonLoggerMiddleware(logger, []string{"/healthz"}))

	// Health check
	router.GET("/healthz", func(c *gin.Context) {
		c.String(200, "OK")
	})

	// Serve OpenAPI specification (JSON format from embedded spec)
	router.GET("/openapi.json", func(c *gin.Context) {
		swagger, err := generated.GetSwagger()
		if err != nil {
			logger.WithError(err).Error("Failed to get OpenAPI spec")
			c.JSON(500, gin.H{"error": "failed to get spec"})
			return
		}
		c.JSON(200, swagger)
	})

	// Create OpenAPI handler
	openAPIHandler := v1.NewOpenAPIHandler(v1Service, config, logger, releaseChannelRepo)

	// Register API routes (paths already include /v1 prefix)
	router.Use(apiVersionMiddleware("v1"))
	generated.RegisterHandlers(router, openAPIHandler)

	return router
}

// validateDefaultReleaseChannel checks if the configured default release channel exists in Kubernetes.
// If no default channel is configured, it returns nil (no validation needed).
// If a default is configured but not found, it returns an error to prevent server startup.
// This ensures instances can't be created with a non-existent default channel.
func validateDefaultReleaseChannel(ctx context.Context, config *config.Config, repo releasechannel.Repository, logger *logrus.Logger) error {
	if config.Unleash.DefaultReleaseChannel == "" {
		return nil
	}

	_, err := repo.Get(ctx, config.Unleash.DefaultReleaseChannel)
	if err != nil {
		return fmt.Errorf("default release channel %q not found: %w", config.Unleash.DefaultReleaseChannel, err)
	}

	logger.Infof("Validated default release channel: %s", config.Unleash.DefaultReleaseChannel)
	return nil
}

func Run(config *config.Config) {
	logger := initLogger()

	kubeClient, err := initKubernetesClient()
	if err != nil {
		logger.Fatal(err)
	}

	_, sqlDatabasesClient, sqlUsersClient, err := initGoogleClients(context.Background())
	if err != nil {
		logger.Fatal(err)
	}

	// Create v1 service
	dbManager := cloudsql.NewManager(sqlDatabasesClient, sqlUsersClient, kubeClient, config, logger)
	repo := kubernetes.NewUnleashRepository(kubeClient, config, logger)
	unleashService := unleashapp.NewService(repo, dbManager, config, logger)

	// Create release channel repository
	releaseChannelRepo := kubernetes.NewReleaseChannelRepository(kubeClient, config.Unleash.InstanceNamespace)

	// Validate default release channel if configured
	validateCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := validateDefaultReleaseChannel(validateCtx, config, releaseChannelRepo, logger); err != nil {
		logger.Fatal(err)
	}

	// Start migration reconciler in background if enabled
	var migrationCancel context.CancelFunc
	if config.Unleash.MigrationEnabled {
		var migrationCtx context.Context
		migrationCtx, migrationCancel = context.WithCancel(context.Background())

		reconciler := migration.NewReconciler(
			repo.(migration.UnleashCRDRepository),
			releaseChannelRepo,
			config,
			logger,
		)

		go reconciler.Start(migrationCtx)
		logger.Info("Migration reconciler started in background")
	}

	// Setup signal handler to gracefully shutdown migration reconciler
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
		<-sigChan
		if migrationCancel != nil {
			logger.Info("Shutting down migration reconciler")
			migrationCancel()
		}
	}()

	router := setupRouter(config, logger, unleashService, releaseChannelRepo)

	logger.Infof("Listening on %s", config.GetServerAddr())
	if err := router.Run(config.GetServerAddr()); err != nil {
		logger.Fatal(err)
	}
}
