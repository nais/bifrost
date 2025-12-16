package config

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	envconfig "github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

type MetaConfig struct {
	Version string `env:"BIFROST_VERSION,default=unknown"`
	Repo    string `env:"BIFROST_REPO,default=nais/bifrost"`
}

func (m *MetaConfig) Commit() string {
	split := strings.Split(m.Version, "-")
	if len(split) == 2 {
		return split[1]
	}

	return "unknown"
}

func (m *MetaConfig) BuildDate() string {
	split := strings.Split(m.Version, "-")
	if len(split) == 2 {
		return split[0]
	}

	return "unknown"
}

func (m *MetaConfig) RepoUrl() string {
	return fmt.Sprintf("https://github.com/%s", m.Repo)
}

func (m *MetaConfig) CommitUrl() string {
	return fmt.Sprintf("%s/commit/%s", m.RepoUrl(), m.Commit())
}

func (m *MetaConfig) VersionUrl() string {
	return fmt.Sprintf("%s/releases/tag/%s", m.RepoUrl(), m.Version)
}

type ServerConfig struct {
	Port            string `env:"BIFROST_PORT,default=8080"`
	Host            string `env:"BIFROST_HOST,default=0.0.0.0"`
	WriteTimeout    int    `env:"BIFROST_WRITE_TIMEOUT,default=15"`
	ReadTimeout     int    `env:"BIFROST_READ_TIMEOUT,default=15"`
	IdleTimeout     int    `env:"BIFROST_IDLE_TIMEOUT,default=60"`
	GracefulTimeout int    `env:"BIFROST_GRACEFUL_TIMEOUT,default=15"`
	TemplatesDir    string `env:"BIFROST_TEMPLATE_DIR,default=./templates"`
}

type GoogleConfig struct {
	ProjectID string `env:"BIFROST_GOOGLE_PROJECT_ID,required"`
}

type TeamsConfig struct {
	TeamsApiURL   string `env:"BIFROST_TEAMS_API_URL,required"`
	TeamsApiToken string `env:"BIFROST_TEAMS_API_TOKEN,required"`
}

type UnleashConfig struct {
	InstanceNamespace           string `env:"BIFROST_UNLEASH_INSTANCE_NAMESPACE,required"`
	InstanceServiceaccount      string `env:"BIFROST_UNLEASH_INSTANCE_SERVICEACCOUNT,required"`
	SQLInstanceID               string `env:"BIFROST_UNLEASH_SQL_INSTANCE_ID,required"`
	SQLInstanceRegion           string `env:"BIFROST_UNLEASH_SQL_INSTANCE_REGION,required"`
	SQLInstanceAddress          string `env:"BIFROST_UNLEASH_SQL_INSTANCE_ADDRESS,required"`
	InstanceWebIngressHost      string `env:"BIFROST_UNLEASH_INSTANCE_WEB_INGRESS_HOST,required"`
	InstanceWebIngressClass     string `env:"BIFROST_UNLEASH_INSTANCE_WEB_INGRESS_CLASS,required"`
	InstanceWebOAuthJWTAudience string `env:"BIFROST_UNLEASH_INSTANCE_WEB_OAUTH_JWT_AUDIENCE,required"`
	InstanceAPIIngressHost      string `env:"BIFROST_UNLEASH_INSTANCE_API_INGRESS_HOST,required"`
	InstanceAPIIngressClass     string `env:"BIFROST_UNLEASH_INSTANCE_API_INGRESS_CLASS,required"`
	TeamsApiURL                 string `env:"BIFROST_UNLEASH_INSTANCE_TEAMS_API_URL,required"`
	TeamsApiSecretName          string `env:"BIFROST_UNLEASH_INSTANCE_TEAMS_API_SECRET_NAME,required"`
	TeamsApiSecretTokenKey      string `env:"BIFROST_UNLEASH_INSTANCE_TEAMS_API_TOKEN_SECRET_KEY,required"`
	DefaultReleaseChannel       string `env:"BIFROST_UNLEASH_DEFAULT_RELEASE_CHANNEL"`
}

type Config struct {
	Meta                MetaConfig
	Server              ServerConfig
	Google              GoogleConfig
	Teams               TeamsConfig
	Unleash             UnleashConfig
	DebugMode           bool
	CloudConnectorProxy string `env:"BIFROST_CLOUD_CONNECTOR_PROXY_IMAGE,default=gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.1.0"`
}

func (c *Config) GoogleProjectURL(path string) string {
	if path == "" {
		path = "home/dashboard"
	}

	return fmt.Sprintf("https://console.cloud.google.com/%s?project=%s", path, c.Google.ProjectID)
}

func (c *Config) GetServerAddr() string {
	return c.Server.Host + ":" + c.Server.Port
}

func (c *Config) GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("config", c)
		c.Next()
	}
}

func Setup(com *cobra.Command) {
	err := godotenv.Load()
	if err != nil {
		if err.Error() != "open .env: no such file or directory" {
			log.Fatal(err)
		}
	}
}

func New(ctx context.Context) *Config {
	var c Config
	if err := envconfig.Process(ctx, &c); err != nil {
		panic(err)
	}

	return &c
}
