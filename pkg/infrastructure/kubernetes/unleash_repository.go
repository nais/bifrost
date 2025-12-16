package kubernetes

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/domain/unleash"
	"github.com/nais/bifrost/pkg/utils"
	fqdnV1alpha3 "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

// UnleashRepository implements the unleash.Repository interface using Kubernetes CRDs
type UnleashRepository struct {
	kubeClient ctrl.Client
	config     *config.Config
	logger     *logrus.Logger
}

// NewUnleashRepository creates a new UnleashRepository
func NewUnleashRepository(kubeClient ctrl.Client, config *config.Config, logger *logrus.Logger) unleash.Repository {
	return &UnleashRepository{
		kubeClient: kubeClient,
		config:     config,
		logger:     logger,
	}
}

// List returns all Unleash instances, optionally excluding those with release channels
func (r *UnleashRepository) List(ctx context.Context, excludeChannelInstances bool) ([]*unleash.Instance, error) {
	serverList := unleashv1.UnleashList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "UnleashList",
			APIVersion: "unleasherator.nais.io/v1",
		},
	}

	opts := ctrl.ListOptions{
		Namespace: r.config.Unleash.InstanceNamespace,
	}

	if err := r.kubeClient.List(ctx, &serverList, &opts); err != nil {
		r.logger.WithContext(ctx).WithError(err).Error("Failed to list Unleash instances")
		return nil, fmt.Errorf("failed to list unleash instances: %w", err)
	}

	instances := make([]*unleash.Instance, 0, len(serverList.Items))
	for i := range serverList.Items {
		instance := r.crdToInstance(&serverList.Items[i])

		// Filter channel instances if requested
		if excludeChannelInstances && instance.HasReleaseChannel() {
			continue
		}

		instances = append(instances, instance)
	}

	r.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation":      "list_unleash",
		"count":          len(instances),
		"excluded":       len(serverList.Items) - len(instances),
		"exclude_filter": excludeChannelInstances,
	}).Debug("Listed Unleash instances")

	return instances, nil
}

// ListCRDs returns all Unleash CRDs, optionally excluding those with release channels
func (r *UnleashRepository) ListCRDs(ctx context.Context, excludeChannelInstances bool) ([]unleashv1.Unleash, error) {
	serverList := unleashv1.UnleashList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "UnleashList",
			APIVersion: "unleasherator.nais.io/v1",
		},
	}

	opts := ctrl.ListOptions{
		Namespace: r.config.Unleash.InstanceNamespace,
	}

	if err := r.kubeClient.List(ctx, &serverList, &opts); err != nil {
		r.logger.WithContext(ctx).WithError(err).Error("Failed to list Unleash CRDs")
		return nil, fmt.Errorf("failed to list unleash instances: %w", err)
	}

	if !excludeChannelInstances {
		return serverList.Items, nil
	}

	// Filter out instances with release channels
	result := make([]unleashv1.Unleash, 0, len(serverList.Items))
	for i := range serverList.Items {
		if serverList.Items[i].Spec.ReleaseChannel.Name == "" {
			result = append(result, serverList.Items[i])
		}
	}

	return result, nil
}

// Get retrieves a single Unleash instance by name
func (r *UnleashRepository) Get(ctx context.Context, name string) (*unleash.Instance, error) {
	serverInstance := &unleashv1.Unleash{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Unleash",
			APIVersion: "unleasherator.nais.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.config.Unleash.InstanceNamespace,
		},
	}

	if err := r.kubeClient.Get(ctx, ctrl.ObjectKeyFromObject(serverInstance), serverInstance); err != nil {
		r.logger.WithContext(ctx).WithError(err).WithField("instance", name).Error("Failed to get Unleash instance")
		return nil, fmt.Errorf("failed to get unleash instance %s: %w", name, err)
	}

	instance := r.crdToInstance(serverInstance)

	r.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "get_unleash",
		"instance":  name,
	}).Debug("Retrieved Unleash instance")

	return instance, nil
}

