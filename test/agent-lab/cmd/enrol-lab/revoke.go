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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/projectbeskar/virtrigaud/internal/clustered/hostsecret"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
)

// hostGVR is the Host resource.
var hostGVR = schema.GroupVersionResource{Group: "infra.virtrigaud.io", Version: "v1beta1", Resource: "hosts"}

// probeResult is the JSON line revoke-probe prints. Times are milliseconds
// after the revocation command (or write) started; -1 means not observed.
// PatchMS is when it returned (the write is then committed).
type probeResult struct {
	Host           string  `json:"host"`
	PatchMS        float64 `json:"patchReturnedMs"`
	ConditionMS    float64 `json:"conditionRevokedMs"`
	SecretMS       float64 `json:"inventoryWithoutHostMs"`
	RefusedMS      float64 `json:"firstRefusedCallMs"`
	LastOKMS       float64 `json:"lastOkCallMs"`
	Refusal        string  `json:"refusal"`
	ProbeCalls     int     `json:"probeCalls"`
	ProbeIntervalM float64 `json:"probeIntervalMs"`
}

// stamp records the first time an event was seen.
type stamp struct {
	mu sync.Mutex
	t  time.Time
}

// set records now unless already set.
func (s *stamp) set(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.t.IsZero() {
		s.t = now
	}
}

// get returns the recorded time.
func (s *stamp) get() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t
}

// since returns ms from t0, or -1 when unset.
func since(t0, t time.Time) float64 {
	if t.IsZero() {
		return -1
	}
	return float64(t.Sub(t0).Microseconds()) / 1000
}

