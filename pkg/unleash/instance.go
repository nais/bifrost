package unleash

import (
	"context"
	"fmt"
	"net/url"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/utils"
	fqdnV1alpha3 "github.com/nais/fqdn-policy/api/v1alpha3"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	admin "google.golang.org/api/sqladmin/v1beta4"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

type UnleashInstance struct {
	Name                 string
	KubernetesNamespace  string
	CreatedAt            metav1.Time
	ServerInstance       *unleashv1.Unleash
	DatabaseInstanceName string
	DatabaseProjectName  string
	Database             *admin.Database
	DatabaseUser         *admin.User
	DatabaseSecret       *corev1.Secret
}

func NewUnleashInstance(serverInstance *unleashv1.Unleash) *UnleashInstance {
	return &UnleashInstance{
		Name:                serverInstance.ObjectMeta.Name,
		KubernetesNamespace: serverInstance.ObjectMeta.Namespace,
		CreatedAt:           serverInstance.ObjectMeta.CreationTimestamp,
		ServerInstance:      serverInstance,
	}
}

func (u UnleashInstance) Age() string {
	return utils.HumanReadableAge(u.CreatedAt)
}

func (u *UnleashInstance) ApiUrl() string {
	if u.ServerInstance != nil {
		return fmt.Sprintf("https://%s/api/", u.ServerInstance.Spec.ApiIngress.Host)
	} else {
		return ""
	}
}

func (u *UnleashInstance) WebUrl() string {
	if u.ServerInstance != nil {
		return fmt.Sprintf("https://%s/", u.ServerInstance.Spec.WebIngress.Host)
	} else {
		return ""
	}
}

func (u *UnleashInstance) IsReady() bool {
	if u.ServerInstance != nil {
		return u.ServerInstance.IsReady()
	} else {
		return false
	}
}

func (u *UnleashInstance) Status() string {
	if u.ServerInstance != nil {
		if u.ServerInstance.IsReady() {
			return "Ready"
		} else {
			return "Not ready"
		}
	} else {
		return "Status unknown"
	}
}

func (u *UnleashInstance) Version() string {
	if u.ServerInstance != nil {
		return u.ServerInstance.Status.Version
	} else {
		return "Unknown"
	}
}

func (u *UnleashInstance) StatusLabel() string {
	if u.ServerInstance != nil {
		if u.ServerInstance.IsReady() {
			return "green"
		} else {
			return "red"
		}
	} else {
		return "orange"
	}
}

func (u *UnleashInstance) GetDatabase(ctx context.Context, client *admin.DatabasesService) error {
	database, err := getDatabase(ctx, client, u.DatabaseInstanceName, u.DatabaseProjectName, u.Name)
	if err != nil {
		return err
	}

	u.Database = database

	return nil
}

func (u *UnleashInstance) GetDatabaseUser(ctx context.Context, client *admin.UsersService) error {
	user, err := getDatabaseUser(ctx, client, u.DatabaseInstanceName, u.DatabaseProjectName, u.Name)
	if err != nil {
		return err
	}

	u.DatabaseUser = user

	return nil
}

func getServer(ctx context.Context, kubeClient ctrl.Client, kubeNamespace string, name string) (*unleashv1.Unleash, error) {
	unleashDefinition := unleashv1.Unleash{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kubeNamespace}}
	err := kubeClient.Get(ctx, ctrl.ObjectKeyFromObject(&unleashDefinition), &unleashDefinition)
	if err != nil {
		return nil, &UnleashError{Err: err, Reason: "failed to get server instance"}
	}
	return &unleashDefinition, nil
}

func deleteServer(ctx context.Context, kubeClient ctrl.Client, kubeNamespace string, name string) error {
	unleashDefinition := unleashv1.Unleash{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kubeNamespace}}
	if err := kubeClient.Delete(ctx, &unleashDefinition); err != nil {
		return &UnleashError{Err: err, Reason: "failed to delete server instance"}
	}
	return nil
}

