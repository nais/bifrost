package config

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"
	"time"

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
	Auth            AuthConfig
}

// AuthConfig configures pre-shared-key authentication for the bifrost API.
// The bifrost API is called only by nais-api, which authenticates end users and
// enforces team ownership itself; bifrost only needs to authenticate that the
// caller is the trusted service. The key is provisioned in fasit and injected
// into both bifrost and nais-api.
type AuthConfig struct {
	// APIKeys is a comma-separated list of accepted pre-shared keys. Multiple
	// values allow zero-downtime key rotation (accept old and new at once).
	APIKeys string `env:"BIFROST_API_KEYS"`
	// Enforced controls whether requests without a valid key are rejected. It
	// defaults to false ("accept-then-enforce"): the first rollout logs
	// unauthenticated calls without blocking them so nais-api can start sending
	// the key; a later rollout flips this to true to fail closed.
	Enforced bool `env:"BIFROST_AUTH_ENFORCED,default=false"`
}

// ParsedAPIKeys returns the configured pre-shared keys, trimmed and non-empty.
func (a AuthConfig) ParsedAPIKeys() []string {
	if a.APIKeys == "" {
		return nil
	}
	keys := make([]string, 0)
	for _, k := range strings.Split(a.APIKeys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

type GoogleConfig struct {
	ProjectID string `env:"BIFROST_GOOGLE_PROJECT_ID,required"`
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
	DefaultReleaseChannel       string `env:"BIFROST_UNLEASH_DEFAULT_RELEASE_CHANNEL"`
	NaisApiAddress              string `env:"BIFROST_UNLEASH_INSTANCE_NAIS_API_ADDRESS,required"`
	NaisApiNamespace            string `env:"BIFROST_UNLEASH_INSTANCE_NAIS_API_NAMESPACE,required"`

	// Migration settings for transitioning custom versions to release channels
	MigrationEnabled       bool          `env:"BIFROST_UNLEASH_MIGRATION_ENABLED,default=false"`
	MigrationTargetChannel string        `env:"BIFROST_UNLEASH_MIGRATION_TARGET_CHANNEL"`
	MigrationHealthTimeout time.Duration `env:"BIFROST_UNLEASH_MIGRATION_HEALTH_TIMEOUT,default=5m"`
	MigrationDelay         time.Duration `env:"BIFROST_UNLEASH_MIGRATION_DELAY,default=30s"`

	// Channel migration settings for transitioning between release channels (e.g., v5 to v6)
	// ChannelMigrationMap is a comma-separated list of source:target pairs, e.g. "stable-v5:stable-v6,rapid-v5:rapid-v6"
	ChannelMigrationEnabled       bool          `env:"BIFROST_UNLEASH_CHANNEL_MIGRATION_ENABLED,default=false"`
	ChannelMigrationMap           string        `env:"BIFROST_UNLEASH_CHANNEL_MIGRATION_MAP"`
	ChannelMigrationHealthTimeout time.Duration `env:"BIFROST_UNLEASH_CHANNEL_MIGRATION_HEALTH_TIMEOUT,default=5m"`
	ChannelMigrationDelay         time.Duration `env:"BIFROST_UNLEASH_CHANNEL_MIGRATION_DELAY,default=30s"`
}

// ParseChannelMigrationMap parses the ChannelMigrationMap string into a map of source→target channel pairs.
// Format: "source1:target1,source2:target2"
func (c *UnleashConfig) ParseChannelMigrationMap() (map[string]string, error) {
	result := make(map[string]string)
	if c.ChannelMigrationMap == "" {
		return result, nil
	}

	for _, pair := range strings.Split(c.ChannelMigrationMap, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("invalid channel migration map entry: %q (expected source:target)", pair)
		}

		source := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])

		if source == target {
			return nil, fmt.Errorf("channel migration source and target are the same: %q", source)
		}

		if _, exists := result[source]; exists {
			return nil, fmt.Errorf("duplicate source channel in migration map: %q", source)
		}

		result[source] = target
	}

	return result, nil
}

