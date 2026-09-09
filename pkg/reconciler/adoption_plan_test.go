package reconciler

import (
	"encoding/json"
	"strings"
	"testing"

	unleashv1 "github.com/nais/unleasherator/api/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestPlanLegacyAdoption_IsPureAndRecordsCanonicalizedFields(t *testing.T) {
	crd := legacyInstance(t, "team-plan")
	crd.Spec.Size = 3
	for i := range crd.Spec.ExtraEnvVars {
		if crd.Spec.ExtraEnvVars[i].Name == "OAUTH_JWT_AUTH" {
			crd.Spec.ExtraEnvVars[i].Value = "false"
		}
	}
	before, err := json.Marshal(crd)
	if err != nil {
		t.Fatal(err)
	}

	first, err := planLegacyAdoption(crd, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := planLegacyAdoption(crd, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(crd)
	if err != nil {
		t.Fatal(err)
	}

	if string(before) != string(after) {
		t.Fatal("planning mutated the source CR")
	}
	if first.targetSpecSHA256 != second.targetSpecSHA256 || first.targetIntent != second.targetIntent {
		t.Fatal("planning the same source twice produced different canonical targets")
	}
	if got := strings.Join(first.canonicalized, ","); got != "spec.ExtraEnvVars,spec.Size" {
		t.Errorf("canonicalized fields = %q", got)
	}
}

func TestPlanLegacyAdoption_RefusesUnsupportedManualShapes(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*unleashv1.Unleash)
		want string
	}{
		{
			name: "extra volume",
			mut: func(crd *unleashv1.Unleash) {
				crd.Spec.ExtraVolumes = []corev1.Volume{{Name: "manual"}}
			},
			want: "extraVolumes",
		},
		{
			name: "unknown environment variable",
			mut: func(crd *unleashv1.Unleash) {
				crd.Spec.ExtraEnvVars = append(crd.Spec.ExtraEnvVars, corev1.EnvVar{Name: "MANUAL_OVERRIDE", Value: "true"})
			},
			want: "MANUAL_OVERRIDE",
		},
		{
			name: "database identity",
			mut: func(crd *unleashv1.Unleash) {
				crd.Spec.Database.SecretName = "different-database"
			},
			want: "database",
		},
		{
			name: "ingress extension",
			mut: func(crd *unleashv1.Unleash) {
				crd.Spec.WebIngress.Annotations = map[string]string{"manual": "true"}
			},
			want: "ingress",
		},
		{
			name: "missing nonce",
			mut: func(crd *unleashv1.Unleash) {
				crd.Spec.Federation.SecretNonce = ""
			},
			want: "secretNonce",
		},
		{
			name: "ambiguous custom image",
			mut: func(crd *unleashv1.Unleash) {
				crd.Spec.CustomImage = "unleash:7.0.0:manual"
			},
			want: "unambiguous image tag",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			crd := legacyInstance(t, "team-"+strings.ReplaceAll(test.name, " ", "-"))
			test.mut(crd)

			_, err := planLegacyAdoption(crd, testConfig())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("plan error = %v, want actionable refusal naming %q", err, test.want)
			}
		})
	}
}
