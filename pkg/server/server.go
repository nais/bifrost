package server

import (
	"context"
	"fmt"
	"os"

	"github.com/gin-gonic/gin"
	v0handlers "github.com/nais/bifrost/pkg/api/http/v0/handlers"
	v1handlers "github.com/nais/bifrost/pkg/api/http/v1/handlers"
	unleashapp "github.com/nais/bifrost/pkg/application/unleash"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/releasechannel"
	"github.com/nais/bifrost/pkg/infrastructure/cloudsql"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	"github.com/nais/bifrost/pkg/unleash"
	fqdnV1alpha3 "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	admin "google.golang.org/api/sqladmin/v1beta4"
	"k8s.io/apimachinery/pkg/runtime"
	client_go_scheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"

	_ "github.com/nais/bifrost/docs" // Import generated docs
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

func setupRouter(config *config.Config, logger *logrus.Logger, unleashService unleash.IUnleashService, v1Service *unleashapp.Service, releaseChannelRepo releasechannel.Repository) *gin.Engine {
	router := gin.Default()
	gin.DefaultWriter = logger.Writer()

	// v0 handlers (existing)
	v0Handlers := v0handlers.NewHandler(config, logger, unleashService)

	// v1 handlers (new)
	v1Handlers := v1handlers.NewUnleashHandler(v1Service, config, logger)
	v1ChannelHandlers := v1handlers.NewReleaseChannelHandler(releaseChannelRepo, logger)

	// Swagger UI
	router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Health check
	router.GET("/healthz", v0Handlers.HealthHandler)

	// v0 API routes (existing, unversioned for backward compatibility)
	unleash := router.Group("/unleash")
	{
		unleash.POST("/new", v0Handlers.UnleashInstancePost)

		unleashInstance := unleash.Group("/:id")
		unleashInstance.Use(v0Handlers.UnleashInstanceMiddleware)
		{
			unleashInstance.POST("/edit", v0Handlers.UnleashInstancePost)
			unleashInstance.DELETE("", v0Handlers.UnleashInstanceDelete)
		}
	}

	// v1 API routes (new, versioned)
	v1 := router.Group("/v1")
	v1.Use(apiVersionMiddleware("v1"))
	{
		// Unleash instances
		v1.GET("/unleash", v1Handlers.ListInstances)
		v1.POST("/unleash", v1Handlers.CreateInstance)
		v1.GET("/unleash/:name", v1Handlers.GetInstance)
		v1.PUT("/unleash/:name", v1Handlers.UpdateInstance)
		v1.DELETE("/unleash/:name", v1Handlers.DeleteInstance)

		// Release channels
		v1.GET("/releasechannels", v1ChannelHandlers.ListChannels)
		v1.GET("/releasechannels/:name", v1ChannelHandlers.GetChannel)
	}

	return router
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

	// Create v0 adapter for backward compatibility
	unleashServiceV0 := unleash.NewServiceAdapter(sqlDatabasesClient, sqlUsersClient, kubeClient, config, logger)

	// Create v1 service directly (no adapter)
	dbManager := cloudsql.NewManager(sqlDatabasesClient, sqlUsersClient, kubeClient, config, logger)
	repo := kubernetes.NewUnleashRepository(kubeClient, config, logger)
	unleashServiceV1 := unleashapp.NewService(repo, dbManager, config, logger)

	// Create release channel repository
	releaseChannelRepo := kubernetes.NewReleaseChannelRepository(kubeClient, config.Unleash.InstanceNamespace)

	router := setupRouter(config, logger, unleashServiceV0, unleashServiceV1, releaseChannelRepo)

	logger.Infof("Listening on %s", config.GetServerAddr())
	if err := router.Run(config.GetServerAddr()); err != nil {
		logger.Fatal(err)
	}
}
