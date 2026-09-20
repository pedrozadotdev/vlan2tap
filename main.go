package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	version = "0.2.0"

	tunDevice = "/dev/net/tun"

	defaultConfigDir  = "/etc/vlan2tap"
	defaultConfigPath = "/etc/vlan2tap/config.json"

	installBinaryPath = "/usr/local/sbin/vlan2tap"
	servicePath       = "/etc/init.d/S94vlan2tap"
	pidFilePath       = "/var/run/vlan2tap.pid"
	logFilePath       = "/var/log/vlan2tap.log"

	iffTAP  = 0x0002
	iffNoPI = 0x1000
	iffUp   = 0x0001

	ethPAll    = 0x0003
	ethPVLAN   = 0x8100
	ethP8021AD = 0x88a8

	arphrdEther = 1

	vlanHeaderLen = 4
	maxFrameSize  = 65536

	tpStatusVLANValid     = 1 << 4
	tpStatusVLANTPIDValid = 1 << 6

	defaultMTU         = 1500
	defaultHookTimeout = 30
)

type Config struct {
	PhysicalInterface string       `json:"physical_interface"`
	MTU               int          `json:"mtu,omitempty"`
	StatsInterval     int          `json:"stats_interval,omitempty"`
	HookTimeout       int          `json:"hook_timeout,omitempty"`
	VLANs             []VLANConfig `json:"vlans"`
}

type VLANConfig struct {
	ID        uint16      `json:"id"`
	Interface string      `json:"interface"`
	Scripts   *ScriptHooks `json:"scripts,omitempty"`
}

type ScriptHooks struct {
	PostUp  string `json:"post_up,omitempty"`
	PreDown string `json:"pre_down,omitempty"`
}

type runtimeVLAN struct {
	config VLANConfig
	tap    *os.File
	name   string
	mac    net.HardwareAddr
	stats  vlanStats

	postUpSucceeded bool
}

type vlanStats struct {
	txTapFrames atomic.Uint64
	txTagged    atomic.Uint64
	txErrors    atomic.Uint64

	rxInline    atomic.Uint64
	rxAux       atomic.Uint64
	rxAccepted  atomic.Uint64
	rxTapFrames atomic.Uint64
	rxErrors    atomic.Uint64
}

type globalStats struct {
	rxPhysical atomic.Uint64
	rxOutgoing atomic.Uint64
	rxUntagged atomic.Uint64
	rxUnknown  atomic.Uint64
}

type ifreqFlags struct {
	Name  [unix.IFNAMSIZ]byte
	Flags uint16
	_     [22]byte
}

type ifreqMTU struct {
	Name [unix.IFNAMSIZ]byte
	MTU  int32
	_    [20]byte
}

type sockaddr struct {
	Family uint16
	Data   [14]byte
}

type ifreqHWAddr struct {
	Name [unix.IFNAMSIZ]byte
	Addr sockaddr
	_    [8]byte
}

type tpacketAuxdata struct {
	Status   uint32
	Len      uint32
	Snaplen  uint32
	Mac      uint16
	Net      uint16
	VlanTCI  uint16
	VlanTPID uint16
}

func htons(v uint16) uint16 {
	return (v << 8) | (v >> 8)
}

func usage() {
	fmt.Fprintf(os.Stderr, `vlan2tap %s

Userspace 802.1Q VLAN interfaces using TAP + AF_PACKET.

Usage:

  vlan2tap run
  vlan2tap run --config /etc/vlan2tap/config.json

  vlan2tap check
  vlan2tap check --config /etc/vlan2tap/config.json

  vlan2tap --install
  vlan2tap --install --physical eth0 --vlan 20 --tap mgmt

  vlan2tap --uninstall

  vlan2tap --version

Default config:

  %s

Example:

{
  "physical_interface": "eth0",
  "mtu": 1500,
  "stats_interval": 0,
  "hook_timeout": 30,
  "vlans": [
    {
      "id": 20,
      "interface": "mgmt",
      "scripts": {
        "post_up": "/etc/vlan2tap/mgmt-up.sh",
        "pre_down": "/etc/vlan2tap/mgmt-down.sh"
      }
    }
  ]
}

Hook environment:

  VLAN2TAP_EVENT
  VLAN2TAP_PHYSICAL
  VLAN2TAP_INTERFACE
  VLAN2TAP_VLAN_ID
  VLAN2TAP_MTU
  VLAN2TAP_MAC

`, version, defaultConfigPath)
}

func defaultConfig() Config {
	return Config{
		PhysicalInterface: "eth0",
		MTU:               defaultMTU,
		StatsInterval:     0,
		HookTimeout:       defaultHookTimeout,
	}
}

