package unleash

import (
	"context"
	"errors"

	"github.com/nais/bifrost/pkg/config"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/sqladmin/v1beta4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

type IUnleashService interface {
	List(ctx context.Context) ([]*UnleashInstance, error)
	Get(ctx context.Context, name string) (*UnleashInstance, error)
	Create(ctx context.Context, uc *UnleashConfig) (*unleashv1.Unleash, error)
	Update(ctx context.Context, uc *UnleashConfig) (*unleashv1.Unleash, error)
	Delete(ctx context.Context, name string) error
}

type ISQLDatabasesService interface {
	Get(project string, instance string, database string) *admin.DatabasesGetCall
	Insert(project string, instance string, database *admin.Database) *admin.DatabasesInsertCall
	Delete(project string, instance string, database string) *admin.DatabasesDeleteCall
}

type ISQLUsersService interface {
	Get(project string, instance string, name string) *admin.UsersGetCall
	Insert(project string, instance string, user *admin.User) *admin.UsersInsertCall
	Delete(project string, instance string) *admin.UsersDeleteCall
}

type UnleashService struct {
	sqlDatabasesClient ISQLDatabasesService
	sqlUsersClient     ISQLUsersService
	kubeClient         ctrl.Client
	config             *config.Config
	logger             *logrus.Logger
}

func NewUnleashService(sqlDatabasesClient ISQLDatabasesService, sqlUsersClient ISQLUsersService, kubeClient ctrl.Client, config *config.Config, logger *logrus.Logger) *UnleashService {
	return &UnleashService{
		sqlDatabasesClient: sqlDatabasesClient,
		sqlUsersClient:     sqlUsersClient,
		kubeClient:         kubeClient,
		config:             config,
		logger:             logger,
	}
}

func (s *UnleashService) List(ctx context.Context) ([]*UnleashInstance, error) {
	instanceList := []*UnleashInstance{}

	serverList := unleashv1.UnleashList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "UnleashList",
			APIVersion: "unleasherator.nais.io/v1",
		},
	}

	opts := ctrl.ListOptions{
		Namespace: s.config.Unleash.InstanceNamespace,
	}

	if err := s.kubeClient.List(ctx, &serverList, &opts); err != nil {
		return nil, err
	}

	for _, instance := range serverList.Items {
		instanceList = append(instanceList, NewUnleashInstance(&instance))
	}

	return instanceList, nil
}

func (s *UnleashService) Get(ctx context.Context, name string) (*UnleashInstance, error) {
	serverInstance := &unleashv1.Unleash{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Unleash",
			APIVersion: "unleasherator.nais.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.config.Unleash.InstanceNamespace,
		},
	}

	if err := s.kubeClient.Get(ctx, ctrl.ObjectKeyFromObject(serverInstance), serverInstance); err != nil {
		return nil, err
	}

	return NewUnleashInstance(serverInstance), nil
}

func (s *UnleashService) Create(ctx context.Context, uc *UnleashConfig) (*unleashv1.Unleash, error) {
	database, dbErr := createDatabase(ctx, s.sqlDatabasesClient, s.config.Google.ProjectID, s.config.Unleash.SQLInstanceID, uc.Name)
	databaseUser, dbUserErr := createDatabaseUser(ctx, s.sqlUsersClient, s.config.Google.ProjectID, s.config.Unleash.SQLInstanceID, uc.Name)
	secretErr := createDatabaseUserSecret(ctx, s.kubeClient, s.config.Unleash.InstanceNamespace, s.config.Unleash.SQLInstanceID, s.config.Unleash.SQLInstanceAddress, s.config.Google.ProjectID, database, databaseUser)
	fqdnError := createFQDNNetworkPolicy(ctx, s.kubeClient, s.config.Unleash.InstanceNamespace, database.Name, s.config.Unleash.TeamsApiURL)
	unleashInstance, serverError := createServer(ctx, s.kubeClient, s.config, uc)

	if err := errors.Join(dbErr, dbUserErr, secretErr, fqdnError, serverError); err != nil {
		return nil, err
	}
	return unleashInstance, nil
}

func (s *UnleashService) Update(ctx context.Context, uc *UnleashConfig) (*unleashv1.Unleash, error) {
	fqdnError := updateFQDNNetworkPolicy(ctx, s.kubeClient, s.config.Unleash.InstanceNamespace, uc.Name, s.config.Unleash.TeamsApiURL)
	unleashInstance, serverError := updateServer(ctx, s.kubeClient, s.config, uc)

	if err := errors.Join(fqdnError, serverError); err != nil {
		return nil, err
	}
	return unleashInstance, nil
}

func (s *UnleashService) Delete(ctx context.Context, name string) error {
	serverErr := deleteServer(ctx, s.kubeClient, s.config.Unleash.InstanceNamespace, name)
	netPolErr := deleteFQDNNetworkPolicy(ctx, s.kubeClient, s.config.Unleash.InstanceNamespace, name)
	dbUserSecretErr := deleteDatabaseUserSecret(ctx, s.kubeClient, s.config.Unleash.InstanceNamespace, name)
	dbErr := deleteDatabase(ctx, s.sqlDatabasesClient, s.config.Google.ProjectID, s.config.Unleash.SQLInstanceID, name)
	dbUserErr := deleteDatabaseUser(ctx, s.sqlUsersClient, s.config.Google.ProjectID, s.config.Unleash.SQLInstanceID, name)

	return errors.Join(serverErr, netPolErr, dbUserSecretErr, dbUserErr, dbErr)
}
