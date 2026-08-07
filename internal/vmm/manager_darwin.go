//go:build darwin

package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"

	"exe/internal/cloudinit"
)

// vzManager runs VMs with Apple's Virtualization.framework. VMs live inside
// this process: if the daemon exits, its VMs power off.
type vzManager struct {
	opts Options

	mu      sync.Mutex
	running map[string]*vz.VirtualMachine

	dlMu sync.Mutex
}

func New(opts Options) (Manager, error) {
	for _, d := range []string{"vms", "images"} {
		if err := os.MkdirAll(filepath.Join(opts.StateDir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &vzManager{opts: opts, running: map[string]*vz.VirtualMachine{}}, nil
}

func (m *vzManager) vmDir(name string) string {
	return filepath.Join(m.opts.StateDir, "vms", name)
}

func (m *vzManager) loadMeta(name string) (*vmMeta, error) {
	b, err := os.ReadFile(filepath.Join(m.vmDir(name), "vm.json"))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var mt vmMeta
	if err := json.Unmarshal(b, &mt); err != nil {
		return nil, err
	}
	return &mt, nil
}

func (m *vzManager) Create(ctx context.Context, spec Spec) (*Info, error) {
	if err := ValidateName(spec.Name); err != nil {
		return nil, err
	}
	dir := m.vmDir(spec.Name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("vm %q already exists", spec.Name)
	}
	base, err := m.EnsureImage(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	disk := filepath.Join(dir, "disk.raw")
	if err := cloneFile(ctx, base, disk); err != nil {
		return nil, err
	}
	if st, err := os.Stat(disk); err == nil {
		if target := int64(spec.DiskGB) << 30; target > st.Size() {
			if err := os.Truncate(disk, target); err != nil {
				return nil, err
			}
		}
	}
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, err
	}
	if err := cloudinit.BuildSeed(filepath.Join(dir, "seed.iso"), spec.Name, m.opts.SSHUser, m.opts.AuthorizedKey); err != nil {
		return nil, err
	}
	mt := vmMeta{Spec: spec, MAC: mac.String(), CreatedAt: time.Now().UTC()}
	b, _ := json.MarshalIndent(mt, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "vm.json"), b, 0o644); err != nil {
		return nil, err
	}
	// Provisioning is done: keep the directory from here on even if boot
	// fails, so console.log stays around for debugging (`exe rm` cleans up).
	ok = true
	return m.Start(ctx, spec.Name)
}

func (m *vzManager) Start(ctx context.Context, name string) (*Info, error) {
	mt, err := m.loadMeta(name)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if vmach, exists := m.running[name]; exists && vmach.State() == vz.VirtualMachineStateRunning {
		m.mu.Unlock()
		return m.info(name, mt)
	}
	m.mu.Unlock()

	cfgv, err := m.buildConfig(name, mt)
	if err != nil {
		return nil, err
	}
	vmach, err := vz.NewVirtualMachine(cfgv)
	if err != nil {
		return nil, err
	}
	preBoot := leaseFingerprints()
	if err := vmach.Start(); err != nil {
		return nil, fmt.Errorf("start vm (is the binary codesigned with the virtualization entitlement? run make): %w", err)
	}
	m.mu.Lock()
	m.running[name] = vmach
	m.mu.Unlock()

	deadline := time.Now().Add(15 * time.Second)
	for vmach.State() != vz.VirtualMachineStateRunning {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("vm did not reach running state (state=%v)", vmach.State())
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("vm %s: booted, waiting for DHCP lease (mac %s)", name, mt.MAC)
	ip, err := waitIP(ctx, mt.MAC, name, preBoot, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("no DHCP lease appeared: %w (see %s)", err, filepath.Join(m.vmDir(name), "console.log"))
	}
	log.Printf("vm %s: ip %s, waiting for SSH", name, ip)
	if err := waitTCP(ctx, net.JoinHostPort(ip, "22"), 3*time.Minute); err != nil {
		return nil, fmt.Errorf("vm has IP %s but SSH did not come up: %w", ip, err)
	}
	log.Printf("vm %s: ready at %s", name, ip)
	return m.info(name, mt)
}

func (m *vzManager) buildConfig(name string, mt *vmMeta) (*vz.VirtualMachineConfiguration, error) {
	dir := m.vmDir(name)

	varStorePath := filepath.Join(dir, "efi_vars.fd")
	var store *vz.EFIVariableStore
	var err error
	if _, serr := os.Stat(varStorePath); os.IsNotExist(serr) {
		store, err = vz.NewEFIVariableStore(varStorePath, vz.WithCreatingEFIVariableStore())
	} else {
		store, err = vz.NewEFIVariableStore(varStorePath)
	}
	if err != nil {
		return nil, fmt.Errorf("efi variable store: %w", err)
	}
	boot, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(store))
	if err != nil {
		return nil, err
	}
	cfgv, err := vz.NewVirtualMachineConfiguration(boot, uint(mt.Spec.CPUs), uint64(mt.Spec.MemoryMB)<<20)
	if err != nil {
		return nil, err
	}
	platform, err := vz.NewGenericPlatformConfiguration()
	if err != nil {
		return nil, err
	}
	cfgv.SetPlatformVirtualMachineConfiguration(platform)

	diskAtt, err := vz.NewDiskImageStorageDeviceAttachment(filepath.Join(dir, "disk.raw"), false)
	if err != nil {
		return nil, err
	}
	diskDev, err := vz.NewVirtioBlockDeviceConfiguration(diskAtt)
	if err != nil {
		return nil, err
	}
	seedAtt, err := vz.NewDiskImageStorageDeviceAttachment(filepath.Join(dir, "seed.iso"), true)
	if err != nil {
		return nil, err
	}
	seedDev, err := vz.NewVirtioBlockDeviceConfiguration(seedAtt)
	if err != nil {
		return nil, err
	}
	cfgv.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{diskDev, seedDev})

	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	netDev, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, err
	}
	hw, err := net.ParseMAC(mt.MAC)
	if err != nil {
		return nil, err
	}
	macAddr, err := vz.NewMACAddress(hw)
	if err != nil {
		return nil, err
	}
	netDev.SetMACAddress(macAddr)
	cfgv.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netDev})

	serialAtt, err := vz.NewFileSerialPortAttachment(filepath.Join(dir, "console.log"), true)
	if err != nil {
		return nil, err
	}
	serial, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAtt)
	if err != nil {
		return nil, err
	}
	cfgv.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serial})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfgv.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	if ok, err := cfgv.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("invalid vm configuration: %v", err)
	}
	return cfgv, nil
}