func normalizeConfig(cfg *Config) {
	if cfg.MTU == 0 {
		cfg.MTU = defaultMTU
	}

	if cfg.HookTimeout == 0 {
		cfg.HookTimeout = defaultHookTimeout
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf(
			"parse %s: %w",
			path,
			err,
		)
	}

	normalizeConfig(&cfg)

	if err := validateConfig(cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func validateConfig(cfg Config) error {
	if cfg.PhysicalInterface == "" {
		return errors.New(
			"physical_interface is required",
		)
	}

	if len(cfg.PhysicalInterface) >= unix.IFNAMSIZ {
		return fmt.Errorf(
			"physical interface name %q is too long",
			cfg.PhysicalInterface,
		)
	}

	if cfg.MTU < 68 || cfg.MTU > 9000 {
		return fmt.Errorf(
			"invalid MTU %d",
			cfg.MTU,
		)
	}

	if cfg.StatsInterval < 0 {
		return errors.New(
			"stats_interval cannot be negative",
		)
	}

	if cfg.HookTimeout <= 0 {
		return errors.New(
			"hook_timeout must be greater than zero",
		)
	}

	if len(cfg.VLANs) == 0 {
		return errors.New(
			"at least one VLAN must be configured",
		)
	}

	ids := make(map[uint16]bool)
	names := make(map[string]bool)

	for _, v := range cfg.VLANs {
		if v.ID == 0 || v.ID > 4094 {
			return fmt.Errorf(
				"invalid VLAN ID %d",
				v.ID,
			)
		}

		if v.Interface == "" {
			return fmt.Errorf(
				"VLAN %d has no interface name",
				v.ID,
			)
		}

		if len(v.Interface) >= unix.IFNAMSIZ {
			return fmt.Errorf(
				"interface name %q is too long",
				v.Interface,
			)
		}

		if v.Interface == cfg.PhysicalInterface {
			return fmt.Errorf(
				"VLAN %d TAP interface cannot have the same name as physical interface %q",
				v.ID,
				cfg.PhysicalInterface,
			)
		}

		if ids[v.ID] {
			return fmt.Errorf(
				"duplicate VLAN ID %d",
				v.ID,
			)
		}

		if names[v.Interface] {
			return fmt.Errorf(
				"duplicate TAP interface %q",
				v.Interface,
			)
		}

		ids[v.ID] = true
		names[v.Interface] = true

		if v.Scripts != nil {
			if err := validateHook(
				v.ID,
				"post_up",
				v.Scripts.PostUp,
			); err != nil {
				return err
			}

			if err := validateHook(
				v.ID,
				"pre_down",
				v.Scripts.PreDown,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

func validateHook(
	vlanID uint16,
	name string,
	path string,
) error {
	if path == "" {
		return nil
	}

	if !filepath.IsAbs(path) {
		return fmt.Errorf(
			"VLAN %d %s script must use an absolute path: %q",
			vlanID,
			name,
			path,
		)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf(
			"VLAN %d %s script %s: %w",
			vlanID,
			name,
			path,
			err,
		)
	}

	if info.IsDir() {
		return fmt.Errorf(
			"VLAN %d %s script is a directory: %s",
			vlanID,
			name,
			path,
		)
	}

	if info.Mode()&0111 == 0 {
		return fmt.Errorf(
			"VLAN %d %s script is not executable: %s",
			vlanID,
			name,
			path,
		)
	}

	return nil
}

func writeConfig(path string, cfg Config) error {
	normalizeConfig(&cfg)

	if err := validateConfigWithoutHooks(cfg); err != nil {
		return err
	}

	data, err := json.MarshalIndent(
		cfg,
		"",
		"  ",
	)
	if err != nil {
		return err
	}

	data = append(data, '\n')

	if err := os.MkdirAll(
		filepath.Dir(path),
		0755,
	); err != nil {
		return err
	}

	return os.WriteFile(
		path,
		data,
		0644,
	)
}

/*
validateConfigWithoutHooks is used when --install is creating a
new config. There cannot be configured hook paths in that generated
config, but keeping filesystem validation out of this helper makes
the intent explicit.
*/
func validateConfigWithoutHooks(cfg Config) error {
	copyCfg := cfg

	for i := range copyCfg.VLANs {
		copyCfg.VLANs[i].Scripts = nil
	}

	return validateConfig(copyCfg)
}

func createTAP(name string) (*os.File, string, error) {
	f, err := os.OpenFile(
		tunDevice,
		os.O_RDWR,
		0,
	)
	if err != nil {
		return nil, "", fmt.Errorf(
			"open %s: %w",
			tunDevice,
			err,
		)
	}

	var req ifreqFlags

	copy(req.Name[:], name)
	req.Flags = iffTAP | iffNoPI

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		f.Fd(),
		uintptr(unix.TUNSETIFF),
		uintptr(unsafe.Pointer(&req)),
	)

	if errno != 0 {
		_ = f.Close()

		return nil, "", fmt.Errorf(
			"TUNSETIFF %s: %w",
			name,
			errno,
		)
	}

	actualName := string(req.Name[:])

	for i, b := range req.Name {
		if b == 0 {
			actualName = string(req.Name[:i])
			break
		}
	}

	return f, actualName, nil
}

func openControlSocket() (int, error) {
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM,
		0,
	)
	if err != nil {
		return -1, err
	}

	return fd, nil
}

func setInterfaceMTU(
	name string,
	mtu int,
) error {
	fd, err := openControlSocket()
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	var req ifreqMTU

	copy(req.Name[:], name)
	req.MTU = int32(mtu)

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCSIFMTU),
		uintptr(unsafe.Pointer(&req)),
	)

	if errno != 0 {
		return fmt.Errorf(
			"set MTU on %s: %w",
			name,
			errno,
		)
	}

	return nil
}

func setInterfaceMAC(
	name string,
	mac net.HardwareAddr,
) error {
	if len(mac) != 6 {
		return fmt.Errorf(
			"invalid Ethernet MAC %s",
			mac,
		)
	}

	fd, err := openControlSocket()
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	var req ifreqHWAddr

	copy(req.Name[:], name)

	req.Addr.Family = arphrdEther
	copy(req.Addr.Data[:], mac)

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCSIFHWADDR),
		uintptr(unsafe.Pointer(&req)),
	)

	if errno != 0 {
		return fmt.Errorf(
			"set MAC on %s: %w",
			name,
			errno,
		)
	}

	return nil
}

