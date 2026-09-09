package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nais/bifrost/pkg/config"
	"github.com/nais/bifrost/pkg/infrastructure/kubernetes"
	unleashv1 "github.com/nais/unleasherator/api/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func legacyInstance(t *testing.T, name string) *unleashv1.Unleash {
	t.Helper()
	rendered := renderManaged(t, name)
	legacy := rendered.DeepCopy()
	delete(legacy.Labels, kubernetes.LabelManagedBy)
	delete(legacy.Annotations, kubernetes.AnnotationDesiredState)
	legacy.Labels[kubernetes.LabelAdopt] = kubernetes.AdoptOptIn
	legacy.UID = types.UID(name + "-uid")
	legacy.Generation = 1
	return legacy
}

func fullAdopter(c client.Client, cfg *config.Config, dryRun bool) *UnleashReconciler {
	logger := logrus.New()
	logger.SetOutput(nopWriter{})
	return NewUnleashReconciler(c, cfg, logger, time.Minute, dryRun)
}

func get(t *testing.T, c client.Client, namespace, name string) *unleashv1.Unleash {
	t.Helper()
	crd := &unleashv1.Unleash{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, crd); err != nil {
		t.Fatal(err)
	}
	return crd
}

func newFakeClientWith(t *testing.T, functions interceptor.Funcs, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := addSchemeForTest(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(functions).Build()
}

func getCheckpoint(t *testing.T, c client.Client, namespace string) (*corev1.ConfigMap, *adoptionCheckpoint) {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: adoptionCheckpointName}, configMap); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := checkpointFromConfigMap(configMap)
	if err != nil {
		t.Fatal(err)
	}
	return configMap, checkpoint
}

func setCurrentCondition(crd *unleashv1.Unleash, condition string, status metav1.ConditionStatus) {
	meta.SetStatusCondition(&crd.Status.Conditions, metav1.Condition{
		Type: condition, Status: status, ObservedGeneration: crd.Generation,
		LastTransitionTime: metav1.Now(),
	})
}

func makeCurrentGenerationHealthy(t *testing.T, c client.Client, crd *unleashv1.Unleash) {
	t.Helper()
	setCurrentCondition(crd, unleashv1.UnleashStatusConditionTypeReconciled, metav1.ConditionTrue)
	setCurrentCondition(crd, unleashv1.UnleashStatusConditionTypeConnected, metav1.ConditionTrue)
	if err := c.Update(context.Background(), crd); err != nil {
		t.Fatal(err)
	}
}

func TestFullAdoption_WritesCanonicalTargetAndDurableCheckpoint(t *testing.T) {
	crd := legacyInstance(t, "team-adopt")
	crd.Spec.Size = 3
	c := newFakeClient(t, crd)

	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	if !kubernetes.IsManagedByBifrost(live) || live.Annotations[kubernetes.AnnotationDesiredState] == "" {
		t.Fatal("full adoption did not atomically establish managed metadata and desired state")
	}
	if live.Spec.Size != 1 {
		t.Errorf("canonical spec was not applied, size = %d", live.Spec.Size)
	}
	configMap, checkpoint := getCheckpoint(t, c, crd.Namespace)
	if checkpoint.Phase != adoptionTargetWritten || checkpoint.ResourceUID != live.UID {
		t.Fatalf("checkpoint = %#v, want target-written checkpoint for %q", checkpoint, live.UID)
	}
	if kubernetes.HashBytes(checkpoint.SourceSpec) != checkpoint.SourceSpecSHA256 {
		t.Fatal("checkpoint did not durably bind its source snapshot to its hash")
	}
	record, present, err := adoptionRecord(live)
	if err != nil || !present || record.Phase != kubernetes.AdoptionPending {
		t.Fatalf("adoption marker = %#v, %v, present=%t", record, err, present)
	}
	if err := checkpointMatchesRecord(checkpoint, configMap, record); err != nil {
		t.Fatalf("CR marker does not bind the ConfigMap checkpoint: %v", err)
	}
	if !kubernetes.MatchesAdoptionTarget(record, live) {
		t.Fatal("CR marker target hashes do not match the canonicalized target")
	}
}

func TestFullAdoption_WaitsForVerificationBeforeNextCandidate(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)
	r := fullAdopter(c, testConfig(), false)

	r.adoptFleet(context.Background())
	r.adoptFleet(context.Background())
	if live := get(t, c, second.Namespace, second.Name); kubernetes.IsManagedByBifrost(live) {
		t.Fatal("adopted a second instance before the first reported current-generation health")
	}

	makeCurrentGenerationHealthy(t, c, get(t, c, first.Namespace, first.Name))
	r.adoptFleet(context.Background()) // marks the checkpoint verified
	if live := get(t, c, second.Namespace, second.Name); kubernetes.IsManagedByBifrost(live) {
		t.Fatal("adopted a second instance in the verification sweep")
	}
	r.adoptFleet(context.Background()) // may now choose the next deterministic candidate
	if live := get(t, c, second.Namespace, second.Name); !kubernetes.IsManagedByBifrost(live) {
		t.Fatal("did not admit the next instance after durable verification")
	}
}