// Create creates a new Unleash instance
func (r *UnleashRepository) Create(ctx context.Context, cfg *unleash.Config) error {
	// Create FQDN network policy
	if err := r.createFQDNNetworkPolicy(ctx, cfg.Name); err != nil {
		return err
	}

	// Create Unleash CRD
	unleashCRD := BuildUnleashCRD(r.config, cfg)
	if err := r.kubeClient.Create(ctx, &unleashCRD); err != nil {
		r.logger.WithContext(ctx).WithError(err).WithField("instance", cfg.Name).Error("Failed to create Unleash CRD")
		return fmt.Errorf("failed to create unleash instance: %w", err)
	}

	versionSource := "default"
	if cfg.CustomVersion != "" {
		versionSource = "custom"
	} else if cfg.ReleaseChannelName != "" {
		versionSource = "releaseChannel"
	}

	r.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation":      "create_unleash",
		"instance":       cfg.Name,
		"version_source": versionSource,
	}).Info("Created Unleash instance")

	return nil
}

// Update updates an existing Unleash instance
func (r *UnleashRepository) Update(ctx context.Context, cfg *unleash.Config) error {
	// Get existing CRD
	unleashOld, err := r.getUnleashCRD(ctx, cfg.Name)
	if err != nil {
		return err
	}

	// Determine old and new version sources for logging
	oldVersionSource := "default"
	if unleashOld.Spec.CustomImage != "" {
		oldVersionSource = "custom"
	} else if unleashOld.Spec.ReleaseChannel.Name != "" {
		oldVersionSource = "releaseChannel"
	}

	newVersionSource := "default"
	if cfg.CustomVersion != "" {
		newVersionSource = "custom"
	} else if cfg.ReleaseChannelName != "" {
		newVersionSource = "releaseChannel"
	}

	// Update FQDN network policy
	if err := r.updateFQDNNetworkPolicy(ctx, cfg.Name); err != nil {
		return err
	}

	// Build new CRD
	unleashNew := BuildUnleashCRD(r.config, cfg)

	// Preserve metadata
	unleashNew.ObjectMeta.ResourceVersion = unleashOld.ObjectMeta.ResourceVersion
	unleashNew.ObjectMeta.CreationTimestamp = unleashOld.ObjectMeta.CreationTimestamp
	unleashNew.ObjectMeta.Generation = unleashOld.ObjectMeta.Generation
	unleashNew.ObjectMeta.UID = unleashOld.ObjectMeta.UID

	// Update CRD
	if err := r.kubeClient.Update(ctx, &unleashNew); err != nil {
		r.logger.WithContext(ctx).WithError(err).WithField("instance", cfg.Name).Error("Failed to update Unleash CRD")
		return fmt.Errorf("failed to update unleash instance: %w", err)
	}

	logFields := logrus.Fields{
		"operation":      "update_unleash",
		"instance":       cfg.Name,
		"version_source": newVersionSource,
	}

	// Log version source changes
	if oldVersionSource != newVersionSource {
		logFields["from"] = oldVersionSource
		logFields["to"] = newVersionSource
		r.logger.WithContext(ctx).WithFields(logFields).Info("Unleash instance version source changed")
	} else {
		r.logger.WithContext(ctx).WithFields(logFields).Info("Updated Unleash instance")
	}

	return nil
}

// Delete removes an Unleash instance
func (r *UnleashRepository) Delete(ctx context.Context, name string) error {
	// Delete FQDN network policy
	if err := r.deleteFQDNNetworkPolicy(ctx, name); err != nil {
		r.logger.WithContext(ctx).WithError(err).WithField("instance", name).Warn("Failed to delete FQDN network policy")
		// Continue with CRD deletion
	}

	// Delete Unleash CRD
	unleashDefinition := unleashv1.Unleash{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.config.Unleash.InstanceNamespace,
		},
	}

	if err := r.kubeClient.Delete(ctx, &unleashDefinition); err != nil {
		r.logger.WithContext(ctx).WithError(err).WithField("instance", name).Error("Failed to delete Unleash CRD")
		return fmt.Errorf("failed to delete unleash instance: %w", err)
	}

	r.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "delete_unleash",
		"instance":  name,
	}).Info("Deleted Unleash instance")

	return nil
}