func setInterfaceUp(name string) error {
	fd, err := openControlSocket()
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	var req ifreqFlags

	copy(req.Name[:], name)

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFFLAGS),
		uintptr(unsafe.Pointer(&req)),
	)

	if errno != 0 {
		return fmt.Errorf(
			"get flags for %s: %w",
			name,
			errno,
		)
	}

	req.Flags |= iffUp

	_, _, errno = unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCSIFFLAGS),
		uintptr(unsafe.Pointer(&req)),
	)

	if errno != 0 {
		return fmt.Errorf(
			"bring %s up: %w",
			name,
			errno,
		)
	}

	return nil
}

func configureTAP(
	name string,
	mtu int,
	mac net.HardwareAddr,
) error {
	if err := setInterfaceMTU(
		name,
		mtu,
	); err != nil {
		return err
	}

	if err := setInterfaceMAC(
		name,
		mac,
	); err != nil {
		return err
	}

	if err := setInterfaceUp(name); err != nil {
		return err
	}

	return nil
}

func openPacketSocket(
	interfaceName string,
) (int, int, error) {
	iface, err := net.InterfaceByName(
		interfaceName,
	)
	if err != nil {
		return -1, 0, fmt.Errorf(
			"physical interface %s: %w",
			interfaceName,
			err,
		)
	}

	fd, err := unix.Socket(
		unix.AF_PACKET,
		unix.SOCK_RAW,
		int(htons(ethPAll)),
	)
	if err != nil {
		return -1, 0, fmt.Errorf(
			"create AF_PACKET socket: %w",
			err,
		)
	}

	if err := unix.SetsockoptInt(
		fd,
		unix.SOL_PACKET,
		unix.PACKET_AUXDATA,
		1,
	); err != nil {
		_ = unix.Close(fd)

		return -1, 0, fmt.Errorf(
			"enable PACKET_AUXDATA: %w",
			err,
		)
	}

	mreq := &unix.PacketMreq{
		Ifindex: int32(iface.Index),
		Type:    unix.PACKET_MR_PROMISC,
	}

	if err := unix.SetsockoptPacketMreq(
		fd,
		unix.SOL_PACKET,
		unix.PACKET_ADD_MEMBERSHIP,
		mreq,
	); err != nil {
		_ = unix.Close(fd)

		return -1, 0, fmt.Errorf(
			"enable packet promiscuous membership: %w",
			err,
		)
	}

	addr := &unix.SockaddrLinklayer{
		Protocol: htons(ethPAll),
		Ifindex:  iface.Index,
	}

	if err := unix.Bind(fd, addr); err != nil {
		_ = unix.Close(fd)

		return -1, 0, fmt.Errorf(
			"bind AF_PACKET to %s: %w",
			interfaceName,
			err,
		)
	}

	return fd, iface.Index, nil
}

func addVLANTag(
	dst []byte,
	src []byte,
	vlan uint16,
) ([]byte, error) {
	if len(src) < 14 {
		return nil, errors.New(
			"short Ethernet frame",
		)
	}

	if len(dst) < len(src)+vlanHeaderLen {
		return nil, errors.New(
			"output buffer too small",
		)
	}

	copy(dst[0:12], src[0:12])

	binary.BigEndian.PutUint16(
		dst[12:14],
		ethPVLAN,
	)

	binary.BigEndian.PutUint16(
		dst[14:16],
		vlan&0x0fff,
	)

	copy(dst[16:], src[12:])

	return dst[:len(src)+vlanHeaderLen], nil
}

func inlineVLAN(
	frame []byte,
) (uint16, bool) {
	if len(frame) < 18 {
		return 0, false
	}

	tpid := binary.BigEndian.Uint16(
		frame[12:14],
	)

	if tpid != ethPVLAN &&
		tpid != ethP8021AD {
		return 0, false
	}

	tci := binary.BigEndian.Uint16(
		frame[14:16],
	)

	return tci & 0x0fff, true
}

func stripInlineVLAN(
	dst []byte,
	src []byte,
) []byte {
	copy(dst[0:12], src[0:12])
	copy(dst[12:], src[16:])

	return dst[:len(src)-vlanHeaderLen]
}

