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

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infravirtrigaudiov1beta1 "github.com/projectbeskar/virtrigaud/api/infra.virtrigaud.io/v1beta1"
	"github.com/projectbeskar/virtrigaud/internal/k8s"
	"github.com/projectbeskar/virtrigaud/internal/operation"
	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
	grpcClient "github.com/projectbeskar/virtrigaud/internal/transport/grpc"
	providerv1 "github.com/projectbeskar/virtrigaud/proto/rpc/provider/v1"
	"github.com/projectbeskar/virtrigaud/test/conformance/clustered/fake"
)

// These specs run the operation-identity lifecycle end to end in a
// development build: a real API server (envtest), VirtualMachine managers
// started and stopped on it, and the instrumented fake clustered Provider in
// operation-identity mode behind a real TLS gRPC server, verifying every
// grant with the shared verifier. A manager "dies" at an injected crash point
// (its reconcile blocks there until the manager is stopped) and a new one,
// with fresh in-memory state, takes over on the same API server.

// identityEnv is one spec's world.
type identityEnv struct {
	ns       string
	provider *infravirtrigaudiov1beta1.Provider
	target   *fake.Target
	client   contracts.Provider
	enrol    operation.StaticEnrolments
	auth     operation.IntentAuthenticator
	// deliveries counts the signed calls per full method, and opIDs records
	// the op_id of every signed Create delivered.
	deliveries sync.Map
	opIDsMu    sync.Mutex
	opIDs      []string
}

func (e *identityEnv) delivered(method string) int64 {
	if v, ok := e.deliveries.Load(method); ok {
		n, _ := v.(*atomic.Int64)
		return n.Load()
	}
	return 0
}

func (e *identityEnv) createOpIDs() map[string]bool {
	e.opIDsMu.Lock()
	defer e.opIDsMu.Unlock()
	out := map[string]bool{}
	for _, id := range e.opIDs {
		out[id] = true
	}
	return out
}

// countingInterceptor counts the signed calls and records Create op_ids.
func (e *identityEnv) countingInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	if m, ok := req.(interface {
		GetOperationEnvelope() *providerv1.OperationEnvelope
	}); ok && m.GetOperationEnvelope() != nil {
		v, _ := e.deliveries.LoadOrStore(info.FullMethod, &atomic.Int64{})
		n, _ := v.(*atomic.Int64)
		n.Add(1)
		if info.FullMethod == providerv1.Provider_Create_FullMethodName {
			body := &providerv1.GrantBody{}
			if proto.Unmarshal(m.GetOperationEnvelope().GetGrant().GetBody(), body) == nil {
				e.opIDsMu.Lock()
				e.opIDs = append(e.opIDs, hex.EncodeToString(body.GetOpId()))
				e.opIDsMu.Unlock()
			}
		}
	}
	return h(ctx, req)
}

// testTLS makes a CA and a localhost server certificate it signs.
func testTLS() (server *tls.Config, clientCfg *tls.Config) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "identity-test-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	ca, err := x509.ParseCertificate(caDER)
	Expect(err).NotTo(HaveOccurred())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS13}
	clientCfg = &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	return server, clientCfg
}

