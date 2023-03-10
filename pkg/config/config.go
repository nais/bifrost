package config

import (
	"context"
	"log"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

type ServerConfig struct {
	Port            string `env:"BIFROST_PORT,default=8080"`
	Host            string `env:"BIFROST_HOST,default=0.0.0.0"`
	WriteTimeout    int    `env:"BIFROST_WRITE_TIMEOUT,default=15"`
	ReadTimeout     int    `env:"BIFROST_READ_TIMEOUT,default=15"`
	IdleTimeout     int    `env:"BIFROST_IDLE_TIMEOUT,default=60"`
	GracefulTimeout int    `env:"BIFROST_GRACEFUL_TIMEOUT,default=15"`
}

type GoogleConfig struct {
	ProjectID string `env:"BIFROST_GOOGLE_PROJECT_ID,required"`
}

type UnleashConfig struct {
	InstanceNamespace string `env:"BIFROST_UNLEASH_INSTANCE_NAMESPACE,required"`
	SQLInstanceID     string `env:"BIFROST_UNLEASH_SQL_INSTANCE_ID,required"`
}

type Config struct {
	Server    ServerConfig
	Google    GoogleConfig
	Unleash   UnleashConfig
	DebugMode bool
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