func parseAuxdata(
	oob []byte,
) (uint16, uint16, bool) {
	msgs, err := unix.ParseSocketControlMessage(
		oob,
	)
	if err != nil {
		return 0, 0, false
	}

	for _, msg := range msgs {
		if msg.Header.Level != unix.SOL_PACKET ||
			msg.Header.Type != unix.PACKET_AUXDATA {
			continue
		}

		if len(msg.Data) <
			int(unsafe.Sizeof(tpacketAuxdata{})) {
			continue
		}

		aux := *(*tpacketAuxdata)(
			unsafe.Pointer(&msg.Data[0]),
		)

		if aux.Status&tpStatusVLANValid == 0 {
			continue
		}

		tpid := uint16(ethPVLAN)

		if aux.Status&tpStatusVLANTPIDValid != 0 &&
			aux.VlanTPID != 0 {
			tpid = aux.VlanTPID
		}

		return aux.VlanTCI & 0x0fff,
			tpid,
			true
	}

	return 0, 0, false
}

func writeTAP(
	v *runtimeVLAN,
	frame []byte,
) error {
	for {
		_, err := v.tap.Write(frame)

		if err == nil {
			v.stats.rxTapFrames.Add(1)
			return nil
		}

		if errors.Is(err, syscall.EINTR) {
			continue
		}

		v.stats.rxErrors.Add(1)

		if errors.Is(err, syscall.EAGAIN) ||
			errors.Is(err, syscall.ENOBUFS) ||
			errors.Is(err, syscall.EIO) {
			return nil
		}

		return err
	}
}

func tapTXLoop(
	v *runtimeVLAN,
	packetFD int,
	ifindex int,
) error {
	in := make([]byte, maxFrameSize)

	out := make(
		[]byte,
		maxFrameSize+vlanHeaderLen,
	)

	addr := &unix.SockaddrLinklayer{
		Ifindex:  ifindex,
		Protocol: htons(ethPAll),
	}

	for {
		n, err := v.tap.Read(in)

		if err != nil {
			return fmt.Errorf(
				"%s TAP read: %w",
				v.name,
				err,
			)
		}

		if n < 14 {
			continue
		}

		v.stats.txTapFrames.Add(1)

		frame, err := addVLANTag(
			out,
			in[:n],
			v.config.ID,
		)
		if err != nil {
			v.stats.txErrors.Add(1)
			continue
		}

		if err := unix.Sendto(
			packetFD,
			frame,
			0,
			addr,
		); err != nil {
			v.stats.txErrors.Add(1)

			return fmt.Errorf(
				"%s physical TX: %w",
				v.name,
				err,
			)
		}

		v.stats.txTagged.Add(1)
	}
}

func physicalRXLoop(
	packetFD int,
	vlans map[uint16]*runtimeVLAN,
	global *globalStats,
) error {
	in := make([]byte, maxFrameSize)
	out := make([]byte, maxFrameSize)
	oob := make([]byte, 256)

	for {
		n, oobn, _, from, err :=
			unix.Recvmsg(
				packetFD,
				in,
				oob,
				0,
			)

		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}

			return fmt.Errorf(
				"physical recvmsg: %w",
				err,
			)
		}

		if n < 14 {
			continue
		}

		global.rxPhysical.Add(1)

		ll, ok := from.(*unix.SockaddrLinklayer)
		if !ok {
			continue
		}

		if ll.Pkttype == unix.PACKET_OUTGOING {
			global.rxOutgoing.Add(1)
			continue
		}

		src := in[:n]

		/*
			Path A:
			VLAN header delivered inline.
		*/
		if vid, tagged := inlineVLAN(src); tagged {
			v := vlans[vid]

			if v == nil {
				global.rxUnknown.Add(1)
				continue
			}

			v.stats.rxInline.Add(1)
			v.stats.rxAccepted.Add(1)

			frame := stripInlineVLAN(
				out,
				src,
			)

			if err := writeTAP(
				v,
				frame,
			); err != nil {
				return fmt.Errorf(
					"%s TAP write: %w",
					v.name,
					err,
				)
			}

			continue
		}

		/*
			Path B:
			driver stripped the VLAN header and exposed
			the metadata through PACKET_AUXDATA.
		*/
		vid, tpid, tagged :=
			parseAuxdata(oob[:oobn])

		if tagged {
			if tpid != ethPVLAN &&
				tpid != ethP8021AD {
				global.rxUnknown.Add(1)
				continue
			}

			v := vlans[vid]

			if v == nil {
				global.rxUnknown.Add(1)
				continue
			}

			v.stats.rxAux.Add(1)
			v.stats.rxAccepted.Add(1)

			if err := writeTAP(
				v,
				src,
			); err != nil {
				return fmt.Errorf(
					"%s TAP write: %w",
					v.name,
					err,
				)
			}

			continue
		}

		/*
			Strict mode:
			untagged traffic received from the physical
			interface never enters a VLAN TAP.
		*/
		global.rxUntagged.Add(1)
	}
}

func hookEnvironment(
	cfg Config,
	v *runtimeVLAN,
	event string,
) []string {
	env := os.Environ()

	env = append(
		env,
		"VLAN2TAP_EVENT="+event,
		"VLAN2TAP_PHYSICAL="+cfg.PhysicalInterface,
		"VLAN2TAP_INTERFACE="+v.name,
		"VLAN2TAP_VLAN_ID="+strconv.Itoa(int(v.config.ID)),
		"VLAN2TAP_MTU="+strconv.Itoa(cfg.MTU),
		"VLAN2TAP_MAC="+v.mac.String(),
	)

	return env
}