// crdToInstance converts an Unleash CRD to domain Instance
func (r *UnleashRepository) crdToInstance(crd *unleashv1.Unleash) *unleash.Instance {
	instance := &unleash.Instance{
		Name:      crd.GetName(),
		Namespace: crd.GetNamespace(),
		CreatedAt: crd.ObjectMeta.CreationTimestamp.Time,
		Version:   crd.Status.Version,
		IsReady:   crd.IsReady(),
		APIUrl:    fmt.Sprintf("https://%s/api/", crd.Spec.ApiIngress.Host),
		WebUrl:    fmt.Sprintf("https://%s/", crd.Spec.WebIngress.Host),

		// Federation configuration
		EnableFederation:  crd.Spec.Federation.Enabled,
		FederationNonce:   crd.Spec.Federation.SecretNonce,
		AllowedTeams:      getEnvVar(crd, "TEAMS_ALLOWED_TEAMS", ""),
		AllowedNamespaces: utils.JoinNoEmpty(crd.Spec.Federation.Namespaces, ","),
		AllowedClusters:   utils.JoinNoEmpty(crd.Spec.Federation.Clusters, ","),
	}

	// Extract version source
	if crd.Spec.CustomImage != "" {
		// Extract version from image string (format: "repo/name:version")
		parts := strings.Split(crd.Spec.CustomImage, ":")
		if len(parts) > 1 {
			instance.CustomVersion = parts[1]
		}
	}
	if crd.Spec.ReleaseChannel.Name != "" {
		instance.ReleaseChannelName = crd.Spec.ReleaseChannel.Name
	}

	// Extract status information
	instance.ResolvedImage = crd.Status.ResolvedReleaseChannelImage
	instance.ChannelNameFromStatus = crd.Status.ReleaseChannelName

	return instance
}

// GetCRD retrieves an Unleash CRD (exported for use by application layer)
func (r *UnleashRepository) GetCRD(ctx context.Context, name string) (*unleashv1.Unleash, error) {
	unleashDefinition := unleashv1.Unleash{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.config.Unleash.InstanceNamespace,
		},
	}

	if err := r.kubeClient.Get(ctx, ctrl.ObjectKeyFromObject(&unleashDefinition), &unleashDefinition); err != nil {
		return nil, fmt.Errorf("failed to get unleash crd: %w", err)
	}

	return &unleashDefinition, nil
}

// getUnleashCRD retrieves an Unleash CRD (internal use)
func (r *UnleashRepository) getUnleashCRD(ctx context.Context, name string) (*unleashv1.Unleash, error) {
	return r.GetCRD(ctx, name)
}

// FQDN Network Policy operations

func (r *UnleashRepository) createFQDNNetworkPolicy(ctx context.Context, name string) error {
	u, err := url.Parse(r.config.Unleash.TeamsApiURL)
	if err != nil {
		return fmt.Errorf("failed to parse teams API URL: %w", err)
	}

	protocolTCP := corev1.ProtocolTCP
	fqdn := fqdnV1alpha3.FQDNNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-fqdn", name),
			Namespace: r.config.Unleash.InstanceNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "FQDNNetworkPolicy",
			APIVersion: "networking.gke.io/v1alpha3",
		},
		Spec: fqdnV1alpha3.FQDNNetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/instance":   name,
					"app.kubernetes.io/part-of":    "unleasherator",
					"app.kubernetes.io/name":       "Unleash",
					"app.kubernetes.io/created-by": "controller-manager",
				},
			},
			Egress: []fqdnV1alpha3.FQDNNetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 443},
							Protocol: &protocolTCP,
						},
					},
					To: []fqdnV1alpha3.FQDNNetworkPolicyPeer{
						{
							FQDNs: []string{
								"sqladmin.googleapis.com",
								"www.gstatic.com",
								"hooks.slack.com",
								"auth.nais.io",
								u.Host,
							},
						},
					},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 80},
							Protocol: &protocolTCP,
						},
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 988},
							Protocol: &protocolTCP,
						},
					},
					To: []fqdnV1alpha3.FQDNNetworkPolicyPeer{
						{
							FQDNs: []string{"metadata.google.internal"},
						},
					},
				},
			},
		},
	}

	if err := r.kubeClient.Create(ctx, &fqdn); err != nil {
		r.logger.WithContext(ctx).WithError(err).WithField("instance", name).Error("Failed to create FQDN network policy")
		return fmt.Errorf("failed to create fqdn network policy: %w", err)
	}

	return nil
}

