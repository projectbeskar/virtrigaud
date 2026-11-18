//go:build virtrigaud_dev

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

	"github.com/projectbeskar/virtrigaud/internal/controller"
)

// registerDevHostEnrolmentCAKeySecretFlag registers
// --dev-host-enrolment-ca-key-secret on fs. Only a development build (the
// virtrigaud_dev build tag) has the flag; a release build does not define it,
// so setting it stops a release manager at flag parsing.
func registerDevHostEnrolmentCAKeySecretFlag(fs *flag.FlagSet, f *controller.HostEnrolmentFlags) {
	fs.StringVar(&f.CAKeySecret, controller.DevHostEnrolmentCAKeySecretFlagName, "",
		"DEVELOPMENT ONLY. <namespace>/<name> of the Secret holding the host-agent subordinate CA's private key "+
			"(for a ClusterIssuer, in cert-manager's cluster resource namespace). Required with --"+
			controller.DevHostEnrolmentIssuerFlagName+". With an external PKI alone, set it whenever that signer keeps "+
			"its key in this cluster: the manager cannot tell. Per-host agent enrolment refuses to run while the manager "+
			"may read that Secret or Secrets in its namespace, or enter pods there.")
}
