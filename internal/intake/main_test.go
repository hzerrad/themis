package intake_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/hzerrad/themis/internal/testutil/kubetest"
)

// k8s talks to a real API server started once for the package, for the
// worker's target resolution. Each test works in its own namespace.
var k8s *kubetest.Env

func TestMain(m *testing.M) {
	env, stop, err := kubetest.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start envtest (run `make envtest` first):", err)
		os.Exit(1)
	}
	k8s = env
	code := m.Run()
	stop()
	os.Exit(code)
}
