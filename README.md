# vlan2tap

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`vlan2tap` provides userspace IEEE 802.1Q VLAN interfaces on Linux systems where kernel VLAN support (`CONFIG_VLAN_8021Q`) is unavailable.

It uses:

- Linux TAP interfaces for the local VLAN interfaces
- `AF_PACKET` raw sockets for access to the physical Ethernet interface
- IEEE 802.1Q tagging for transmitted frames
- `PACKET_AUXDATA` to recover VLAN metadata when the network driver strips VLAN headers
- Optional per-VLAN lifecycle scripts
- A simple JSON configuration file

The project was originally developed for the **Sipeed NanoKVM**, whose stock Linux kernel may be built without `CONFIG_VLAN_8021Q`, making normal Linux VLAN interfaces such as `eth0.20` unavailable.

No custom kernel or kernel module is required.

---

## How it works

Normally, Linux VLAN interfaces are created with something similar to:

```sh
ip link add link eth0 name eth0.20 type vlan id 20
```

This requires VLAN support in the kernel.

`vlan2tap` instead implements the VLAN boundary in userspace:

```text
                     Linux network stack
                            │
                            │ untagged Ethernet
                            │
                         ┌──────┐
                         │ mgmt │  TAP
                         └───┬──┘
                             │
                             │
                      ┌──────▼──────┐
                      │  vlan2tap   │
                      │             │
                      │ TX: add tag │
                      │ RX: remove  │
                      │     tag     │
                      └──────┬──────┘
                             │
                             │ VLAN 20 tagged
                             │
                           eth0
                             │
                             │
                           switch
```

From the Linux network stack's perspective, `mgmt` behaves like a normal Ethernet interface.

Frames transmitted through `mgmt` are tagged with VLAN 20 before being transmitted through `eth0`.

Frames received on `eth0` for VLAN 20 have their VLAN information removed before being injected into `mgmt`.

---

## Requirements

The Linux kernel must provide:

- TUN/TAP (`CONFIG_TUN`)
- Packet sockets (`CONFIG_PACKET`)
- `/dev/net/tun`
- `AF_PACKET`
- `PACKET_AUXDATA`

Kernel `CONFIG_VLAN_8021Q` support is **not required**.

The program needs root privileges because it:

- Creates TAP interfaces
- Configures network interfaces
- Opens raw `AF_PACKET` sockets
- Enables packet-socket promiscuous membership

You can verify TUN/TAP support with:

```sh
ls -l /dev/net/tun
```

---

## Building

`vlan2tap` is written in Go.

Clone the repository and download dependencies:

```sh
go mod tidy
```

For a normal Linux build:

```sh
go build -trimpath -ldflags="-s -w" -o vlan2tap .
```

### NanoKVM / RISC-V

NanoKVM uses RISC-V Linux.

Cross-compile a static binary with:

```sh
CGO_ENABLED=0 \
GOOS=linux \
GOARCH=riscv64 \
go build \
  -trimpath \
  -ldflags="-s -w" \
  -o vlan2tap .
```

Verify the resulting binary:

```sh
file vlan2tap
```

---

# Configuration

The default configuration file is:

```text
/etc/vlan2tap/config.json
```

A minimal configuration looks like:

```json
{
  "physical_interface": "eth0",
  "mtu": 1500,
  "stats_interval": 0,
  "hook_timeout": 30,
  "vlans": [
    {
      "id": 20,
      "interface": "mgmt"
    }
  ]
}
```

## Configuration options

### `physical_interface`

Physical Ethernet interface carrying the tagged VLANs.

Example:

```json
"physical_interface": "eth0"
```

---

### `mtu`

MTU assigned to the TAP interfaces.

Default:

```json
"mtu": 1500
```

---

### `stats_interval`

Interval in seconds for periodic forwarding statistics.

Set to `0` to disable periodic statistics.

Example:

```json
"stats_interval": 10
```

---

### `hook_timeout`

Maximum number of seconds a lifecycle script may run.

Default:

```json
"hook_timeout": 30
```

If a `post_up` script exceeds this timeout, startup fails.

A `pre_down` timeout is logged but does not prevent shutdown.

---

### `vlans`

List of VLAN interfaces managed by `vlan2tap`.

Example:

```json
"vlans": [
  {
    "id": 20,
    "interface": "mgmt"
  },
  {
    "id": 30,
    "interface": "servers"
  }
]
```

This creates:

```text
VLAN 20 <-> mgmt
VLAN 30 <-> servers
```

Both VLANs use the physical interface specified by `physical_interface`.

---

# Lifecycle scripts

Each VLAN can optionally execute scripts when its interface becomes available or is about to be removed.

Example:

```json
{
  "id": 20,
  "interface": "mgmt",
  "scripts": {
    "post_up": "/etc/vlan2tap/mgmt-up.sh",
    "pre_down": "/etc/vlan2tap/mgmt-down.sh"
  }
}
```

Scripts are optional.

For example, this is also valid:

```json
{
  "id": 30,
  "interface": "servers"
}
```

## `post_up`

Executed after:

1. The TAP interface has been created.
2. Its MTU has been configured.
3. Its MAC address has been configured.
4. The TAP has been brought UP.
5. The `AF_PACKET` socket has been opened.
6. RX/TX forwarding has started.

This means the VLAN datapath is operational before `post_up` executes.

A common use is assigning IP addresses and routes.

If `post_up` fails or times out, `vlan2tap` startup fails.

---

## `pre_down`

Executed before the TAP interface and forwarding path are removed.

It can be used to remove:

- IP addresses
- Routes
- Policy routing
- Other configuration associated with the interface

Failures are logged, but shutdown continues.

---

## Hook environment

Lifecycle scripts receive the following environment variables:

```text
VLAN2TAP_EVENT
VLAN2TAP_PHYSICAL
VLAN2TAP_INTERFACE
VLAN2TAP_VLAN_ID
VLAN2TAP_MTU
VLAN2TAP_MAC
```

For example, a VLAN 20 `post_up` script may receive:

```text
VLAN2TAP_EVENT=post-up
VLAN2TAP_PHYSICAL=eth0
VLAN2TAP_INTERFACE=mgmt
VLAN2TAP_VLAN_ID=20
VLAN2TAP_MTU=1500
VLAN2TAP_MAC=48:da:35:6f:08:9d
```

Scripts are executed directly rather than through `sh -c`.

They therefore need to be executable:

```sh
chmod +x /etc/vlan2tap/mgmt-up.sh
chmod +x /etc/vlan2tap/mgmt-down.sh
```

The script path must be absolute.

---

# Static management IP example

The following configuration creates a VLAN 20 management interface:

```json
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
```

Example `/etc/vlan2tap/mgmt-up.sh`:

```sh
#!/bin/sh
set -eu

ip addr flush dev "$VLAN2TAP_INTERFACE"

ip addr add \
    10.10.10.2/24 \
    dev "$VLAN2TAP_INTERFACE"

ip route replace \
    default \
    via 10.10.10.1 \
    dev "$VLAN2TAP_INTERFACE"

ip route replace \
    10.10.20.0/24 \
    via 10.10.10.3 \
    dev "$VLAN2TAP_INTERFACE"
```

Example `/etc/vlan2tap/mgmt-down.sh`:

```sh
#!/bin/sh

ip route del \
    10.10.20.0/24 \
    via 10.10.10.3 \
    dev "$VLAN2TAP_INTERFACE" \
    2>/dev/null || true

ip route del \
    default \
    via 10.10.10.1 \
    dev "$VLAN2TAP_INTERFACE" \
    2>/dev/null || true

ip addr flush dev "$VLAN2TAP_INTERFACE" \
    2>/dev/null || true
```

Make both executable:

```sh
chmod +x /etc/vlan2tap/mgmt-up.sh
chmod +x /etc/vlan2tap/mgmt-down.sh
```

---

# Running manually

Validate the configuration first:

```sh
vlan2tap check
```

A successful check resembles:

```text
Config:             /etc/vlan2tap/config.json
Physical:           eth0
MTU:                1500
Hook timeout:       30s
Physical MAC:       48:da:35:6f:08:9d
TUN/TAP:            available
AF_PACKET:          available
PACKET_AUXDATA:     available

VLAN interfaces:
  VLAN 20   -> mgmt
             post_up:  /etc/vlan2tap/mgmt-up.sh
             pre_down: /etc/vlan2tap/mgmt-down.sh

Configuration OK.
```

Start the daemon:

```sh
vlan2tap run
```

A different configuration can be specified with:

```sh
vlan2tap run --config /path/to/config.json
```

---

# Installation

`vlan2tap` can install itself as a boot service.

Run the binary as root:

```sh
./vlan2tap --install \
    --physical eth0 \
    --vlan 20 \
    --tap mgmt
```

On a new installation this creates:

```text
/usr/local/sbin/vlan2tap
/etc/vlan2tap/config.json
/etc/init.d/S94vlan2tap
```

The generated initial configuration does not contain lifecycle scripts.

You can add them afterward if needed.

## Existing configuration

If:

```text
/etc/vlan2tap/config.json
```

already exists, `--install` preserves it.

This allows the same command to be used when upgrading the `vlan2tap` binary without destroying administrator configuration.

After installation the service is automatically started.

---

# Service management

Start:

```sh
/etc/init.d/S94vlan2tap start
```

Stop:

```sh
/etc/init.d/S94vlan2tap stop
```

Restart:

```sh
/etc/init.d/S94vlan2tap restart
```

Status:

```sh
/etc/init.d/S94vlan2tap status
```

Logs are written to:

```text
/var/log/vlan2tap.log
```

For example:

```sh
tail -f /var/log/vlan2tap.log
```

---

# NanoKVM installation

Build the RISC-V binary on another machine:

```sh
CGO_ENABLED=0 \
GOOS=linux \
GOARCH=riscv64 \
go build \
  -trimpath \
  -ldflags="-s -w" \
  -o vlan2tap .
```

Copy it to NanoKVM:

```sh
scp vlan2tap root@NANOKVM_IP:/tmp/vlan2tap
```

Then on NanoKVM:

```sh
chmod +x /tmp/vlan2tap
```

Install:

```sh
/tmp/vlan2tap --install \
    --physical eth0 \
    --vlan 20 \
    --tap mgmt
```

Verify:

```sh
/etc/init.d/S94vlan2tap status
```

Check the TAP:

```sh
ip -d link show mgmt
```

The TAP should be UP:

```text
mgmt: <BROADCAST,MULTICAST,UP,LOWER_UP>
```

The TAP MAC address is intentionally copied from the physical interface.

Verify:

```sh
cat /sys/class/net/eth0/address
cat /sys/class/net/mgmt/address
```

Both should contain the same MAC address.

---

# Boot behavior on NanoKVM

NanoKVM uses the standard Buildroot-style `/etc/init.d/rcS` startup loop:

```sh
for i in /etc/init.d/S??* ;do
    ...
done
```

The installed service is:

```text
/etc/init.d/S94vlan2tap
```

It therefore starts automatically during boot.

No modification to `/etc/init.d/rcS` is required.

The service creates all configured TAP interfaces and establishes the VLAN forwarding path before reporting startup success.

---

# Multiple VLANs

Multiple VLANs can share one physical Ethernet interface.

Example:

```json
{
  "physical_interface": "eth0",
  "mtu": 1500,
  "stats_interval": 10,
  "hook_timeout": 30,
  "vlans": [
    {
      "id": 20,
      "interface": "mgmt",
      "scripts": {
        "post_up": "/etc/vlan2tap/mgmt-up.sh",
        "pre_down": "/etc/vlan2tap/mgmt-down.sh"
      }
    },
    {
      "id": 30,
      "interface": "servers"
    },
    {
      "id": 40,
      "interface": "storage"
    }
  ]
}
```