// newIdentityEnv makes the namespace, a clustered cloudhypervisor Provider
// with one Ready host (host-a), a class, the fake in identity mode (requiring
// it) enrolled with the Provider's uid behind a TLS gRPC server, and an
// operation-identity client signed by a development signer.
func newIdentityEnv(ctx context.Context) *identityEnv {
	e := &identityEnv{}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "op-identity-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	e.ns = ns.Name

	prov := envtestProvider(ns.Name, "ch", nil)
	prov.Spec.Type = infravirtrigaudiov1beta1.ProviderTypeCloudHypervisor
	prov.Spec.Topology = infravirtrigaudiov1beta1.ProviderTopologyCluster
	Expect(k8sClient.Create(ctx, prov)).To(Succeed())
	prov.Status.Runtime = &infravirtrigaudiov1beta1.ProviderRuntimeStatus{Phase: "Running", Endpoint: "provider:9443"}
	Expect(k8sClient.Status().Update(ctx, prov)).To(Succeed())
	e.provider = prov
	Expect(k8sClient.Create(ctx, &infravirtrigaudiov1beta1.HostPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: ns.Name},
		Spec: infravirtrigaudiov1beta1.HostPoolSpec{ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: prov.Name},
			Strategy: infravirtrigaudiov1beta1.PoolStrategySpread},
	})).To(Succeed())
	host := &infravirtrigaudiov1beta1.Host{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a", Namespace: ns.Name},
		Spec: infravirtrigaudiov1beta1.HostSpec{ProviderRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: prov.Name},
			PoolRef: infravirtrigaudiov1beta1.LocalObjectReference{Name: "pool"}, Endpoint: "qemu+ssh://virt@host-a/system",
			Schedulable: true},
	}
	Expect(k8sClient.Create(ctx, host)).To(Succeed())
	cpu, mem := int32(16), int64(1<<20)
	host.Status = infravirtrigaudiov1beta1.HostStatus{Health: infravirtrigaudiov1beta1.HostHealthReady, AllocatableCPU: &cpu, AllocatableMemoryMiB: &mem}
	Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	Expect(k8sClient.Create(ctx, envtestClass(ns.Name, "small", nil))).To(Succeed())

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	installID, err := operation.NewRandomID()
	Expect(err).NotTo(HaveOccurred())
	signer, err := operation.NewDevSigner(key, installID, nil)
	Expect(err).NotTo(HaveOccurred())
	target, err := fake.NewIdentity(fake.IdentityConfig{Anchor: signer.Anchor(), Signer: signer, Required: true,
		ProviderUID: string(prov.UID)})
	Expect(err).NotTo(HaveOccurred())
	e.target = target
	e.auth = operation.IntentAuthenticatorOf(signer)
	cfg := target.OperationIdentity()
	e.enrol = operation.StaticEnrolments{Epoch: cfg.Epoch, Hosts: map[string]map[string][]byte{ns.Name + "/" + prov.Name: cfg.Enrolments}}

	serverTLS, clientTLS := testTLS()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.UnaryInterceptor(e.countingInterceptor))
	providerv1.RegisterProviderServer(srv, target.Server())
	go func() { _ = srv.Serve(lis) }()
	DeferCleanup(srv.Stop)
	cli, err := grpcClient.NewOperationIdentityClient(ctx, lis.Addr().String(), string(prov.Spec.Type), prov.Name, nil,
		&grpcClient.TLSConfig{PrebuiltConfig: clientTLS}, signer)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = cli.Close() })
	e.client = ownerFilterClient{Client: cli}

	// Remove this spec's VMs (and their finalizers) once its managers have
	// stopped, so no later spec's manager reconciles them.
	DeferCleanup(func() {
		var vms infravirtrigaudiov1beta1.VirtualMachineList
		Expect(k8sClient.List(ctx, &vms, client.InNamespace(e.ns))).To(Succeed())
		for i := range vms.Items {
			key := client.ObjectKeyFromObject(&vms.Items[i])
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				vm := &infravirtrigaudiov1beta1.VirtualMachine{}
				if err := k8sClient.Get(ctx, key, vm); err != nil {
					return client.IgnoreNotFound(err)
				}
				vm.Finalizers = nil
				return k8sClient.Update(ctx, vm)
			})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &vms.Items[i]))).To(Succeed())
		}
	})
	return e
}

// identityResolver hands every Provider the env's operation-identity client.
type identityResolver struct{ p contracts.Provider }

// ownerFilterClient is the operation-identity client with the owner-filtered
// listing the clustered pre-schedule check needs, which the instrumented fake
// does not implement: it reports no VM for any owner. Everything else,
// including every signed operation, goes to the fake over gRPC.
type ownerFilterClient struct{ *grpcClient.Client }

func (c ownerFilterClient) GetCapabilities(ctx context.Context) (contracts.Capabilities, error) {
	caps, err := c.Client.GetCapabilities(ctx)
	caps.SupportsListOwnerFilter = true
	return caps, err
}

func (c ownerFilterClient) ListVMsForOwner(context.Context, contracts.OwnerFilter) (contracts.VMList, error) {
	return contracts.VMList{OwnerFilterApplied: true}, nil
}

