package resources_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/testutil/kubetest"
)

var k8s *kubetest.Env

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, f := range []string{"incidentprofiles.yaml", "themisconfigs.yaml"} {
		data, err := os.ReadFile(filepath.Join("../../deploy/helm/themis/crds", f)) //nolint:gosec // fixed paths
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		crds = append(crds, &crd)
	}
	env, stop, err := kubetest.Start(crds...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer stop()
	k8s = env
	return m.Run()
}