func runHook(
	cfg Config,
	v *runtimeVLAN,
	event string,
	path string,
) error {
	if path == "" {
		return nil
	}

	log.Printf(
		"vlan=%d tap=%s: running %s hook: %s",
		v.config.ID,
		v.name,
		event,
		path,
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(cfg.HookTimeout)*time.Second,
	)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		path,
	)

	cmd.Env = hookEnvironment(
		cfg,
		v,
		event,
	)

	/*
		Forward hook output directly into vlan2tap's own
		stdout/stderr. Under S94vlan2tap this means the
		output goes into /var/log/vlan2tap.log.
	*/
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf(
			"%s hook timed out after %d seconds: %s",
			event,
			cfg.HookTimeout,
			path,
		)
	}

	if err != nil {
		return fmt.Errorf(
			"%s hook failed: %s: %w",
			event,
			path,
			err,
		)
	}

	log.Printf(
		"vlan=%d tap=%s: %s hook completed",
		v.config.ID,
		v.name,
		event,
	)

	return nil
}

func runPostUp(
	cfg Config,
	v *runtimeVLAN,
) error {
	if v.config.Scripts == nil ||
		v.config.Scripts.PostUp == "" {
		return nil
	}

	if err := runHook(
		cfg,
		v,
		"post-up",
		v.config.Scripts.PostUp,
	); err != nil {
		return err
	}

	v.postUpSucceeded = true

	return nil
}

func runPreDown(
	cfg Config,
	v *runtimeVLAN,
) error {
	if v.config.Scripts == nil ||
		v.config.Scripts.PreDown == "" {
		return nil
	}

	return runHook(
		cfg,
		v,
		"pre-down",
		v.config.Scripts.PreDown,
	)
}

func rollbackHooks(
	cfg Config,
	started []*runtimeVLAN,
) {
	/*
		Roll back in reverse startup order.
	*/
	for i := len(started) - 1; i >= 0; i-- {
		v := started[i]

		if !v.postUpSucceeded {
			continue
		}

		if err := runPreDown(
			cfg,
			v,
		); err != nil {
			log.Printf(
				"vlan=%d tap=%s: rollback pre-down failed: %v",
				v.config.ID,
				v.name,
				err,
			)
		}
	}
}

func printStats(
	vlans map[uint16]*runtimeVLAN,
	global *globalStats,
) {
	log.Printf(
		"physical: rx=%d outgoing=%d untagged=%d unknown_vlan=%d",
		global.rxPhysical.Load(),
		global.rxOutgoing.Load(),
		global.rxUntagged.Load(),
		global.rxUnknown.Load(),
	)

	ids := make(
		[]int,
		0,
		len(vlans),
	)

	for id := range vlans {
		ids = append(
			ids,
			int(id),
		)
	}

	sort.Ints(ids)

	for _, rawID := range ids {
		v := vlans[uint16(rawID)]

		log.Printf(
			"vlan=%d tap=%s "+
				"tx_tap=%d tx_tagged=%d tx_errors=%d "+
				"rx_inline=%d rx_aux=%d rx_accepted=%d "+
				"rx_tap=%d rx_errors=%d",
			v.config.ID,
			v.name,
			v.stats.txTapFrames.Load(),
			v.stats.txTagged.Load(),
			v.stats.txErrors.Load(),
			v.stats.rxInline.Load(),
			v.stats.rxAux.Load(),
			v.stats.rxAccepted.Load(),
			v.stats.rxTapFrames.Load(),
			v.stats.rxErrors.Load(),
		)
	}
}

func writePIDFile() error {
	return os.WriteFile(
		pidFilePath,
		[]byte(
			strconv.Itoa(os.Getpid()) + "\n",
		),
		0644,
	)
}

func removePIDFile() {
	_ = os.Remove(pidFilePath)
}