func (r identityResolver) GetProvider(context.Context, *infravirtrigaudiov1beta1.Provider) (contracts.Provider, error) {
	return r.p, nil
}

// crashAt is a crash hook that fires once at point: it signals crashed and
// blocks until the manager is stopped.
func crashAt(point operationCrashPoint, kind contracts.OperationKind, crashed chan struct{}) func(context.Context, operationCrashPoint) bool {
	var fired atomic.Bool
	_ = kind
	return func(ctx context.Context, p operationCrashPoint) bool {
		if p != point || !fired.CompareAndSwap(false, true) {
			return false
		}
		close(crashed)
		<-ctx.Done()
		return true
	}
}

// startManager starts a VirtualMachine manager on the envtest API server with
// a fresh reconciler (and hook), and returns its stop.
func (e *identityEnv) startManager(ctx context.Context, hook func(context.Context, operationCrashPoint) bool) (stop func()) {
	mgrCtx, cancel := context.WithCancel(ctx)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: k8sClient.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	Expect(err).NotTo(HaveOccurred())
	Expect((&VirtualMachineReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(),
		RemoteResolver: identityResolver{p: e.client}, Recorder: record.NewFakeRecorder(1000),
		OperationEnrolments: e.enrol, OperationIntentAuth: e.auth, operationCrashHook: hook,
	}).SetupWithManager(mgr)).To(Succeed())
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	DeferCleanup(stop)
	return stop
}

func (e *identityEnv) createVM(ctx context.Context, name string) client.ObjectKey {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Spec: infravirtrigaudiov1beta1.VirtualMachineSpec{
			ProviderRef:  infravirtrigaudiov1beta1.ObjectRef{Name: e.provider.Name},
			ClassRef:     infravirtrigaudiov1beta1.ObjectRef{Name: "small"},
			ImportedDisk: &infravirtrigaudiov1beta1.ImportedDiskRef{DiskID: "disk-" + name, Format: "qcow2", Source: "manual"},
			PowerState:   infravirtrigaudiov1beta1.PowerStateOff,
		},
	}
	Expect(k8sClient.Create(ctx, vm)).To(Succeed())
	return client.ObjectKeyFromObject(vm)
}

func getIdentityVM(ctx context.Context, g Gomega, key client.ObjectKey) *infravirtrigaudiov1beta1.VirtualMachine {
	vm := &infravirtrigaudiov1beta1.VirtualMachine{}
	g.Expect(k8sClient.Get(ctx, key, vm)).To(Succeed())
	return vm
}

func (e *identityEnv) intentSecretCount(ctx context.Context, g Gomega) int {
	var list corev1.SecretList
	g.Expect(k8sClient.List(ctx, &list, client.InNamespace(e.ns),
		client.MatchingLabels{operationIntentLabel: operationIntentLabelTrue})).To(Succeed())
	return len(list.Items)
}

// waitCreated waits until the VM's Create attempt is Terminal and Succeeded
// with its id recorded, and its intent Secret gone.
func (e *identityEnv) waitCreated(ctx context.Context, key client.ObjectKey) *infravirtrigaudiov1beta1.VirtualMachine {
	var vm *infravirtrigaudiov1beta1.VirtualMachine
	Eventually(func(g Gomega) {
		vm = getIdentityVM(ctx, g, key)
		a := findAttempt(vm, contracts.OperationKindCreate)
		g.Expect(a).NotTo(BeNil())
		g.Expect(a.Phase).To(Equal(infravirtrigaudiov1beta1.OperationAttemptPhaseTerminal))
		g.Expect(a.Outcome).To(Equal(infravirtrigaudiov1beta1.OperationAttemptOutcomeSucceeded))
		g.Expect(vm.Status.ID).NotTo(BeEmpty())
		g.Expect(vm.Status.Placement.Host).To(Equal("host-a"))
		g.Expect(e.intentSecretCount(ctx, g)).To(BeZero())
	}, "30s", "100ms").Should(Succeed())
	return vm
}

