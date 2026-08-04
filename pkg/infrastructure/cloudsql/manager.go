package cloudsql

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/googleapi"
	admin "google.golang.org/api/sqladmin/v1beta4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// operationPollInterval is how often we poll a Cloud SQL long-running operation.
	operationPollInterval = 2 * time.Second
	// operationWaitTimeout bounds how long we wait for a single Cloud SQL operation
	// to reach DONE, so a stuck operation cannot block a caller indefinitely.
	operationWaitTimeout = 5 * time.Minute
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

// OperationsService interface for polling Cloud SQL long-running operations.
type OperationsService interface {
	Get(project string, operation string) *admin.OperationsGetCall
}

// Manager handles Cloud SQL database and user lifecycle
type Manager struct {
	databasesClient  DatabasesService
	usersClient      UsersService
	operationsClient OperationsService
	kubeClient       ctrl.Client
	config           *config.Config
	logger           *logrus.Logger
}

// NewManager creates a new Cloud SQL Manager
func NewManager(databasesClient DatabasesService, usersClient UsersService, operationsClient OperationsService, kubeClient ctrl.Client, config *config.Config, logger *logrus.Logger) *Manager {
	return &Manager{
		databasesClient:  databasesClient,
		usersClient:      usersClient,
		operationsClient: operationsClient,
		kubeClient:       kubeClient,
		config:           config,
		logger:           logger,
	}
}

// isAlreadyExists reports whether a Cloud SQL API error indicates the resource
// already exists (HTTP 409), so callers can treat create as idempotent.
func isAlreadyExists(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusConflict
}

// isNotFound reports whether a Cloud SQL API error indicates the resource does
// not exist (HTTP 404), so callers can treat delete as idempotent.
func isNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

// waitForOperation polls a Cloud SQL long-running operation until it reaches
// DONE (or the operation/context deadline is hit). Cloud SQL Insert/Delete calls
// return immediately with an in-flight operation; without awaiting it, dependent
// steps race the operation (e.g. a user drop firing before its database drop
// completes, or a pod connecting before the database exists).
func (m *Manager) waitForOperation(ctx context.Context, op *admin.Operation) error {
	if op == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, operationWaitTimeout)
	defer cancel()

	for {
		if op.Status == "DONE" {
			if op.Error != nil && len(op.Error.Errors) > 0 {
				return fmt.Errorf("cloud sql operation %s failed: %s", op.Name, op.Error.Errors[0].Message)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for cloud sql operation %s: %w", op.Name, ctx.Err())
		case <-time.After(operationPollInterval):
		}

		refreshed, err := m.operationsClient.Get(m.config.Google.ProjectID, op.Name).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("failed to poll cloud sql operation %s: %w", op.Name, err)
		}
		op = refreshed
	}
}

// CreateDatabase creates a new Cloud SQL database. It is idempotent: an existing
// database is treated as success, and the create operation is awaited to
// completion so callers can rely on the database actually existing on return.
func (m *Manager) CreateDatabase(ctx context.Context, name string) error {
	database := &admin.Database{
		Name: name,
	}

	op, err := m.databasesClient.Insert(m.config.Google.ProjectID, m.config.Unleash.SQLInstanceID, database).Context(ctx).Do()
	if err != nil {
		if isAlreadyExists(err) {
			m.logger.WithContext(ctx).WithField("database", name).Info("Cloud SQL database already exists, treating as created")
			return nil
		}
		m.logger.WithContext(ctx).WithError(err).WithField("database", name).Error("Failed to create database")
		return fmt.Errorf("failed to create database: %w", err)
	}

	if err := m.waitForOperation(ctx, op); err != nil {
		return fmt.Errorf("failed to create database: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "create_database",
		"database":  name,
	}).Info("Created Cloud SQL database")

	return nil
}