func run(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf(
			"load config: %w",
			err,
		)
	}

	physical, err := net.InterfaceByName(
		cfg.PhysicalInterface,
	)
	if err != nil {
		return fmt.Errorf(
			"physical interface %s: %w",
			cfg.PhysicalInterface,
			err,
		)
	}

	if len(physical.HardwareAddr) != 6 {
		return fmt.Errorf(
			"%s has no Ethernet MAC",
			cfg.PhysicalInterface,
		)
	}

	vlans := make(
		map[uint16]*runtimeVLAN,
	)

	var ordered []*runtimeVLAN
	var opened []*os.File

	cleanupTAPs := func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}

	/*
		Stage 1:
		Create and fully configure all TAPs.
	*/
	for _, vc := range cfg.VLANs {
		tap, actualName, err :=
			createTAP(vc.Interface)

		if err != nil {
			cleanupTAPs()
			return err
		}

		opened = append(
			opened,
			tap,
		)

		if err := configureTAP(
			actualName,
			cfg.MTU,
			physical.HardwareAddr,
		); err != nil {
			cleanupTAPs()
			return err
		}

		v := &runtimeVLAN{
			config: vc,
			tap:    tap,
			name:   actualName,
			mac: append(
				net.HardwareAddr(nil),
				physical.HardwareAddr...,
			),
		}

		vlans[vc.ID] = v

		ordered = append(
			ordered,
			v,
		)

		log.Printf(
			"created %s: vlan=%d mtu=%d mac=%s",
			actualName,
			vc.ID,
			cfg.MTU,
			v.mac,
		)
	}

	/*
		Stage 2:
		Open the physical packet socket only after every
		TAP is UP.
	*/
	packetFD, ifindex, err :=
		openPacketSocket(
			cfg.PhysicalInterface,
		)

	if err != nil {
		cleanupTAPs()
		return err
	}

	defer unix.Close(packetFD)
	defer cleanupTAPs()

	var global globalStats

	errs := make(
		chan error,
		len(vlans)+1,
	)

	/*
		Stage 3:
		Start forwarding before executing post-up.

		By the time a post-up script assigns an address
		to the TAP, both RX and TX forwarding paths are
		already active.
	*/
	for _, v := range ordered {
		go func(v *runtimeVLAN) {
			errs <- tapTXLoop(
				v,
				packetFD,
				ifindex,
			)
		}(v)
	}

	go func() {
		errs <- physicalRXLoop(
			packetFD,
			vlans,
			&global,
		)
	}()

	log.Printf(
		"vlan2tap %s datapath started: physical=%s ifindex=%d",
		version,
		cfg.PhysicalInterface,
		ifindex,
	)

	log.Printf(
		"PACKET_AUXDATA enabled; packet socket promiscuous membership enabled",
	)

	/*
		Give an immediately failing forwarding goroutine
		a chance to report failure before running hooks.
	*/
	select {
	case err := <-errs:
		return fmt.Errorf(
			"forwarder failed during startup: %w",
			err,
		)

	default:
	}

	/*
		Stage 4:
		Execute post-up hooks in config order.
	*/
	var started []*runtimeVLAN

	for _, v := range ordered {
		if err := runPostUp(
			cfg,
			v,
		); err != nil {
			log.Printf(
				"vlan=%d tap=%s: startup hook failed: %v",
				v.config.ID,
				v.name,
				err,
			)

			rollbackHooks(
				cfg,
				started,
			)

			return err
		}

		if v.postUpSucceeded {
			started = append(
				started,
				v,
			)
		}
	}

	/*
		Only publish the PID after the datapath and all
		post-up hooks have succeeded.

		This means S94vlan2tap does not report the service
		as successfully started while a post-up hook is
		still failing.
	*/
	if err := writePIDFile(); err != nil {
		rollbackHooks(
			cfg,
			started,
		)

		return fmt.Errorf(
			"write PID file: %w",
			err,
		)
	}

	defer removePIDFile()

	log.Printf(
		"vlan2tap %s ready",
		version,
	)

	if cfg.StatsInterval > 0 {
		go func() {
			ticker := time.NewTicker(
				time.Duration(
					cfg.StatsInterval,
				) * time.Second,
			)

			defer ticker.Stop()

			for range ticker.C {
				printStats(
					vlans,
					&global,
				)
			}
		}()
	}

	signals := make(
		chan os.Signal,
		1,
	)

	signal.Notify(
		signals,
		syscall.SIGINT,
		syscall.SIGTERM,
	)

	var result error

	select {
	case sig := <-signals:
		log.Printf(
			"received %s, shutting down",
			sig,
		)

	case err := <-errs:
		result = fmt.Errorf(
			"forwarder failed: %w",
			err,
		)

		log.Printf(
			"%v",
			result,
		)
	}

	/*
		Stage 5:
		pre-down executes while the TAPs and physical
		forwarding path still exist.

		Reverse order mirrors startup.
	*/
	for i := len(ordered) - 1; i >= 0; i-- {
		v := ordered[i]

		if err := runPreDown(
			cfg,
			v,
		); err != nil {
			/*
				pre-down is best-effort. Never prevent
				the daemon from shutting down.
			*/
			log.Printf(
				"vlan=%d tap=%s: pre-down failed: %v",
				v.config.ID,
				v.name,
				err,
			)
		}
	}

	printStats(
		vlans,
		&global,
	)

	log.Printf(
		"vlan2tap stopped",
	)

	return result
}

