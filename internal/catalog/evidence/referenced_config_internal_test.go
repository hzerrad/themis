package evidence

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func entry(at time.Time, fieldsType, fields string) metav1.ManagedFieldsEntry {
	t := metav1.NewTime(at)
	e := metav1.ManagedFieldsEntry{Manager: "m", Operation: metav1.ManagedFieldsOperationUpdate, Time: &t, FieldsType: fieldsType}
	if fields != "" {
		e.FieldsV1 = metav1.NewFieldsV1(fields)
	}
	return e
}

// envtest's API server rebuilds managedFields, so the entries it can't make
// are tested here.
func TestLastWrite(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	const (
		data     = `{"f:data":{".":{},"f:key":{}}}`
		binary   = `{"f:binaryData":{".":{},"f:key":{}}}`
		metadata = `{"f:metadata":{"f:labels":{".":{},"f:team":{}}}}`
	)
	untimed := entry(t0, "FieldsV1", data)
	untimed.Time = nil
	tests := []struct {
		name    string
		entries []metav1.ManagedFieldsEntry
		want    time.Time
	}{
		{"metadata after data", []metav1.ManagedFieldsEntry{entry(t0, "FieldsV1", data), entry(t1, "FieldsV1", metadata)}, t0},
		{"data", []metav1.ManagedFieldsEntry{entry(t0, "FieldsV1", metadata), entry(t1, "FieldsV1", data)}, t1},
		{"binary data", []metav1.ManagedFieldsEntry{entry(t0, "FieldsV1", data), entry(t1, "FieldsV1", binary)}, t1},
		{"no fields", []metav1.ManagedFieldsEntry{entry(t0, "FieldsV1", data), entry(t1, "FieldsV1", "")}, t1},
		{"undecodable fields", []metav1.ManagedFieldsEntry{entry(t0, "FieldsV1", data), entry(t1, "FieldsV1", "not json")}, t1},
		{"unknown fields type", []metav1.ManagedFieldsEntry{entry(t0, "FieldsV1", data), entry(t1, "FieldsV2", metadata)}, t1},
		{"metadata only", []metav1.ManagedFieldsEntry{entry(t0, "FieldsV1", metadata)}, time.Time{}},
		{"no time", []metav1.ManagedFieldsEntry{untimed}, time.Time{}},
		{"none kept", nil, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{ManagedFields: tt.entries}}
			if got := lastWrite(cm); !got.Equal(tt.want) {
				t.Errorf("lastWrite = %v, want %v", got, tt.want)
			}
		})
	}
}
