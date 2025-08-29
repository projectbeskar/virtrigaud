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
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// Server represents a fake Proxmox VE API server
type Server struct {
	router    *mux.Router
	vms       map[int]*VM
	tasks     map[string]*Task
	snapshots map[string][]*Snapshot
	mu        sync.RWMutex
	logger    *slog.Logger
	config    *Config
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
	VMID      int               `json:"vmid"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Node      string            `json:"node"`
	CPUs      int               `json:"cpus,omitempty"`
	Memory    int64             `json:"maxmem,omitempty"`
	Template  int               `json:"template,omitempty"`
	QMPStatus string            `json:"qmpstatus,omitempty"`
	PID       int               `json:"pid,omitempty"`
	Lock      string            `json:"lock,omitempty"`
	Config    map[string]string `json:"-"`
	Networks  []NetworkConfig   `json:"-"`
	IPAddrs   []string          `json:"-"`
	CreatedAt time.Time         `json:"-"`
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
		router:    mux.NewRouter(),
		vms:       make(map[int]*VM),
		tasks:     make(map[string]*Task),
		snapshots: make(map[string][]*Snapshot),
		logger:    slog.Default(),
		config:    config,
	}

	s.setupRoutes()
	s.seedData()

	return s
}

// setupRoutes configures the fake API routes
func (s *Server) setupRoutes() {
	api := s.router.PathPrefix("/api2/json").Subrouter()

	// VM operations
	api.HandleFunc("/nodes/{node}/qemu", s.handleCreateVM).Methods("POST")
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

	// Task operations
	api.HandleFunc("/nodes/{node}/tasks/{taskid}/status", s.handleGetTaskStatus).Methods("GET")

	// Snapshot operations
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

	s.router.ServeHTTP(w, r)
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

	// Check if VM already exists
	if _, exists := s.vms[vmid]; exists {
		s.writeError(w, http.StatusConflict, "VM already exists")
		return
	}

	// Create new VM
	vm := &VM{
		VMID:      vmid,
		Name:      r.FormValue("name"),
		Status:    "stopped",
		Node:      node,
		QMPStatus: "stopped",
		CreatedAt: time.Now(),
	}

	if cpus := r.FormValue("cores"); cpus != "" {
		if c, err := strconv.Atoi(cpus); err == nil {
			vm.CPUs = c
		}
	}

	if memory := r.FormValue("memory"); memory != "" {
		if m, err := strconv.ParseInt(memory, 10, 64); err == nil {
			vm.Memory = m * 1024 * 1024 // Convert MB to bytes
		}
	}

	s.vms[vmid] = vm

	// Create async task
	taskID := s.createTask(node, "qmcreate", strconv.Itoa(vmid))

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

	// Check if VM exists
	if _, exists := s.vms[vmid]; !exists {
		w.WriteHeader(http.StatusNotFound)
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
	vm, exists := s.vms[vmid]
	s.mu.RUnlock()

	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	s.writeResponse(w, vm)
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

	targetVMIDStr := r.FormValue("vmid")
	targetVMID, err := strconv.Atoi(targetVMIDStr)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid target VMID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check source VM exists
	sourceVM, exists := s.vms[sourceVMID]
	if !exists {
		s.writeError(w, http.StatusNotFound, "Source VM not found")
		return
	}

	// Check target VM doesn't exist
	if _, exists := s.vms[targetVMID]; exists {
		s.writeError(w, http.StatusConflict, "Target VM already exists")
		return
	}

	// Create cloned VM
	clonedVM := &VM{
		VMID:      targetVMID,
		Name:      r.FormValue("name"),
		Status:    "stopped",
		Node:      node,
		CPUs:      sourceVM.CPUs,
		Memory:    sourceVM.Memory,
		QMPStatus: "stopped",
		CreatedAt: time.Now(),
	}

	s.vms[targetVMID] = clonedVM

	// Create async task
	taskID := s.createTask(node, "qmclone", targetVMIDStr)

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

		vm, exists := s.vms[vmid]
		if !exists {
			s.writeError(w, http.StatusNotFound, "VM not found")
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
	if task.Status == "running" && time.Since(task.CreatedAt) > s.config.TaskDelay {
		s.mu.Lock()
		task.Status = "stopped"
		task.ExitCode = "OK"
		s.mu.Unlock()
	}

	s.writeResponse(w, task)
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

	// Check VM exists
	if _, exists := s.vms[vmid]; !exists {
		s.writeError(w, http.StatusNotFound, "VM not found")
		return
	}

	// Find and delete snapshot
	vmKey := fmt.Sprintf("%d", vmid)
	snapshots := s.snapshots[vmKey]
	for i, snap := range snapshots {
		if snap.Name == snapName {
			s.snapshots[vmKey] = append(snapshots[:i], snapshots[i+1:]...)
			break
		}
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

// createTask creates a new async task
func (s *Server) createTask(node, taskType, id string) string {
	timestamp := time.Now().Unix()
	taskID := fmt.Sprintf("UPID:%s:%08X:%s:%s:%s:%d:",
		node, rand.Intn(99999999), taskType, id, "root@pam", timestamp)

	task := &Task{
		UPID:      taskID,
		Type:      taskType,
		ID:        id,
		User:      "root@pam",
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

// writeError writes an error response
func (s *Server) writeError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	response := APIResponse{
		Errors: map[string]string{"error": message},
	}
	_ = json.NewEncoder(w).Encode(response)
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
	vm, exists := s.vms[vmid]
	s.mu.RUnlock()

	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Build fake config that mirrors PVE format
	config := map[string]interface{}{
		"cores":  vm.CPUs,
		"memory": vm.Memory / (1024 * 1024), // Convert to MB
		"name":   vm.Name,
		"vmid":   vm.VMID,
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

	s.writeResponse(w, config)
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

	vm, exists := s.vms[vmid]
	if !exists {
		s.writeError(w, http.StatusNotFound, "VM not found")
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

	// Store disk resize info in config
	if vm.Config == nil {
		vm.Config = make(map[string]string)
	}
	vm.Config[disk+"_size"] = size

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

	port := listener.Addr().(*net.TCPAddr).Port
	endpoint := fmt.Sprintf("http://localhost:%d", port)

	go func() {
		if err := http.Serve(listener, server); err != nil {
			slog.Error("Fake PVE server error", "error", err)
		}
	}()

	return server, endpoint, nil
}