// ReconcilerConfig configures the controller-runtime reconcile loop that keeps
// bifrost-managed Unleash instances converged to their desired configuration.
type ReconcilerConfig struct {
	// Enabled turns on the reconcile loop. Default off so it can be rolled out
	// per environment behind a feature flag.
	Enabled bool `env:"BIFROST_RECONCILER_ENABLED,default=false"`
	// DryRun runs the loop in observe mode: it computes and records (via the
	// bifrost_reconciler_actions_total{action="would_change"} metric) what it
	// would change but never writes. This is the dark-launch step — enable the
	// reconciler with DryRun on, confirm the blast radius, then set it false.
	DryRun bool `env:"BIFROST_RECONCILER_DRY_RUN,default=false"`
	// AutoAdopt makes the reconciler stamp the managed-by label on unlabelled
	// Unleash instances in its own namespace, so a fleet created before the
	// label existed becomes visible in one observable event instead of being
	// adopted one user PUT at a time over months.
	//
	// It is a separate switch from Enabled and orthogonal to DryRun on purpose:
	// adoption writes, but only metadata, and the intended rollout is
	// Enabled+DryRun+AutoAdopt — measure the fleet before converging it. Folding
	// it into DryRun would make "observe only, no writes" untrue; folding it
	// into Enabled would make it impossible to turn off once the fleet is
	// adopted, which it should be, since the sweep is then pure cost.
	AutoAdopt bool `env:"BIFROST_RECONCILER_AUTO_ADOPT,default=false"`
	// ResyncInterval is how often every managed instance is re-rendered even
	// without a CR event, so global-config changes propagate and drift heals.
	ResyncInterval time.Duration `env:"BIFROST_RECONCILER_RESYNC_INTERVAL,default=10m"`
	// LeaderElection ensures at most one replica reconciles. Requires lease RBAC;
	// keep off while running a single replica.
	LeaderElection bool `env:"BIFROST_RECONCILER_LEADER_ELECTION,default=false"`
	// LeaderElectionNamespace is where the lease lives; empty auto-detects the
	// pod namespace in-cluster.
	LeaderElectionNamespace string `env:"BIFROST_RECONCILER_LEADER_ELECTION_NAMESPACE"`
}

type Config struct {
	Meta       MetaConfig
	Server     ServerConfig
	Google     GoogleConfig
	Unleash    UnleashConfig
	Reconciler ReconcilerConfig
	// LogLevel is the logrus level bifrost logs at. The chart has exposed
	// backend.logLevel all along but nothing read it: the logger was hardcoded
	// to debug, which also enabled every controller-runtime verbosity level —
	// including the per-item V(5) reconcile chatter, two to three JSON lines per
	// instance per resync.
	LogLevel            string `env:"BIFROST_LOG_LEVEL,default=info"`
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

// Validate rejects configuration that parsed but is unusable.
//
// The `,required` tag is not the guard it looks like: go-envconfig only checks
// whether the variable was *found*, and os.LookupEnv reports a set-but-empty
// variable as found. Every `,required` field can therefore still arrive as "",
// and for some of them an empty value silently *widens* scope instead of
// narrowing it — an empty BIFROST_UNLEASH_INSTANCE_NAMESPACE is
// metav1.NamespaceAll in both client-go and controller-runtime, so it does not
// mean "nothing", it means every namespace in the cluster, including the tenant
// namespaces unleasherator owns.
//
// The fields are found by walking the struct tags rather than being listed here.
// A hand-written list covered 4 of the 14 `,required` fields and could not
// notice a fifteenth being added, so what it encoded was not "the dangerous
// ones" but "the ones somebody had thought about".
func (c *Config) Validate() error {
	if err := validateRequired(reflect.ValueOf(c).Elem()); err != nil {
		return err
	}

	// Adoption exists to queue instances created before the desired-state
	// annotation did, so every instance it stamps arrives without a recorded
	// intent. The reconciler observes those and never writes them, which is what
	// makes adoption survivable — but that is a rule in one function, and the
	// blast radius on the other side of it is every instance in the namespace
	// rewritten from a lossy read-back of its own spec. Adoption therefore only
	// runs alongside a reconciler that cannot write at all.
	if c.Reconciler.AutoAdopt && !c.Reconciler.DryRun {
		return fmt.Errorf("BIFROST_RECONCILER_AUTO_ADOPT=true requires BIFROST_RECONCILER_DRY_RUN=true: adoption queues instances that carry no desired-state annotation, and converging one means rendering it from a lossy read-back of its own spec")
	}

	return nil
}

// validateRequired walks a config struct and holds every `,required` string
// field to what the tag reads as: present and usable. Non-string fields are
// left alone — envconfig fails to parse an empty duration or bool, so those
// cannot arrive blank in the first place.
func validateRequired(v reflect.Value) error {
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}

		if field.Type.Kind() == reflect.Struct {
			if err := validateRequired(v.Field(i)); err != nil {
				return err
			}
			continue
		}

		name, required := requiredEnvName(field)
		if !required || field.Type.Kind() != reflect.String {
			continue
		}

		value := v.Field(i).String()
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must be set to a non-empty value", name)
		}
		// Padding is rejected rather than trimmed, because only some readers
		// trim: a padded namespace passes as non-empty here, adopts instances
		// under the trimmed name, and leaves the informer cache looking up the
		// padded one forever.
		if value != strings.TrimSpace(value) {
			return fmt.Errorf("%s has leading or trailing whitespace: %q", name, value)
		}
	}

	return nil
}

// requiredEnvName returns the variable a field reads and whether it is tagged
// `,required`. The tag is `NAME[,opt[,opt...]]`, so the options are everything
// after the first comma.
func requiredEnvName(field reflect.StructField) (string, bool) {
	tag, ok := field.Tag.Lookup("env")
	if !ok {
		return "", false
	}

	parts := strings.Split(tag, ",")
	return parts[0], slices.Contains(parts[1:], "required")
}

func New(ctx context.Context) *Config {
	var c Config
	if err := envconfig.Process(ctx, &c); err != nil {
		panic(err)
	}

	if err := c.Validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	return &c
}
