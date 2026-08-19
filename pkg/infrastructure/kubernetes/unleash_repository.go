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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		// A missing instance is an ordinary answer, not a failure: the create
		// path asks this question about every new instance. Logging it at error
		// level would put a spurious error line in front of every successful
		// create.
		log := r.logger.WithContext(ctx).WithError(err).WithField("instance", name)
		if apierrors.IsNotFound(err) {
			log.Debug("Unleash instance not found")
		} else {
			log.Error("Failed to get Unleash instance")
		}
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
	netpolCreated, err := r.createFQDNNetworkPolicy(ctx, cfg.Name)
	if err != nil {
		return err
	}

	// Create Unleash CRD
	unleashCRD := BuildUnleashCRD(r.config, cfg)
	if err := r.kubeClient.Create(ctx, &unleashCRD); err != nil {
		r.logger.WithContext(ctx).WithError(err).WithField("instance", cfg.Name).Error("Failed to create Unleash CRD")
		// Clean up the network policy we just created. The caller's rollback
		// keys off the CRD having been created, so without this the policy is
		// orphaned and every retry fails on it.
		//
		// Not on AlreadyExists: that means the instance is live, and the policy
		// belongs to it even if this call happened to create it in a race.
		// Deleting it would cut egress for a running instance.
		if netpolCreated && !apierrors.IsAlreadyExists(err) {
			if e := r.deleteFQDNNetworkPolicy(ctx, cfg.Name); e != nil && !apierrors.IsNotFound(e) {
				r.logger.WithContext(ctx).WithError(e).WithField("instance", cfg.Name).
					Error("Failed to clean up FQDN network policy after CRD create failure")
			}
		}
		return fmt.Errorf("failed to create unleash instance: %w", err)
	}

	return nil
}

// Update updates an existing Unleash instance.
//
// opts.ExpectedResourceVersion turns the write into a conditional one. The CRD
// is rendered wholesale from cfg, so without it a write that landed after the
// caller read the instance is overwritten silently — the resourceVersion copied
// from the Get below postdates the data cfg was built from and protects nothing.
func (r *UnleashRepository) Update(ctx context.Context, cfg *unleash.Config, opts unleash.UpdateOptions) error {
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
	if opts.ExpectedResourceVersion != "" {
		// The caller's read is the one the config was derived from, so it is the
		// version the write must be conditional on. Anything newer means another
		// writer got in between and the API server rejects this with a Conflict.
		unleashNew.ObjectMeta.ResourceVersion = opts.ExpectedResourceVersion
	}
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

// ReconcileIngressClasses re-applies the configured web and api ingress classes
// to all existing Unleash instances.
//
// Unleasherator renders each ingress' ingressClassName directly from the Unleash
// CRD's spec.webIngress.class / spec.apiIngress.class and has no controller-side
// default. A configured class change therefore only reaches an instance once its
// CRD spec is updated, so existing instances keep their old class until this runs.
func (r *UnleashRepository) ReconcileIngressClasses(ctx context.Context) error {
	crds, err := r.ListCRDs(ctx, false)
	if err != nil {
		return fmt.Errorf("failed to list unleash instances for ingress class reconciliation: %w", err)
	}

	webClass := r.config.Unleash.InstanceWebIngressClass
	apiClass := r.config.Unleash.InstanceAPIIngressClass

	var errs []error
	updated := 0
	for i := range crds {
		crd := &crds[i]
		if crd.Spec.WebIngress.Class == webClass && crd.Spec.ApiIngress.Class == apiClass {
			continue
		}

		patch := ctrl.MergeFrom(crd.DeepCopy())
		crd.Spec.WebIngress.Class = webClass
		crd.Spec.ApiIngress.Class = apiClass
		if err := r.kubeClient.Patch(ctx, crd, patch); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", crd.Name, err))
			continue
		}
		updated++
	}

	r.logger.WithContext(ctx).WithFields(logrus.Fields{
		"instanceCount": len(crds),
		"updated":       updated,
	}).Info("Reconciled ingress classes for existing instances")

	if len(errs) > 0 {
		return fmt.Errorf("errors reconciling ingress classes: %v", errs)
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
		// Carried so a caller can make a later write conditional on this read.
		ResourceVersion: crd.GetResourceVersion(),
		CreatedAt:       crd.ObjectMeta.CreationTimestamp.Time,
		Version:         crd.Status.Version,
		IsReady:         crd.IsReady(),
		APIUrl:          fmt.Sprintf("https://%s/api/", crd.Spec.ApiIngress.Host),
		WebUrl:          fmt.Sprintf("https://%s/", crd.Spec.WebIngress.Host),

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

func (r *UnleashRepository) createFQDNNetworkPolicy(ctx context.Context, name string) (bool, error) {
	u, err := url.Parse(r.config.Unleash.TeamsApiURL)
	if err != nil {
		return false, fmt.Errorf("failed to parse teams API URL: %w", err)
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
		// A policy left behind by an earlier attempt must not wedge this one:
		// without tolerating AlreadyExists here, a create that got past the
		// policy but failed on the CRD can never be retried successfully.
		if apierrors.IsAlreadyExists(err) {
			r.logger.WithContext(ctx).WithField("instance", name).Info("FQDN network policy already exists, reusing it")
			return false, nil
		}
		r.logger.WithContext(ctx).WithError(err).WithField("instance", name).Error("Failed to create FQDN network policy")
		return false, fmt.Errorf("failed to create fqdn network policy: %w", err)
	}

	return true, nil
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

	// TEAMS_ALLOWED_TEAMS governs who may log in to the Unleash UI and exists
	// whether or not the instance is federated. Reading it only in the
	// federated branch meant a non-federated instance round-tripped to an empty
	// list: the config this produces is written back as the authoritative
	// desired-state annotation, so the loss became the recorded intent and the
	// reconciler would enforce it on every resync.
	allowedTeams := getEnvVar(crd, "TEAMS_ALLOWED_TEAMS", "")

	// Extract federation config
	if crd.Spec.Federation.Enabled {
		builder.WithFederation(
			crd.Spec.Federation.SecretNonce,
			allowedTeams,
			utils.JoinNoEmpty(crd.Spec.Federation.Namespaces, ","),
			utils.JoinNoEmpty(crd.Spec.Federation.Clusters, ","),
		)
	} else {
		builder.SetAllowedTeams(allowedTeams)
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

	// Mint a nonce on first create and write it back to cfg, not just into the
	// spec. stampManagedMetadata records cfg as the desired-state annotation, so
	// keeping the minted value local would store an intent that disagrees with
	// the spec it produced — and every later reconcile would read that as drift.
	if cfg.FederationNonce == "" {
		cfg.FederationNonce = utils.RandomString(8)
	}
	federationNonce := cfg.FederationNonce

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
			// State the intent explicitly rather than relying on the CRD's
			// default. The API server stamps enabled=true into the stored
			// object, and `omitempty` on a bool means a rendered false is
			// indistinguishable from unset — so leaving this out makes the
			// reconciler see permanent drift that no patch can resolve.
			Prometheus: unleashv1.UnleashPrometheusConfig{Enabled: true},
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

	// Mark the instance as bifrost-managed and record the intent it was rendered
	// from, so the reconciler can find and re-render it non-lossily.
	stampManagedMetadata(&server, cfg)

	return server
}

func boolRef(b bool) *bool {
	return &b
}

func int64Ref(i int64) *int64 {
	return &i
}