This results in:

```text
                       ┌── mgmt     VLAN 20
                       │
Linux ─── vlan2tap ────┼── servers  VLAN 30
                       │
                       └── storage  VLAN 40
                              │
                             eth0
```

Each TAP receives only traffic belonging to its configured VLAN.

---

# VLAN receive handling

Some Ethernet drivers preserve the 802.1Q header when passing a frame to an `AF_PACKET` socket.

In that case `vlan2tap` receives:

```text
DST MAC
SRC MAC
0x8100
VLAN TCI
EtherType
Payload
```

`vlan2tap` removes the VLAN header before injecting the frame into the appropriate TAP.

Other drivers perform VLAN receive offload and strip the VLAN header before the packet reaches the packet socket.

Linux can expose the removed VLAN information using `PACKET_AUXDATA`.

`vlan2tap` enables `PACKET_AUXDATA` and supports both receive paths:

```text
                  incoming VLAN frame
                          │
              ┌───────────┴───────────┐
              │                       │
        inline VLAN header      driver stripped tag
              │                       │
        parse 802.1Q TCI        PACKET_AUXDATA
              │                       │
              └───────────┬───────────┘
                          │
                    select VLAN
                          │
                    appropriate TAP
```

This is important on platforms such as NanoKVM where the Ethernet driver may strip the VLAN header.

---

# Untagged traffic

`vlan2tap` operates in strict VLAN mode.

Untagged traffic received on the physical interface is **not** injected into any configured TAP.

For example, with:

```json
{
  "id": 20,
  "interface": "mgmt"
}
```

only VLAN 20 traffic reaches `mgmt`.

```text
eth0 RX
   │
   ├── VLAN 20 ──────> mgmt
   │
   ├── VLAN 30 ──────> dropped unless configured
   │
   └── untagged ─────> dropped by vlan2tap
```

Note that `vlan2tap` does not prevent the normal Linux network stack from using the physical interface itself.

If a fully tagged-only setup is desired, remove IP configuration from `eth0` and configure the switch port appropriately.

---

# Promiscuous mode

`vlan2tap` uses `PACKET_MR_PROMISC` on its `AF_PACKET` socket.

This creates promiscuous membership for the packet socket without permanently enabling the physical interface's global promiscuous flag.

Therefore this is normal:

```sh
ip link show eth0
```

showing no global `PROMISC` flag while `vlan2tap` is operating.

---

# Statistics

Set:

```json
"stats_interval": 10
```

to print forwarding statistics every 10 seconds.

Example:

```text
physical: rx=184 outgoing=51 untagged=12 unknown_vlan=3
vlan=20 tap=mgmt tx_tap=42 tx_tagged=42 tx_errors=0 rx_inline=0 rx_aux=78 rx_accepted=78 rx_tap=78 rx_errors=0
```

Of particular interest:

- `tx_tap` — frames read from the TAP
- `tx_tagged` — frames successfully transmitted with a VLAN tag
- `rx_inline` — received frames with an inline VLAN header
- `rx_aux` — received frames whose VLAN was recovered through `PACKET_AUXDATA`
- `rx_accepted` — received frames matching a configured VLAN
- `rx_tap` — frames successfully written into the TAP
- `rx_errors` / `tx_errors` — forwarding errors
- `untagged` — physical RX frames without VLAN metadata
- `unknown_vlan` — tagged frames for VLANs not configured in `vlan2tap`

Use:

```json
"stats_interval": 0
```

to disable periodic statistics.

Final statistics are still printed when the daemon shuts down normally.

---

# DHCP

DHCP works normally over a `vlan2tap` interface.

For example:

```sh
udhcpc -i mgmt
```

produces DHCP traffic through:

```text
udhcpc
   │
 mgmt
   │
vlan2tap
   │
802.1Q VLAN 20
   │
 eth0
   │
switch
```

The DHCP server sees the MAC address of the TAP, which by default is copied from the physical Ethernet interface.

This makes existing DHCP reservations based on the physical device MAC usable through the VLAN interface.

