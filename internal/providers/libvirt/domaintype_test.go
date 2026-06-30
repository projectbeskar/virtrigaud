package libvirt

import "testing"

// TestDomainTypeFromProbe locks in the KVM-detection fallback (#282): a readable
// /dev/kvm selects hardware-accelerated "kvm"; both failure cases (absent, or
// present-but-unreadable) fall back to "qemu" (TCG), which starts on any host.
func TestDomainTypeFromProbe(t *testing.T) {
	tests := []struct {
		name     string
		readable bool
		exists   bool
		want     string
	}{
		{"readable -> kvm", true, false, "kvm"},
		{"present but unreadable -> qemu", false, true, "qemu"},
		{"absent -> qemu", false, false, "qemu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domainTypeFromProbe(tt.readable, tt.exists); got != tt.want {
				t.Errorf("domainTypeFromProbe(readable=%v, exists=%v) = %q, want %q",
					tt.readable, tt.exists, got, tt.want)
			}
		})
	}
}