func createServer(ctx context.Context, kubeClient ctrl.Client, config *config.Config, uc *UnleashConfig) (*unleashv1.Unleash, error) {
	unleashDefinition := UnleashDefinition(config, uc)
	if err := kubeClient.Create(ctx, &unleashDefinition); err != nil {
		return nil, &UnleashError{Err: err, Reason: "failed to create server instance"}
	}
	return &unleashDefinition, nil
}

func updateServer(ctx context.Context, kubeClient ctrl.Client, config *config.Config, uc *UnleashConfig) (*unleashv1.Unleash, error) {
	unleashDefinitionOld, err := getServer(ctx, kubeClient, config.Unleash.InstanceNamespace, uc.Name)
	if err != nil {
		return nil, err
	}

	unleashDefinitionNew := UnleashDefinition(config, uc)
	unleashDefinitionNew.ObjectMeta.ResourceVersion = unleashDefinitionOld.ObjectMeta.ResourceVersion
	unleashDefinitionNew.ObjectMeta.CreationTimestamp = unleashDefinitionOld.ObjectMeta.CreationTimestamp
	unleashDefinitionNew.ObjectMeta.Generation = unleashDefinitionOld.ObjectMeta.Generation
	unleashDefinitionNew.ObjectMeta.UID = unleashDefinitionOld.ObjectMeta.UID

	if err := kubeClient.Update(ctx, &unleashDefinitionNew); err != nil {
		return nil, &UnleashError{Err: err, Reason: "failed to update server instance"}
	}

	return &unleashDefinitionNew, nil
}

func getFQDNNetworkPolicy(ctx context.Context, kubeClient ctrl.Client, kubeNamespace string, name string) (*fqdnV1alpha3.FQDNNetworkPolicy, error) {
	fqdn := fqdnV1alpha3.FQDNNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-fqdn", name), Namespace: kubeNamespace}}
	if err := kubeClient.Get(ctx, ctrl.ObjectKeyFromObject(&fqdn), &fqdn); err != nil {
		return nil, &UnleashError{Err: err, Reason: "failed to get fqdn network policy"}
	}
	return &fqdn, nil
}

func deleteFQDNNetworkPolicy(ctx context.Context, kubeClient ctrl.Client, kubeNamespace string, name string) error {
	fqdn := fqdnV1alpha3.FQDNNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-fqdn", name), Namespace: kubeNamespace}}
	if err := kubeClient.Delete(ctx, &fqdn); err != nil {
		return &UnleashError{Err: err, Reason: "failed to delete fqdn network policy"}
	}
	return nil
}

func createFQDNNetworkPolicy(ctx context.Context, kubeClient ctrl.Client, kubeNamespace, name, teamsAPIURL string) error {
	u, err := url.Parse(teamsAPIURL)
	if err != nil {
		return &UnleashError{Err: fmt.Errorf("parsing %q: %w", teamsAPIURL, err), Reason: "failed to parse teams API URL"}
	}
	fqdn := FQDNNetworkPolicyDefinition(name, kubeNamespace, u.Host)
	if err := kubeClient.Create(ctx, &fqdn); err != nil {
		return &UnleashError{Err: err, Reason: "failed to create fqdn network policy"}
	}
	return nil
}

func updateFQDNNetworkPolicy(ctx context.Context, kubeClient ctrl.Client, kubeNamespace string, name string) error {
	fqdnOld, err := getFQDNNetworkPolicy(ctx, kubeClient, kubeNamespace, name)
	if err != nil {
		return err
	}

	fqdnNew := FQDNNetworkPolicyDefinition(name, kubeNamespace)
	fqdnNew.ObjectMeta.ResourceVersion = fqdnOld.ObjectMeta.ResourceVersion
	fqdnNew.ObjectMeta.CreationTimestamp = fqdnOld.ObjectMeta.CreationTimestamp
	fqdnNew.ObjectMeta.Generation = fqdnOld.ObjectMeta.Generation
	fqdnNew.ObjectMeta.UID = fqdnOld.ObjectMeta.UID

	if err := kubeClient.Update(ctx, &fqdnNew); err != nil {
		return &UnleashError{Err: err, Reason: "failed to update fqdn network policy"}
	}

	return nil
}
