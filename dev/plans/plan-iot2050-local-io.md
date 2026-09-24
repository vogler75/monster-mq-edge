# Plan: Local I/O for Siemens IOT2050 and Raspberry Pi

Status: proposed; no implementation or hardware validation performed.
Date: 2026-09-23.
Scope: new `LocalIO` feature implemented only in the Go Edge broker, with
dedicated GraphQL bridge queries and mutations as requested by the user.
Keep shared schema parity without adding a Kotlin GPIO runtime. Generic device
functions remain available for backup import/export.
Required first-release targets: Raspberry Pi 4 Model B, Raspberry Pi 5 and
Siemens IOT2050 with the user-confirmed SIMATIC IOT2000
Input/Output shield, 5 DI / 2 DO / 2 AI. Expected order number:
`6ES7647-0KA01-0AA2`; confirm the label during commissioning.
Raspberry Pi GPIO support is a release requirement, not a later extension.

## Decision

Implement a small, owned, pure-Go Linux I/O layer and a `LocalIO` bridge in
MonsterMQ Edge. Use the kernel GPIO character-device v2 interface directly,
without `warthog618/go-gpiocdev`, its former `gpiod` package, CGO, or a runtime
dependency on libgpiod/MRAA command-line tools. Use existing Linux subsystem
drivers for analog inputs and, later, I2C, SPI, PWM and serial devices.

Separate **Linux access**, **board wiring**, **shield behavior**, and **MQTT
mapping**. This supports additional shields without teaching the broker
about every physical connector. It cannot automatically support every Arduino
shield: some need a device-specific protocol driver, different electrical
levels, or a microcontroller for timing.

This plan supersedes the dependency, board-specific bridge architecture,
proposed GraphQL extension and mock-only test strategy in
[the Raspberry Pi GPIO plan](plan-rpi-gpio-bridge.md). Both board families use
the same GPIO backend, LocalIO manager, MQTT contract and GraphQL bridge API;
their hardware differences belong in board and expansion profiles.