func check(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	fmt.Printf(
		"Config:             %s\n",
		configPath,
	)

	fmt.Printf(
		"Physical:           %s\n",
		cfg.PhysicalInterface,
	)

	fmt.Printf(
		"MTU:                %d\n",
		cfg.MTU,
	)

	fmt.Printf(
		"Hook timeout:       %ds\n",
		cfg.HookTimeout,
	)

	physical, err := net.InterfaceByName(
		cfg.PhysicalInterface,
	)
	if err != nil {
		return fmt.Errorf(
			"physical interface: %w",
			err,
		)
	}

	fmt.Printf(
		"Physical MAC:       %s\n",
		physical.HardwareAddr,
	)

	if _, err := os.Stat(tunDevice); err != nil {
		return fmt.Errorf(
			"%s unavailable: %w",
			tunDevice,
			err,
		)
	}

	fmt.Printf(
		"TUN/TAP:            available\n",
	)

	fd, _, err := openPacketSocket(
		cfg.PhysicalInterface,
	)
	if err != nil {
		return fmt.Errorf(
			"AF_PACKET test: %w",
			err,
		)
	}

	_ = unix.Close(fd)

	fmt.Printf(
		"AF_PACKET:          available\n",
	)

	fmt.Printf(
		"PACKET_AUXDATA:     available\n",
	)

	fmt.Printf(
		"\nVLAN interfaces:\n",
	)

	for _, v := range cfg.VLANs {
		fmt.Printf(
			"  VLAN %-4d -> %s\n",
			v.ID,
			v.Interface,
		)

		if v.Scripts != nil {
			if v.Scripts.PostUp != "" {
				fmt.Printf(
					"             post_up:  %s\n",
					v.Scripts.PostUp,
				)
			}

			if v.Scripts.PreDown != "" {
				fmt.Printf(
					"             pre_down: %s\n",
					v.Scripts.PreDown,
				)
			}
		}
	}

	fmt.Printf(
		"\nConfiguration OK.\n",
	)

	return nil
}

func copyFile(
	src string,
	dst string,
	mode os.FileMode,
) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".new"

	out, err := os.OpenFile(
		tmp,
		os.O_WRONLY|
			os.O_CREATE|
			os.O_TRUNC,
		mode,
	)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(
		out,
		in,
	)

	closeErr := out.Close()

	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}

	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}

	if err := os.Chmod(
		tmp,
		mode,
	); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	return os.Rename(
		tmp,
		dst,
	)
}

func serviceScript() string {
	return `#!/bin/sh

DAEMON="/usr/local/sbin/vlan2tap"
CONFIG="/etc/vlan2tap/config.json"
PIDFILE="/var/run/vlan2tap.pid"
LOGFILE="/var/log/vlan2tap.log"

is_running() {
    [ -f "$PIDFILE" ] || return 1

    PID="$(cat "$PIDFILE" 2>/dev/null)"
    [ -n "$PID" ] || return 1

    kill -0 "$PID" 2>/dev/null
}

start_service() {
    if is_running; then
        echo "vlan2tap already running"
        return 0
    fi

    rm -f "$PIDFILE"

    echo "Starting vlan2tap..."

    "$DAEMON" run --config "$CONFIG" \
        >>"$LOGFILE" 2>&1 &

    PID="$!"

    i=0

    while [ "$i" -lt 300 ]; do
        if [ -f "$PIDFILE" ]; then
            if kill -0 "$PID" 2>/dev/null; then
                echo "OK"
                return 0
            fi
        fi

        if ! kill -0 "$PID" 2>/dev/null; then
            echo "vlan2tap failed to start"
            tail -n 30 "$LOGFILE" 2>/dev/null
            return 1
        fi

        sleep 0.1
        i=$((i + 1))
    done

    echo "vlan2tap startup timed out"

    kill "$PID" 2>/dev/null

    return 1
}

stop_service() {
    if ! is_running; then
        rm -f "$PIDFILE"

        echo "vlan2tap not running"
        return 0
    fi

    PID="$(cat "$PIDFILE")"

    echo "Stopping vlan2tap..."

    kill "$PID" 2>/dev/null

    i=0

    while kill -0 "$PID" 2>/dev/null; do
        if [ "$i" -ge 350 ]; then
            echo "forcing vlan2tap to stop"

            kill -9 "$PID" 2>/dev/null
            break
        fi

        sleep 0.1
        i=$((i + 1))
    done

    rm -f "$PIDFILE"

    echo "OK"
}

case "$1" in
    start)
        start_service
        ;;

    stop)
        stop_service
        ;;

    restart)
        stop_service || exit $?
        start_service
        ;;

    status)
        if is_running; then
            echo "vlan2tap is running (PID $(cat "$PIDFILE"))"
            exit 0
        fi

        echo "vlan2tap is not running"
        exit 1
        ;;

    *)
        echo "Usage: $0 {start|stop|restart|status}"
        exit 1
        ;;
esac
`
}

