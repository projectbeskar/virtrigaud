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
	"fmt"
	"math"

	"k8s.io/client-go/rest"
)

const (
	// defaultClientQPS is the manager's default client-side Kubernetes API
	// rate limit, in requests per second (the value the manager has always
	// used).
	defaultClientQPS = 20
	// defaultClientBurst is the manager's default client-side Kubernetes API
	// burst (the value the manager has always used).
	defaultClientBurst = 40

	// clientQPSFlag is the name of the flag that sets the client QPS.
	clientQPSFlag = "client-qps"
	// clientBurstFlag is the name of the flag that sets the client burst.
	clientBurstFlag = "client-burst"
)

// clientRateLimits is the manager's client-side Kubernetes API rate limit:
// the QPS and burst of the token bucket client-go puts in front of every
// request a REST client sends. They are startup configuration, not limits
// the API server enforces.
type clientRateLimits struct {
	// QPS is the sustained number of requests per second each REST client
	// may send.
	QPS float64
	// Burst is the number of requests each REST client may send at once
	// before QPS applies.
	Burst int
}

// bindClientRateLimitFlags registers --client-qps and --client-burst on fs,
// with the defaults the manager has always used (20 and 40), and returns the
// limits they set once fs is parsed.
func bindClientRateLimitFlags(fs *flag.FlagSet) *clientRateLimits {
	l := &clientRateLimits{}
	fs.Float64Var(&l.QPS, clientQPSFlag, defaultClientQPS,
		"Client-side rate limit of the manager's Kubernetes API requests, in requests per second: a positive "+
			"number. It applies separately to each REST client the manager builds (controller-runtime builds one per "+
			"API resource type). Not an API-server limit. Raising it can speed up VM creation when the manager "+
			"waits on it, at the cost of more manager CPU and more load on the API server and etcd.")
	fs.IntVar(&l.Burst, clientBurstFlag, defaultClientBurst,
		"Client-side burst of the manager's Kubernetes API requests: a positive whole number of requests each "+
			"REST client may send at once before --"+clientQPSFlag+" applies. Not an API-server limit.")
	return l
}

// validate refuses a QPS or burst that is not positive, and a QPS that is not
// a finite number client-go can hold: client-go would turn a zero QPS into
// its own default (5) and a negative one into no rate limit at all, neither
// of which is what the administrator asked for.
func (l clientRateLimits) validate() error {
	if math.IsNaN(l.QPS) || l.QPS > math.MaxFloat32 || float32(l.QPS) <= 0 {
		return fmt.Errorf("invalid --%s=%v: it must be a positive number of requests per second, such as %d (the default)",
			clientQPSFlag, l.QPS, defaultClientQPS)
	}
	if l.Burst <= 0 {
		return fmt.Errorf("invalid --%s=%d: it must be a positive whole number of requests, such as %d (the default)",
			clientBurstFlag, l.Burst, defaultClientBurst)
	}
	return nil
}

// apply sets cfg's QPS and burst. It leaves cfg.RateLimiter unset, as the
// manager always has, so every REST client built from cfg still gets its own
// token bucket with these limits.
func (l clientRateLimits) apply(cfg *rest.Config) {
	cfg.QPS = float32(l.QPS)
	cfg.Burst = l.Burst
}
