/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"io"
	"math"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// parseClientRateLimits parses args with only the client rate-limit flags
// registered.
func parseClientRateLimits(t *testing.T, args ...string) (*clientRateLimits, error) {
	t.Helper()
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	l := bindClientRateLimitFlags(fs)
	return l, fs.Parse(args)
}

func TestClientRateLimitFlags_DefaultsAre20And40(t *testing.T) {
	l, err := parseClientRateLimits(t)
	require.NoError(t, err)
	assert.Equal(t, clientRateLimits{QPS: 20, Burst: 40}, *l, "omitting the flags keeps the manager's historical limits")
	require.NoError(t, l.validate())

	cfg := &rest.Config{}
	l.apply(cfg)
	assert.Equal(t, float32(20), cfg.QPS)
	assert.Equal(t, 40, cfg.Burst)
	assert.Nil(t, cfg.RateLimiter, "no shared limiter: each REST client keeps its own token bucket")
}

func TestClientRateLimitFlags_Explicit(t *testing.T) {
	l, err := parseClientRateLimits(t, "--client-qps=100", "--client-burst=40")
	require.NoError(t, err)
	assert.Equal(t, clientRateLimits{QPS: 100, Burst: 40}, *l)
	require.NoError(t, l.validate())

	l, err = parseClientRateLimits(t, "--client-qps=2.5")
	require.NoError(t, err)
	assert.Equal(t, clientRateLimits{QPS: 2.5, Burst: 40}, *l, "a fractional QPS is accepted; the burst keeps its default")
	require.NoError(t, l.validate())
}

func TestClientRateLimits_InvalidRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		limits clientRateLimits
		flag   string
	}{
		"zero QPS":            {clientRateLimits{QPS: 0, Burst: 40}, "--client-qps"},
		"negative QPS":        {clientRateLimits{QPS: -1, Burst: 40}, "--client-qps"},
		"NaN QPS":             {clientRateLimits{QPS: math.NaN(), Burst: 40}, "--client-qps"},
		"infinite QPS":        {clientRateLimits{QPS: math.Inf(1), Burst: 40}, "--client-qps"},
		"QPS beyond float32":  {clientRateLimits{QPS: 1e39, Burst: 40}, "--client-qps"},
		"QPS zero in float32": {clientRateLimits{QPS: 1e-50, Burst: 40}, "--client-qps"},
		"zero burst":          {clientRateLimits{QPS: 20, Burst: 0}, "--client-burst"},
		"negative burst":      {clientRateLimits{QPS: 20, Burst: -1}, "--client-burst"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.limits.validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid "+tc.flag)
		})
	}

	_, err := parseClientRateLimits(t, "--client-burst=1.5")
	assert.Error(t, err, "the burst is a whole number")
	_, err = parseClientRateLimits(t, "--client-qps=lots")
	assert.Error(t, err)
}

// TestClientRateLimits_ReachTheRESTClients builds REST clients the way
// controller-runtime does for each API resource type (apiutil.RESTClientForGVK)
// from a config the limits were applied to: each gets its own token bucket at
// the configured QPS, as with the previous hard-coded values.
func TestClientRateLimits_ReachTheRESTClients(t *testing.T) {
	cfg := &rest.Config{Host: "https://kube-apiserver.invalid"}
	clientRateLimits{QPS: 100, Burst: 40}.apply(cfg)
	httpClient, err := rest.HTTPClientFor(cfg)
	require.NoError(t, err)
	codecs := serializer.NewCodecFactory(scheme)

	var limiters []any
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "infra.virtrigaud.io", Version: "v1beta1", Kind: "VirtualMachine"},
		{Group: "infra.virtrigaud.io", Version: "v1beta1", Kind: "Host"},
	} {
		rc, err := apiutil.RESTClientForGVK(gvk, false, cfg, codecs, httpClient)
		require.NoError(t, err, gvk.Kind)
		rl := rc.GetRateLimiter()
		require.NotNil(t, rl, gvk.Kind)
		assert.InDelta(t, 100.0, float64(rl.QPS()), 1e-6, "%s client's QPS", gvk.Kind)
		limiters = append(limiters, rl)
	}
	assert.NotSame(t, limiters[0], limiters[1], "each REST client has its own token bucket")
}

const managerSubprocessArgEnv = "VIRTRIGAUD_TEST_MANAGER_ARG"

// TestClientRateLimitFlags_InvalidRefusedAtStartup: a non-positive limit
// stops the manager at once (exit 1, before any cluster connection) with an
// error that names the flag.
func TestClientRateLimitFlags_InvalidRefusedAtStartup(t *testing.T) {
	if os.Getenv(managerSubprocessEnv) == "1" {
		os.Args = []string{"manager", os.Getenv(managerSubprocessArgEnv)}
		main()
		return
	}
	for arg, want := range map[string]string{
		"--client-qps=0":    "invalid --client-qps",
		"--client-qps=-5":   "invalid --client-qps",
		"--client-burst=0":  "invalid --client-burst",
		"--client-burst=-1": "invalid --client-burst",
	} {
		t.Run(arg, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestClientRateLimitFlags_InvalidRefusedAtStartup$") // #nosec G204 -- re-runs this test binary
			// No usable kubeconfig: should the value be accepted by mistake,
			// main() fails to find a cluster instead of reaching a real one.
			cmd.Env = append(os.Environ(), managerSubprocessEnv+"=1", managerSubprocessArgEnv+"="+arg,
				"KUBECONFIG="+t.TempDir()+"/none", "KUBERNETES_SERVICE_HOST=", "HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "the manager must exit: %s", out)
			assert.Equal(t, 1, exitErr.ExitCode())
			assert.Contains(t, string(out), want)
			assert.Contains(t, string(out), "invalid Kubernetes API client rate limits")
		})
	}
}