// CreateDatabaseUser creates a new Cloud SQL user with a random password and
// awaits the create operation.
func (m *Manager) CreateDatabaseUser(ctx context.Context, name string) (string, error) {
	password, err := generatePassword(16)
	if err != nil {
		return "", fmt.Errorf("failed to generate password: %w", err)
	}

	user := &admin.User{
		Name:     name,
		Password: password,
	}

	op, err := m.usersClient.Insert(m.config.Google.ProjectID, m.config.Unleash.SQLInstanceID, user).Context(ctx).Do()
	if err != nil {
		m.logger.WithContext(ctx).WithError(err).WithField("user", name).Error("Failed to create database user")
		return "", fmt.Errorf("failed to create database user: %w", err)
	}

	if err := m.waitForOperation(ctx, op); err != nil {
		return "", fmt.Errorf("failed to create database user: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "create_database_user",
		"user":      name,
	}).Info("Created Cloud SQL user")

	return password, nil
}

// CreateSecret creates (or updates) a Kubernetes secret with database
// credentials. It is idempotent so that a retry after a partial provisioning
// failure converges the secret to the current password instead of failing with
// AlreadyExists.
func (m *Manager) CreateSecret(ctx context.Context, databaseName, password string) error {
	data := map[string][]byte{
		"POSTGRES_USER":     []byte(databaseName),
		"POSTGRES_PASSWORD": []byte(password),
		"POSTGRES_DB":       []byte(databaseName),
		"POSTGRES_HOST":     []byte(m.config.Unleash.SQLInstanceAddress),
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      databaseName,
			Namespace: m.config.Unleash.InstanceNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		Data: data,
	}

	if err := m.kubeClient.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := &corev1.Secret{}
			if getErr := m.kubeClient.Get(ctx, ctrl.ObjectKeyFromObject(secret), existing); getErr != nil {
				return fmt.Errorf("failed to get existing database secret: %w", getErr)
			}
			existing.Data = data
			if updateErr := m.kubeClient.Update(ctx, existing); updateErr != nil {
				return fmt.Errorf("failed to update database secret: %w", updateErr)
			}
			m.logger.WithContext(ctx).WithField("database", databaseName).Info("Updated existing database credentials secret")
			return nil
		}
		m.logger.WithContext(ctx).WithError(err).WithField("database", databaseName).Error("Failed to create database secret")
		return fmt.Errorf("failed to create database secret: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "create_database_secret",
		"database":  databaseName,
	}).Info("Created database credentials secret")

	return nil
}

// DeleteDatabase deletes a Cloud SQL database. It is idempotent (a missing
// database is treated as success) and awaits the delete operation so a
// subsequent user delete does not race the still-running database drop.
func (m *Manager) DeleteDatabase(ctx context.Context, name string) error {
	op, err := m.databasesClient.Delete(m.config.Google.ProjectID, m.config.Unleash.SQLInstanceID, name).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			m.logger.WithContext(ctx).WithField("database", name).Info("Cloud SQL database already absent, nothing to delete")
			return nil
		}
		m.logger.WithContext(ctx).WithError(err).WithField("database", name).Error("Failed to delete database")
		return fmt.Errorf("failed to delete database: %w", err)
	}

	if err := m.waitForOperation(ctx, op); err != nil {
		return fmt.Errorf("failed to delete database: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "delete_database",
		"database":  name,
	}).Info("Deleted Cloud SQL database")

	return nil
}

// DeleteDatabaseUser deletes a Cloud SQL user. It is idempotent and awaits the
// delete operation.
func (m *Manager) DeleteDatabaseUser(ctx context.Context, name string) error {
	op, err := m.usersClient.Delete(m.config.Google.ProjectID, m.config.Unleash.SQLInstanceID).Name(name).Context(ctx).Do()
	if err != nil {
		if isNotFound(err) {
			m.logger.WithContext(ctx).WithField("user", name).Info("Cloud SQL user already absent, nothing to delete")
			return nil
		}
		m.logger.WithContext(ctx).WithError(err).WithField("user", name).Error("Failed to delete database user")
		return fmt.Errorf("failed to delete database user: %w", err)
	}

	if err := m.waitForOperation(ctx, op); err != nil {
		return fmt.Errorf("failed to delete database user: %w", err)
	}

	m.logger.WithContext(ctx).WithFields(logrus.Fields{
		"operation": "delete_database_user",
		"user":      name,
	}).Info("Deleted Cloud SQL user")

	return nil
}

// DeleteSecret deletes a Kubernetes secret. A missing secret is treated as
// success so delete/cleanup is idempotent.
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
		if apierrors.IsNotFound(err) {
			return nil
		}
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