func (m *vzManager) Stop(ctx context.Context, name string) error {
	if _, err := m.loadMeta(name); err != nil {
		return err
	}
	m.mu.Lock()
	vmach := m.running[name]
	m.mu.Unlock()
	if vmach == nil || vmach.State() == vz.VirtualMachineStateStopped {
		return ErrNotRunning
	}
	if vmach.CanRequestStop() {
		if _, err := vmach.RequestStop(); err == nil {
			if waitState(vmach, vz.VirtualMachineStateStopped, 30*time.Second) {
				m.forget(name)
				return nil
			}
		}
	}
	if vmach.CanStop() {
		if err := vmach.Stop(); err != nil {
			return err
		}
	}
	if !waitState(vmach, vz.VirtualMachineStateStopped, 15*time.Second) {
		return fmt.Errorf("vm %s did not stop", name)
	}
	m.forget(name)
	return nil
}

func (m *vzManager) forget(name string) {
	m.mu.Lock()
	delete(m.running, name)
	m.mu.Unlock()
}

func (m *vzManager) Delete(ctx context.Context, name string) error {
	if _, err := m.loadMeta(name); err != nil {
		return err
	}
	m.mu.Lock()
	vmach := m.running[name]
	m.mu.Unlock()
	if vmach != nil && vmach.CanStop() {
		_ = vmach.Stop()
		waitState(vmach, vz.VirtualMachineStateStopped, 10*time.Second)
	}
	m.forget(name)
	return os.RemoveAll(m.vmDir(name))
}