func install(
	configPath string,
	physical string,
	vlanID int,
	tapName string,
) error {
	if os.Geteuid() != 0 {
		return errors.New(
			"--install must be run as root",
		)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}

	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}

	fmt.Printf(
		"Installing vlan2tap %s\n",
		version,
	)

	if err := os.MkdirAll(
		defaultConfigDir,
		0755,
	); err != nil {
		return fmt.Errorf(
			"create %s: %w",
			defaultConfigDir,
			err,
		)
	}

	if err := os.MkdirAll(
		filepath.Dir(installBinaryPath),
		0755,
	); err != nil {
		return err
	}

	if self != installBinaryPath {
		if err := copyFile(
			self,
			installBinaryPath,
			0755,
		); err != nil {
			return fmt.Errorf(
				"install binary: %w",
				err,
			)
		}
	}

	fmt.Printf(
		"Installed binary: %s\n",
		installBinaryPath,
	)

	/*
		Do not overwrite an existing administrator
		configuration.
	*/
	if _, err := os.Stat(
		defaultConfigPath,
	); err == nil {
		fmt.Printf(
			"Keeping existing config: %s\n",
			defaultConfigPath,
		)

	} else if !os.IsNotExist(err) {
		return err

	} else {
		/*
			If a custom config was explicitly supplied,
			install that as the canonical service config.
		*/
		if configPath != defaultConfigPath {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return err
			}

			if err := writeConfig(
				defaultConfigPath,
				cfg,
			); err != nil {
				return err
			}

			fmt.Printf(
				"Installed config: %s\n",
				defaultConfigPath,
			)

		} else {
			if vlanID == 0 {
				return fmt.Errorf(
					"%s does not exist; specify --vlan and --tap",
					defaultConfigPath,
				)
			}

			if vlanID < 1 || vlanID > 4094 {
				return errors.New(
					"--vlan must be between 1 and 4094",
				)
			}

			if tapName == "" {
				return errors.New(
					"--tap is required when creating the initial config",
				)
			}

			cfg := defaultConfig()

			cfg.PhysicalInterface = physical

			cfg.VLANs = []VLANConfig{
				{
					ID:        uint16(vlanID),
					Interface: tapName,
				},
			}

			if err := writeConfig(
				defaultConfigPath,
				cfg,
			); err != nil {
				return fmt.Errorf(
					"write config: %w",
					err,
				)
			}

			fmt.Printf(
				"Created config: %s\n",
				defaultConfigPath,
			)
		}
	}

	if err := os.WriteFile(
		servicePath,
		[]byte(serviceScript()),
		0755,
	); err != nil {
		return fmt.Errorf(
			"write service: %w",
			err,
		)
	}

	fmt.Printf(
		"Installed service: %s\n",
		servicePath,
	)

	/*
		We have verified NanoKVM's /etc/init.d/rcS uses:

		    for i in /etc/init.d/S??*

		Therefore S94vlan2tap is sufficient. Do not
		modify the vendor rcS.
	*/

	if err := check(
		defaultConfigPath,
	); err != nil {
		return fmt.Errorf(
			"installed configuration check failed: %w",
			err,
		)
	}

	fmt.Printf(
		"\nStarting service...\n",
	)

	cmd := exec.Command(
		servicePath,
		"restart",
	)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf(
			"start service: %w",
			err,
		)
	}

	fmt.Printf(
		"\nInstallation complete.\n",
	)

	fmt.Printf(
		"Config:  %s\n",
		defaultConfigPath,
	)

	fmt.Printf(
		"Service: %s\n",
		servicePath,
	)

	fmt.Printf(
		"Log:     %s\n",
		logFilePath,
	)

	return nil
}

func uninstall() error {
	if os.Geteuid() != 0 {
		return errors.New(
			"--uninstall must be run as root",
		)
	}

	if _, err := os.Stat(servicePath); err == nil {
		cmd := exec.Command(
			servicePath,
			"stop",
		)

		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		_ = cmd.Run()
	}

	_ = os.Remove(servicePath)
	_ = os.Remove(installBinaryPath)
	_ = os.Remove(pidFilePath)

	/*
		Never delete /etc/vlan2tap automatically.
		It may contain administrator-created hook scripts.
	*/
	fmt.Printf(
		"vlan2tap uninstalled.\n",
	)

	fmt.Printf(
		"Configuration preserved: %s\n",
		defaultConfigDir,
	)

	return nil
}

func main() {
	log.SetFlags(
		log.Ldate |
			log.Ltime |
			log.Lmicroseconds,
	)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "--version", "version":
		fmt.Printf(
			"vlan2tap %s\n",
			version,
		)

	case "run":
		fs := flag.NewFlagSet(
			"run",
			flag.ExitOnError,
		)

		configPath := fs.String(
			"config",
			defaultConfigPath,
			"configuration file",
		)

		_ = fs.Parse(os.Args[2:])

		if err := run(
			*configPath,
		); err != nil {
			log.Fatal(err)
		}

	case "check":
		fs := flag.NewFlagSet(
			"check",
			flag.ExitOnError,
		)

		configPath := fs.String(
			"config",
			defaultConfigPath,
			"configuration file",
		)

		_ = fs.Parse(os.Args[2:])

		if err := check(
			*configPath,
		); err != nil {
			log.Fatal(err)
		}

	case "--install":
		fs := flag.NewFlagSet(
			"--install",
			flag.ExitOnError,
		)

		configPath := fs.String(
			"config",
			defaultConfigPath,
			"configuration file to install",
		)

		physical := fs.String(
			"physical",
			"eth0",
			"physical Ethernet interface",
		)

		vlanID := fs.Int(
			"vlan",
			0,
			"initial VLAN ID",
		)

		tapName := fs.String(
			"tap",
			"",
			"initial TAP interface name",
		)

		_ = fs.Parse(os.Args[2:])

		if err := install(
			*configPath,
			*physical,
			*vlanID,
			*tapName,
		); err != nil {
			log.Fatal(err)
		}

	case "--uninstall":
		if err := uninstall(); err != nil {
			log.Fatal(err)
		}

	case "-h", "--help", "help":
		usage()

	default:
		fmt.Fprintf(
			os.Stderr,
			"unknown command: %s\n\n",
			os.Args[1],
		)

		usage()
		os.Exit(2)
	}
}
