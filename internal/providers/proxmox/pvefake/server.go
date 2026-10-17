/*
Copyright 2025.

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

// Package pvefake provides a fake Proxmox VE API server for testing
package pvefake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
)

// Server represents a fake Proxmox VE API server
type Server struct {
	router       *mux.Router
	vms          map[int]*VM
	tasks        map[string]*Task
	snapshots    map[string][]*Snapshot
	lastDownload *DownloadRequest
	lastPowerOp  *PowerOpRequest
	requests     []RecordedRequest
	nextID       int
	mu           sync.RWMutex
	logger       *slog.Logger
	config       *Config

	// faults are the injected request faults (InjectFault), in order.
	faults []*Fault
	// failingTasks are the task types whose tasks finish with a failure
	// (FailTasks).
	failingTasks map[string]bool
	// hidden are the VMIDs /cluster/resources leaves out
	// (HideFromResources).
	hidden map[int]bool
	// offline are the nodes that are down (SetNodeOffline).
	offline map[string]bool
	// emptyResourceNames lists a guest without a name with no name in
	// /cluster/resources, instead of PVE's "VM <vmid>"
	// (SetEmptyResourceNames).
	emptyResourceNames bool
	// errorsInStatusLine sends error messages as the HTTP reason phrase with
	// a {"data":null} body, as real PVE does (ErrorsInStatusLine).
	errorsInStatusLine atomic.Bool
	// nodeNames are the nodes /nodes lists, in order (SetNodes); empty means
	// pve, pve2.
	nodeNames []string
	// taskSeq numbers the UPIDs createTaskAs issues (guarded by mu).
	taskSeq uint32
}

// SetEmptyResourceNames makes /cluster/resources list a guest whose config
// has no name with an empty name (true) or, like PVE, as "VM <vmid>" (false,
// the default). Safe for concurrent use.
func (s *Server) SetEmptyResourceNames(empty bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emptyResourceNames = empty
}

// SetNodeOffline takes node down (or back up): every /nodes/<node>/ call
// fails with HTTP 595, as PVE answers for an unreachable node, and
// /cluster/resources lists its guests with status "unknown". Safe for
// concurrent use.
func (s *Server) SetNodeOffline(node string, offline bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offline[node] = offline
}

// nodeOfflineFor reports whether r addresses a node that is down.
func (s *Server) nodeOfflineFor(r *http.Request) bool {
	const prefix = "/api2/json/nodes/"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		return false
	}
	node, _, _ := strings.Cut(rest, "/")
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.offline[node]
}

// Fault is a failure the fake server injects into the requests that match it
// (InjectFault): a request with Method whose path contains PathContains.
type Fault struct {
	// Method is the HTTP method to match.
	Method string
	// PathContains is a substring the request path must contain.
	PathContains string
	// Status and Body are the error answer.
	Status int
	Body   string
	// AfterApply processes the request first (its effects remain) and then
	// answers the error, as a PVE answer lost after PVE acted would.
	AfterApply bool
	// Raw answers Body as plain text instead of PVE's JSON (e.g. a proxy's
	// error page).
	Raw bool
	// Then runs (without the server lock) when the fault answers, before the
	// answer is written: e.g. to put a VM at the VMID the request names.
	Then func(r *http.Request)
	// Times is how many matching requests fail; 0 means every one.
	Times int
	// PassThrough runs Then and then serves the request normally, answering
	// no error: e.g. to change a VM between a client's read and its write.
	PassThrough bool
}

// InjectFault makes the requests that match f fail (see Fault). Safe for
// concurrent use.
func (s *Server) InjectFault(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc := f
	s.faults = append(s.faults, &fc)
}

// FailTasks makes every later task of taskType (e.g. "qmclone", "qmcreate")
// finish with exit status "ERROR". A failed qmclone or qmcreate task removes
// the VM it was making, as PVE does. Safe for concurrent use.
func (s *Server) FailTasks(taskType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failingTasks[taskType] = true
}

// HideFromResources leaves VM vmid out of /cluster/resources, as PVE
// does for a guest the token may not audit. Safe for concurrent use.
func (s *Server) HideFromResources(vmid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hidden[vmid] = true
}

// SetTaskDelay sets how long a task runs before it reports done. Safe for
// concurrent use.
func (s *Server) SetTaskDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.TaskDelay = d
}

// PutVM adds (or replaces) a VM. Safe for concurrent use.
func (s *Server) PutVM(vm VM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := vm
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	s.vms[v.VMID] = &v
}

// GetVM returns a copy of the VM vmid. Safe for concurrent use.
func (s *Server) GetVM(vmid int) (VM, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vm, ok := s.vms[vmid]
	if !ok {
		return VM{}, false
	}
	return *vm, true
}

// VMsNamed returns copies of the VMs (templates included) named name. Safe
// for concurrent use.
func (s *Server) VMsNamed(name string) []VM {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []VM
	for _, vm := range s.vms {
		if vm.Name == name {
			out = append(out, *vm)
		}
	}
	return out
}

// SetDescription sets the description of VM vmid. Safe for concurrent use.
func (s *Server) SetDescription(vmid int, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vm, ok := s.vms[vmid]; ok {
		vm.Description = description
	}
}

// SetLock sets the config lock of VM vmid ("" clears it). Safe for concurrent
// use.
func (s *Server) SetLock(vmid int, lock string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vm, ok := s.vms[vmid]; ok {
		vm.Lock = lock
	}
}

// matchFault returns the injected fault r triggers, consuming one of its
// Times, or nil.
func (s *Server) matchFault(r *http.Request) *Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.faults {
		if f.Method != r.Method || !strings.Contains(r.URL.Path, f.PathContains) {
			continue
		}
		if f.Times < 0 {
			continue // used up
		}
		out := *f
		if f.Times > 0 {
			f.Times--
			if f.Times == 0 {
				f.Times = -1
			}
		}
		return &out
	}
	return nil
}

// RecordedRequest is one API request the fake server received, recorded before
// authentication and routing, so tests can assert which PVE calls an operation
// made (or that it made none).
type RecordedRequest struct {
	Method string
	Path   string
}

// Requests returns a copy of every API request the fake server has received,
// in arrival order. Safe for concurrent use.
func (s *Server) Requests() []RecordedRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]RecordedRequest(nil), s.requests...)
}

// MutatingRequests returns the received requests whose method can create or
// modify PVE state (anything but GET and HEAD), in arrival order. Safe for
// concurrent use.
func (s *Server) MutatingRequests() []RecordedRequest {
	var out []RecordedRequest
	for _, r := range s.Requests() {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			out = append(out, r)
		}
	}
	return out
}

// DownloadRequest records the parameters of the most recent storage download-url
// request so ImagePrepare tests can assert node/storage/content/filename/url
// propagation.
type DownloadRequest struct {
	Node     string
	Storage  string
	Content  string
	Filename string
	URL      string
}

// LastDownloadRequest returns the most recent storage download-url request seen
// by the fake server, or nil if none has occurred. Safe for concurrent use.
func (s *Server) LastDownloadRequest() *DownloadRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastDownload
}

// PowerOpRequest records the most recent power operation and its parameters so
// tests can assert e.g. graceful-shutdown timeout/forceStop propagation.
type PowerOpRequest struct {
	Operation string
	Timeout   string
	ForceStop string
}

// LastPowerOp returns the most recent power operation seen by the fake server,
// or nil if none has occurred. Safe for concurrent use.
func (s *Server) LastPowerOp() *PowerOpRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPowerOp
}

// Config holds fake server configuration
type Config struct {
	// FailureMode can be "none", "random", "always"
	FailureMode string
	// SlowMode adds delays to operations
	SlowMode bool
	// FailureRate for random failures (0.0-1.0)
	FailureRate float64
	// TaskDelay simulates async task processing time
	TaskDelay time.Duration
}

// VM represents a fake VM in the server
type VM struct {
	VMID      int    `json:"vmid"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Node      string `json:"node"`
	CPUs      int    `json:"cpus,omitempty"`
	Memory    int64  `json:"maxmem,omitempty"`
	Template  int    `json:"template,omitempty"`
	QMPStatus string `json:"qmpstatus,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Lock      string `json:"lock,omitempty"`
	// Description is the VM's description (Notes); it is in its config only.
	Description string            `json:"-"`
	Config      map[string]string `json:"-"`
	Networks    []NetworkConfig   `json:"-"`
	IPAddrs     []string          `json:"-"`
	CreatedAt   time.Time         `json:"-"`
}

// NetworkConfig represents a fake network interface
type NetworkConfig struct {
	Index  int    `json:"index"`
	Model  string `json:"model"`
	Bridge string `json:"bridge"`
	VLAN   int    `json:"vlan,omitempty"`
	MAC    string `json:"mac,omitempty"`
	IP     string `json:"ip,omitempty"`
}

// Task represents a fake task
type Task struct {
	UPID      string `json:"upid"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	User      string `json:"user"`
	Node      string `json:"node"`
	PID       int    `json:"pid"`
	StartTime int64  `json:"starttime"`
	Status    string `json:"status"`
	ExitCode  string `json:"exitstatus,omitempty"`
	CreatedAt time.Time

	// finalize applies the task's effect when it ends (ok: it succeeded).
	finalize func(ok bool)
	// manual tasks end only through CompleteTasks.
	manual bool
}

// Snapshot represents a fake snapshot
type Snapshot struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	SnapTime    int64  `json:"snaptime"`
	VMSTATE     int    `json:"vmstate,omitempty"`
	Parent      string `json:"parent,omitempty"`
}

// APIResponse represents the PVE API response format
type APIResponse struct {
	Data   interface{} `json:"data"`
	Errors interface{} `json:"errors,omitempty"`
}

// NewServer creates a new fake PVE server
func NewServer() *Server {
	config := &Config{
		FailureMode: os.Getenv("FAKE_PVE_FAILURE_MODE"),
		SlowMode:    os.Getenv("FAKE_PVE_SLOW_MODE") == "true",
		TaskDelay:   2 * time.Second,
	}

	if rate := os.Getenv("FAKE_PVE_FAILURE_RATE"); rate != "" {
		if f, err := strconv.ParseFloat(rate, 64); err == nil {
			config.FailureRate = f
		}
	}

	if delay := os.Getenv("FAKE_PVE_TASK_DELAY"); delay != "" {
		if d, err := time.ParseDuration(delay); err == nil {
			config.TaskDelay = d
		}
	}

	s := &Server{
		router:       mux.NewRouter(),
		vms:          make(map[int]*VM),
		tasks:        make(map[string]*Task),
		snapshots:    make(map[string][]*Snapshot),
		logger:       slog.Default(),
		config:       config,
		failingTasks: make(map[string]bool),
		hidden:       make(map[int]bool),
		offline:      make(map[string]bool),
	}

	s.setupRoutes()
	s.seedData()

	return s
}

// setupRoutes configures the fake API routes
func (s *Server) setupRoutes() {
	api := s.router.PathPrefix("/api2/json").Subrouter()

	// Cluster
	api.HandleFunc("/nodes", s.handleListNodes).Methods("GET")
	api.HandleFunc("/cluster/nextid", s.handleClusterNextID).Methods("GET")
	api.HandleFunc("/cluster/resources", s.handleClusterResources).Methods("GET")

	// VM operations
	api.HandleFunc("/nodes/{node}/qemu", s.handleCreateVM).Methods("POST")
	api.HandleFunc("/nodes/{node}/qemu", s.handleListVMs).Methods("GET")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}", s.handleDeleteVM).Methods("DELETE")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/status/current", s.handleGetVM).Methods("GET")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/config", s.handleGetVMConfig).Methods("GET")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/config", s.handleReconfigureVM).Methods("PUT")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/resize", s.handleResizeDisk).Methods("PUT")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/clone", s.handleCloneVM).Methods("POST")

	// Power operations
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/status/start", s.handlePowerOp("start")).Methods("POST")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/status/stop", s.handlePowerOp("stop")).Methods("POST")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/status/reboot", s.handlePowerOp("reboot")).Methods("POST")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/status/shutdown", s.handlePowerOp("shutdown")).Methods("POST")

	// Storage operations
	api.HandleFunc("/nodes/{node}/storage/{storage}/download-url", s.handleDownloadURL).Methods("POST")
	api.HandleFunc("/nodes/{node}/storage/{storage}/content", s.handleStorageContent).Methods("GET")

	// Task operations
	api.HandleFunc("/nodes/{node}/tasks/{taskid}/status", s.handleGetTaskStatus).Methods("GET")
	api.HandleFunc("/nodes/{node}/tasks", s.handleListTasks).Methods("GET")

	// Snapshot operations
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/snapshot", s.handleListSnapshots).Methods("GET")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/snapshot", s.handleCreateSnapshot).Methods("POST")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/snapshot/{snapname}", s.handleDeleteSnapshot).Methods("DELETE")
	api.HandleFunc("/nodes/{node}/qemu/{vmid}/snapshot/{snapname}/rollback", s.handleRevertSnapshot).Methods("POST")

	// Health check
	s.router.HandleFunc("/health", s.handleHealth).Methods("GET")
}

// seedData creates some initial test data
func (s *Server) seedData() {
	// Create a template VM
	template := &VM{
		VMID:      9000,
		Name:      "ubuntu-22-template",
		Status:    "stopped",
		Node:      "pve",
		Template:  1,
		QMPStatus: "stopped",
		CreatedAt: time.Now(),
	}
	s.vms[template.VMID] = template

	// Create a test VM
	testVM := &VM{
		VMID:      100,
		Name:      "test-vm",
		Status:    "running",
		Node:      "pve",
		CPUs:      2,
		Memory:    2048 * 1024 * 1024, // 2GB in bytes
		QMPStatus: "running",
		PID:       12345,
		CreatedAt: time.Now(),
	}
	s.vms[testVM.VMID] = testVM
}

// ServeHTTP implements http.Handler
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Log request
	s.logger.Debug("Fake PVE API request", "method", r.Method, "path", r.URL.Path)
	s.mu.Lock()
	s.requests = append(s.requests, RecordedRequest{Method: r.Method, Path: r.URL.Path})
	s.mu.Unlock()

	// Simulate authentication (accept any token)
	if auth := r.Header.Get("Authorization"); auth == "" {
		s.writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	// Check for simulated failures
	if s.shouldFail() {
		s.writeError(w, http.StatusInternalServerError, "Simulated failure")
		return
	}

	// Add slow mode delay
	if s.config.SlowMode {
		time.Sleep(100 * time.Millisecond)
	}

	if s.nodeOfflineFor(r) {
		s.writeError(w, 595, "no route to host")
		return
	}

	if f := s.matchFault(r); f != nil {
		if f.PassThrough {
			if f.Then != nil {
				f.Then(r)
			}
			s.router.ServeHTTP(w, r)
			return
		}
		if f.AfterApply {
			s.router.ServeHTTP(httptest.NewRecorder(), r)
		}
		if f.Then != nil {
			f.Then(r)
		}
		if f.Raw {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(f.Status)
			_, _ = w.Write([]byte(f.Body))
			return
		}
		s.writeError(w, f.Status, f.Body)
		return
	}

	s.router.ServeHTTP(w, r)
}

// handleClusterResources mimics GET /cluster/resources?type=vm: every guest of
// the cluster, templates included, with its node.
func (s *Server) handleClusterResources(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(s.vms))
	for _, vm := range s.vms {
		if s.hidden[vm.VMID] {
			continue
		}
		node := vm.Node
		if node == "" {
			node = "pve"
		}
		status := vm.Status
		if s.offline[node] {
			status = "unknown" // PVE keeps listing a down node's guests
		}
		entry := map[string]interface{}{
			"id":     fmt.Sprintf("qemu/%d", vm.VMID),
			"type":   "qemu",
			"vmid":   vm.VMID,
			"node":   node,
			"status": status,
			"maxcpu": vm.CPUs,
			"maxmem": vm.Memory,
		}
		switch {
		case vm.Name != "":
			entry["name"] = vm.Name
		case !s.emptyResourceNames:
			// pvestatd reports `$conf->{name} || "VM $vmid"`.
			entry["name"] = fmt.Sprintf("VM %d", vm.VMID)
		}
		if vm.Template == 1 {
			entry["template"] = 1
		}
		out = append(out, entry)
	}
	s.writeResponse(w, out)
}

// shouldFail determines if this request should fail based on configuration
func (s *Server) shouldFail() bool {
	switch s.config.FailureMode {
	case "always":
		return true
	case "random":
		return rand.Float64() < s.config.FailureRate
	default:
		return false
	}
}

// handleCreateVM handles VM creation
func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]

	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	vmidStr := r.FormValue("vmid")
	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if VM already exists (PVE answers 500 "... already exists" before
	// doing anything).
	if _, exists := s.vms[vmid]; exists {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("unable to create VM %d - VM %d already exists on node '%s'", vmid, vmid, node))
		return
	}

	// Like PVE, the VM exists at once as a temporary config, locked "create",
	// with no name and no description; the requested config is written when
	// the task ends (a failed task removes the VM).
	vm := &VM{
		VMID:      vmid,
		Status:    "stopped",
		Node:      node,
		QMPStatus: "stopped",
		Lock:      "create",
		CreatedAt: time.Now(),
	}
	final := *vm
	final.Lock = ""
	final.Name = r.FormValue("name")
	final.Description = r.FormValue("description")
	if cpus := r.FormValue("cores"); cpus != "" {
		if c, err := strconv.Atoi(cpus); err == nil {
			final.CPUs = c
		}
	}
	if memory := r.FormValue("memory"); memory != "" {
		if m, err := strconv.ParseInt(memory, 10, 64); err == nil {
			final.Memory = m * 1024 * 1024 // Convert MB to bytes
		}
	}

	s.vms[vmid] = vm

	taskID := s.startMakeTask(r, node, "qmcreate", strconv.Itoa(vmid), vmid, final)

	s.writeResponse(w, taskID)
}

// startMakeTask starts the task of a create or clone that makes VM vmid,
// whose config becomes final when the task succeeds (and which is removed
// when it fails). With no task delay the task ends at once. The caller holds
// s.mu.
func (s *Server) startMakeTask(r *http.Request, node, taskType, id string, vmid int, final VM) string {
	taskID := s.createTaskAs(requestUser(r), node, taskType, id)
	task := s.tasks[taskID]
	task.finalize = func(ok bool) {
		if !ok {
			delete(s.vms, vmid)
			return
		}
		if vm, exists := s.vms[vmid]; exists {
			f := final
			*vm = f
		}
	}
	if s.config.TaskDelay == 0 {
		s.finishTask(task)
	}
	return taskID
}

// finishTask ends a running task: exit status "OK", or "ERROR" for a type
// FailTasks names, and applies its effect. The caller holds s.mu.
func (s *Server) finishTask(task *Task) {
	if task.Status != "running" {
		return
	}
	task.Status = "stopped"
	task.ExitCode = "OK"
	ok := !s.failingTasks[task.Type]
	if !ok {
		task.ExitCode = "ERROR"
	}
	if task.finalize != nil {
		task.finalize(ok)
	}
}

// CompleteTasks ends every running task now. Safe for concurrent use.
func (s *Server) CompleteTasks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.tasks {
		s.finishTask(task)
	}
}

// StartTask starts a running task of taskType on node for id, as user (e.g.
// another PVE user's clone), and returns its UPID. It runs until
// CompleteTasks. Safe for concurrent use.
func (s *Server) StartTask(node, taskType, id, user string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	upid := s.createTaskAs(user, node, taskType, id)
	s.tasks[upid].manual = true
	return upid
}

// requestUser is the PVE user a request authenticates as: the API token id of
// "PVEAPIToken=<user@realm!token>=<secret>", or root@pam.
func requestUser(r *http.Request) string {
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "PVEAPIToken="); ok {
		if id, _, found := strings.Cut(v, "="); found {
			return id
		}
	}
	return "root@pam"
}

// handleListTasks mimics GET /nodes/{node}/tasks?source=active: the tasks still
// running on node.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	node := mux.Vars(r)["node"]
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]interface{}, 0)
	for _, t := range s.tasks {
		if t.Node == node && t.Status == "running" {
			out = append(out, map[string]interface{}{
				"upid": t.UPID, "type": t.Type, "id": t.ID, "user": t.User, "node": t.Node,
			})
		}
	}
	s.writeResponse(w, out)
}

// handleListVMs handles listing VMs on a node (GET /nodes/{node}/qemu). It
// returns all seeded/created VMs, including templates, so ImagePrepare's
// verify-by-name and idempotency probes can find them.
// handleListNodes returns the cluster's nodes. It reports two nodes (one not
// named "pve") so a test can prove the provider discovers the node from the API
// rather than assuming the legacy "pve" default.
func (s *Server) handleListNodes(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	names := s.nodeNames
	s.mu.RUnlock()
	if len(names) == 0 {
		names = []string{"pve", "pve2"}
	}
	out := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]interface{}{"node": n, "status": "online"})
	}
	s.writeResponse(w, out)
}

// SetNodes sets the nodes /nodes lists, in that order (PVE does not sort
// them). Safe for concurrent use.
func (s *Server) SetNodes(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodeNames = append([]string(nil), names...)
}

// handleClusterNextID mimics PVE's /cluster/nextid: it returns a free VMID,
// handing out monotonically increasing ids (skipping any already in use).
func (s *Server) handleClusterNextID(w http.ResponseWriter, r *http.Request) {
	// ?vmid=<id> asks whether that ID is free, as PVE does: 400 "VM <id>
	// already exists" for one in use, the ID itself otherwise.
	if q := r.URL.Query().Get("vmid"); q != "" {
		id, err := strconv.Atoi(q)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid vmid")
			return
		}
		s.mu.RLock()
		_, taken := s.vms[id]
		s.mu.RUnlock()
		if taken {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("VM %d already exists", id))
			return
		}
		s.writeResponse(w, strconv.Itoa(id))
		return
	}
	s.mu.Lock()
	if s.nextID < 200000 {
		s.nextID = 200000
	}
	for {
		s.nextID++
		if _, taken := s.vms[s.nextID]; !taken {
			break
		}
	}
	id := s.nextID
	s.mu.Unlock()
	s.writeResponse(w, strconv.Itoa(id))
}

// handleStorageContent mimics PVE's /nodes/{node}/storage/{storage}/content: it
// lists each VM's primary disk volume with a real (thin) size/used/format, so
// GetDiskInfo can report true values instead of guessing from the config string.
// The reported size (20 GiB) deliberately differs from the config's 32 GiB so a
// test can prove the values came from the storage API, not the config.
func (s *Server) handleStorageContent(w http.ResponseWriter, r *http.Request) {
	storage := mux.Vars(r)["storage"]

	s.mu.RLock()
	vols := make([]map[string]interface{}, 0, len(s.vms))
	for vmid := range s.vms {
		vols = append(vols, map[string]interface{}{
			"volid":  fmt.Sprintf("%s:vm-%d-disk-0", storage, vmid),
			"size":   int64(20 * 1024 * 1024 * 1024), // 20 GiB provisioned
			"used":   int64(5 * 1024 * 1024 * 1024),  // 5 GiB actually used (thin)
			"format": "raw",
		})
	}
	s.mu.RUnlock()

	s.writeResponse(w, vols)
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]

	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]*VM, 0, len(s.vms))
	for _, vm := range s.vms {
		if vm.Node == "" || vm.Node == node {
			list = append(list, vm)
		}
	}

	s.writeResponse(w, list)
}

// handleDownloadURL handles the storage download-url import endpoint used by
// ImagePrepare. It records the request parameters for later assertion and returns
// an async task, mirroring PVE's behavior for a long-running download.
func (s *Server) handleDownloadURL(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]
	storage := vars["storage"]

	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	s.mu.Lock()
	s.lastDownload = &DownloadRequest{
		Node:     node,
		Storage:  storage,
		Content:  r.FormValue("content"),
		Filename: r.FormValue("filename"),
		URL:      r.FormValue("url"),
	}
	taskID := s.createTask(node, "download", storage)
	s.mu.Unlock()

	s.writeResponse(w, taskID)
}

// handleDeleteVM handles VM deletion
func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]
	vmidStr := vars["vmid"]

	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if VM exists on this node
	vm, exists := s.vmOnNode(node, vmid)
	if !exists {
		s.writeMissingVM(w, node, vmid)
		return
	}

	// PVE refuses to destroy a running VM — callers must stop it first. Mirroring
	// that here makes the fake exercise the provider's stop-before-destroy path
	// (#261 P0-1); a bare destroy of a running VM would orphan it.
	if vm.Status == "running" {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("VM %d is running - destroy failed", vmid))
		return
	}

	// Delete VM
	delete(s.vms, vmid)

	// Create async task
	taskID := s.createTask(node, "qmdestroy", vmidStr)

	s.writeResponse(w, taskID)
}

// handleGetVM handles VM status retrieval
func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmidStr := vars["vmid"]

	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	s.mu.RLock()
	vm, exists := s.vmOnNode(vars["node"], vmid)
	var snapshot VM
	if exists {
		snapshot = *vm
	}
	s.mu.RUnlock()

	if !exists {
		s.writeMissingVM(w, vars["node"], vmid)
		return
	}

	s.writeResponse(w, snapshot)
}

// vmOnNode returns the VM vmid when it lives on node (a VM with no node lives
// on every node). The caller holds s.mu.
func (s *Server) vmOnNode(node string, vmid int) (*VM, bool) {
	vm, ok := s.vms[vmid]
	if !ok || (vm.Node != "" && vm.Node != node) {
		return nil, false
	}
	return vm, true
}

// writeMissingVM answers a per-VM call for a VM that is not on node the way
// real PVE does: HTTP 500 "Configuration file ... does not exist" (not 404),
// also when the VM lives on another node.
func (s *Server) writeMissingVM(w http.ResponseWriter, node string, vmid int) {
	s.writeError(w, http.StatusInternalServerError,
		fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, vmid))
}

// handleCloneVM handles VM cloning
func (s *Server) handleCloneVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]
	sourceVMIDStr := vars["vmid"]

	sourceVMID, err := strconv.Atoi(sourceVMIDStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid source VMID")
		return
	}

	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	targetVMIDStr := r.FormValue("newid")
	targetVMID, err := strconv.Atoi(targetVMIDStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid target VMID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check source VM exists
	sourceVM, exists := s.vmOnNode(node, sourceVMID)
	if !exists {
		s.writeMissingVM(w, node, sourceVMID)
		return
	}
	// PVE's `target` places the clone on another node.
	targetNode := node
	if t := r.FormValue("target"); t != "" {
		targetNode = t
	}

	// Check target VM doesn't exist
	if _, exists := s.vms[targetVMID]; exists {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("unable to create VM %d: config file already exists", targetVMID))
		return
	}

	// Like PVE, the clone exists at once as a temporary config on the source
	// node, locked "clone", with no name and no description. When the task
	// ends it gets its final config: the request's description when it is
	// non-empty and the source's otherwise, on the target node. The task's id
	// is the SOURCE VMID, as in PVE.
	description := sourceVM.Description
	if d := r.FormValue("description"); d != "" {
		description = d
	}
	temp := &VM{
		VMID:      targetVMID,
		Status:    "stopped",
		Node:      node,
		QMPStatus: "stopped",
		Lock:      "clone",
		CreatedAt: time.Now(),
	}
	final := *temp
	final.Lock = ""
	final.Name = r.FormValue("name")
	final.Node = targetNode
	final.CPUs = sourceVM.CPUs
	final.Memory = sourceVM.Memory
	final.Description = description

	s.vms[targetVMID] = temp

	taskID := s.startMakeTask(r, node, "qmclone", sourceVMIDStr, targetVMID, final)

	s.writeResponse(w, taskID)
}

// handlePowerOp creates a handler for power operations
func (s *Server) handlePowerOp(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		node := vars["node"]
		vmidStr := vars["vmid"]

		vmid, err := strconv.Atoi(vmidStr)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "Invalid VMID")
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		// Record the operation + params so tests can assert e.g. graceful-shutdown
		// timeout/forceStop propagation.
		s.lastPowerOp = &PowerOpRequest{
			Operation: operation,
			Timeout:   r.FormValue("timeout"),
			ForceStop: r.FormValue("forceStop"),
		}

		vm, exists := s.vmOnNode(node, vmid)
		if !exists {
			s.writeMissingVM(w, node, vmid)
			return
		}

		// Update VM status based on operation
		switch operation {
		case "start":
			vm.Status = "running"
			vm.QMPStatus = "running"
			vm.PID = rand.Intn(99999) + 1000
		case "stop":
			vm.Status = "stopped"
			vm.QMPStatus = "stopped"
			vm.PID = 0
		case "reboot":
			// Status stays the same for reboot
		}

		// Create async task
		taskID := s.createTask(node, "qm"+operation, vmidStr)

		s.writeResponse(w, taskID)
	}
}

// handleGetTaskStatus handles task status retrieval
func (s *Server) handleGetTaskStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	taskID := vars["taskid"]

	s.mu.RLock()
	task, exists := s.tasks[taskID]
	s.mu.RUnlock()

	if !exists {
		s.writeError(w, http.StatusNotFound, "Task not found")
		return
	}

	// Simulate task completion after delay
	s.mu.Lock()
	if !task.manual && time.Since(task.CreatedAt) >= s.config.TaskDelay {
		s.finishTask(task)
	}
	snapshot := *task
	snapshot.finalize = nil
	s.mu.Unlock()

	s.writeResponse(w, snapshot)
}

// handleCreateSnapshot handles snapshot creation
func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]
	vmidStr := vars["vmid"]

	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	snapName := r.FormValue("snapname")
	if snapName == "" {
		s.writeError(w, http.StatusBadRequest, "Snapshot name required")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check VM exists
	if _, exists := s.vms[vmid]; !exists {
		s.writeError(w, http.StatusNotFound, "VM not found")
		return
	}

	// Create snapshot
	snapshot := &Snapshot{
		Name:        snapName,
		Description: r.FormValue("description"),
		SnapTime:    time.Now().Unix(),
	}

	vmKey := fmt.Sprintf("%d", vmid)
	s.snapshots[vmKey] = append(s.snapshots[vmKey], snapshot)

	// Create async task
	taskID := s.createTask(node, "qmsnapshot", vmidStr)

	s.writeResponse(w, taskID)
}

// MoveVM moves VM vmid to node, as a PVE HA migration does (tests).
func (s *Server) MoveVM(vmid int, node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vm, ok := s.vms[vmid]; ok {
		vm.Node = node
	}
}

// handleListSnapshots lists a VM's snapshots as PVE does: the snapshots plus a
// "current" pseudo-entry for the running state. A VM that does not exist is
// answered as PVE answers it: HTTP 500, "Configuration file ... does not
// exist".
func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmid, err := strconv.Atoi(vars["vmid"])
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// PVE answers for the node named in the path: a VM on another node (e.g.
	// after an HA move), like one that does not exist, has no configuration
	// file there.
	if vm, exists := s.vms[vmid]; !exists || vm.Node != vars["node"] {
		s.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", vars["node"], vmid))
		return
	}
	list := make([]*Snapshot, 0, len(s.snapshots[vars["vmid"]])+1)
	list = append(list, s.snapshots[vars["vmid"]]...)
	list = append(list, &Snapshot{Name: "current", Description: "You are here!"})
	s.writeResponse(w, list)
}

// handleDeleteSnapshot handles snapshot deletion
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]
	vmidStr := vars["vmid"]
	snapName := vars["snapname"]

	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check the VM exists on this node (PVE answers per node).
	if vm, exists := s.vms[vmid]; !exists || vm.Node != node {
		s.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", node, vmid))
		return
	}

	// Find and delete snapshot; a missing one is answered as PVE does.
	vmKey := fmt.Sprintf("%d", vmid)
	snapshots := s.snapshots[vmKey]
	found := false
	for i, snap := range snapshots {
		if snap.Name == snapName {
			s.snapshots[vmKey] = append(snapshots[:i], snapshots[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("snapshot '%s' does not exist", snapName))
		return
	}

	// Create async task
	taskID := s.createTask(node, "qmdelsnapshot", vmidStr)

	s.writeResponse(w, taskID)
}

// handleRevertSnapshot handles snapshot revert
func (s *Server) handleRevertSnapshot(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]
	vmidStr := vars["vmid"]

	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check VM exists
	if _, exists := s.vms[vmid]; !exists {
		s.writeError(w, http.StatusNotFound, "VM not found")
		return
	}

	// Create async task
	taskID := s.createTask(node, "qmrollback", vmidStr)

	s.writeResponse(w, taskID)
}

// handleHealth handles health check
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	})
}

// createTask creates a new async task, run as root@pam.
func (s *Server) createTask(node, taskType, id string) string {
	return s.createTaskAs("root@pam", node, taskType, id)
}

// createTaskAs creates a new async task run as user. The caller holds s.mu.
func (s *Server) createTaskAs(user, node, taskType, id string) string {
	timestamp := time.Now().Unix()
	s.taskSeq++
	taskID := fmt.Sprintf("UPID:%s:%08X:%s:%s:%s:%d:",
		node, s.taskSeq, taskType, id, user, timestamp)

	task := &Task{
		UPID:      taskID,
		Type:      taskType,
		ID:        id,
		User:      user,
		Node:      node,
		PID:       rand.Intn(99999) + 1000,
		StartTime: time.Now().Unix(),
		Status:    "running",
		CreatedAt: time.Now(),
	}

	s.tasks[taskID] = task
	return taskID
}

// writeResponse writes a successful JSON response
func (s *Server) writeResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := APIResponse{Data: data}
	_ = json.NewEncoder(w).Encode(response)
}

// writeError writes an error response. By default the message is in the
// body; with ErrorsInStatusLine it is in the HTTP reason phrase with a body
// of {"data":null}, as real PVE answers.
func (s *Server) writeError(w http.ResponseWriter, statusCode int, message string) {
	// Not under s.mu: handlers call writeError while holding it.
	if s.errorsInStatusLine.Load() {
		writeStatusLineError(w, statusCode, message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	response := APIResponse{
		Errors: map[string]string{"error": message},
	}
	_ = json.NewEncoder(w).Encode(response)
}

// writeStatusLineError answers with statusCode and message as the HTTP reason
// phrase, and the body {"data":null}, as real PVE does. net/http always
// writes the standard reason phrase, so the connection is hijacked and the
// response written raw (and the connection closed).
func writeStatusLineError(w http.ResponseWriter, statusCode int, message string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(`{"data":null}`))
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck // test fake
	// A reason phrase cannot carry CR or LF.
	message = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(message))
	body := `{"data":null}`
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json;charset=UTF-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		statusCode, message, len(body), body)
	_ = buf.Flush()
}

// ErrorsInStatusLine makes the fake answer errors as real PVE does: the
// message in the HTTP status line's reason phrase, the body {"data":null}
// (tests).
func (s *Server) ErrorsInStatusLine(on bool) {
	s.errorsInStatusLine.Store(on)
}

// handleGetVMConfig handles VM configuration retrieval
func (s *Server) handleGetVMConfig(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	vmidStr := vars["vmid"]

	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	vm, exists := s.vmOnNode(vars["node"], vmid)
	if !exists {
		s.writeMissingVM(w, vars["node"], vmid)
		return
	}

	// Build fake config that mirrors PVE format
	config := map[string]interface{}{
		"cores":  vm.CPUs,
		"memory": vm.Memory / (1024 * 1024), // Convert to MB
		"vmid":   vm.VMID,
		"scsi0":  fmt.Sprintf("local-lvm:vm-%d-disk-0,size=32G", vmid),
	}
	if vm.Name != "" {
		config["name"] = vm.Name
	}

	// Add network interfaces
	for i, net := range vm.Networks {
		netString := fmt.Sprintf("%s,bridge=%s", net.Model, net.Bridge)
		if net.VLAN > 0 {
			netString += fmt.Sprintf(",tag=%d", net.VLAN)
		}
		if net.MAC != "" {
			netString += fmt.Sprintf(",macaddr=%s", net.MAC)
		}
		config[fmt.Sprintf("net%d", i)] = netString
	}

	// Add disk config
	config["scsi0"] = "local-lvm:vm-100-disk-0,size=32G"
	// Raw config keys a test seeded (PutVM with Config: disks such as scsi1,
	// a boot order) or a resize changed override the defaults above.
	for k, v := range vm.Config {
		config[k] = v
	}

	if vm.Description != "" {
		config["description"] = vm.Description
	}
	if vm.Template == 1 {
		config["template"] = 1
	}
	if vm.Lock != "" {
		config["lock"] = vm.Lock
	}
	config["digest"] = configDigest(vm)

	s.writeResponse(w, config)
}

// modifiedConfigMessage is PVE's answer to a config update whose digest is
// not the config's current one.
const modifiedConfigMessage = "detected modified configuration - file changed by other user? Try again."

// configDigest is the fake's config digest: like PVE's (a SHA-1 of the config
// file), it changes whenever the VM's config does. Clients treat it as opaque.
func configDigest(vm *VM) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d|%s|%d|%d|%d|%s|%q", vm.VMID, vm.Name, vm.CPUs, vm.Memory, vm.Template, vm.Lock, vm.Description)
	keys := make([]string, 0, len(vm.Config))
	for k := range vm.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "|%s=%q", k, vm.Config[k])
	}
	for _, n := range vm.Networks {
		_, _ = fmt.Fprintf(h, "|%+v", n)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// handleReconfigureVM handles VM reconfiguration
func (s *Server) handleReconfigureVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]
	vmidStr := vars["vmid"]

	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	vm, exists := s.vmOnNode(node, vmid)
	if !exists {
		s.writeMissingVM(w, node, vmid)
		return
	}
	// PVE applies an update that carries a digest only to the config it was
	// computed from.
	if digest := r.FormValue("digest"); digest != "" && digest != configDigest(vm) {
		s.writeError(w, http.StatusInternalServerError, modifiedConfigMessage)
		return
	}

	// Update VM configuration
	if cores := r.FormValue("cores"); cores != "" {
		if c, err := strconv.Atoi(cores); err == nil {
			vm.CPUs = c
		}
	}

	if memory := r.FormValue("memory"); memory != "" {
		if m, err := strconv.ParseInt(memory, 10, 64); err == nil {
			vm.Memory = m * 1024 * 1024 // Convert MB to bytes
		}
	}

	if _, ok := r.Form["description"]; ok {
		vm.Description = r.FormValue("description")
	}
	for _, key := range strings.Split(r.FormValue("delete"), ",") {
		if strings.TrimSpace(key) == "description" {
			vm.Description = ""
		}
	}

	// Create async task
	taskID := s.createTask(node, "qmconfig", vmidStr)

	s.writeResponse(w, taskID)
}

// handleResizeDisk handles disk resizing
func (s *Server) handleResizeDisk(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	node := vars["node"]
	vmidStr := vars["vmid"]

	vmid, err := strconv.Atoi(vmidStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid VMID")
		return
	}

	if err := r.ParseForm(); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid form data")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	vm, exists := s.vms[vmid]
	if !exists {
		s.writeError(w, http.StatusNotFound, "VM not found")
		return
	}

	disk := r.FormValue("disk")
	size := r.FormValue("size")

	if disk == "" || size == "" {
		s.writeError(w, http.StatusBadRequest, "Disk and size required")
		return
	}

	// Like PVE, the resize rewrites the disk's size= option in the config
	// (an absolute size; PVE also accepts +N, which nothing here sends).
	if vm.Config == nil {
		vm.Config = make(map[string]string)
	}
	current, ok := vm.Config[disk]
	if !ok && disk == "scsi0" {
		current = fmt.Sprintf("local-lvm:vm-%d-disk-0,size=32G", vmid)
	}
	if current == "" {
		s.writeError(w, http.StatusInternalServerError, fmt.Sprintf("disk '%s' does not exist", disk))
		return
	}
	parts := strings.Split(current, ",")
	resized := false
	for i, part := range parts {
		if strings.HasPrefix(part, "size=") {
			parts[i] = "size=" + size
			resized = true
		}
	}
	if !resized {
		parts = append(parts, "size="+size)
	}
	vm.Config[disk] = strings.Join(parts, ",")

	// Create async task
	taskID := s.createTask(node, "qmresize", vmidStr)

	s.writeResponse(w, taskID)
}

// StartFakeServer starts a fake PVE server on a random port
func StartFakeServer() (*Server, string, error) {
	server := NewServer()

	// Start HTTP server on random port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, "", fmt.Errorf("failed to start fake server: %w", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port //nolint:errcheck // Test server port assignment
	endpoint := fmt.Sprintf("http://localhost:%d", port)

	go func() {
		if err := http.Serve(listener, server); err != nil {
			slog.Error("Fake PVE server error", "error", err)
		}
	}()

	return server, endpoint, nil
}