func (m *vzManager) List(ctx context.Context) ([]*Info, error) {
	entries, err := os.ReadDir(filepath.Join(m.opts.StateDir, "vms"))
	if err != nil {
		return nil, err
	}
	var out []*Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mt, err := m.loadMeta(e.Name())
		if err != nil {
			continue
		}
		inf, err := m.info(e.Name(), mt)
		if err != nil {
			continue
		}
		out = append(out, inf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *vzManager) Get(ctx context.Context, name string) (*Info, error) {
	mt, err := m.loadMeta(name)
	if err != nil {
		return nil, err
	}
	return m.info(name, mt)
}

func (m *vzManager) info(name string, mt *vmMeta) (*Info, error) {
	st := "stopped"
	m.mu.Lock()
	vmach := m.running[name]
	m.mu.Unlock()
	if vmach != nil {
		st = stateString(vmach.State())
	}
	inf := &Info{
		Name:      name,
		State:     st,
		CPUs:      mt.Spec.CPUs,
		MemoryMB:  mt.Spec.MemoryMB,
		DiskGB:    mt.Spec.DiskGB,
		MAC:       mt.MAC,
		CreatedAt: mt.CreatedAt,
	}
	if st == "running" {
		if ip, err := lookupLease(mt.MAC, name, nil); err == nil {
			inf.IP = ip
		}
	}
	return inf, nil
}

func stateString(s vz.VirtualMachineState) string {
	switch s {
	case vz.VirtualMachineStateStopped:
		return "stopped"
	case vz.VirtualMachineStateRunning:
		return "running"
	case vz.VirtualMachineStatePaused:
		return "paused"
	case vz.VirtualMachineStateError:
		return "error"
	case vz.VirtualMachineStateStarting:
		return "starting"
	case vz.VirtualMachineStatePausing:
		return "pausing"
	case vz.VirtualMachineStateResuming:
		return "resuming"
	case vz.VirtualMachineStateStopping:
		return "stopping"
	default:
		return fmt.Sprintf("state-%d", int(s))
	}
}

func waitState(vmach *vz.VirtualMachine, want vz.VirtualMachineState, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if vmach.State() == want {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return vmach.State() == want
}

// cloneFile copies base to dst, using APFS copy-on-write when possible.
func cloneFile(ctx context.Context, base, dst string) error {
	if err := exec.CommandContext(ctx, "cp", "-c", base, dst).Run(); err == nil {
		return nil
	}
	in, err := os.Open(base)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// EnsureImage downloads the base image once. If another process is already
// downloading to <image>.partial (e.g. a manual curl), it waits for that
// download as long as the file keeps growing.
func (m *vzManager) EnsureImage(ctx context.Context) (string, error) {
	m.dlMu.Lock()
	defer m.dlMu.Unlock()
	dest := filepath.Join(m.opts.StateDir, "images", filepath.Base(m.opts.ImageURL))
	partial := dest + ".partial"

	var last int64 = -1
	stall := 0
	for {
		if _, err := os.Stat(dest); err == nil {
			return dest, nil
		}
		st, err := os.Stat(partial)
		if err != nil {
			break
		}
		if st.Size() == last {
			if stall++; stall >= 3 {
				break
			}
		} else {
			stall, last = 0, st.Size()
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	log.Printf("downloading base image %s", m.opts.ImageURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.opts.ImageURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", m.opts.ImageURL, resp.StatusCode)
	}
	tmp := dest + ".exe-download"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	_, cerr := io.Copy(f, resp.Body)
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		os.Remove(tmp)
		return "", cerr
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	log.Printf("base image ready: %s", dest)
	return dest, nil
}