---

# Checking the configuration

Run:

```sh
vlan2tap check
```

The command validates:

- JSON configuration
- Physical interface
- VLAN IDs
- Duplicate VLAN IDs
- Duplicate TAP names
- TUN/TAP availability
- `AF_PACKET` availability
- `PACKET_AUXDATA`
- Lifecycle script paths
- Script executable permissions

It does not start the forwarding service.

---

# Upgrading

Build the new binary and copy it to the device:

```sh
scp vlan2tap root@NANOKVM_IP:/tmp/vlan2tap
```

Then:

```sh
chmod +x /tmp/vlan2tap
/tmp/vlan2tap --install
```

Existing:

```text
/etc/vlan2tap/config.json
```

is preserved.

Administrator-created lifecycle scripts are also untouched.

---

# Uninstalling

Run:

```sh
/usr/local/sbin/vlan2tap --uninstall
```

This stops the service and removes:

```text
/usr/local/sbin/vlan2tap
/etc/init.d/S94vlan2tap
```

The configuration directory is deliberately preserved:

```text
/etc/vlan2tap/
```

This prevents `--uninstall` from accidentally deleting administrator-created scripts or configuration.

Remove it manually if it is no longer required:

```sh
rm -rf /etc/vlan2tap
```

---

# Troubleshooting

## `/dev/net/tun` does not exist

Check:

```sh
ls -l /dev/net/tun
```

The kernel must provide TUN/TAP support.

---

## `Operation not permitted`

Run `vlan2tap` as root.

The daemon needs privileges for raw packet sockets and network-interface configuration.

---

## Hook fails during startup

Check:

```sh
tail -n 100 /var/log/vlan2tap.log
```

Also run:

```sh
vlan2tap check
```

Make sure the hook:

- Exists
- Uses an absolute path
- Is executable
- Has a valid interpreter in its shebang

For example:

```sh
#!/bin/sh
```

and:

```sh
chmod +x /etc/vlan2tap/mgmt-up.sh
```

---

## TAP exists but receives no traffic

Check the switch configuration and verify that the expected VLAN is tagged on the physical port.

Capture the physical interface:

```sh
tcpdump -eni eth0
```

For VLAN 20, you should see traffic similar to:

```text
ethertype 802.1Q (0x8100), vlan 20
```

Then capture the TAP:

```sh
tcpdump -eni mgmt
```

Traffic on the TAP should appear without the 802.1Q header.

---

## VLAN TX works but RX does not

Check daemon statistics.

If the NIC strips VLAN tags, successful receive traffic should increase:

```text
rx_aux
```

rather than:

```text
rx_inline
```

This is expected and is the reason `vlan2tap` supports `PACKET_AUXDATA`.

---

# Safety when migrating a management interface

When using `vlan2tap` to move the management address of a remote device from untagged Ethernet to a tagged VLAN, do not remove the existing management path until the VLAN path has been tested.

A safer migration sequence is:

1. Install `vlan2tap`.
2. Verify the service starts automatically.
3. Create the management VLAN TAP.
4. Test tagged VLAN connectivity using a temporary IP or DHCP.
5. Configure the `post_up` lifecycle script.
6. Verify the management address works through the TAP.
7. Reboot and verify management connectivity again.
8. Only then disable/reject untagged management traffic on the switch port.

Changing the switch to tagged-only before validating the boot-time VLAN path can make a remote device unreachable.

---

# Design limitations

`vlan2tap` is a userspace workaround for systems without kernel VLAN support.

Compared with native kernel VLAN interfaces, it introduces:

- Additional packet copies
- Context switches between kernel and userspace
- Higher CPU usage
- Lower maximum throughput

It is intended primarily for embedded systems and management interfaces where native `8021q` support is unavailable.

For systems with a kernel that provides `CONFIG_VLAN_8021Q`, native Linux VLAN interfaces should generally be preferred.

---

# License

This project is licensed under the MIT License.

See [LICENSE](LICENSE) for details.
