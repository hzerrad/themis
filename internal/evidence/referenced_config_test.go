package evidence_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
)

// withConfig adds references to the target's pod template and returns the view.
func withConfig(t *testing.T, target incident.TargetRef, edit func(*corev1.PodSpec)) evidence.ReferencedConfigView {
	t.Helper()
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &d); err != nil {
		t.Fatal(err)
	}
	edit(&d.Spec.Template.Spec)
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
	item, err := evidence.NewReferencedConfig().Collect(t.Context(), k8s, target, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var v evidence.ReferencedConfigView
	if err := json.Unmarshal(item.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func configMap(t *testing.T, ns, name, value string) {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string]string{"key": value}}
	if err := k8s.Client.Create(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
}

func TestReferencedConfig_ListsEveryKindOfReferenceOnce(t *testing.T) {
	target := loadScenario1(t)
	optional := true
	v := withConfig(t, target, func(s *corev1.PodSpec) {
		s.Volumes = append(s.Volumes,
			corev1.Volume{Name: "a", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "from-volume"}}}},
			corev1.Volume{Name: "b", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "tls", Optional: &optional}}},
			corev1.Volume{Name: "c", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "projected"}}},
			}}}},
		)
		c := &s.Containers[0]
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "from-volume"}}})
		c.Env = append(c.Env, corev1.EnvVar{Name: "KEY", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "from-env"}, Key: "key"}}})
	})
	var got []string
	for _, r := range v.Refs {
		got = append(got, r.Kind+"/"+r.Name)
	}
	if want := "ConfigMap/from-env ConfigMap/from-volume ConfigMap/projected Secret/tls"; strings.Join(got, " ") != want {
		t.Errorf("refs = %v, want %s", got, want)
	}
	if !v.Refs[3].Optional || v.Refs[1].Optional {
		t.Errorf("optional flags are wrong: %+v", v.Refs)
	}
}

func envFrom(name string) func(*corev1.PodSpec) {
	return func(s *corev1.PodSpec) {
		s.Containers[0].EnvFrom = append(s.Containers[0].EnvFrom, corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}})
	}
}

func TestReferencedConfig_SaysWhenAConfigMapChangedAndNeverWhatItHolds(t *testing.T) {
	target := loadScenario1(t)
	configMap(t, target.Namespace, "settings", "s3cret-value")
	v := withConfig(t, target, envFrom("settings"))
	if len(v.Refs) != 1 || v.Refs[0].ChangedAt.IsZero() || time.Since(v.Refs[0].ChangedAt) > time.Minute {
		t.Errorf("refs = %+v, want settings changed just now", v.Refs)
	}
	item, err := evidence.NewReferencedConfig().Collect(t.Context(), k8s, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(item.Data), "s3cret-value") {
		t.Errorf("the item holds a value: %s", item.Data)
	}
}

func TestReferencedConfig_MissingConfigMap(t *testing.T) {
	v := withConfig(t, loadScenario1(t), envFrom("never-created"))
	if len(v.Refs) != 1 || !v.Refs[0].Missing {
		t.Errorf("refs = %+v, want one missing", v.Refs)
	}
}

// A Secret is listed by name only, even when it exists: Themis has no
// permission to read one, and must not ask.
func TestReferencedConfig_NeverReadsASecret(t *testing.T) {
	target := loadScenario1(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: target.Namespace, Name: "db"}}
	if err := k8s.Client.Create(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	v := withConfig(t, target, func(s *corev1.PodSpec) {
		s.Containers[0].EnvFrom = append(s.Containers[0].EnvFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}}})
	})
	if len(v.Refs) != 1 || !v.Refs[0].ChangedAt.IsZero() || v.Refs[0].Missing {
		t.Errorf("refs = %+v, want the name alone", v.Refs)
	}
}