// runRevokeProbe revokes a Host and measures when it stops being used.
func runRevokeProbe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("revoke-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig of a principal that may patch the Host and read its Provider's inventory Secret")
	ns := fs.String("namespace", "", "Host namespace (the Provider's)")
	host := fs.String("host", "", "Host name")
	secretName := fs.String("inventory-secret", "", "the Provider's host inventory Secret (<provider>-hosts)")
	addr := fs.String("provider", "", "Provider gRPC address host:port")
	certFile := fs.String("tls-cert", "", "client certificate the Provider accepts (PEM)")
	keyFile := fs.String("tls-key", "", "key of -tls-cert (PEM)")
	caFile := fs.String("tls-ca", "", "CA of the Provider's server certificate (PEM)")
	serverName := fs.String("server-name", "", "server name to verify (the Provider Service FQDN)")
	interval := fs.Duration("interval", 10*time.Millisecond, "probe interval")
	revokeCmd := fs.String("revoke-cmd", "", "command (sh -c) that revokes the Host, e.g. vrtg host revoke; default: a merge patch of spec.revoked")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kubeconfig == "" || *ns == "" || *host == "" || *secretName == "" || *addr == "" {
		return errors.New("-kubeconfig, -namespace, -host, -inventory-secret and -provider are required")
	}
	tlsCfg, err := clientTLS(*certFile, *keyFile, *caFile, *serverName)
	if err != nil {
		return err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return fmt.Errorf("load the kubeconfig: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return fmt.Errorf("dial the Provider: %w", err)
	}
	defer func() { _ = conn.Close() }()
	pc := providerv1.NewProviderClient(conn)
	call := func() error {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err := pc.GetHostInfo(cctx, &providerv1.GetHostInfoRequest{HostId: *host})
		return err
	}
	// The host must answer before the revocation.
	for i := 0; ; i++ {
		if err := call(); err == nil {
			break
		} else if i >= 50 {
			return fmt.Errorf("the host does not answer before the revocation: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	var condAt, secretAt, refusedAt, lastOK stamp
	var refusal string
	var calls int
	ready := make(chan struct{}, 2)
	go watchHost(wctx, dyn, *ns, *host, &condAt, ready, stderr)
	go watchSecret(wctx, cs, *ns, *secretName, *host, &secretAt, ready, stderr)
	<-ready
	<-ready

	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(*interval)
		defer t.Stop()
		for {
			err := call()
			now := time.Now()
			calls++
			if err != nil {
				refusedAt.set(now)
				st, _ := status.FromError(err)
				refusal = st.Code().String() + ": " + st.Message()
				return
			}
			lastOK.mu.Lock()
			lastOK.t = now
			lastOK.mu.Unlock()
			select {
			case <-wctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	t0 := time.Now()
	if *revokeCmd != "" {
		// #nosec G204 -- the operator's own revocation command, from the command line.
		out, err := exec.CommandContext(ctx, "sh", "-c", *revokeCmd).CombinedOutput()
		if err != nil {
			return fmt.Errorf("revoke the Host (%s): %w", strings.TrimSpace(string(out)), err)
		}
	} else {
		patch := []byte(`{"spec":{"revoked":true}}`)
		if _, err := dyn.Resource(hostGVR).Namespace(*ns).Patch(ctx, *host, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("revoke the Host: %w", err)
		}
	}
	t1 := time.Now()

	deadline := time.After(3 * time.Minute)
	for condAt.get().IsZero() || secretAt.get().IsZero() || refusedAt.get().IsZero() {
		select {
		case <-deadline:
			wcancel()
			<-done
			return printResult(stdout, probeResult{Host: *host, PatchMS: since(t0, t1), ConditionMS: since(t0, condAt.get()),
				SecretMS: since(t0, secretAt.get()), RefusedMS: since(t0, refusedAt.get()), LastOKMS: since(t0, lastOK.get()),
				Refusal: refusal, ProbeCalls: calls, ProbeIntervalM: float64(interval.Microseconds()) / 1000})
		case <-time.After(5 * time.Millisecond):
		}
	}
	wcancel()
	<-done
	return printResult(stdout, probeResult{Host: *host, PatchMS: since(t0, t1), ConditionMS: since(t0, condAt.get()),
		SecretMS: since(t0, secretAt.get()), RefusedMS: since(t0, refusedAt.get()), LastOKMS: since(t0, lastOK.get()),
		Refusal: refusal, ProbeCalls: calls, ProbeIntervalM: float64(interval.Microseconds()) / 1000})
}

// clientTLS builds the mutual TLS configuration of the Provider connection.
func clientTLS(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" || caFile == "" || serverName == "" {
		return nil, errors.New("-tls-cert, -tls-key, -tls-ca and -server-name are required")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load the client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Clean(caFile))
	if err != nil {
		return nil, fmt.Errorf("read the CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the CA file holds no certificate")
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13}, nil
}

// printResult writes r as one JSON line.
func printResult(w io.Writer, r probeResult) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode the result: %w", err)
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// watchHost records when the Host reports AgentEnrolled=False/Revoked.
func watchHost(ctx context.Context, dyn dynamic.Interface, ns, name string, at *stamp, ready chan<- struct{}, stderr io.Writer) {
	w, err := dyn.Resource(hostGVR).Namespace(ns).Watch(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String()})
	ready <- struct{}{}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "watch the Host:", err)
		return
	}
	defer w.Stop()
	for ev := range w.ResultChan() {
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if ok && m["type"] == "AgentEnrolled" && m["status"] == "False" && m["reason"] == "Revoked" {
				at.set(time.Now())
				return
			}
		}
	}
}

// watchSecret records when the inventory Secret no longer lists the host.
func watchSecret(ctx context.Context, cs kubernetes.Interface, ns, name, host string, at *stamp, ready chan<- struct{}, stderr io.Writer) {
	w, err := cs.CoreV1().Secrets(ns).Watch(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String()})
	ready <- struct{}{}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "watch the inventory Secret:", err)
		return
	}
	defer w.Stop()
	for ev := range w.ResultChan() {
		if ev.Type == watch.Deleted {
			at.set(time.Now())
			return
		}
		s, ok := ev.Object.(*corev1.Secret)
		if !ok {
			continue
		}
		var inv hostsecret.Inventory
		if err := json.Unmarshal(s.Data[hostsecret.SecretDataKey], &inv); err != nil {
			continue
		}
		listed := false
		for _, h := range inv.Hosts {
			if h.ID == host {
				listed = true
			}
		}
		if !listed {
			at.set(time.Now())
			return
		}
	}
}
