package unleash

import (
	"context"
	"errors"
	"fmt"

	fqdnV1alpha3 "github.com/GoogleCloudPlatform/gke-fqdnnetworkpolicies-golang/api/v1alpha3"
	"github.com/nais/bifrost/pkg/config"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	admin "google.golang.org/api/sqladmin/v1beta4"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"
)

type Unleash struct {
	TeamName            string
	KubernetesNamespace string
	DatabaseInstance    *admin.DatabaseInstance
	Database            *admin.Database
	DatabaseUser        *admin.User
	Secret              *corev1.Secret
}

func (u *Unleash) GetDatabaseUser(ctx context.Context, client *admin.Service) error {
	user, err := getDatabaseUser(ctx, client, u.DatabaseInstance, u.Database.Name)
	if err != nil {
		return err
	}

	u.DatabaseUser = user

	return nil
}

func (u *Unleash) Delete(ctx context.Context, googleClient *admin.Service, kubeClient ctrl.Client) error {
	dbUserErr := deleteDatabaseUser(ctx, googleClient, u.DatabaseInstance, u.Database.Name)
	dbErr := deleteDatabase(ctx, googleClient, u.DatabaseInstance, u.Database.Name)
	dbUserSecretErr := deleteDatabaseUserSecret(ctx, kubeClient, u.KubernetesNamespace, u.Database.Name)

	return errors.Join(dbUserErr, dbErr, dbUserSecretErr)
}

func GetInstances(ctx context.Context, googleClient *admin.Service, databaseInstance *admin.DatabaseInstance, kubeNamespace string) ([]Unleash, error) {
	databases, err := getDatabases(ctx, googleClient, databaseInstance)
	if err != nil {
		return nil, err
	}

	var instances []Unleash

	for _, database := range databases {
		if database.Name == "postgres" {
			continue
		}

		instances = append(instances, Unleash{
			TeamName:            database.Name,
			KubernetesNamespace: kubeNamespace,
			DatabaseInstance:    databaseInstance,
			Database:            database,
		})
	}

	return instances, nil
}

func GetInstance(ctx context.Context, googleClient *admin.Service, databaseInstance *admin.DatabaseInstance, databaseName string, kubeNamespace string) (Unleash, error) {
	database, err := getDatabase(ctx, googleClient, databaseInstance, databaseName)
	if err != nil {
		return Unleash{}, err
	}

	return Unleash{
		TeamName:            database.Name,
		KubernetesNamespace: kubeNamespace,
		DatabaseInstance:    databaseInstance,
		Database:            database,
	}, nil
}

func createUnleash(ctx context.Context, kubeClient ctrl.Client, config *config.Config, unleashDefinition unleashv1.Unleash, databaseName string, iapAudience string) error {
	schema := runtime.NewScheme()
	unleashv1.AddToScheme(schema)
	opts := ctrl.Options{
		Scheme: schema,
	}
	c, err := ctrl.New(ctrl_config.GetConfigOrDie(), opts)
	if err != nil {
		return err
	}

	err = c.Create(ctx, &unleashDefinition)
	if err != nil {
		return err
	}
	return nil
}

func CreateInstance(ctx context.Context,
	googleClient *admin.Service,
	databaseInstance *admin.DatabaseInstance,
	databaseName string,
	config *config.Config,
	kubeClient ctrl.Client,
) error {
	iapAudience := fmt.Sprintf("/projects/%s/global/backendServices/%s", config.Google.ProjectID, config.Google.IAPBackendServiceID)

	database, dbErr := createDatabase(ctx, googleClient, databaseInstance, databaseName)
	databaseUser, dbUserErr := createDatabaseUser(ctx, googleClient, databaseInstance, databaseName)
	secretErr := createDatabaseUserSecret(ctx, kubeClient, config.Unleash.InstanceNamespace, databaseInstance, database, databaseUser)
	fqdnCreationError := createFQDNNetworkPolicy(ctx, kubeClient, config.Unleash.InstanceNamespace, database.Name)
	unleashSpec := newUnleashSpec(config, databaseName, iapAudience)
	createCrdError := createUnleash(ctx, kubeClient, config, unleashSpec, databaseName, iapAudience)

	if err := errors.Join(dbErr, dbUserErr, secretErr, fqdnCreationError, createCrdError); err != nil {
		return err
	}
	return nil
}

func createFQDNNetworkPolicy(ctx context.Context, kubeClient ctrl.Client, kubeNamespace string, teamName string) error {
	fqdn := newFQDNNetworkPolicySpec(teamName, kubeNamespace)

	schema := runtime.NewScheme()
	fqdnV1alpha3.AddToScheme(schema)
	opts := ctrl.Options{
		Scheme: schema,
	}
	c, err := ctrl.New(ctrl_config.GetConfigOrDie(), opts)
	if err != nil {
		return err
	}

	err = c.Create(ctx, &fqdn)
	if err != nil {
		return err
	}
	return nil
}