func (r *UnleashRepository) updateFQDNNetworkPolicy(ctx context.Context, name string) error {
	// Get old policy
	fqdnOld, err := r.getFQDNNetworkPolicy(ctx, name)
	if err != nil {
		return err
	}

	// Parse teams API URL
	u, err := url.Parse(r.config.Unleash.TeamsApiURL)
	if err != nil {
		return fmt.Errorf("failed to parse teams API URL: %w", err)
	}

	// Build new policy inline
	protocolTCP := corev1.ProtocolTCP
	fqdnNew := fqdnV1alpha3.FQDNNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-fqdn", name),
			Namespace: r.config.Unleash.InstanceNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "FQDNNetworkPolicy",
			APIVersion: "networking.gke.io/v1alpha3",
		},
		Spec: fqdnV1alpha3.FQDNNetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/instance":   name,
					"app.kubernetes.io/part-of":    "unleasherator",
					"app.kubernetes.io/name":       "Unleash",
					"app.kubernetes.io/created-by": "controller-manager",
				},
			},
			Egress: []fqdnV1alpha3.FQDNNetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 443},
							Protocol: &protocolTCP,
						},
					},
					To: []fqdnV1alpha3.FQDNNetworkPolicyPeer{
						{
							FQDNs: []string{
								"sqladmin.googleapis.com",
								"www.gstatic.com",
								"hooks.slack.com",
								"auth.nais.io",
								u.Host,
							},
						},
					},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 80},
							Protocol: &protocolTCP,
						},
						{
							Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 988},
							Protocol: &protocolTCP,
						},
					},
					To: []fqdnV1alpha3.FQDNNetworkPolicyPeer{
						{
							FQDNs: []string{"metadata.google.internal"},
						},
					},
				},
			},
		},
	}

	// Preserve metadata
	fqdnNew.ObjectMeta.ResourceVersion = fqdnOld.ObjectMeta.ResourceVersion
	fqdnNew.ObjectMeta.CreationTimestamp = fqdnOld.ObjectMeta.CreationTimestamp
	fqdnNew.ObjectMeta.Generation = fqdnOld.ObjectMeta.Generation
	fqdnNew.ObjectMeta.UID = fqdnOld.ObjectMeta.UID

	// Update policy
	if err := r.kubeClient.Update(ctx, &fqdnNew); err != nil {
		r.logger.WithContext(ctx).WithError(err).WithField("instance", name).Error("Failed to update FQDN network policy")
		return fmt.Errorf("failed to update fqdn network policy: %w", err)
	}

	return nil
}

func (r *UnleashRepository) deleteFQDNNetworkPolicy(ctx context.Context, name string) error {
	fqdn := fqdnV1alpha3.FQDNNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-fqdn", name),
			Namespace: r.config.Unleash.InstanceNamespace,
		},
	}

	if err := r.kubeClient.Delete(ctx, &fqdn); err != nil {
		return fmt.Errorf("failed to delete fqdn network policy: %w", err)
	}

	return nil
}

func (r *UnleashRepository) getFQDNNetworkPolicy(ctx context.Context, name string) (*fqdnV1alpha3.FQDNNetworkPolicy, error) {
	fqdn := fqdnV1alpha3.FQDNNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-fqdn", name),
			Namespace: r.config.Unleash.InstanceNamespace,
		},
	}

	if err := r.kubeClient.Get(ctx, ctrl.ObjectKeyFromObject(&fqdn), &fqdn); err != nil {
		return nil, fmt.Errorf("failed to get fqdn network policy: %w", err)
	}

	return &fqdn, nil
}