func TestFullAdoption_RejectsStaleReadiness(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)
	r := fullAdopter(c, testConfig(), false)
	r.adoptFleet(context.Background())

	live := get(t, c, first.Namespace, first.Name)
	makeCurrentGenerationHealthy(t, c, live)
	live = get(t, c, first.Namespace, first.Name)
	for i := range live.Status.Conditions {
		live.Status.Conditions[i].ObservedGeneration = live.Generation - 1
	}
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	r.adoptFleet(context.Background())

	if kubernetes.IsManagedByBifrost(get(t, c, second.Namespace, second.Name)) {
		t.Fatal("stale readiness admitted the next instance")
	}
}

func TestFullAdoption_PendingCheckpointSurvivesRestart(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)
	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())
	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	if kubernetes.IsManagedByBifrost(get(t, c, second.Namespace, second.Name)) {
		t.Fatal("a new reconciler bypassed the persisted pending health gate")
	}
}

func TestFullAdoption_RecoversTargetWriteWhenCheckpointPhaseWasNotUpdated(t *testing.T) {
	crd := legacyInstance(t, "team-a")
	c := newFakeClient(t, crd)
	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	configMap, checkpoint := getCheckpoint(t, c, crd.Namespace)
	checkpoint.Phase = adoptionPrepared
	raw, err := marshalAdoptionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	configMap.Data[adoptionCheckpointKey] = raw
	if err := c.Update(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
	live := get(t, c, crd.Namespace, crd.Name)
	live.Generation++
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	_, checkpoint = getCheckpoint(t, c, crd.Namespace)
	if checkpoint.Phase != adoptionTargetWritten {
		t.Fatalf("checkpoint phase = %q, want recovery to %q", checkpoint.Phase, adoptionTargetWritten)
	}
}

func TestFullAdoption_DryRunPlansButDoesNotWrite(t *testing.T) {
	crd := legacyInstance(t, "team-preview")
	crd.Spec.Size = 4
	c := newFakeClient(t, crd)
	before, err := json.Marshal(get(t, c, crd.Namespace, crd.Name))
	if err != nil {
		t.Fatal(err)
	}

	fullAdopter(c, testConfig(), true).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	after, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("dry-run changed the legacy CR")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: crd.Namespace, Name: adoptionCheckpointName}, &corev1.ConfigMap{}); err == nil {
		t.Fatal("dry-run created a durable checkpoint")
	}
}

func TestFullAdoption_RequiresExplicitCRApproval(t *testing.T) {
	crd := legacyInstance(t, "team-unapproved")
	delete(crd.Labels, kubernetes.LabelAdopt)
	c := newFakeClient(t, crd)

	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	if kubernetes.IsManagedByBifrost(get(t, c, crd.Namespace, crd.Name)) {
		t.Fatal("full adoption normalized a CR without explicit approval")
	}
}

func TestFullAdoption_ReportsAListFailureAsBlocked(t *testing.T) {
	crd := legacyInstance(t, "team-list-failure")
	c := newFakeClientWith(t, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return errors.New("Kubernetes API unavailable")
		},
	}, crd)
	before := seriesValue(t, "bifrost_reconciler_adoptions_total", map[string]string{"result": adoptionError})

	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	if got := seriesValue(t, "bifrost_reconciler_adoption_checkpoint_state", map[string]string{"state": adoptionStateBlocked}); got != 1 {
		t.Errorf("checkpoint state blocked = %v, want 1", got)
	}
	if got := seriesValue(t, "bifrost_reconciler_adoptions_total", map[string]string{"result": adoptionError}); got != before+1 {
		t.Errorf("adoption errors = %v, want %v", got, before+1)
	}
}

func TestFullAdoption_RefusesUnknownManualShapes(t *testing.T) {
	crd := legacyInstance(t, "team-manual")
	crd.Spec.PodLabels = map[string]string{"manual": "true"}
	before, err := json.Marshal(crd.Spec)
	if err != nil {
		t.Fatal(err)
	}
	c := newFakeClient(t, crd)

	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	live := get(t, c, crd.Namespace, crd.Name)
	after, err := json.Marshal(live.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || kubernetes.IsManagedByBifrost(live) {
		t.Fatal("unsafe manual shape was changed instead of refused")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: crd.Namespace, Name: adoptionCheckpointName}, &corev1.ConfigMap{}); err == nil {
		t.Fatal("a refused shape created a checkpoint")
	}
}

