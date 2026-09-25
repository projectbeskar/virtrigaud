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

package contracts

import "testing"

// TestImagePrepareResponse_ConfirmsIdentity pins the ADR-0009 D7 echo rule:
// only an artifact whose UID and digest both equal the request's confirms it;
// namespace, name and reused are never consulted.
func TestImagePrepareResponse_ConfirmsIdentity(t *testing.T) {
	const (
		digest      = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		otherDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)
	req := ImagePrepareRequest{
		Image:        ObjectIdentity{UID: "uid-1", Namespace: "team-a", Name: "ubuntu"},
		SourceDigest: digest,
	}
	echo := func(uid, d string) ImagePrepareResponse {
		return ImagePrepareResponse{Artifact: &PreparedArtifact{
			Name: "x", Image: ObjectIdentity{UID: uid, Namespace: "other-ns", Name: "other"}, SourceDigest: d,
		}}
	}

	for name, tc := range map[string]struct {
		req  ImagePrepareRequest
		resp ImagePrepareResponse
		want bool
	}{
		"matching uid and digest (namespace/name ignored)": {req, echo("uid-1", digest), true},
		"no artifact (older provider, legacy mode)":        {req, ImagePrepareResponse{PreparedImageID: "ubuntu"}, false},
		"another uid":    {req, echo("uid-2", digest), false},
		"another digest": {req, echo("uid-1", otherDigest), false},
		"empty echo":     {req, echo("", ""), false},
		"matching uid and digest but no artifact name": {req, ImagePrepareResponse{Artifact: &PreparedArtifact{
			Image: ObjectIdentity{UID: "uid-1"}, SourceDigest: digest}}, false},
		"request without identity (legacy) confirms nothing": {
			ImagePrepareRequest{TargetName: "ubuntu"}, echo("", ""), false},
		"request without digest confirms nothing": {
			ImagePrepareRequest{Image: ObjectIdentity{UID: "uid-1"}}, echo("uid-1", ""), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.resp.ConfirmsIdentity(tc.req); got != tc.want {
				t.Fatalf("ConfirmsIdentity() = %t, want %t", got, tc.want)
			}
		})
	}
}