// LoadConfigFromCRD extracts a ConfigBuilder from an existing Unleash CRD for updates
func LoadConfigFromCRD(crd *unleashv1.Unleash) *unleash.ConfigBuilder {
	builder := unleash.NewConfigBuilder().WithName(crd.GetName())

	// Extract version source
	if crd.Spec.CustomImage != "" {
		parts := strings.Split(crd.Spec.CustomImage, ":")
		if len(parts) > 1 {
			builder.WithCustomVersion(parts[1])
		}
	} else if crd.Spec.ReleaseChannel.Name != "" {
		builder.WithReleaseChannel(crd.Spec.ReleaseChannel.Name)
	}

	// Extract federation config
	if crd.Spec.Federation.Enabled {
		builder.WithFederation(
			crd.Spec.Federation.SecretNonce,
			getEnvVar(crd, "TEAMS_ALLOWED_TEAMS", ""),
			utils.JoinNoEmpty(crd.Spec.Federation.Namespaces, ","),
			utils.JoinNoEmpty(crd.Spec.Federation.Clusters, ","),
		)
	}

	// Extract operational settings
	builder.WithLogLevel(getEnvVar(crd, "LOG_LEVEL", "warn"))

	poolMax, _ := strconv.Atoi(getEnvVar(crd, "DATABASE_POOL_MAX", "3"))
	poolTimeout, _ := strconv.Atoi(getEnvVar(crd, "DATABASE_POOL_IDLE_TIMEOUT_MS", "1000"))
	builder.WithDatabasePool(poolMax, poolTimeout)

	return builder
}

// getEnvVar extracts an environment variable value from Unleash CRD
func getEnvVar(crd *unleashv1.Unleash, name, defaultValue string) string {
	for _, envVar := range crd.Spec.ExtraEnvVars {
		if envVar.Name == name {
			return envVar.Value
		}
	}
	return defaultValue
}