func TestFullAdoption_SelectsCanonicalizableCandidateDeterministically(t *testing.T) {
	last, first := legacyInstance(t, "team-z"), legacyInstance(t, "team-a")
	c := newFakeClient(t, last, first)

	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	_, checkpoint := getCheckpoint(t, c, first.Namespace)
	if checkpoint.ResourceName != "team-a" {
		t.Errorf("selected %q, want lexicographically first canonicalizable candidate", checkpoint.ResourceName)
	}
}

func TestFullAdoption_CheckpointDeletionBlocksTheFleet(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)
	r := fullAdopter(c, testConfig(), false)
	r.adoptFleet(context.Background())

	configMap, _ := getCheckpoint(t, c, first.Namespace)
	if err := c.Delete(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	if kubernetes.IsManagedByBifrost(get(t, c, second.Namespace, second.Name)) {
		t.Fatal("checkpoint deletion admitted another candidate")
	}
}

func TestAdoptionCheckpoint_RecordRejectsReplacedConfigMapUID(t *testing.T) {
	crd := legacyInstance(t, "team-a")
	plan, err := planLegacyAdoption(crd, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := newAdoptionCheckpoint(plan, crd.Namespace, crd.Name, crd.UID, crd.Generation)
	configMap, err := checkpointConfigMap(crd.Namespace, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	configMap.UID = "replacement-uid"
	record, err := kubernetes.NewAdoptionRecord(crd, configMap.Name, checkpoint.ID, "original-uid", plan.target.Spec, plan.targetIntent)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpointMatchesRecord(checkpoint, configMap, record); err == nil {
		t.Fatal("replacement ConfigMap UID was accepted")
	}
}

func TestFullAdoption_StaleCheckpointDataEntersDurableFailure(t *testing.T) {
	crd := legacyInstance(t, "team-a")
	c := newFakeClient(t, crd)
	r := fullAdopter(c, testConfig(), false)
	r.adoptFleet(context.Background())

	configMap, checkpoint := getCheckpoint(t, c, crd.Namespace)
	checkpoint.ID = "replacement-checkpoint"
	raw, err := marshalAdoptionCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	configMap.Data[adoptionCheckpointKey] = raw
	if err := c.Update(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}

	r.adoptFleet(context.Background())

	_, checkpoint = getCheckpoint(t, c, crd.Namespace)
	if checkpoint.Phase != adoptionFailed || checkpoint.Failure == "" {
		t.Fatalf("checkpoint = %#v, want durable manual recovery state", checkpoint)
	}
}

func TestFullAdoption_SourceDeletionBeforeVerificationEntersDurableFailure(t *testing.T) {
	crd := legacyInstance(t, "team-a")
	c := newFakeClient(t, crd)
	r := fullAdopter(c, testConfig(), false)
	r.adoptFleet(context.Background())
	if err := c.Delete(context.Background(), get(t, c, crd.Namespace, crd.Name)); err != nil {
		t.Fatal(err)
	}

	r.adoptFleet(context.Background())

	_, checkpoint := getCheckpoint(t, c, crd.Namespace)
	if checkpoint.Phase != adoptionFailed {
		t.Fatalf("checkpoint phase = %q, want %q", checkpoint.Phase, adoptionFailed)
	}
}

func TestFullAdoption_BlocksWhenPendingTargetChanges(t *testing.T) {
	first, second := legacyInstance(t, "team-a"), legacyInstance(t, "team-b")
	c := newFakeClient(t, first, second)
	r := fullAdopter(c, testConfig(), false)
	r.adoptFleet(context.Background())

	live := get(t, c, first.Namespace, first.Name)
	live.Annotations[kubernetes.AnnotationDesiredState] = `{"tampered":true}`
	makeCurrentGenerationHealthy(t, c, live)
	r.adoptFleet(context.Background())

	if kubernetes.IsManagedByBifrost(get(t, c, second.Namespace, second.Name)) {
		t.Fatal("changed pending target admitted the next candidate")
	}
	_, checkpoint := getCheckpoint(t, c, first.Namespace)
	if checkpoint.Phase != adoptionFailed {
		t.Fatalf("checkpoint phase = %q, want durable failure", checkpoint.Phase)
	}
}

func TestFullAdoption_YieldsForChannelMigrationTransaction(t *testing.T) {
	crd := legacyInstance(t, "team-a")
	crd.Annotations[kubernetes.AnnotationChannelMigration] = `{"phase":"target-written"}`
	c := newFakeClient(t, crd)

	fullAdopter(c, testConfig(), false).adoptFleet(context.Background())

	if kubernetes.IsManagedByBifrost(get(t, c, crd.Namespace, crd.Name)) {
		t.Fatal("legacy adoption changed a CR while a channel migration transaction was active")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: crd.Namespace, Name: adoptionCheckpointName}, &corev1.ConfigMap{}); err == nil {
		t.Fatal("legacy adoption created a checkpoint while a channel migration transaction was active")
	}
}
