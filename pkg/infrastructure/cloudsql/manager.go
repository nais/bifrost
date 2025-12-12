package cloudsql

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/nais/bifrost/pkg/config"
	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/sqladmin/v1beta4"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

// DatabasesService interface for Cloud SQL databases operations
type DatabasesService interface {
	Get(project string, instance string, database string) *admin.DatabasesGetCall
	Insert(project string, instance string, database *admin.Database) *admin.DatabasesInsertCall
	Delete(project string, instance string, database string) *admin.DatabasesDeleteCall
}

// UsersService interface for Cloud SQL users operations
type UsersService interface {
	Get(project string, instance string, name string) *admin.UsersGetCall
	Insert(project string, instance string, user *admin.User) *admin.UsersInsertCall
	Delete(project string, instance string) *admin.UsersDeleteCall
}

// Manager handles Cloud SQL database and user lifecycle
type Manager struct {
	databasesClient DatabasesService
	usersClient     UsersService
	kubeClient      ctrl.Client
	config          *config.Config
	logger          *logrus.Logger
}

// NewManager creates a new Cloud SQL Manager
func NewManager(databasesClient DatabasesService, usersClient UsersService, kubeClient ctrl.Client, config *config.Config, logger *logrus.Logger) *Manager {
	return &Manager{
		databasesClient: databasesClient,
		usersClient:     usersClient,
		kubeClient:      kubeClient,
		config:          config,
		logger:          logger,
	}
}

// CreateDatabase creates a new Cloud SQL database
func (m *Manager) CreateDatabase(ctx context.Context, name string) error {
	database := &admin.Database{
		Name: name,
	}

	_, err := m.databasesClient.Insert(m.config.Google.ProjectID, m.config.Unleash.SQLInstanceID, database).Context(ctx).Do()
	if err != nil {
		m.logger.WithContext(ctx).WithError(err).WithField("database", name).Error("Failed to create database")
		return fmt.Errorf("failed to create database: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "create_database",
		"database":  name,
	}).Info("Created Cloud SQL database")

	return nil
}

// CreateDatabaseUser creates a new Cloud SQL user with a random password
func (m *Manager) CreateDatabaseUser(ctx context.Context, name string) (string, error) {
	password, err := generatePassword(16)
	if err != nil {
		return "", fmt.Errorf("failed to generate password: %w", err)
	}

	user := &admin.User{
		Name:     name,
		Password: password,
	}

	_, err = m.usersClient.Insert(m.config.Google.ProjectID, m.config.Unleash.SQLInstanceID, user).Context(ctx).Do()
	if err != nil {
		m.logger.WithContext(ctx).WithError(err).WithField("user", name).Error("Failed to create database user")
		return "", fmt.Errorf("failed to create database user: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "create_database_user",
		"user":      name,
	}).Info("Created Cloud SQL user")

	return password, nil
}

// CreateSecret creates a Kubernetes secret with database credentials
func (m *Manager) CreateSecret(ctx context.Context, databaseName, password string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      databaseName,
			Namespace: m.config.Unleash.InstanceNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		Data: map[string][]byte{
			"POSTGRES_USER":     []byte(databaseName),
			"POSTGRES_PASSWORD": []byte(password),
			"POSTGRES_DB":       []byte(databaseName),
			"POSTGRES_HOST":     []byte(m.config.Unleash.SQLInstanceAddress),
		},
	}

	if err := m.kubeClient.Create(ctx, secret); err != nil {
		m.logger.WithContext(ctx).WithError(err).WithField("database", databaseName).Error("Failed to create database secret")
		return fmt.Errorf("failed to create database secret: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "create_database_secret",
		"database":  databaseName,
	}).Info("Created database credentials secret")

	return nil
}

// DeleteDatabase deletes a Cloud SQL database
func (m *Manager) DeleteDatabase(ctx context.Context, name string) error {
	_, err := m.databasesClient.Delete(m.config.Google.ProjectID, m.config.Unleash.SQLInstanceID, name).Context(ctx).Do()
	if err != nil {
		m.logger.WithContext(ctx).WithError(err).WithField("database", name).Error("Failed to delete database")
		return fmt.Errorf("failed to delete database: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "delete_database",
		"database":  name,
	}).Info("Deleted Cloud SQL database")

	return nil
}

// DeleteDatabaseUser deletes a Cloud SQL user
func (m *Manager) DeleteDatabaseUser(ctx context.Context, name string) error {
	_, err := m.usersClient.Delete(m.config.Google.ProjectID, m.config.Unleash.SQLInstanceID).Name(name).Context(ctx).Do()
	if err != nil {
		m.logger.WithContext(ctx).WithError(err).WithField("user", name).Error("Failed to delete database user")
		return fmt.Errorf("failed to delete database user: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "delete_database_user",
		"user":      name,
	}).Info("Deleted Cloud SQL user")

	return nil
}

// DeleteSecret deletes a Kubernetes secret
func (m *Manager) DeleteSecret(ctx context.Context, name string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.config.Unleash.InstanceNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
	}

	if err := m.kubeClient.Delete(ctx, secret); err != nil {
		m.logger.WithContext(ctx).WithError(err).WithField("secret", name).Error("Failed to delete database secret")
		return fmt.Errorf("failed to delete database secret: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "delete_database_secret",
		"secret":    name,
	}).Info("Deleted database credentials secret")

	return nil
}

// generatePassword creates a random password of the specified length
func generatePassword(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(bytes), nil
}