// waitDeleted waits (finishing the fake's tasks) until the VM is gone.
func (e *identityEnv) waitDeleted(ctx context.Context, key client.ObjectKey) {
	Eventually(func(g Gomega) {
		g.Expect(e.target.FinishTasks()).To(Succeed())
		vm := &infravirtrigaudiov1beta1.VirtualMachine{}
		err := k8sClient.Get(ctx, key, vm)
		g.Expect(client.IgnoreNotFound(err)).To(Succeed())
		g.Expect(err).To(HaveOccurred(), "the VM is gone")
		g.Expect(e.intentSecretCount(ctx, g)).To(BeZero())
	}, "30s", "100ms").Should(Succeed())
}

var _ = Describe("Operation identity: attempt markers against the instrumented fake (envtest, development build)", func() {
	ctx := context.Background()

	It("creates and deletes a VM with signed operations, each executed once", func() {
		e := newIdentityEnv(ctx)
		e.startManager(ctx, nil)
		key := e.createVM(ctx, "plain")
		vm := e.waitCreated(ctx, key)
		Expect(bindingIDOf(vm)).To(HaveLen(32))
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_CREATE)).To(Equal(1))

		Expect(k8sClient.Delete(ctx, vm)).To(Succeed())
		e.waitDeleted(ctx, key)
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_DELETE)).To(Equal(1))
		Expect(e.delivered(providerv1.Provider_Delete_FullMethodName)).To(BeNumerically(">=", 1))
	})

	DescribeTable("a manager restarted at a crash point of the Create ends with one Create and a Terminal marker",
		func(point operationCrashPoint) {
			e := newIdentityEnv(ctx)
			crashed := make(chan struct{})
			stop := e.startManager(ctx, crashAt(point, contracts.OperationKindCreate, crashed))
			key := e.createVM(ctx, "crash")
			Eventually(crashed, "30s").Should(BeClosed())
			stop()

			e.startManager(ctx, nil)
			e.waitCreated(ctx, key)
			Consistently(func() int {
				return e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_CREATE)
			}, "1s", "100ms").Should(Equal(1), "exactly one Create was executed")
			Expect(e.createOpIDs()).To(HaveLen(1), "every delivery carried the same op_id")
		},
		Entry("after the marker is Prepared, before the send", crashAfterPrepared),
		Entry("after the send, before its answer is recorded", crashAfterSend),
		Entry("after the marker is Terminal, before the intent Secret is deleted", crashAfterTerminal),
	)

	DescribeTable("a manager restarted at a crash point of the Delete ends with one Delete and the VM released",
		func(point operationCrashPoint) {
			e := newIdentityEnv(ctx)
			stopA := e.startManager(ctx, nil)
			key := e.createVM(ctx, "crash-del")
			vm := e.waitCreated(ctx, key)
			stopA()

			crashed := make(chan struct{})
			stopB := e.startManager(ctx, crashAt(point, contracts.OperationKindDelete, crashed))
			Expect(k8sClient.Delete(ctx, vm)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(e.target.FinishTasks()).To(Succeed())
				g.Expect(crashed).To(BeClosed())
			}, "30s", "100ms").Should(Succeed())
			stopB()

			e.startManager(ctx, nil)
			e.waitDeleted(ctx, key)
			Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_DELETE)).To(Equal(1), "exactly one Delete was executed")
		},
		Entry("after the marker is Prepared, before the send", crashAfterPrepared),
		Entry("after the send, before its answer is recorded", crashAfterSend),
		Entry("after the task's outcome is read, before the marker is Terminal", crashAfterReply),
		Entry("after the marker is Terminal, before the intent Secret is deleted", crashAfterTerminal),
	)

	It("holds a VM whose Create marker was lost and never mints a new op_id", func() {
		e := newIdentityEnv(ctx)
		crashed := make(chan struct{})
		stop := e.startManager(ctx, crashAt(crashAfterPrepared, contracts.OperationKindCreate, crashed))
		key := e.createVM(ctx, "lost")
		Eventually(crashed, "30s").Should(BeClosed())
		stop()

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			vm := getIdentityVM(ctx, Default, key)
			vm.Status.OperationAttempts = nil
			return k8sClient.Status().Update(ctx, vm)
		})).To(Succeed())

		e.startManager(ctx, nil)
		Eventually(func(g Gomega) {
			c := meta.FindStatusCondition(getIdentityVM(ctx, g, key).Status.Conditions, k8s.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(k8s.ReasonOperationStateLost))
		}, "30s", "100ms").Should(Succeed())
		Consistently(func() int64 {
			return e.delivered(providerv1.Provider_Create_FullMethodName)
		}, "1s", "100ms").Should(BeZero(), "nothing is sent")
		Expect(getIdentityVM(ctx, Default, key).Status.OperationAttempts).To(BeEmpty())
	})

	It("holds a Create whose intent Secret was tampered with after Prepared, and sends nothing", func() {
		e := newIdentityEnv(ctx)
		crashed := make(chan struct{})
		stop := e.startManager(ctx, crashAt(crashAfterPrepared, contracts.OperationKindCreate, crashed))
		key := e.createVM(ctx, "tamper")
		Eventually(crashed, "30s").Should(BeClosed())
		stop()

		a := findAttempt(getIdentityVM(ctx, Default, key), contracts.OperationKindCreate)
		Expect(a).NotTo(BeNil())
		By("replacing the immutable intent Secret with one carrying other request bytes (delete and re-create)")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: intentSecretName(a.OpID)}, secret)).To(Succeed())
		forged := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secret.Name, Namespace: secret.Namespace, Labels: secret.Labels,
			Annotations: secret.Annotations, OwnerReferences: secret.OwnerReferences}, Type: secret.Type, Data: map[string][]byte{}}
		for k, v := range secret.Data {
			forged.Data[k] = append([]byte(nil), v...)
		}
		forged.Data[intentKeyRequest] = append(forged.Data[intentKeyRequest], 0x01)
		Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		Expect(k8sClient.Create(ctx, forged)).To(Succeed())

		e.startManager(ctx, nil)
		Eventually(func(g Gomega) {
			c := meta.FindStatusCondition(getIdentityVM(ctx, g, key).Status.Conditions, k8s.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(k8s.ReasonOperationIntentTampered))
		}, "30s", "100ms").Should(Succeed())
		Consistently(func() int64 {
			return e.delivered(providerv1.Provider_Create_FullMethodName)
		}, "1s", "100ms").Should(BeZero(), "nothing is sent")
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_CREATE)).To(BeZero())
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: forged.Name}, &corev1.Secret{})).To(Succeed(),
			"the Secret is kept as evidence")
	})

	It("re-sends a restored in-flight Create from its restored intent Secret, answered from the host's record (one effect)", func() {
		e := newIdentityEnv(ctx)
		crashed := make(chan struct{})
		stop := e.startManager(ctx, crashAt(crashAfterSend, contracts.OperationKindCreate, crashed))
		key := e.createVM(ctx, "restore")
		Eventually(crashed, "30s").Should(BeClosed())
		stop()
		before := getIdentityVM(ctx, Default, key)
		a := findAttempt(before, contracts.OperationKindCreate)
		Expect(a).NotTo(BeNil())
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: e.ns, Name: intentSecretName(a.OpID)}, secret)).To(Succeed())

		stopB := e.startManager(ctx, nil)
		e.waitCreated(ctx, key)
		stopB()

		By("restoring the VM status and the intent Secret as they were before the Create finished (an etcd restore)")
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			vm := getIdentityVM(ctx, Default, key)
			vm.Status = *before.Status.DeepCopy()
			return k8sClient.Status().Update(ctx, vm)
		})).To(Succeed())
		restored := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secret.Name, Namespace: secret.Namespace, Labels: secret.Labels,
			Annotations: secret.Annotations, OwnerReferences: secret.OwnerReferences}, Type: secret.Type, Data: secret.Data}
		Expect(k8sClient.Create(ctx, restored)).To(Succeed())

		e.startManager(ctx, nil)
		e.waitCreated(ctx, key)
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_CREATE)).To(Equal(1), "the host answered from its record")
		Expect(e.createOpIDs()).To(HaveLen(1))
	})

	It("holds a restored in-flight Create whose intent Secret was not restored", func() {
		e := newIdentityEnv(ctx)
		crashed := make(chan struct{})
		stop := e.startManager(ctx, crashAt(crashAfterSend, contracts.OperationKindCreate, crashed))
		key := e.createVM(ctx, "restore-lost")
		Eventually(crashed, "30s").Should(BeClosed())
		stop()
		before := getIdentityVM(ctx, Default, key)

		stopB := e.startManager(ctx, nil)
		e.waitCreated(ctx, key)
		stopB()
		deliveries := e.delivered(providerv1.Provider_Create_FullMethodName)

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			vm := getIdentityVM(ctx, Default, key)
			vm.Status = *before.Status.DeepCopy()
			return k8sClient.Status().Update(ctx, vm)
		})).To(Succeed())

		e.startManager(ctx, nil)
		Eventually(func(g Gomega) {
			c := meta.FindStatusCondition(getIdentityVM(ctx, g, key).Status.Conditions, k8s.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(k8s.ReasonOperationIntentLost))
		}, "30s", "100ms").Should(Succeed())
		Consistently(func() int64 {
			return e.delivered(providerv1.Provider_Create_FullMethodName)
		}, "1s", "100ms").Should(Equal(deliveries), "nothing is re-sent without the exact request")
	})

	It("writes one marker and sends one attempt when 12 reconciles race", func() {
		e := newIdentityEnv(ctx)
		mgrCtx, cancel := context.WithCancel(ctx)
		DeferCleanup(cancel)
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: k8sClient.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Controller: ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(indexConsumerGrants(mgr, &infravirtrigaudiov1beta1.VirtualMachine{}, vmConsumerGrantIndexValues)).To(Succeed())
		Expect(indexPlacementProvider(mgr)).To(Succeed())
		Expect(indexCreateRefusals(mgr)).To(Succeed())
		indexer := placementVMIndexer(mgr)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
		reconciler := func(enrol operation.EnrolmentSource) *VirtualMachineReconciler {
			r := &VirtualMachineReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(),
				RemoteResolver: identityResolver{p: e.client}, Recorder: record.NewFakeRecorder(1000), OperationEnrolments: enrol,
				OperationIntentAuth: e.auth}
			r.placementIndex = indexer
			return r
		}
		key := e.createVM(ctx, "race")
		req := ctrl.Request{NamespacedName: key}

		By("placing the VM with no enrolment yet: its pending host is recorded and nothing is sent")
		first := reconciler(nil)
		Eventually(func(g Gomega) {
			_, err := first.Reconcile(ctx, req)
			g.Expect(err).NotTo(HaveOccurred())
			vm := &infravirtrigaudiov1beta1.VirtualMachine{}
			g.Expect(mgr.GetClient().Get(ctx, key, vm)).To(Succeed())
			g.Expect(pendingHost(vm)).To(Equal("host-a"))
			c := meta.FindStatusCondition(vm.Status.Conditions, k8s.ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(k8s.ReasonOperationEnrolmentUnavailable))
		}, "30s", "100ms").Should(Succeed())
		Expect(e.delivered(providerv1.Provider_Create_FullMethodName)).To(BeZero())

		By("12 managers' reconciles at once")
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				r := reconciler(e.enrol)
				<-start
				_, _ = r.Reconcile(ctx, req)
			}()
		}
		close(start)
		wg.Wait()

		Eventually(func(g Gomega) {
			vm := getIdentityVM(ctx, g, key)
			g.Expect(vm.Status.OperationAttempts).To(HaveLen(1))
			g.Expect(vm.Status.OperationAttempts[0].Attempt).To(Equal(int64(1)))
		}, "10s", "100ms").Should(Succeed())
		Expect(e.createOpIDs()).To(HaveLen(1), "one attempt (one op_id) was sent")
		Expect(e.target.AcceptedOperations(providerv1.OperationKind_OPERATION_KIND_CREATE)).To(Equal(1))
		Eventually(func(g Gomega) {
			g.Expect(e.intentSecretCount(ctx, g)).To(BeNumerically("<=", 1), "the racing reconciles' unused intent Secrets are removed")
		}, "10s", "100ms").Should(Succeed())
	})
})