The inspected upstream library changelog lists v0.9.1 dated 2024-10-30 and
the earlier rename from `gpiod`. Release age alone does not establish that a
library is abandoned. Avoiding it is a project dependency decision; owning
the replacement also means owning ABI compatibility and hardware tests.
See [upstream changelog](https://github.com/warthog618/go-gpiocdev/blob/master/CHANGELOG.md).

## Hardware facts and unresolved details

The IOT2050 exposes Arduino-style digital, analog and alternate-function
connections. A header label such as `D8` is not GPIO line offset 8.
The operating instructions distinguish the X83 I/O-voltage jumper from X82,
which controls the VIN connection. Record the actual board revision, jumper
settings, shield supply and grounding before selecting a profile. Header
voltage and shield field-terminal voltage are separate things.
See [IOT2050 operating instructions, sections 10.2 and 10.5](https://cache.industry.siemens.com/dl/files/073/109974073/att_1295970/v1/iot2050_operating_instructions_en_en-US.pdf).

**Board revision matters.** Siemens' BSP contains a June 2026 device-tree
patch for revised Advanced M2/PG2 Arduino connectors, moving digital I/O to
PCAL9535 expanders, and a July 2026 MRAA patch adding revision-aware GPIO v2
handling. Older hardware has a different combination of SoC pins, direction
controls, analog switches and pull controls. These sources are evidence for
separate profiles, not evidence that the user's installed image has these
patches. Review the exact BSP tag used on the target.

- [Siemens revised-connector device tree](https://github.com/siemens/meta-iot2050/blob/dd6dbe4727f64e93b5cf79ab39ce27c1bafe51ca/meta/recipes-kernel/linux/files/patches/0029-arm64-dts-ti-iot2050-Add-revised-Arduino-connector-s.patch).
- [Siemens revision-aware GPIO implementation](https://github.com/siemens/meta-iot2050/blob/dd6dbe4727f64e93b5cf79ab39ce27c1bafe51ca/meta/recipes-app/mraa/files/0017-iot2050-add-GPIO-chardev-v2-and-revision-aware-pinmu.patch).

The initial shield profile uses this published logical mapping:

| Shield function | Arduino header signal | Public channel |
| --- | --- | --- |
| DI0, DI1, DI2, DI3, DI4 | D12, D11, D10, D9, D4 respectively | DI0 through DI4 |
| DQ0, DQ1 | D8, D7 respectively | DQ0, DQ1 |
| Channel 0 voltage/current paths, U0 / I0 | A0 / A1 | AI0, selected measurement mode |
| Channel 1 voltage/current paths, U1 / I1 | A2 / A3 | AI1, selected measurement mode |

Source: [Siemens training document, page 34](https://www.automation.siemens.com/sce-static/learning-training-documents/tia-portal/hw-config-iot2000/sce-014-101-hardware-configuration-iot2000edu-r1806-en.pdf).
Confirm terminal markings against the delivered shield and the
[extension-module manual](https://support.industry.siemens.com/cs/attachments/109745681/iot2000_extension_modules_operating_instructions_enUS_en-US.pdf);
do not confuse terminal numbering with these software names. Treat voltage
and current as alternative modes of each advertised analog channel in v1,
not four independently configurable process channels. Verify wiring and
whether simultaneous use is permitted before expanding that model.

The shield's advertised voltage/current ranges are 0–10 V and 0–20 mA.
4–20 mA support means interpreting an appropriate current measurement and
applying engineering scaling, not creating a new electrical input range.
Resolve transfer factors, polarity, conversion rate, accuracy and load limits
from the exact manual and bench measurements in phase 1. Do not equate an
IIO ADC voltage with the voltage at a shield terminal.

## Scope and compatibility

| Capability | First release | Extension path |
| --- | --- | --- |
| Confirmed Siemens shield | All 5 DI, 2 DO, 2 AI; voltage and current modes | Additional shield revisions get separate validation |
| Bare Arduino header / simple contact or relay shield | Named GPIO/IIO channels where the board profile permits them | Add a declarative shield profile; verify electrical compatibility |
| I2C sensor or GPIO-expander shield | Architecture only | Prefer a kernel driver; otherwise add a compiled Go device driver |
| SPI ADC, DAC or other peripheral | Architecture only | Kernel driver or bounded spidev transactions plus a device driver |
| PWM or UART shield | Architecture only | Hardware PWM or serial transport plus its protocol driver |
| Precision pulses, high-speed counting, timing-sensitive LEDs/motors | Outside first release | Kernel driver or an external MCU publishing MQTT/serial data |
| Raspberry Pi 4 Model B, 40-pin header | Required: configurable digital inputs/outputs, edge events, bias, active-low, debounce and output lifecycle | GPIO HAT profiles; bus peripherals through the same extension drivers |
| Raspberry Pi 5, 40-pin header | Required: the same functions using the RP1 GPIO controller | Same public pin names and API as Pi 4 |
| Other Raspberry Pi models and Linux boards | Not automatically certified by Pi 4/5 results | Add/qualify board profiles; Pi 3/Zero 2 W are natural next candidates |

No Arduino sketch compatibility, automatic shield identification, arbitrary
electrical compatibility or real-time scheduling guarantee is promised.
There is no universal shield identification protocol. Select the shield
explicitly; optional EEPROM identification is driver-specific. Stacking
requires conflict-free pins, chip selects, addresses, voltage and power.
Do not probe unknown I2C addresses by writing to them.

## Architecture

```text
Versioned configuration + board profile + shield profile
                          |
                validate / resolve / reserve
                          |
             LocalIO manager and channel workers
                  /                         \
       input/state publisher         output command worker
                  |                         |
           existing MQTT broker and authorized clients
                          |
                 Linux subsystem adapters
             GPIO v2 / IIO / later bus adapters
                          |
          board mux controls -> shield -> field terminals
```

### 1. Owned Linux GPIO backend

Use `golang.org/x/sys/unix` (already present indirectly) for file descriptors,
polling and syscalls. Keep ioctl constants, layouts and unsafe pointer handling
in a small Linux-only package derived from a pinned `linux/gpio.h` UAPI.
The GPIO v2 API was introduced in Linux 5.10; test the actual ioctl capability
and driver behavior instead of accepting a kernel version string alone.
See [kernel GPIO API](https://docs.kernel.org/userspace-api/gpio/chardev.html).

Implement only what the bridge needs:

- Chip/line discovery and metadata, exclusive line requests, get/set values,
  input edge events and line reconfiguration.
- Direction, logical active-low behavior, initial output values and supported
  bias/drive options. Unsupported requested options produce clear errors.
- `GPIO_GET_CHIPINFO_IOCTL`, `GPIO_V2_GET_LINEINFO_IOCTL`,
  `GPIO_V2_GET_LINE_IOCTL`, `GPIO_V2_LINE_GET_VALUES_IOCTL`,
  `GPIO_V2_LINE_SET_VALUES_IOCTL`, `GPIO_V2_LINE_SET_CONFIG_IOCTL`.
- Nonblocking event reads, cancellable poll/epoll, bounded buffers, event
  sequence-gap detection, monotonic event timestamps and descriptor cleanup.
  Publish wall-clock observation time separately; monotonic time is not UTC.
- Set output direction and initial value in one request; group related lines
  on one chip where useful. No atomicity claim across different chips.

Keep explicit fixed-width fields and padding, including 64-bit alignment on
32-bit ARM. Check struct sizes, offsets and ioctl values against the pinned
UAPI for Linux arm64, armv7 and amd64; a compile alone is insufficient.
An optional build-time C layout checker may run in CI; the shipped broker
must still build with `CGO_ENABLED=0`.

Require GPIO v2 for v1 of this feature. Report an unsupported kernel with an
upgrade recommendation; no silent legacy GPIO sysfs or `/dev/mem` fallback.
The rest of the broker must still run on unsupported hosts. Non-Linux builds
return a clear unsupported-feature status, never simulated successful I/O.

### 2. Board profiles and pin multiplexing

The board profile maps IOT2050 `D0`–`D13` / `A0`–`A5`, or Raspberry Pi header
signals such as `GPIO17`, to stable controller identity, line names/offsets,
optional analog channels and supported alternate functions. Use
device-tree/sysfs identity plus chip/line metadata; neither `gpiochip0` nor
an unqualified line name is globally stable or necessarily unique.

Also describe direction/output-enable controls, analog/digital selection,
external pull controls, shared mux groups and activation/deactivation order.
These controls must be reserved alongside the exposed channel. A GPIO line
request alone does not replace the board's mux or level-shifter setup.

Prefer pinctrl/device-tree provisioning at boot for SoC mux state. Where a
supported Siemens image needs its debugfs `pinmux-select` interface, provide
a narrowly scoped provisioning command in the same binary, using a pinned
board recipe. It is image-specific, not a stable Linux GPIO ABI. Keep this
out of the normal unprivileged data path and verify the result at startup.
No MRAA runtime dependency and no copying its `/dev/mem` fallback.
See [Siemens debugfs mux support](https://github.com/siemens/meta-iot2050/blob/dd6dbe4727f64e93b5cf79ab39ce27c1bafe51ca/meta/recipes-app/mraa/files/0003-iot2050-add-debugfs-pinmux-support.patch).

Reserve GPIO-controlled switches for the whole active lifetime if their
state requires retained ownership. Use kernel-owned expander GPIO lines,
not raw I2C writes competing with the expander driver. Sequence initialization
so output drivers stay inactive until the signal value and route are ready.
On failure, unwind owned resources in reverse order to the verified inactive
configuration. Reject missing or ambiguous board identification.

#### Required Raspberry Pi profiles

Ship `raspberry-pi-4b`, `raspberry-pi-5` and a `raspberry-pi-auto` selector that
accepts only qualified models. Identify the board from device-tree model and
compatible data, then resolve the header controller using device identity,
driver metadata and expected line names. Pi 4 and Pi 5 require distinct
controller resolution: Pi 5 exposes the header through RP1. Do not select the
first gpiochip, hard-code `gpiochip0`/`gpiochip4`, or use direct register access.
Raspberry Pi documents both the controller differences and changing gpiochip
enumeration in its [GPIO best-practices guide](https://pip-assets.raspberrypi.com/categories/685-whitepapers-app-notes/documents/RP-006553-WP/A-history-of-GPIO-usage-on-Raspberry-Pi-devices-and-current-best-practices).

Expose the conventional GPIO signal names (`GPIO17`, `GPIO27`, etc.) as the
canonical channel IDs on both models. The dashboard must also display physical
header positions: `GPIO17` is physical pin 11 and `GPIO27` is physical pin 13.
A physical-pin picker resolves to the canonical signal before saving. Reject
ambiguous bare integers and never confuse header position, signal number and
chip offset. Show all 40 positions but make power/ground positions unselectable.
The profile allowlists only header GPIOs, not unrelated internal controller lines.

Raspberry Pi header GPIO uses 3.3 V logic. GPIO2/GPIO3 have fixed pull-ups;
profile validation must not claim that software bias can remove external
resistors. A board's available bias/drive/edge capabilities are verified on
the supported kernel. See [official header documentation](https://www.raspberrypi.com/documentation/computers/raspberry-pi.html#gpio-and-the-40-pin-header).
Keep GPIO0/GPIO1 (physical pins 27/28) reserved for HAT identification by default,
consistent with the [HAT design guide](https://github.com/raspberrypi/hats/blob/master/designguide.md).

Respect configured overlays and pinctrl consumers for I2C, SPI, UART, PWM,
displays and HATs. A line can conflict with an alternate function even when
no GPIO consumer name appears. Resolve that ownership and reject conflicting
activation; do not unbind drivers or rewrite boot overlays during a bridge
mutation. Provision required alternate-function changes explicitly before
activation. A free pin is configured as input/output through GPIO v2, not
through the Siemens-specific mux recipe.

Provide `raspberry-pi-40pin-header` as the built-in bare-header expansion profile;
no HAT is required. The existing `shieldProfile` configuration field selects
either a bare header, an Arduino shield or a Pi HAT profile. This keeps one
schema and one bridge implementation. Do not claim Arduino shields physically
fit a Pi header; adapters and electrical compatibility need their own profiles.

The standard Pi 4/5 header has no analog input channels. Digital-only Pi
activation must not require an IIO device. An analog request on the bare-header
profile fails validation; an external ADC/HAT and an appropriate driver/profile
are required for analog measurements. Hardware PWM and bus devices remain the
separately scoped extensions in the compatibility table.

### 3. Shield profiles and extension drivers

A versioned shield profile declares channel names/types, header resources,
fixed directions, polarity, supported ranges, transfer functions, electrical
requirements, shared resources and compatible board profiles. Profiles are
data with strict validation, not shell commands or executable scripts.

For an ordinary GPIO shield, adding a profile should require no broker code.
The same rule applies to a simple GPIO Pi HAT. Pi and IOT2050 profiles share
channel types and lifecycle semantics while declaring different pin resources.
For a new peripheral chip/protocol, add a small compiled Go driver using a
transport interface. Keep the driver registry static in the single binary;
do not introduce Go shared-library plugins. A profile can select only a
driver already supported by the binary.

The resource allocator rejects duplicate pins, conflicting alternate modes,
duplicate I2C addresses on the same bus and duplicate SPI chip selects.
Shared buses are permitted only with compatible settings and serialized
transactions. Claim unused alternatives where the shield or board makes
them electrically inseparable from an active channel.

### 4. Analog and later transports

Read ADC data through Linux IIO, resolving the device by identity rather
than assuming `iio:device0`. Start with bounded-rate raw/scale/offset reads;
respect the driver's documented units and missing-attribute behavior.
Convert ADC measurement -> shield terminal V/mA -> optional engineering
units. Store calibration and source units explicitly. If the selected image
does not expose the ADC correctly, provisioning its driver/device tree is a
prerequisite; do not guess a register interface or report a fabricated zero.

AI0 and AI1 each select `voltage-0-10V`, `current-0-20mA` or
`current-4-20mA`. The profile selects the correct A0/A1 or A2/A3 path. Publish
raw value, engineering value, units, timestamp and quality; distinguish stale,
unavailable and out-of-range measurements. Do not silently clamp errors into
apparently valid readings. Support sample interval, deadband and heartbeat.
For higher-rate acquisition, use [IIO buffers](https://docs.kernel.org/iio/iio_devbuf.html)
in a later phase if the driver supports them.

Future bus adapters use [i2c-dev](https://docs.kernel.org/i2c/dev-interface.html),
[spidev](https://docs.kernel.org/spi/spidev.html),
[hardware PWM](https://docs.kernel.org/driver-api/pwm.html) and Linux tty APIs.
Transport access does not implement a peripheral's register/protocol semantics.
Prefer an existing kernel driver where available; no software bit-banging of
these buses in MQTT handlers.

## Configuration, GraphQL and broker integration

### First release: stored devices and dashboard management

Add `Features.LocalIO` (default false) to the main YAML configuration. Store
each device as type `LocalIO` in the existing `DeviceConfigStore`, with its
versioned configuration in the existing JSON `Config` field. No new table,
column or collection is required. The existing `Namespace` is the MQTT topic
prefix, and `NodeID` identifies the physical host that owns the hardware.
Do not store GPIO file descriptors or transient discovery results in config.

Ship embedded known profiles and permit validated local shield-profile
files installed by an administrator. GraphQL selects profiles by ID/version;
it cannot supply arbitrary server file paths, scripts or ioctl recipes.
Device configuration has one owner: the database. YAML enables the subsystem,
not a second source of channel configuration.

Add dedicated LocalIO bridge queries and mutations, following the existing
WinCCUa/MQTT bridge pattern. The existing generic `importDevices` remains a
backup/restore path and deliberately saves records disabled; preserve that
behavior and add the manager to its reload path. No GPIO implementation exists
in the inspected Kotlin broker to port; the runtime is new Go Edge work.

Example existing `DeviceInput` for `importDevices`, with proposed LocalIO JSON:

```json
{
  "name": "cabinet-io",
  "namespace": "edge/iot2050/io",
  "nodeId": "iot2050-01",
  "enabled": false,
  "type": "LocalIO",
  "config": {
    "version": 1,
    "boardProfile": "siemens-iot2050-auto",
    "shieldProfile": "siemens-iot2000-io",
    "profileVersion": 1,
    "channels": [
      {"name": "DI0", "mode": "DIGITAL_INPUT", "debounceMs": 20, "heartbeatMs": 10000},
      {"name": "DQ0", "mode": "DIGITAL_OUTPUT", "initialValue": false,
       "stopValue": false, "timeoutValue": false, "commandTimeoutMs": 5000},
      {"name": "AI0", "mode": "VOLTAGE_0_10V", "sampleIntervalMs": 100, "deadband": 0.02},
      {"name": "AI1", "mode": "CURRENT_4_20MA", "sampleIntervalMs": 100,
       "engineering": {"inputMin": 4, "inputMax": 20, "outputMin": 0,
                       "outputMax": 100, "unit": "percent"}}
    ]
  }
}
```

Unlisted channels are disabled. `auto` selects only a recognized and supported
board revision. Channel direction/range must be allowed by the shield profile;
configuration cannot turn a fixed shield input into an output.

Example Raspberry Pi `LocalIoBridgeInput` for the dedicated `create` mutation:

```json
{
  "name": "pi-gpio",
  "namespace": "edge/pi/io",
  "nodeId": "pi-01",
  "enabled": false,
  "config": {
    "version": 1,
    "boardProfile": "raspberry-pi-auto",
    "shieldProfile": "raspberry-pi-40pin-header",
    "profileVersion": 1,
    "channels": [
      {"name": "GPIO17", "mode": "DIGITAL_INPUT", "debounceMs": 20,
       "heartbeatMs": 10000, "options": {"bias": "PULL_UP", "edge": "BOTH"}},
      {"name": "GPIO27", "mode": "DIGITAL_OUTPUT", "initialValue": false,
       "stopValue": false, "timeoutValue": false, "commandTimeoutMs": 5000}
    ]
  }
}
```

This same configuration shape works on qualified Pi 4 and Pi 5 images. The
channel worker publishes `edge/pi/io/channels/GPIO17/state` and accepts output
commands at `edge/pi/io/channels/GPIO27/set`. Changing between board families
changes the selected profile/channel mappings, not the GraphQL API or MQTT
message format. The profile's JSON schema defines and validates `options`.

### Dedicated GraphQL bridge API; hardware feature remains Edge-only

Use a dedicated `localIoBridges` query and `localIoBridge` mutation group,
following the broker's existing bridge management convention. Configuration
lives in the same `DeviceConfigStore` record used by generic import/export.
There is no second source of device data and no new database layout.

The user has explicitly approved this GraphQL extension. Proposed exact SDL
below reuses the existing `JSON` scalar. Keep the bridge envelope and lifecycle
typed, and validate versioned channel/profile configuration as JSON so adding
a new shield or peripheral driver does not require another GraphQL extension.

```graphql
enum LocalIoRuntimeState { DISABLED VALIDATING STARTING RUNNING STOPPING FAULT UNSUPPORTED }
enum LocalIoProfileKind { BOARD SHIELD }

type LocalIoStatus {
  state: LocalIoRuntimeState!
  timestamp: String!
  errors: [String!]!
  details: JSON!
}
type LocalIoBridge {
  name: String!
  namespace: String!
  nodeId: String!
  enabled: Boolean!
  config: JSON!
  createdAt: String!
  updatedAt: String!
  isOnCurrentNode: Boolean!
  status: LocalIoStatus!
}
input LocalIoBridgeInput {
  name: String!
  namespace: String!
  nodeId: String!
  enabled: Boolean
  config: JSON!
}
type LocalIoBridgeResult {
  success: Boolean!
  bridge: LocalIoBridge
  errors: [String!]!
}
type LocalIoProfile {
  id: String!
  version: Int!
  kind: LocalIoProfileKind!
  title: String!
  definition: JSON!
}
type LocalIoInventory {
  nodeId: String!
  supported: Boolean!
  boardProfile: String
  resources: JSON!
  errors: [String!]!
}
extend type Query {
  localIoBridges(name: String, node: String): [LocalIoBridge!]!
  localIoProfiles(kind: LocalIoProfileKind): [LocalIoProfile!]!
  localIoInventory(nodeId: String): LocalIoInventory!
}
type LocalIoBridgeMutations {
  validate(input: LocalIoBridgeInput!): LocalIoBridgeResult!
  create(input: LocalIoBridgeInput!): LocalIoBridgeResult!
  update(name: String!, input: LocalIoBridgeInput!): LocalIoBridgeResult!
  delete(name: String!): LocalIoBridgeResult!
  start(name: String!): LocalIoBridgeResult!
  stop(name: String!): LocalIoBridgeResult!
  toggle(name: String!, enabled: Boolean!): LocalIoBridgeResult!
}
extend type Mutation {
  localIoBridge: LocalIoBridgeMutations!
}
```

The JSON `config` uses the structure shown in the import example above. For
`create`/`update`, pass name, namespace, nodeId, enabled and config; omit `type`,
which the dedicated resolver fixes to `LocalIO`. Pin mode names in the example
are JSON-schema values, not GraphQL enums. Return validation errors with
channel/property paths; accepting arbitrary JSON does not imply accepting
unknown fields, invalid modes or unbounded values.

`localIoProfiles` exposes the versioned board/shield manifests and their config
schemas, channel capabilities, defaults and resource constraints. The catalog
exists before any bridge is configured. `localIoInventory` resolves read-only
hardware metadata on the requested host; it never changes mux state or claims
outputs. Edge accepts only its own concrete node ID. Neither query can be
used to read arbitrary server files or upload executable profile definitions.

Create defaults to disabled when `enabled` is absent. Update preserves the
current enabled state when omitted. Start/stop/toggle persist the corresponding
enabled state; `status` reports what actually runs, including activation failure.
Require explicit initial/stop/timeout values and command timeout for each output.
The dashboard defaults new bridges to disabled and makes activation explicit.

Serialize lifecycle operations per device and arbitrate resources across
bridges. Validate the whole proposed configuration before saving or touching
hardware. A valid enabled update quiesces commands, applies stop values,
releases/reacquires affected resources in a controlled sequence and admits
only fresh commands afterward. Failed validation leaves the current config
and running bridge unchanged. Acquisition must be rechecked at activation:
read-only validation cannot guarantee a resource will remain available.

A hardware activation failure returns `success: false`, records errors and
publishes `FAULT`, distinguishing saved desired config from the actual applied
revision in `status.details`. It must not leave partially active outputs.
Delete stops/releases the bridge before deleting its record; cleanup failure
returns an error instead of claiming success. Repeated stop/delete are
idempotent. Reject rename-by-update and changing an existing bridge's nodeId;
stop/delete/recreate is the v1 transfer procedure. No wildcard hardware ownership
or reassign operation is included in the initial API.

Generic `getDevices` and `importDevices` remain available for export/import
of type `LocalIO`. Imports always disable the device, including when replacing
an active bridge; they validate JSON and stop the active instance before
replacement. Wire LocalIO into the import/reload lifecycle without changing
other device types. `Features.DeviceImportExport` is needed only for this
backup path, not for ordinary dedicated bridge management.

### Cross-repository impact

- **Go Edge:** add `Features.LocalIO` (default false), LocalIO manager/store
  integration, `internal/graphql/schema/localio.graphqls`, generated gqlgen
  bindings, dedicated resolvers and authorization rules. Advertise `LocalIO`
  in existing `enabledFeatures` when the feature/runtime is available.
- **Kotlin contract only:** add the identical SDL to the canonical
  `broker/src/main/resources/schema-local-io.graphqls` and register it with
  the GraphQL server as needed for strict subset parity. There is no Kotlin
  GPIO implementation, LocalIO runtime feature, or device activation.
  Inventory explicitly reports unsupported; lifecycle mutations return a
  clear unsupported-feature result; queries can return empty catalogs/lists.
  Preserve unknown LocalIO records through existing generic export/import.
  Do not advertise `LocalIO` in Kotlin's `enabledFeatures`.
- **External dashboard:** add a feature-gated bridge page using the dedicated
  API. Reuse shared bridge UI components and existing topic subscriptions.
  Hide the page and avoid these queries on Kotlin or older Edge brokers
  without the feature. The same dashboard build works with every backend.

This is an additive API change authorized for this bridge; no existing field,
argument, enum or nullability is changed. Keep definitions identical across
both schemas and verify the relationship with a contract test. If a later
implementation instruction prohibits even Kotlin contract edits, resolve the
explicit schema-parity exception before shipping Edge-only SDL. Nothing here
requires a JVM hardware implementation.

Require administrator permissions for inventory/configuration/lifecycle
operations and existing MQTT topic ACLs for live data and output commands.
Bridge output controls use the existing authorized `publish` operation and the
same expiring command envelope as MQTT clients. No direct pin-write mutation
or new subscription is needed.

The dashboard editor exposes board/shield selection, resolved pin mapping,
channel mode, analog scaling, debounce, inactive output values, namespace,
validation errors and desired versus actual runtime state. Render extensible
driver settings from profile JSON schemas, not a hard-coded form for every
shield. Save, enable/disable, start/stop and delete use `localIoBridge` mutations;
backup/restore retains the generic device functions. No UI implementation is
added to the Edge repository.

Publish MQTT runtime status as well as GraphQL status. Retained metadata is a
cache, not authoritative configuration. Refresh it on startup and clear stale
retained device state after delete or a namespace change. A bridge whose
hardware is unsupported remains inspectable with actionable errors.

Also offer same-binary `io inspect`, `io validate` and `io prepare` for local
commissioning. CLI syntax is proposed, not currently implemented. Inspection
and validation never activate pins. These are diagnostic/provisioning helpers,
not a competing configuration database.

### MQTT contract

| Topic under the configured prefix | Behavior |
| --- | --- |
| `channels/<name>/state` | JSON value, quality, timestamp and sequence; optionally retained |
| `channels/<name>/set` | Digital output command; never replay retained commands |
| `channels/<name>/ack` | Command ID, accepted/applied/error status and reason |
| `status` | Availability, board/profile identity, active channels, errors and counters |
| `capabilities` | Resolved channel types, ranges and supported operations |

Use a strict command envelope such as
`{"id":"operator-42","value":true,"expiresAt":"2026-09-23T12:00:05Z"}`.
Reject expired, malformed, retained, unauthorized and input-channel writes.
Bound accepted command lifetime; validate expiry again immediately before
hardware access. Deduplicate IDs within a bounded per-boot window. Only
absolute set-value commands are in v1; no toggle/pulse operations whose
duplicates can cause additional actuation. Do not restore outputs from
retained state, historical topics or commands received before activation.

Reuse `internal/broker/server.go`'s local publisher and the pubsub bus.
`SubscribeWithOverflow` already reports dropped bus events; consume that
signal. Perform all hardware I/O in bounded workers outside `OnPublished`.
An overflow faults the affected command worker, invalidates pending commands,
applies its configured fault value where possible and requires a fresh
activation. Report event gaps and resample input state after input overflow.
Do not imply that MQTT QoS 1 guarantees physical execution; the application
acknowledgment is the execution result.

Use existing MQTT authentication and topic ACLs; deployments with remotely
writable outputs require authenticated, explicitly authorized publishers.
Check command admission for MQTT, GraphQL publish, bridges and inline/internal
publish paths, since some internal paths may bypass normal MQTT ACL checks.
Carry trusted origin information internally if needed, without extending
GraphQL. Untrusted payload fields cannot establish identity. Keep state,
command and acknowledgment topics disjoint and reject wildcard command topics.

Kernel output readback reports the commanded/read-back GPIO state, not proof
that a connected actuator moved. Publish `feedbackVerified: false` unless a
separate physical feedback input is configured and actually checked.

On orderly stop, timeout or recoverable I/O failure, apply the configured
inactive value before releasing resources. A userspace watchdog cannot
guarantee output behavior during SIGKILL, kernel failure or power loss.
Record measured boot/release behavior and require external circuitry/watchdog
where an installation needs a guaranteed de-energized state. An external
consumer should use status heartbeat expiry to detect an ungraceful broker
failure; a crashed in-process bridge cannot publish its own offline message.

## Implementation sequence

1. **Hardware discovery and proof of access.** Record order number, hardware
   revision, Debian/image/BSP version, kernel, device tree, GPIO/IIO inventory,
   mux interfaces, jumper settings and terminal ratings. Resolve actual
   polarity and ADC scaling. Demonstrate one input, one output with a dummy
   load and one analog input using a small pure-Go diagnostic. Capture a
   compatibility report with the supported image and profile IDs. This is the
   go/no-go gate for first-release hardware support. Also identify the Pi 4
   and Pi 5 hardware and Raspberry Pi OS/kernel versions; demonstrate a GPIO
   input/output loopback on each. Do not require ADC discovery on bare Pi boards.
2. **Linux backend and profile resolver.** Implement the limited GPIO v2 ABI,
   IIO reads, cancellation, ownership and strict profile validation. Implement
   the exact IOT2050 revision discovered in phase 1; recognize other revisions
   as unsupported until tested. Implement Pi 4 and Pi 5 controller resolution
   in this same phase. Add the Siemens shield, IOT2050 bare-header and Raspberry
   Pi bare-header profiles; none of these mandatory board targets is deferred.
3. **Broker and management integration.** Add the disabled-by-default Edge
   feature, stored JSON configuration, dedicated LocalIO queries/mutations,
   generic import validation/reload, workers, MQTT contract and commissioning
   tools. Align the additive SDL with the canonical Kotlin contract; keep
   GPIO runtime code in Go Edge only. Add the feature-gated external dashboard
   editor and generated bindings.
4. **Commissioning and release.** Validate every Siemens channel and mode,
   failure behavior, permissions, coexistence and resource use. Ship a wiring
   guide, supported-hardware matrix, complete example config and test report.
   Qualify both Raspberry Pi models through the same broker/API test suite
   and the Pi hardware criteria below before declaring the release complete.
5. **Extensibility demonstration.** Add and bench-test a simple non-Siemens
   Arduino-compatible shield using only a profile; then select one concrete
   bus-based shield and add its required driver. PWM/serial/other boards are
   separate increments, not prerequisites for the Siemens release.

Proposed code locations:

```text
internal/localio/linuxgpio/    Linux UAPI implementation; unsupported-host stub
internal/localio/iio/          ADC discovery and reads
internal/localio/profiles/     Board/shield definitions and resource resolution
internal/bridge/localio/       Config, lifecycle, channel workers, MQTT mapping
test/integration/              Full broker tests with Linux GPIO simulator
doc/local-io/                  Hardware matrix, wiring and commissioning reports
```

Update `internal/config/config.go`, `yaml-json-schema.json`,
`config.yaml.example` and `scripts/deb/config.yaml` together. Add schemas for
LocalIO device JSON and profile files, and package a disabled import example. Review
`systemd/monstermq-edge.service` and Debian installation scripts for scoped
GPIO/IIO access. Provision mux state before service start; run the broker as
an unprivileged account with only the required device access. Do not grant
world-writable device nodes or require a permanently privileged broker.

## Acceptance criteria

All applicable first-release rows below must pass on each declared board/image:
IOT2050 with the confirmed shield, Raspberry Pi 4 Model B and Raspberry Pi 5.
Siemens terminal/analog tests apply to that shield; the Pi header tests below
apply to both Pi models. Run common broker/lifecycle/API tests on all three.
Record results and measurements; compilation or simulator success does not
mark physical hardware support as passed.

| ID | Acceptance criterion and evidence |
| --- | --- |
| AC-01 | Dependency audit finds neither `go-gpiocdev` nor `gpiod`, no MRAA/libgpiod runtime requirement and no CGO in the production build. Native build and Linux arm64/armv7 cross-builds succeed. Non-Linux activation reports unsupported without fabricated values. |
| AC-02 | GPIO UAPI sizes, field offsets and ioctl numbers match the pinned kernel header on arm64, armv7 and amd64. Real Linux GPIO requests and event reads pass on arm64 and an amd64 Linux CI runner. |
| AC-03 | The compatibility report identifies each actual board revision and image. Unknown/mismatched revisions, ambiguous chip identities, absent GPIO v2 and missing capabilities required by configured channels refuse I/O activation with a specific reason. A bare Pi digital profile works without IIO/ADC. Ordinary MQTT service remains operational. |
| AC-04 | Renumbering GPIO/IIO device nodes does not change the physical channel selected. Discovery lists shield channel -> header signal -> controller/line or ADC path and its claimed control resources. |
| AC-05 | All five DI channels report both states and transitions through a real MQTT listener. At least 100 transitions per channel at 1 Hz produce the expected ordered state changes with no unexplained losses. Verify polarity against applied terminal voltage. |
| AC-06 | With 20 ms debounce, a controlled bounce sequence settling within 10 ms emits one stable change after the configured interval. If kernel debounce is unavailable, the documented software stable-state filter passes the same test; unsupported edge modes fail explicitly. |
| AC-07 | Both DQ channels switch the correct terminals with the documented load/supply. Authorized MQTT commands receive applied acknowledgments only after successful writes. Measure at least 20 cycles per channel within the manufacturer's switching limits; readback is not labeled verified actuator feedback. |
| AC-08 | A scope/logic analyzer records startup, shutdown, failed activation and restart on both outputs. No unintended active pulse occurs during broker-controlled initialization/cleanup at the recorded measurement resolution. Record boot, released-line and power-loss behavior separately. |
| AC-09 | Both analog channels pass voltage and current tests at 0/25/50/75/100% of the supported range, plus 4/12/20 mA scaling checks. Before testing, document the numeric error bound from shield + board ADC specifications and instrument uncertainty; every point stays within that bound. Record raw and converted values. |
| AC-10 | Analog deadband/heartbeat works at the configured sample rate. Missing ADC, read errors and over/underrange yield explicit quality states and no invented zero or clamped-good value. Stale time is bounded by the configured status policy. |
| AC-11 | Validation rejects duplicate resources, DI configured as DO, GPIO/ADC or GPIO/SPI conflicts, invalid profile versions/ranges, wildcard command topics and overlapping state/command topics before activation. A simulated acquisition failure leaves no leaked claims or active outputs. |
| AC-12 | An authenticated allowed client can control outputs; a denied or anonymous client cannot. Retained messages including retained replay, expired commands, duplicate IDs, malformed values and pre-activation commands cause no additional output transition. Test all enabled publish ingress paths. |
| AC-13 | With a 5 s command timeout, an activated output reaches its configured timeout value within 5.25 s under the agreed normal-load fixture. Graceful stop applies the stop value. SIGKILL/power-loss results are documented without claiming a software guarantee. |
| AC-14 | Inject command-bus overflow and GPIO event sequence gaps. Commands do not execute from a stale backlog; faults are visible, input state resynchronizes, and ordinary broker traffic continues. Shutdown exits workers and releases descriptors within 2 s in recoverable test cases. |
| AC-15 | With 1,000 unrelated MQTT messages/s and configured I/O rates, p95 input-event-to-local-subscriber and command-to-applied-ack latency is at most 100 ms, excluding configured debounce/hardware conversion delay. Report p99/max too; these are qualification targets, not real-time guarantees. |
| AC-16 | A 24 h soak with all supported shield channels enabled has no monotonically growing descriptor/goroutine count, no unexplained event loss and no memory growth trend after warm-up. Report absolute CPU/RSS and impact on broker latency, including an idle baseline. |
| AC-17 | The service operates under its packaged unprivileged account after provisioning. Permission denial, another process owning a line and ADC loss produce actionable status. It never takes pins away from kernel drivers or another process. |
| AC-18 | YAML, device JSON and profiles validate against their schemas. Dedicated localIoBridges/profile/inventory queries and localIoBridge validate/create/update/delete/start/stop/toggle mutations pass network-level GraphQL tests and work through the dashboard. Enabled state survives restart; imports through the generic API stay disabled. Invalid updates preserve the prior config/runtime. A failed activation exposes FAULT and no partially active outputs. Inspection/validation never activates pins. |
| AC-19 | The documented LocalIO bridge SDL is identical in both broker schemas; no existing interface fields or database layouts change. Schema parity tests, access-control tests and existing API tests pass. The GPIO runtime exists only in Go Edge and defaults off. Kotlin exposes no LocalIO capability and rejects activation explicitly. The same dashboard build handles new Edge, Kotlin and older Edge without unsupported GraphQL calls. |
| AC-20 | Linux integration tests start the real broker and use MQTT network clients with kernel `gpio-sim`; ownership errors, values, edges, authentication and lifecycle are exercised without replacing the broker with a mock. Hardware-only cases are marked separately, not silently skipped in release qualification. |
| AC-21 | The same pure-Go Linux arm64 broker binary passes GPIO input/output and dedicated GraphQL management tests on physical Pi 4 and Pi 5 boards. Only board detection/configuration differs; no Pi-specific GPIO library, runtime daemon, CGO or register-mapping backend is used. Record board and OS/kernel versions for both. |
| AC-22 | On both Pis, GPIO17 resolves to header pin 11 and GPIO27 to pin 13. Every selectable header signal matches the official pin map. Power/ground, reserved HAT-ID pins and non-header controller lines are rejected. Renumbering device nodes does not change the selected header controller; Pi 5 selects RP1, never a BCM2712 internal bank. |
| AC-23 | On each Pi, a documented 3.3 V loopback fixture connects GPIO27 output to GPIO17 input through a protective series resistor. At least 100 rising/falling cycles at 1 Hz traverse real MQTT clients and produce matching input states, acknowledgments and physical readings. Repeat with logical active-low, supported pull modes and debounce; unsupported options return explicit errors. |
| AC-24 | Pi 4 and Pi 5 each pass startup/stop/restart/timeout, stale/retained-command rejection, ACL, descriptor cleanup, latency and soak criteria. Test a kernel/overlay-owned header pin and a competing userspace GPIO claim: both are rejected without disturbing the owner. A Pi analog request without an ADC profile fails before any GPIO is driven. |
| AC-25 | The external dashboard's same LocalIO bridge page selects IOT2050, Pi 4 or Pi 5 profiles and renders their actual capabilities/pin numbering. Create/update/start/stop/delete and generic export/import preserve the selected profile and channel options. Pi GPIO operation does not depend on Siemens mux controls, analog channels or shield presence. |

Extension acceptance: a second simple shield works by adding only profile data
and configuration; a concrete I2C/SPI shield works through a reusable transport
plus its driver without modifying broker routing or GraphQL. Test stacking
conflicts and ensure each advertised combination appears in the hardware matrix.

Use the kernel's [GPIO simulator](https://docs.kernel.org/6.12/admin-guide/gpio/gpio-sim.html)
for CI rather than relying on mock pin objects. Add board tests for mux
sequencing, voltage/current conversion and terminal behavior because gpio-sim
does not model those. Run `make test`, `make test-race`, `make lint` and the
pure-Go cross-builds as applicable on the relevant CI hosts.

## Inputs needed before implementation qualification

The shield family is confirmed. Still collect the IOT2050 order number and
hardware revision, installed image/kernel, actual shield label, jumper settings,
desired input/analog rates, output load and required inactive states. These
are phase-1 commissioning inputs, not blockers to adopting this plan. Do not
publish a universal IOT2050 compatibility claim before the corresponding
hardware profiles have been measured.
For Raspberry Pi, obtain one Pi 4 Model B and one Pi 5 with supported Linux
images, and record overlays/HATs and intended GPIO wiring. Missing test hardware
leaves that target's qualification pending; it does not silently remove
Raspberry Pi from the required first-release scope.
