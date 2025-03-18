package server

import (
	"context"
	"fmt"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/handler"
	"github.com/nais/bifrost/pkg/server/utils"
	"github.com/nais/bifrost/pkg/unleash"
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

func setupRouter(config *config.Config, logger *logrus.Logger, unleashService unleash.IUnleashService) *gin.Engine {
	router := gin.Default()
	gin.DefaultWriter = logger.Writer()

	h := handler.NewHandler(config, logger, unleashService)

	router.Use(h.ErrorHandler)
	router.Static("/assets", "./assets")

	router.HTMLRender = utils.LoadTemplates(config)
	router.GET("/", func(c *gin.Context) {
		c.Redirect(302, "/unleash")
	})

	router.GET("/healthz", h.HealthHandler)

	unleash := router.Group("/unleash")
	{
		unleash.GET("/", h.UnleashIndex)
		unleash.GET("/new", h.UnleashNew)
		unleash.POST("/new", h.UnleashInstancePost)

		unleashInstance := unleash.Group("/:id")
		unleashInstance.Use(h.UnleashInstanceMiddleware)
		{
			unleashInstance.GET("/", h.UnleashInstanceShow)
			unleashInstance.GET("/edit", h.UnleashInstanceEdit)
			unleashInstance.POST("/edit", h.UnleashInstancePost)
			unleashInstance.GET("/delete", h.UnleashInstanceDelete)
			unleashInstance.POST("/delete", h.UnleashInstanceDeletePost)
		}
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

	unleashService := unleash.NewUnleashService(sqlDatabasesClient, sqlUsersClient, kubeClient, config, logger)

	router := setupRouter(config, logger, unleashService)

	logger.Infof("Listening on %s", config.GetServerAddr())
	if err := router.Run(config.GetServerAddr()); err != nil {
		logger.Fatal(err)
	}
}