// BuildUnleashCRD creates an Unleash CRD from domain config
func BuildUnleashCRD(c *config.Config, cfg *unleash.Config) unleashv1.Unleash {
	cloudSqlProto := corev1.ProtocolTCP
	cloudSqlPort := intstr.FromInt(3307)

	federationNonce := cfg.FederationNonce
	if federationNonce == "" {
		federationNonce = utils.RandomString(8)
	}

	const (
		UnleashCustomImageRepo = "europe-north1-docker.pkg.dev/nais-io/nais/images/"
		UnleashCustomImageName = "unleash-v4"
		UnleashRequestCPU      = "100m"
		UnleashRequestMemory   = "128Mi"
		UnleashLimitMemory     = "256Mi"
		SqlProxyRequestCPU     = "10m"
		SqlProxyRequestMemory  = "100Mi"
		SqlProxyLimitMemory    = "100Mi"
	)

	server := unleashv1.Unleash{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Unleash",
			APIVersion: "unleash.nais.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.Name,
			Namespace: c.Unleash.InstanceNamespace,
		},
		Spec: unleashv1.UnleashSpec{
			Size: 1,
			Database: unleashv1.UnleashDatabaseConfig{
				Host:                  "localhost",
				Port:                  "5432",
				SSL:                   "false",
				SecretName:            cfg.Name,
				SecretUserKey:         "POSTGRES_USER",
				SecretPassKey:         "POSTGRES_PASSWORD",
				SecretDatabaseNameKey: "POSTGRES_DB",
			},
			WebIngress: unleashv1.UnleashIngressConfig{
				Enabled: true,
				Host:    fmt.Sprintf("%s-%s", cfg.Name, c.Unleash.InstanceWebIngressHost),
				Path:    "/",
				Class:   c.Unleash.InstanceWebIngressClass,
			},
			ApiIngress: unleashv1.UnleashIngressConfig{
				Enabled: true,
				Host:    fmt.Sprintf("%s-%s", cfg.Name, c.Unleash.InstanceAPIIngressHost),
				Path:    "/",
				Class:   c.Unleash.InstanceAPIIngressClass,
			},
			NetworkPolicy: unleashv1.UnleashNetworkPolicyConfig{
				Enabled:  true,
				AllowDNS: true,
				ExtraEgressRules: []networkingv1.NetworkPolicyEgressRule{
					{
						Ports: []networkingv1.NetworkPolicyPort{{
							Protocol: &cloudSqlProto,
							Port:     &cloudSqlPort,
						}},
						To: []networkingv1.NetworkPolicyPeer{{
							IPBlock: &networkingv1.IPBlock{
								CIDR: fmt.Sprintf("%s/32", c.Unleash.SQLInstanceAddress),
							},
						}},
					},
				},
			},
			Federation: unleashv1.UnleashFederationConfig{
				Enabled:     cfg.EnableFederation,
				Namespaces:  utils.SplitNoEmpty(cfg.AllowedNamespaces, ","),
				Clusters:    utils.SplitNoEmpty(cfg.AllowedClusters, ","),
				SecretNonce: federationNonce,
			},
			ExtraEnvVars: []corev1.EnvVar{
				{
					Name:  "OAUTH_JWT_AUDIENCE",
					Value: c.Unleash.InstanceWebOAuthJWTAudience,
				},
				{
					Name:  "OAUTH_JWT_AUTH",
					Value: "true",
				},
				{
					Name:  "TEAMS_API_URL",
					Value: c.Unleash.TeamsApiURL,
				},
				{
					Name: "TEAMS_API_TOKEN",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: c.Unleash.TeamsApiSecretName,
							},
							Key: c.Unleash.TeamsApiSecretTokenKey,
						},
					},
				},
				{
					Name:  "TEAMS_ALLOWED_TEAMS",
					Value: cfg.AllowedTeams,
				},
				{
					Name:  "LOG_LEVEL",
					Value: cfg.LogLevel,
				},
				{
					Name:  "DATABASE_POOL_MAX",
					Value: fmt.Sprintf("%d", cfg.DatabasePoolMax),
				},
				{
					Name:  "DATABASE_POOL_IDLE_TIMEOUT_MS",
					Value: fmt.Sprintf("%d", cfg.DatabasePoolIdleTimeoutMs),
				},
			},
			ExtraContainers: []corev1.Container{{
				Name:  "sql-proxy",
				Image: c.CloudConnectorProxy,
				Args: []string{
					"--structured-logs",
					"--port=5432",
					fmt.Sprintf("%s:%s:%s", c.Google.ProjectID,
						c.Unleash.SQLInstanceRegion,
						c.Unleash.SQLInstanceID),
				},
				SecurityContext: &corev1.SecurityContext{
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
					Privileged:               boolRef(false),
					RunAsUser:                int64Ref(65532),
					RunAsNonRoot:             boolRef(true),
					AllowPrivilegeEscalation: boolRef(false),
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(SqlProxyRequestCPU),
						corev1.ResourceMemory: resource.MustParse(SqlProxyRequestMemory),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse(SqlProxyLimitMemory),
					},
				},
			}},
			ExistingServiceAccountName: c.Unleash.InstanceServiceaccount,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(UnleashRequestCPU),
					corev1.ResourceMemory: resource.MustParse(UnleashRequestMemory),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse(UnleashLimitMemory),
				},
			},
		},
	}

	// Set version source: either custom image or release channel
	if cfg.CustomVersion != "" {
		server.Spec.CustomImage = fmt.Sprintf("%s%s:%s", UnleashCustomImageRepo, UnleashCustomImageName, cfg.CustomVersion)
	} else if cfg.ReleaseChannelName != "" {
		server.Spec.ReleaseChannel.Name = cfg.ReleaseChannelName
	}

	return server
}

func boolRef(b bool) *bool {
	return &b
}

func int64Ref(i int64) *int64 {
	return &i
}
