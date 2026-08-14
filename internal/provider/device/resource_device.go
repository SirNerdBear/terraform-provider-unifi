package device

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-cty/cty"

	"github.com/SirNerdBear/terraform-provider-unifi/internal/provider/utils"

	"github.com/filipowm/go-unifi/unifi"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"

	"github.com/SirNerdBear/terraform-provider-unifi/internal/provider/base"
)

func ResourceDevice() *schema.Resource {
	return &schema.Resource{
		Description: "The `unifi_device` resource manages UniFi network devices such as access points, switches, gateways, etc.\n\n" +
			"Devices must first be adopted by the UniFi controller before they can be managed through Terraform. " +
			"This resource cannot create new devices, but instead allows you to manage existing devices that have already been adopted. " +
			"The recommended approach is to adopt devices through the UniFi controller UI first, then import them into Terraform using the device's MAC address.\n\n" +
			"This resource supports managing device names, port configurations, and other device-specific settings.",

		CreateContext: resourceDeviceCreate,
		ReadContext:   resourceDeviceRead,
		UpdateContext: resourceDeviceUpdate,
		DeleteContext: resourceDeviceDelete,
		Importer: &schema.ResourceImporter{
			StateContext: resourceDeviceImport,
		},

		Schema: map[string]*schema.Schema{
			"id": {
				Description: "The unique identifier of the device in the UniFi controller.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"site": {
				Description: "The name of the UniFi site where the device is located. If not specified, the default site will be used.",
				Type:        schema.TypeString,
				Computed:    true,
				Optional:    true,
				ForceNew:    true,
			},
			"mac": {
				Description:      "The MAC address of the device in standard format (e.g., 'aa:bb:cc:dd:ee:ff'). This is used to identify and manage specific devices that have already been adopted by the controller.",
				Type:             schema.TypeString,
				Optional:         true,
				Computed:         true,
				ForceNew:         true,
				DiffSuppressFunc: utils.MacDiffSuppressFunc,
				ValidateFunc:     validation.StringMatch(utils.MacAddressRegexp, "Mac address is invalid"),
			},
			"name": {
				Description: "A friendly name for the device that will be displayed in the UniFi controller UI. Examples:\n" +
					"* 'Office-AP-1' for an access point\n" +
					"* 'Core-Switch-01' for a switch\n" +
					"* 'Main-Gateway' for a gateway\n" +
					"Choose descriptive names that indicate location and purpose.",
				Type:     schema.TypeString,
				Optional: true,
				Computed: true,
			},
			"disabled": {
				Description: "Whether the device is administratively disabled. When true, the device will not forward traffic or provide services.",
				Type:        schema.TypeBool,
				Computed:    true,
			},
			"switch_vlan_enabled": {
				Description: "Whether per-port VLAN configuration is enabled on the device. Required for `port_override` blocks with VLAN-tagging profiles (e.g. an IoT-VLAN `port_profile_id`) to actually take effect on access points that expose passthrough Ethernet ports (UAP-UHDIW and similar in-wall units). " +
					"Switches honor port profile VLAN bindings unconditionally; APs ignore them unless this flag is true. " +
					"Note: the underlying field uses `omitempty` so setting this to `false` has no effect — once enabled on a device, it can only be disabled via the UI.",
				Type:     schema.TypeBool,
				Optional: true,
				Computed: true,
				// The controller ignores attempts to disable this (`omitempty` drops
				// a `false` from the payload), so a `true` -> `false` change would
				// otherwise read back as `true` and produce a perpetual diff.
				// Suppress that one transition to match the API's write-once behavior.
				DiffSuppressFunc: func(k, old, newValue string, d *schema.ResourceData) bool {
					return old == "true" && newValue == "false"
				},
			},
			// Device-level settings. go-unifi carries all of these; they were
			// simply not in the schema. Several are overridden by the SITE-WIDE
			// settings on unifi_setting_global_switch unless the device is listed
			// in its switch_exclusions -- flowctrl and jumboframe especially.
			"dot1x_fallback_networkconf_id": {
				Description: "Network a port falls back to when 802.1X authentication fails.",
				Type:        schema.TypeString,
				Optional:    true,
			},
			"dot1x_portctrl_enabled": {
				Description: "Enable 802.1X port control on the device.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"dpi_enabled": {
				Description: "Enable deep packet inspection on the device.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"flowctrl_enabled": {
				Description: "Enable 802.3x flow control on the device. Overridden by the site-wide setting unless the device is listed in `switch_exclusions` on `unifi_setting_global_switch`.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"jumboframe_enabled": {
				Description: "Enable jumbo frames. Same site-wide override caveat as `flowctrl_enabled`.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"led_override": {
				Description:  "Status LED behaviour: follow the site setting, or force on/off.",
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validation.StringInSlice([]string{"default", "on", "off"}, false),
			},
			"led_override_color": {
				Description: "Status LED colour as a hex string, when `led_override` is `on`.",
				Type:        schema.TypeString,
				Optional:    true,
			},
			"led_override_color_brightness": {
				Description: "Status LED brightness percentage, when `led_override` is `on`.",
				Type:        schema.TypeInt,
				Optional:    true,
			},
			"mgmt_network_id": {
				Description: "Network the device uses for management -- the controller calls this the Network Override. A device with this set TAGS its management traffic with that VLAN, which is how a device reaches a management VLAN over a trunk whose native VLAN is something else. Switches usually do not need it (their uplink native VLAN does the job); APs here all use it.",
				Type:        schema.TypeString,
				Optional:    true,
			},
			"outlet_enabled": {
				Description: "Enable outlets (USP-PDU / RPS hardware).",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"outlet_power_cycle_enabled": {
				Description: "Allow power cycling of outlets.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"power_source_ctrl": {
				Description:  "PoE power source type for the device.",
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validation.StringInSlice([]string{"auto", "8023af", "8023at", "8023bt-type3", "8023bt-type4", "pasv24", "poe-injector", "ac", "adapter", "dc", "rps"}, false),
			},
			"power_source_ctrl_budget": {
				Description: "PoE budget in watts when power source control is enabled.",
				Type:        schema.TypeInt,
				Optional:    true,
			},
			"power_source_ctrl_enabled": {
				Description: "Enable PoE power source control.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"resetbtn_enabled": {
				Description:  "Whether the physical reset button is active.",
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validation.StringInSlice([]string{"on", "off"}, false),
			},
			"snmp_contact": {
				Description: "SNMP contact string.",
				Type:        schema.TypeString,
				Optional:    true,
			},
			"snmp_location": {
				Description: "SNMP location string.",
				Type:        schema.TypeString,
				Optional:    true,
			},

			// Every lcm_* field carries `omitempty`, so an undeclared one is
			// never sent -- but that also means a zero value cannot be written.
			// Orientation cannot be set back to 0, and the *_override booleans
			// cannot be turned off, from Terraform.
			"lcm_brightness": {
				Description:  "LCD screen brightness, 1-100. Requires `lcm_brightness_override = true`.",
				Type:         schema.TypeInt,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.IntBetween(1, 100),
			},
			"lcm_brightness_override": {
				Description: "Use `lcm_brightness` instead of the controller default.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"lcm_idle_timeout": {
				Description:  "Seconds before the LCD screen sleeps, 10-3600. Requires `lcm_idle_timeout_override = true`.",
				Type:         schema.TypeInt,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.IntBetween(10, 3600),
			},
			"lcm_idle_timeout_override": {
				Description: "Use `lcm_idle_timeout` instead of the controller default.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"lcm_night_mode_begins": {
				Description: "Start of LCD night mode, `HH:MM` on a 24-hour clock.",
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
			},
			"lcm_night_mode_ends": {
				Description: "End of LCD night mode, `HH:MM` on a 24-hour clock.",
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
			},
			"lcm_orientation_override": {
				Description: "LCD screen rotation in degrees: 0, 90, 180 or 270. 0 cannot be written " +
					"(it is the zero value and is omitted); rotate back through the UI.",
				Type:         schema.TypeInt,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.IntInSlice([]int{0, 90, 180, 270}),
			},
			"lcm_settings_restricted_access": {
				Description: "Require the device password to change settings from the LCD screen.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"lcm_tracker_enabled": {
				Description: "Show the device's location tracker on the LCD screen.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"lcm_tracker_seed": {
				Description:  "Location tracker seed, up to 50 characters.",
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.StringLenBetween(0, 50),
			},
			"stp_priority": {
				Description:  "Spanning tree bridge priority. Lower wins the root election; 32768 is the default.",
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validation.StringInSlice([]string{"0", "4096", "8192", "12288", "16384", "20480", "24576", "28672", "32768", "36864", "40960", "45056", "49152", "53248", "57344", "61440"}, false),
			},
			"stp_version": {
				Description:  "Spanning tree version.",
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validation.StringInSlice([]string{"stp", "rstp", "disabled"}, false),
			},
			// Static IP. go-unifi types this as a STRUCT with `omitempty`,
			// which does nothing on a struct -- so an empty ConfigNetwork is
			// marshalled as {} on every write and the controller DROPS the
			// device's static addressing. Verified: five managed devices lost
			// theirs while every unmanaged one kept it. Declaring the block is
			// what prevents that, so record it for every managed device.
			"config_network": {
				Description: "Device management addressing. Declare it for every managed device: " +
					"leaving it out sends an empty object and the controller drops the static config, " +
					"after which the device DHCPs on its next boot.",
				Type:     schema.TypeList,
				MaxItems: 1,
				Optional: true,
				Computed: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"type": {
							Type:         schema.TypeString,
							Description:  "`static` or `dhcp`.",
							Optional:     true,
							Computed:     true,
							ValidateFunc: validation.StringInSlice([]string{"static", "dhcp"}, false),
						},
						"ip":         {Type: schema.TypeString, Description: "Management IP when static.", Optional: true, Computed: true},
						"netmask":    {Type: schema.TypeString, Description: "Netmask.", Optional: true, Computed: true},
						"gateway":    {Type: schema.TypeString, Description: "Default gateway.", Optional: true, Computed: true},
						"dns1":       {Type: schema.TypeString, Description: "Primary DNS.", Optional: true, Computed: true},
						"dns2":       {Type: schema.TypeString, Description: "Secondary DNS.", Optional: true, Computed: true},
						"dns_suffix": {Type: schema.TypeString, Description: "DNS search suffix.", Optional: true, Computed: true},
					},
				},
			},
			"port_override": {
				// TODO: this should really be a map or something when possible in the SDK
				// see https://github.com/hashicorp/terraform-plugin-sdk/issues/62
				Description: "A list of port-specific configuration overrides for UniFi switches. This allows you to customize individual port settings such as:\n" +
					"  * Port names and labels for easy identification\n" +
					"  * Port profiles for VLAN and security settings\n" +
					"  * Per-port native (untagged) and tagged VLAN behavior, inline, without authoring a `unifi_port_profile`\n" +
					"  * Operating modes for special functions\n\n" +
					"Common use cases include:\n" +
					"  * Setting up trunk ports for inter-switch connections\n" +
					"  * Configuring PoE settings for powered devices\n" +
					"  * Creating mirrored ports for network monitoring\n" +
					"  * Setting up link aggregation between switches or servers\n\n" +
					"**Warning:** the controller stores port overrides as a single array on the device and the provider replaces the " +
					"entire array on every apply. Any port whose override is set outside Terraform (e.g. via the UniFi UI or another " +
					"tool) and is NOT declared here will have its override reset to the controller default on the next apply. Declare " +
					"every port you want overridden.\n\n" +
					"**Tagged-VLAN model:** there is no positive \"allowed VLANs\" list. With `forward = \"customize\"`, tagged traffic is " +
					"*all* networks **minus** the ones listed in `excluded_network_ids`, so an empty `excluded_network_ids` means \"trunk " +
					"everything\", not \"trunk nothing\".",
				Type:     schema.TypeSet,
				Optional: true,
				// Uses the SDK's default full-element hash. A number-only hash
				// was tried and makes editing an existing entry produce NO diff
				// at all -- same number, same hash, so the set looks unchanged
				// however much the block changed. Controller echoes are kept out
				// of the set by fromPortOverride instead, which surfaces only the
				// attributes the practitioner declared.
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"number": {
							Description: "The physical port number on the switch to configure.",
							Type:        schema.TypeInt,
							Required:    true,
						},
						"name": {
							Description: "A friendly name for the port that will be displayed in the UniFi controller UI. Examples:\n" +
								"  * 'Uplink to Core Switch'\n" +
								"  * 'Conference Room AP'\n" +
								"  * 'Server LACP Group 1'\n" +
								"  * 'VoIP Phone Port'",
							Type:     schema.TypeString,
							Optional: true,
						},
						"port_profile_id": {
							Description: "The ID of a pre-configured port profile to apply to this port. Port profiles define settings like VLANs, PoE, and other port-specific configurations.",
							Type:        schema.TypeString,
							Optional:    true,
						},
						"op_mode": {
							Description: "The operating mode of the port. Valid values are:\n" +
								"  * `switch` - Normal switching mode (default)\n" +
								"    - Standard port operation for connecting devices\n" +
								"    - Supports VLANs and all standard switching features\n" +
								"  * `mirror` - Port mirroring for traffic analysis\n" +
								"    - Copies traffic from other ports for monitoring\n" +
								"    - Useful for network troubleshooting and security\n" +
								"  * `aggregate` - Link aggregation/bonding mode\n" +
								"    - Combines multiple ports for increased bandwidth\n" +
								"    - Used for switch uplinks or high-bandwidth servers",
							Type:         schema.TypeString,
							Optional:     true,
							Default:      "switch",
							ValidateFunc: validation.StringInSlice([]string{"switch", "mirror", "aggregate"}, false),
							DiffSuppressFunc: func(k, old, newValue string, d *schema.ResourceData) bool {
								if old == "" && newValue == "switch" {
									return true
								}
								return false
							},
						},
						"poe_mode": {
							Description: "The Power over Ethernet (PoE) mode for the port. Valid values are:\n" +
								"* `auto` - Automatically detect and power PoE devices (recommended)\n" +
								"  - Provides power based on device negotiation\n" +
								"  - Safest option for most PoE devices\n" +
								"* `pasv24` - Passive 24V PoE\n" +
								"  - For older UniFi devices requiring passive 24V\n" +
								"  - Use with caution to avoid damage\n" +
								"* `passthrough` - PoE passthrough mode\n" +
								"  - For daisy-chaining PoE devices\n" +
								"  - Available on select UniFi switches\n" +
								"* `off` - Disable PoE on the port\n" +
								"  - For non-PoE devices\n" +
								"  - To prevent unwanted power delivery",
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringInSlice([]string{"auto", "pasv24", "passthrough", "off"}, false),
						},
						// Remaining port_override fields, generated from
						// go-unifi's DevicePortOverrides so the set is complete:
						// an entry is REPLACED on write, so any field the schema
						// cannot express is deleted from a managed port.
						"dot1x_ctrl": {
							Description:  "802.1X control mode for this port.",
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringInSlice([]string{"auto", "force_authorized", "force_unauthorized", "mac_based", "multi_host"}, false),
						},
						"dot1x_idle_timeout": {
							Description: "802.1X idle timeout in seconds.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"egress_rate_limit_kbps": {
							Description: "Egress rate limit in Kbps. Requires `egress_rate_limit_kbps_enabled`.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"egress_rate_limit_kbps_enabled": {
							Description: "Enable the egress rate limit.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"fec_mode": {
							Description:  "Forward error correction mode, for SFP+ ports.",
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringInSlice([]string{"rs-fec", "fc-fec", "default", "disabled"}, false),
						},
						"flow_control_enabled": {
							Description: "Enable 802.3x flow control (pause frames).",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"lldpmed_enabled": {
							Description: "Enable LLDP-MED on this port.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"lldpmed_notify_enabled": {
							Description: "Send LLDP-MED topology change notifications.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"mirror_port_idx": {
							Description: "Source port to mirror, when `op_mode = \"mirror\"`.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"multicast_router_networkconf_ids": {
							Description: "Networks on which this port is treated as a multicast router port. Needs IGMP snooping enabled on the network.",
							Type:        schema.TypeSet,
							Optional:    true,
							Elem:        &schema.Schema{Type: schema.TypeString},
						},
						"port_keepalive_enabled": {
							Description: "Enable port keepalive.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"priority_queue1_level": {
							Description: "Egress priority queue 1 level.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"priority_queue2_level": {
							Description: "Egress priority queue 2 level.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"priority_queue3_level": {
							Description: "Egress priority queue 3 level.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"priority_queue4_level": {
							Description: "Egress priority queue 4 level.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"stormctrl_bcast_enabled": {
							Description: "Enable broadcast storm control.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"stormctrl_bcast_level": {
							Description: "Broadcast storm control threshold as a percentage. Used when `stormctrl_type = \"level\"`.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"stormctrl_bcast_rate": {
							Description: "Broadcast storm control threshold in packets per second. Used when `stormctrl_type = \"rate\"`.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"stormctrl_mcast_enabled": {
							Description: "Enable multicast storm control.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"stormctrl_mcast_level": {
							Description: "Multicast storm control threshold as a percentage.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"stormctrl_mcast_rate": {
							Description: "Multicast storm control threshold in packets per second.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"stormctrl_type": {
							Description:  "Whether storm control thresholds are expressed as a percentage (`level`) or packets per second (`rate`).",
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringInSlice([]string{"level", "rate"}, false),
						},
						"stormctrl_ucast_enabled": {
							Description: "Enable unknown-unicast storm control.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"stormctrl_ucast_level": {
							Description: "Unknown-unicast storm control threshold as a percentage.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						"stormctrl_ucast_rate": {
							Description: "Unknown-unicast storm control threshold in packets per second.",
							Type:        schema.TypeInt,
							Optional:    true,
						},
						// The controller accepts all of the following on a port
						// override; they were simply missing from the schema.
						//
						// CAVEAT on the booleans: the go-unifi fields are `bool`
						// with `omitempty`, so a false is dropped from the payload
						// and the controller keeps what it had. They can be turned
						// ON from Terraform but not OFF.
						"port_security_enabled": {
							Description: "Enable port security on this port.\n\n" +
								"With `port_security_mac_address` set, only those MACs may use the port. With NO MAC " +
								"addresses and no native network, the controller reports the port as `forward = \"disabled\"` " +
								"-- this is how a port is administratively shut.\n\n" +
								"**Cannot be turned off from Terraform**: the underlying field is `omitempty`, so a false is " +
								"dropped from the payload and the controller keeps the previous value.",
							Type:     schema.TypeBool,
							Optional: true,
						},
						"port_security_mac_address": {
							Description: "MAC addresses permitted on this port when `port_security_enabled` is true. " +
								"Leave empty to shut the port rather than restrict it.",
							Type:     schema.TypeSet,
							Optional: true,
							Elem: &schema.Schema{
								Type:         schema.TypeString,
								ValidateFunc: validation.StringMatch(utils.MacAddressRegexp, "Mac address is invalid"),
							},
						},
						"stp_port_mode": {
							Description: "Enable STP on this port. Cannot be turned off from Terraform -- see `port_security_enabled`.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"autoneg": {
							Description: "Enable speed/duplex auto-negotiation. Pinning a port means `autoneg = false` plus " +
								"`speed`, but a false cannot be WRITTEN (see `port_security_enabled`) -- an existing " +
								"`autoneg = false` on the controller is preserved because the field is omitted.",
							Type:     schema.TypeBool,
							Optional: true,
						},
						"speed": {
							Description: "Pin the port speed in Mbps. Only meaningful with auto-negotiation off.",
							Type:        schema.TypeInt,
							Optional:    true,
							ValidateFunc: validation.IntInSlice([]int{
								10, 100, 1000, 2500, 5000, 10000, 20000, 25000, 40000, 50000, 100000,
							}),
						},
						"full_duplex": {
							Description: "Full duplex when auto-negotiation is off. Cannot be turned off from Terraform -- see `port_security_enabled`.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"isolation": {
							Description: "Isolate this port from other isolated ports on the switch. Cannot be turned off from Terraform -- see `port_security_enabled`.",
							Type:        schema.TypeBool,
							Optional:    true,
						},
						"aggregate_num_ports": {
							Description: "The number of ports to include in a link aggregation group (LAG). Valid range: 2-8 ports. Used when:\n" +
								"* Creating switch-to-switch uplinks for increased bandwidth\n" +
								"* Setting up high-availability connections\n" +
								"* Connecting to servers requiring more bandwidth\n" +
								"Note: All ports in the LAG must be sequential and have matching configurations.",
							Type:         schema.TypeInt,
							Optional:     true,
							ValidateFunc: validation.IntBetween(2, 8),
							DiffSuppressFunc: func(k, old, newValue string, d *schema.ResourceData) bool {
								if old == strconv.Itoa(0) && newValue == "" {
									return true
								}
								return false
							},
						},
						"native_networkconf_id": {
							Description: "The ID of the network to use as the native (untagged) network on this port. " +
								"This is typically used for:\n" +
								"* Access ports where devices need untagged access\n" +
								"* Trunk ports to specify the native VLAN\n" +
								"* Management networks for network devices\n\n" +
								"Computed when not set, so the controller's current value (which it may auto-populate on a port) " +
								"is preserved without producing a diff. Note: the underlying field uses `omitempty`, so once set it " +
								"cannot be cleared back to empty through Terraform — change it to another network ID instead.",
							Type:     schema.TypeString,
							Optional: true,
						},
						"tagged_vlan_mgmt": {
							Description: "VLAN tagging behavior for the port. Valid values are:\n" +
								"* `auto` - Automatically handle VLAN tags (recommended)\n" +
								"* `block_all` - Block all VLAN tagged traffic\n" +
								"* `custom` - Custom VLAN configuration (use with `forward = \"customize\"` and `excluded_network_ids`)\n\n" +
								"Computed when not set, so the controller's current value is preserved without producing a diff. " +
								"Note: the underlying field uses `omitempty`, so once set it cannot be cleared back to empty " +
								"through Terraform — change it to another value instead.",
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringInSlice([]string{"auto", "block_all", "custom"}, false),
						},
						"forward": {
							Description: "VLAN forwarding mode for the port. Valid values are:\n" +
								"  * `all` - Forward all VLANs (trunk port)\n" +
								"  * `native` - Only forward untagged traffic (access port)\n" +
								"  * `customize` - Forward selected VLANs (use with `excluded_network_ids`)\n" +
								"  * `disabled` - Disable VLAN forwarding\n\n" +
								"This attribute has NO default: leaving it unset keeps the port's existing forwarding behavior " +
								"(the value is computed from the controller). Note: the underlying field uses `omitempty`, so once " +
								"set it cannot be cleared back to empty through Terraform — change it to another value instead.",
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringInSlice([]string{"all", "native", "customize", "disabled"}, false),
						},
						"excluded_network_ids": {
							Description: "Set of network IDs to exclude when `forward = \"customize\"`. Tagged traffic on the port is " +
								"*all* networks minus the ones listed here, so an empty set means \"trunk everything\". " +
								"Computed when not set, so the controller's current exclusions are preserved without producing a diff.",
							Type:     schema.TypeSet,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
						"voice_networkconf_id": {
							Description: "The ID of the network to use for Voice over IP (VoIP) traffic on this port, for automatic " +
								"voice-VLAN assignment in conjunction with LLDP-MED.\n\n" +
								"Computed when not set, so the controller's current value is preserved without producing a diff. " +
								"Note: the underlying field uses `omitempty`, so once set it cannot be cleared back to empty " +
								"through Terraform — change it to another network ID instead.",
							Type:     schema.TypeString,
							Optional: true,
						},
						"setting_preference": {
							Description: "Whether the port's settings are taken from a profile (`auto`) or set per-port (`manual`). " +
								"Valid values are `auto` and `manual`. Per-port VLAN overrides (`native_networkconf_id`, " +
								"`tagged_vlan_mgmt`, `forward`, `excluded_network_ids`) generally require `setting_preference = \"manual\"` " +
								"to persist on the controller; with `auto` the controller may revert inline overrides to profile/auto " +
								"behavior. Setting this to `manual` also overrides any `port_profile_id` on the same port. " +
								"Computed when not set, so the value the controller attaches to the port is preserved without producing a diff.",
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringInSlice([]string{"auto", "manual"}, false),
						},
					},
				},
			},

			"radio": {
				Description: "Per-band radio configuration for access points. Each block configures ONE band " +
					"(`ng` = 2.4GHz, `na` = 5GHz, `6e` = 6GHz). Only the bands you declare are managed — undeclared " +
					"bands are left untouched (the provider read-modify-writes the device's full radio table to preserve " +
					"them, so declaring just one band will not wipe the others). Common uses: disable a band " +
					"(`tx_power_mode = \"disabled\"`), pin a channel/width, or set a minimum-RSSI client kick. Applies to " +
					"access points; has no effect on switches.\n\n" +
					"Note: like other device fields, only non-zero values are written, so a field cannot be set back to its " +
					"zero value through Terraform — manage by overriding with explicit non-zero values.",
				Type:     schema.TypeSet,
				Optional: true,
				Set:      radioSetHash,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Description:  "The radio band this block configures: `ng` (2.4GHz), `na` (5GHz), or `6e` (6GHz).",
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: validation.StringInSlice([]string{"ng", "na", "6e"}, false),
						},
						"channel": {
							Description: "The channel for this radio (band-specific), or `auto` to let the controller choose.",
							Type:        schema.TypeString,
							Optional:    true,
							Computed:    true,
						},
						"ht": {
							Description:  "Channel width in MHz for this radio (e.g. 20, 40, 80, 160, 320).",
							Type:         schema.TypeInt,
							Optional:     true,
							Computed:     true,
							ValidateFunc: validation.IntInSlice([]int{20, 40, 80, 160, 240, 320, 1080, 2160, 4320}),
						},
						"tx_power_mode": {
							Description:  "Transmit-power mode: `auto`, `low`, `medium`, `high`, `custom`, or `disabled`. `disabled` turns the radio off (e.g. to suppress an unused 2.4GHz band on an in-wall AP).",
							Type:         schema.TypeString,
							Optional:     true,
							Computed:     true,
							ValidateFunc: validation.StringInSlice([]string{"auto", "low", "medium", "high", "custom", "disabled"}, false),
						},
						"tx_power": {
							Description: "Custom transmit power in dBm, used when `tx_power_mode = \"custom\"`; otherwise leave unset.",
							Type:        schema.TypeString,
							Optional:    true,
							Computed:    true,
						},
						"min_rssi_enabled": {
							Description: "Whether the minimum-RSSI client-disconnect threshold is enabled on this radio. Applied together with `min_rssi`.",
							Type:        schema.TypeBool,
							Optional:    true,
							Computed:    true,
						},
						"min_rssi": {
							Description: "Minimum RSSI in dBm (negative) below which clients are disconnected, when `min_rssi_enabled` is true.",
							Type:        schema.TypeInt,
							Optional:    true,
							Computed:    true,
						},
					},
				},
			},

			"ether_lighting": {
				Description: "Etherlighting configuration for switches with per-port LEDs (e.g. USW Pro Max). " +
					"`mode = \"network\"` colors each port's LED by the VLAN/network it serves (per-network colors come from " +
					"the site-level Etherlighting palette); `mode = \"speed\"` colors by link speed. Only the fields you set " +
					"are written — unset fields keep their controller-side values (read-modify-write overlay). Devices without " +
					"Etherlighting hardware ignore this object.",
				Type:     schema.TypeList,
				MaxItems: 1,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"mode": {
							Description:  "Color scheme: `network` (color by VLAN/network) or `speed` (color by link speed).",
							Type:         schema.TypeString,
							Optional:     true,
							Computed:     true,
							ValidateFunc: validation.StringInSlice([]string{"network", "speed"}, false),
						},
						"led_mode": {
							Description:  "`etherlighting` (colored per-port LEDs) or `standard` (plain status LEDs).",
							Type:         schema.TypeString,
							Optional:     true,
							Computed:     true,
							ValidateFunc: validation.StringInSlice([]string{"etherlighting", "standard"}, false),
						},
						"behavior": {
							Description:  "LED animation: `steady` or `breath`.",
							Type:         schema.TypeString,
							Optional:     true,
							Computed:     true,
							ValidateFunc: validation.StringInSlice([]string{"steady", "breath"}, false),
						},
						"brightness": {
							Description:  "LED brightness, 1-100.",
							Type:         schema.TypeInt,
							Optional:     true,
							Computed:     true,
							ValidateFunc: validation.IntBetween(1, 100),
						},
					},
				},
			},

			"rps_port_override": {
				Description: "Outlet configuration for a redundant power supply (USP-RPS). Each block is one outlet. " +
					"The controller REPLACES the whole outlet table on write, so declare every outlet you want to keep. " +
					"Leaving the block out entirely preserves whatever the controller has.",
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"number": {
							Description:  "Outlet number, 1-8.",
							Type:         schema.TypeInt,
							Required:     true,
							ValidateFunc: validation.IntBetween(1, 8),
						},
						"name": {
							Description:  "Outlet name, up to 32 characters.",
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validation.StringLenBetween(0, 32),
						},
						"mode": {
							Description: "`auto` (supply power only when the device's own PSU fails), `force_active`, " +
								"`manual` or `disabled`.",
							Type:     schema.TypeString,
							Optional: true,
							ValidateFunc: validation.StringInSlice(
								[]string{"auto", "force_active", "manual", "disabled"}, false),
						},
					},
				},
			},

			"allow_adoption": {
				Description: "Whether to automatically adopt the device when creating this resource. When true:\n" +
					"* The controller will attempt to adopt the device\n" +
					"* Device must be in a pending adoption state\n" +
					"* Device must be accessible on the network\n" +
					"Set to false if you want to manage adoption manually.",
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},
			"forget_on_destroy": {
				Description: "Whether to forget (un-adopt) the device when this resource is destroyed. When true:\n" +
					"* The device will be removed from the controller\n" +
					"* The device will need to be readopted to be managed again\n" +
					"* Device configuration will be reset\n" +
					"Set to false to keep the device adopted when removing from Terraform management.",
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},
		},
	}
}

func resourceDeviceImport(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	c, ok := meta.(*base.Client)
	if !ok {
		return nil, fmt.Errorf("unexpected meta type: %T", meta)
	}
	id := d.Id()
	site, _ := d.Get("site").(string)
	if site == "" {
		site = c.Site
	}

	if colons := strings.Count(id, ":"); colons == 1 || colons == 6 {
		importParts := strings.SplitN(id, ":", 2)
		site = importParts[0]
		id = importParts[1]
	}

	if utils.MacAddressRegexp.MatchString(id) {
		// look up id by mac
		mac := utils.CleanMAC(id)
		device, err := c.GetDeviceByMAC(ctx, site, mac)
		if err != nil {
			return nil, err
		}

		id = device.ID
	}

	if id != "" {
		d.SetId(id)
	}
	if site != "" {
		if err := d.Set("site", site); err != nil {
			return nil, err
		}
	}

	return []*schema.ResourceData{d}, nil
}

func resourceDeviceCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, ok := meta.(*base.Client)
	if !ok {
		return diag.Errorf("unexpected meta type: %T", meta)
	}

	site, _ := d.Get("site").(string)
	if site == "" {
		site = c.Site
	}

	mac, _ := d.Get("mac").(string)
	if mac == "" {
		return diag.Errorf("no MAC address specified, please import the device using terraform import")
	}

	mac = utils.CleanMAC(mac)
	device, err := c.GetDeviceByMAC(ctx, site, mac)

	if device == nil {
		return diag.Errorf("device not found using mac %q", mac)
	}
	if err != nil {
		return diag.FromErr(err)
	}

	if !device.Adopted {
		allowAdoption, _ := d.Get("allow_adoption").(bool)
		if !allowAdoption {
			return diag.Errorf("Device must be adopted before it can be managed")
		}

		err := c.AdoptDevice(ctx, site, mac)
		if err != nil {
			return diag.FromErr(err)
		}

		device, err = waitForDeviceState(ctx, d, meta, unifi.DeviceStateConnected, []unifi.DeviceState{unifi.DeviceStateAdopting, unifi.DeviceStatePending, unifi.DeviceStateProvisioning, unifi.DeviceStateUpgrading}, 2*time.Minute)
		if err != nil {
			return diag.FromErr(err)
		}
	}

	d.SetId(device.ID)
	return resourceDeviceUpdate(ctx, d, meta)
}

func resourceDeviceUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, ok := meta.(*base.Client)
	if !ok {
		return diag.Errorf("unexpected meta type: %T", meta)
	}

	site, _ := d.Get("site").(string)
	if site == "" {
		site = c.Site
	}

	req, err := resourceDeviceGetResourceData(d)
	if err != nil {
		return diag.FromErr(err)
	}

	req.ID = d.Id()
	req.SiteID = site

	// Radio table and Etherlighting are controller-side structures managed
	// with patch semantics: fetch the device's current config once and overlay
	// only the declared fields, so undeclared bands/fields keep their
	// controller-side values. (Radio table additionally needs the full merged
	// array sent because UniFi replaces arrays wholesale on PUT.) When neither
	// block is declared, nothing extra is sent (prior behavior).
	// rps_override is fetched unconditionally, not only when declared. It is a
	// bare struct with `omitempty`, which does nothing on a struct, so leaving
	// it zero serialises {} and erases a redundant power supply's outlet table.
	radios, _ := d.Get("radio").(*schema.Set)
	etherLighting, _ := d.Get("ether_lighting").([]interface{})
	rpsPorts, _ := d.Get("rps_port_override").(*schema.Set)
	{
		current, err := c.GetDevice(ctx, site, d.Id())
		if err != nil {
			return diag.FromErr(fmt.Errorf("unable to read current device config for merge: %w", err))
		}
		if radios.Len() > 0 {
			req.RadioTable = mergeRadios(current.RadioTable, radios)
		}
		if len(etherLighting) > 0 {
			etherLightingMap, _ := etherLighting[0].(map[string]interface{})
			req.EtherLighting = mergeEtherLighting(current.EtherLighting, etherLightingMap)
		}
		req.RpsOverride = current.RpsOverride
		if rpsPorts.Len() > 0 {
			req.RpsOverride.RpsPortTable = rpsPortTable(rpsPorts)
		}
	}

	// go-unifi v1.9.2's updateDevice converts a successful-but-empty PUT response into
	// unifi.ErrNotFound (see utils.ReReadOnUpdateNotFound / issue #98); re-read to tell
	// a spurious error from a genuine out-of-band deletion.
	resp, err := c.UpdateDevice(ctx, site, req)
	resp, found, err := utils.ReReadOnUpdateNotFound(resp, err, func() (*unifi.Device, error) {
		return c.GetDevice(ctx, site, req.ID)
	})
	if err != nil {
		return diag.FromErr(err)
	}
	if !found {
		d.SetId("")
		return nil
	}

	// Second, minimal PUT carrying only the zero values go-unifi dropped. Safe
	// because the device PUT merges: everything absent from this body keeps
	// whatever the previous write left.
	if zeros := declaredZeros(d); len(zeros) > 0 {
		if err := c.Do(ctx, http.MethodPut,
			fmt.Sprintf("s/%s/rest/device/%s", site, req.ID), zeros, nil); err != nil {
			return diag.FromErr(fmt.Errorf("unable to write zero-valued attributes %v: %w",
				zeros, err))
		}
	}

	_, err = waitForDeviceState(ctx, d, meta, unifi.DeviceStateConnected, []unifi.DeviceState{unifi.DeviceStateAdopting, unifi.DeviceStateProvisioning}, 1*time.Minute)
	if err != nil {
		return diag.FromErr(err)
	}

	return resourceDeviceSetResourceData(resp, d, site)
}

func resourceDeviceDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, ok := meta.(*base.Client)
	if !ok {
		return diag.Errorf("unexpected meta type: %T", meta)
	}

	if forgetOnDestroy, _ := d.Get("forget_on_destroy").(bool); !forgetOnDestroy {
		return nil
	}

	site, _ := d.Get("site").(string)
	mac, _ := d.Get("mac").(string)

	if site == "" {
		site = c.Site
	}
	err := retry.RetryContext(ctx, 1*time.Minute, func() *retry.RetryError {
		internalErr := c.ForgetDevice(ctx, site, mac)
		if internalErr == nil {
			return nil
		}
		if utils.IsServerErrorContains(internalErr, "api.err.DeviceBusy") {
			return retry.RetryableError(internalErr)
		}
		return retry.NonRetryableError(internalErr)
	})
	if err != nil {
		return diag.FromErr(err)
	}

	_, err = waitForDeviceState(ctx, d, meta, unifi.DeviceStatePending, []unifi.DeviceState{unifi.DeviceStateConnected, unifi.DeviceStateDeleting}, 1*time.Minute)
	if !errors.Is(err, unifi.ErrNotFound) {
		return diag.FromErr(err)
	}

	return nil
}

func resourceDeviceRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, ok := meta.(*base.Client)
	if !ok {
		return diag.Errorf("unexpected meta type: %T", meta)
	}

	id := d.Id()

	site, _ := d.Get("site").(string)
	if site == "" {
		site = c.Site
	}

	resp, err := c.GetDevice(ctx, site, id)
	if errors.Is(err, unifi.ErrNotFound) {
		d.SetId("")
		return nil
	}
	if err != nil {
		return diag.FromErr(err)
	}

	return resourceDeviceSetResourceData(resp, d, site)
}

func resourceDeviceSetResourceData(resp *unifi.Device, d *schema.ResourceData, site string) diag.Diagnostics {
	portOverrides := setFromPortOverrides(resp.PortOverrides, d)

	values := map[string]interface{}{
		"site":                site,
		"mac":                 resp.MAC,
		"name":                resp.Name,
		"disabled":            resp.Disabled,
		"switch_vlan_enabled": resp.SwitchVLANEnabled,
		"config_network": []interface{}{map[string]interface{}{
			"type":       resp.ConfigNetwork.Type,
			"ip":         resp.ConfigNetwork.IP,
			"netmask":    resp.ConfigNetwork.Netmask,
			"gateway":    resp.ConfigNetwork.Gateway,
			"dns1":       resp.ConfigNetwork.DNS1,
			"dns2":       resp.ConfigNetwork.DNS2,
			"dns_suffix": resp.ConfigNetwork.DNSsuffix,
		}},
		"dpi_enabled":                    resp.DPIEnabled,
		"dot1x_fallback_networkconf_id":  resp.Dot1XFallbackNetworkID,
		"dot1x_portctrl_enabled":         resp.Dot1XPortctrlEnabled,
		"flowctrl_enabled":               resp.FlowctrlEnabled,
		"jumboframe_enabled":             resp.JumboframeEnabled,
		"led_override":                   resp.LedOverride,
		"led_override_color":             resp.LedOverrideColor,
		"led_override_color_brightness":  resp.LedOverrideColorBrightness,
		"mgmt_network_id":                resp.MgmtNetworkID,
		"outlet_enabled":                 resp.OutletEnabled,
		"outlet_power_cycle_enabled":     resp.OutletPowerCycleEnabled,
		"power_source_ctrl":              resp.PowerSourceCtrl,
		"power_source_ctrl_budget":       resp.PowerSourceCtrlBudget,
		"power_source_ctrl_enabled":      resp.PowerSourceCtrlEnabled,
		"resetbtn_enabled":               resp.ResetbtnEnabled,
		"snmp_contact":                   resp.SnmpContact,
		"snmp_location":                  resp.SnmpLocation,
		"lcm_brightness":                 resp.LcmBrightness,
		"lcm_brightness_override":        resp.LcmBrightnessOverride,
		"lcm_idle_timeout":               resp.LcmIDleTimeout,
		"lcm_idle_timeout_override":      resp.LcmIDleTimeoutOverride,
		"lcm_night_mode_begins":          resp.LcmNightModeBegins,
		"lcm_night_mode_ends":            resp.LcmNightModeEnds,
		"lcm_orientation_override":       resp.LcmOrientationOverride,
		"lcm_settings_restricted_access": resp.LcmSettingsRestrictedAccess,
		"lcm_tracker_enabled":            resp.LcmTrackerEnabled,
		"lcm_tracker_seed":               resp.LcmTrackerSeed,
		"stp_priority":                   resp.StpPriority,
		"stp_version":                    resp.StpVersion,
		"port_override":                  portOverrides,
		"radio":                          radiosFromDevice(resp, d),
		"ether_lighting":                 etherLightingFromDevice(resp, d),
		"rps_port_override":              rpsPortsFromDevice(resp, d),
	}
	for k, v := range values {
		if err := d.Set(k, v); err != nil {
			return diag.FromErr(err)
		}
	}

	return nil
}

// notWritten are attributes that exist only in the provider, so they must
// never be sent to the controller by writeDeclaredZeros.
var notWritten = map[string]bool{
	"mac": true, "name": true, "site": true, "id": true,
	"allow_adoption": true, "forget_on_destroy": true, "disabled": true,
}

// declaredZeros returns the scalar attributes the practitioner explicitly set
// to a zero value (0, false, ""). go-unifi tags most of them `omitempty`, so
// those are dropped from the payload -- and because the controller's device
// PUT MERGES rather than replaces, the old value simply survives and the write
// silently does nothing.
//
// Verified on hardware: the controller accepts an explicit 0 and stores it, so
// this is a client limitation, not a controller one.
func declaredZeros(d *schema.ResourceData) map[string]interface{} {
	raw := d.GetRawConfig()
	if raw.IsNull() || !raw.IsKnown() || !raw.Type().IsObjectType() {
		return nil
	}
	out := map[string]interface{}{}
	for name, v := range raw.AsValueMap() {
		if notWritten[name] || v.IsNull() || !v.IsKnown() {
			continue
		}
		switch v.Type() {
		case cty.Number:
			n, _ := v.AsBigFloat().Float64()
			if n == 0 {
				out[name] = 0
			}
		case cty.Bool:
			if v.False() {
				out[name] = false
			}
		case cty.String:
			if v.AsString() == "" {
				out[name] = ""
			}
		}
	}
	return out
}

// rpsPortTable converts declared outlet blocks into the controller's table.
func rpsPortTable(ports *schema.Set) []unifi.DeviceRpsPortTable {
	out := make([]unifi.DeviceRpsPortTable, 0, ports.Len())
	for _, raw := range ports.List() {
		m, _ := raw.(map[string]interface{})
		number, _ := m["number"].(int)
		name, _ := m["name"].(string)
		mode, _ := m["mode"].(string)
		out = append(out, unifi.DeviceRpsPortTable{
			PortIDX:  number,
			Name:     name,
			PortMode: mode,
		})
	}
	return out
}

// rpsPortsFromDevice returns outlet state only when the user declares the
// block, so unmanaged devices never produce a diff.
func rpsPortsFromDevice(resp *unifi.Device, d *schema.ResourceData) []map[string]interface{} {
	declared, _ := d.Get("rps_port_override").(*schema.Set)
	if declared == nil || declared.Len() == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(resp.RpsOverride.RpsPortTable))
	for _, p := range resp.RpsOverride.RpsPortTable {
		out = append(out, map[string]interface{}{
			"number": p.PortIDX,
			"name":   p.Name,
			"mode":   p.PortMode,
		})
	}
	return out
}

// etherLightingFromDevice returns ether_lighting state only when the user
// declares the block, so unmanaged devices never produce a diff.
func etherLightingFromDevice(resp *unifi.Device, d *schema.ResourceData) []map[string]interface{} {
	etherLighting, _ := d.Get("ether_lighting").([]interface{})
	if len(etherLighting) == 0 {
		return nil
	}
	return []map[string]interface{}{{
		"mode":       resp.EtherLighting.Mode,
		"led_mode":   resp.EtherLighting.LedMode,
		"behavior":   resp.EtherLighting.Behavior,
		"brightness": resp.EtherLighting.Brightness,
	}}
}

// mergeEtherLighting overlays the declared ether_lighting fields onto the
// device's current config, preserving any fields the user didn't set.
func mergeEtherLighting(current unifi.DeviceEtherLighting, m map[string]interface{}) unifi.DeviceEtherLighting {
	r := current
	if v, _ := m["mode"].(string); v != "" {
		r.Mode = v
	}
	if v, _ := m["led_mode"].(string); v != "" {
		r.LedMode = v
	}
	if v, _ := m["behavior"].(string); v != "" {
		r.Behavior = v
	}
	if v, _ := m["brightness"].(int); v != 0 {
		r.Brightness = v
	}
	return r
}

// radioSetHash keys the `radio` set by band only, so changes to Computed
// fields (channel, ht, …) don't churn set membership during plan/apply.
func radioSetHash(v interface{}) int {
	m, _ := v.(map[string]interface{})
	name, _ := m["name"].(string)
	return schema.HashString(name)
}

// radiosFromDevice returns radio state for only the bands the user manages
// (present in config/state), so undeclared bands on the device never produce
// a diff.
func radiosFromDevice(resp *unifi.Device, d *schema.ResourceData) []map[string]interface{} {
	managed := map[string]bool{}
	radioSet, _ := d.Get("radio").(*schema.Set)
	for _, item := range radioSet.List() {
		m, _ := item.(map[string]interface{})
		name, _ := m["name"].(string)
		managed[name] = true
	}
	radios := make([]map[string]interface{}, 0, len(managed))
	for _, r := range resp.RadioTable {
		if managed[r.Radio] {
			radios = append(radios, fromRadio(r))
		}
	}
	return radios
}

func fromRadio(r unifi.DeviceRadioTable) map[string]interface{} {
	return map[string]interface{}{
		"name":             r.Radio,
		"channel":          r.Channel,
		"ht":               r.Ht,
		"tx_power_mode":    r.TxPowerMode,
		"tx_power":         r.TxPower,
		"min_rssi":         r.MinRssi,
		"min_rssi_enabled": r.MinRssiEnabled,
	}
}

// mergeRadios overlays the declared radio blocks onto the device's current
// radio_table, preserving every band's existing settings and changing only the
// non-zero fields the user specified. Bands not present in `current` are
// appended. The full merged list is returned so the wholesale-replace PUT keeps
// all bands intact.
func mergeRadios(current []unifi.DeviceRadioTable, set *schema.Set) []unifi.DeviceRadioTable {
	byBand := map[string]unifi.DeviceRadioTable{}
	order := make([]string, 0, len(current))
	for _, r := range current {
		byBand[r.Radio] = r
		order = append(order, r.Radio)
	}
	for _, item := range set.List() {
		m, _ := item.(map[string]interface{})
		band, _ := m["name"].(string)
		r, ok := byBand[band]
		if !ok {
			r = unifi.DeviceRadioTable{Radio: band}
			order = append(order, band)
		}
		if v, _ := m["channel"].(string); v != "" {
			r.Channel = v
		}
		if v, _ := m["ht"].(int); v != 0 {
			r.Ht = v
		}
		if v, _ := m["tx_power_mode"].(string); v != "" {
			r.TxPowerMode = v
		}
		if v, _ := m["tx_power"].(string); v != "" {
			r.TxPower = v
		}
		if v, _ := m["min_rssi"].(int); v != 0 {
			r.MinRssi = v
			r.MinRssiEnabled, _ = m["min_rssi_enabled"].(bool)
		}
		byBand[band] = r
	}
	out := make([]unifi.DeviceRadioTable, 0, len(order))
	for _, b := range order {
		out = append(out, byBand[b])
	}
	return out
}

func resourceDeviceGetResourceData(d *schema.ResourceData) (*unifi.Device, error) {
	portOverrideSet, _ := d.Get("port_override").(*schema.Set)
	pos, err := setToPortOverrides(portOverrideSet)
	if err != nil {
		return nil, fmt.Errorf("unable to process port_override block: %w", err)
	}

	// TODO: pass Disabled once we figure out how to enable the device afterwards

	mac, _ := d.Get("mac").(string)
	name, _ := d.Get("name").(string)
	switchVLANEnabled, _ := d.Get("switch_vlan_enabled").(bool)

	// Only overlay when declared. An empty struct is still sent (omitempty is
	// a no-op on structs), so a device with no block declared loses its static
	// addressing -- which is exactly what happened before this existed.
	var configNetwork unifi.DeviceConfigNetwork
	if raw, ok := d.Get("config_network").([]interface{}); ok && len(raw) > 0 {
		if m, ok := raw[0].(map[string]interface{}); ok {
			configNetwork.Type, _ = m["type"].(string)
			configNetwork.IP, _ = m["ip"].(string)
			configNetwork.Netmask, _ = m["netmask"].(string)
			configNetwork.Gateway, _ = m["gateway"].(string)
			configNetwork.DNS1, _ = m["dns1"].(string)
			configNetwork.DNS2, _ = m["dns2"].(string)
			configNetwork.DNSsuffix, _ = m["dns_suffix"].(string)
		}
	}
	dpiEnabled, _ := d.Get("dpi_enabled").(bool)
	dot1xFallbackNetworkconfId, _ := d.Get("dot1x_fallback_networkconf_id").(string)
	dot1xPortctrlEnabled, _ := d.Get("dot1x_portctrl_enabled").(bool)
	flowctrlEnabled, _ := d.Get("flowctrl_enabled").(bool)
	jumboframeEnabled, _ := d.Get("jumboframe_enabled").(bool)
	ledOverride, _ := d.Get("led_override").(string)
	ledOverrideColor, _ := d.Get("led_override_color").(string)
	ledOverrideColorBrightness, _ := d.Get("led_override_color_brightness").(int)
	mgmtNetworkId, _ := d.Get("mgmt_network_id").(string)
	outletEnabled, _ := d.Get("outlet_enabled").(bool)
	outletPowerCycleEnabled, _ := d.Get("outlet_power_cycle_enabled").(bool)
	powerSourceCtrl, _ := d.Get("power_source_ctrl").(string)
	powerSourceCtrlBudget, _ := d.Get("power_source_ctrl_budget").(int)
	powerSourceCtrlEnabled, _ := d.Get("power_source_ctrl_enabled").(bool)
	resetbtnEnabled, _ := d.Get("resetbtn_enabled").(string)
	snmpContact, _ := d.Get("snmp_contact").(string)
	snmpLocation, _ := d.Get("snmp_location").(string)
	stpPriority, _ := d.Get("stp_priority").(string)
	stpVersion, _ := d.Get("stp_version").(string)
	lcmBrightness, _ := d.Get("lcm_brightness").(int)
	lcmBrightnessOverride, _ := d.Get("lcm_brightness_override").(bool)
	lcmIdleTimeout, _ := d.Get("lcm_idle_timeout").(int)
	lcmIdleTimeoutOverride, _ := d.Get("lcm_idle_timeout_override").(bool)
	lcmNightModeBegins, _ := d.Get("lcm_night_mode_begins").(string)
	lcmNightModeEnds, _ := d.Get("lcm_night_mode_ends").(string)
	lcmOrientationOverride, _ := d.Get("lcm_orientation_override").(int)
	lcmSettingsRestrictedAccess, _ := d.Get("lcm_settings_restricted_access").(bool)
	lcmTrackerEnabled, _ := d.Get("lcm_tracker_enabled").(bool)
	lcmTrackerSeed, _ := d.Get("lcm_tracker_seed").(string)

	return &unifi.Device{
		MAC:                         mac,
		Name:                        name,
		SwitchVLANEnabled:           switchVLANEnabled,
		ConfigNetwork:               configNetwork,
		DPIEnabled:                  dpiEnabled,
		Dot1XFallbackNetworkID:      dot1xFallbackNetworkconfId,
		Dot1XPortctrlEnabled:        dot1xPortctrlEnabled,
		FlowctrlEnabled:             flowctrlEnabled,
		JumboframeEnabled:           jumboframeEnabled,
		LedOverride:                 ledOverride,
		LedOverrideColor:            ledOverrideColor,
		LedOverrideColorBrightness:  ledOverrideColorBrightness,
		MgmtNetworkID:               mgmtNetworkId,
		OutletEnabled:               outletEnabled,
		OutletPowerCycleEnabled:     outletPowerCycleEnabled,
		PowerSourceCtrl:             powerSourceCtrl,
		PowerSourceCtrlBudget:       powerSourceCtrlBudget,
		PowerSourceCtrlEnabled:      powerSourceCtrlEnabled,
		ResetbtnEnabled:             resetbtnEnabled,
		SnmpContact:                 snmpContact,
		SnmpLocation:                snmpLocation,
		StpPriority:                 stpPriority,
		StpVersion:                  stpVersion,
		LcmBrightness:               lcmBrightness,
		LcmBrightnessOverride:       lcmBrightnessOverride,
		LcmIDleTimeout:              lcmIdleTimeout,
		LcmIDleTimeoutOverride:      lcmIdleTimeoutOverride,
		LcmNightModeBegins:          lcmNightModeBegins,
		LcmNightModeEnds:            lcmNightModeEnds,
		LcmOrientationOverride:      lcmOrientationOverride,
		LcmSettingsRestrictedAccess: lcmSettingsRestrictedAccess,
		LcmTrackerEnabled:           lcmTrackerEnabled,
		LcmTrackerSeed:              lcmTrackerSeed,
		PortOverrides:               pos,
	}, nil
}

func setToPortOverrides(set *schema.Set) ([]unifi.DevicePortOverrides, error) {
	// use a map here to remove any duplication
	overrideMap := map[int]unifi.DevicePortOverrides{}
	for _, item := range set.List() {
		data, ok := item.(map[string]interface{})
		if !ok {
			return nil, errors.New("unexpected data in block")
		}
		po, err := toPortOverride(data)
		if err != nil {
			return nil, fmt.Errorf("unable to create port override: %w", err)
		}
		overrideMap[po.PortIDX] = po
	}

	pos := make([]unifi.DevicePortOverrides, 0, len(overrideMap))
	for _, item := range overrideMap {
		pos = append(pos, item)
	}
	return pos, nil
}

// declaredPortAttrs maps port number -> the attribute names the practitioner
// actually wrote for that port. A port absent from the map (import, or a port
// overridden outside Terraform) yields nil, which means "surface everything".
func declaredPortAttrs(d *schema.ResourceData) map[int]map[string]bool {
	set, ok := d.Get("port_override").(*schema.Set)
	if !ok || set == nil {
		return nil
	}
	declared := make(map[int]map[string]bool, set.Len())
	for _, item := range set.List() {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		number, _ := m["number"].(int)
		attrs := make(map[string]bool, len(m))
		for k, v := range m {
			if !isZeroAttr(v) {
				attrs[k] = true
			}
		}
		declared[number] = attrs
	}
	return declared
}

// isZeroAttr reports whether an attribute is absent from the config. The SDK
// materialises every schema attribute in the map, so "not written" is
// indistinguishable from "written as the zero value" -- which is acceptable
// here because writing a zero value and omitting the field produce the same
// (omitempty) payload either way.
func isZeroAttr(v interface{}) bool {
	switch t := v.(type) {
	case string:
		return t == ""
	case int:
		return t == 0
	case bool:
		return !t
	case *schema.Set:
		return t == nil || t.Len() == 0
	case nil:
		return true
	}
	return false
}

func setFromPortOverrides(pos []unifi.DevicePortOverrides, d *schema.ResourceData) []map[string]interface{} {
	declared := declaredPortAttrs(d)
	list := make([]map[string]interface{}, 0, len(pos))
	for _, po := range pos {
		list = append(list, fromPortOverride(po, declared[po.PortIDX]))
	}
	return list
}

func toPortOverride(data map[string]interface{}) (unifi.DevicePortOverrides, error) {
	idx, _ := data["number"].(int)
	name, _ := data["name"].(string)
	profileID, _ := data["port_profile_id"].(string)
	opMode, _ := data["op_mode"].(string)
	poeMode, _ := data["poe_mode"].(string)
	aggregateNumPorts, _ := data["aggregate_num_ports"].(int)
	dot1xCtrl, _ := data["dot1x_ctrl"].(string)
	dot1xIdleTimeout, _ := data["dot1x_idle_timeout"].(int)
	egressRateLimitKbps, _ := data["egress_rate_limit_kbps"].(int)
	egressRateLimitKbpsEnabled, _ := data["egress_rate_limit_kbps_enabled"].(bool)
	fecMode, _ := data["fec_mode"].(string)
	flowControlEnabled, _ := data["flow_control_enabled"].(bool)
	lldpmedEnabled, _ := data["lldpmed_enabled"].(bool)
	lldpmedNotifyEnabled, _ := data["lldpmed_notify_enabled"].(bool)
	mirrorPortIdx, _ := data["mirror_port_idx"].(int)
	// Guarded, not asserted: callers may omit the key entirely and a hard
	// assertion on a nil interface panics.
	var multicastRouterNetworkconfIds []string
	if set, ok := data["multicast_router_networkconf_ids"].(*schema.Set); ok {
		multicastRouterNetworkconfIds, _ = utils.SetToStringSlice(set)
	}
	portKeepaliveEnabled, _ := data["port_keepalive_enabled"].(bool)
	priorityQueue1Level, _ := data["priority_queue1_level"].(int)
	priorityQueue2Level, _ := data["priority_queue2_level"].(int)
	priorityQueue3Level, _ := data["priority_queue3_level"].(int)
	priorityQueue4Level, _ := data["priority_queue4_level"].(int)
	stormctrlBcastEnabled, _ := data["stormctrl_bcast_enabled"].(bool)
	stormctrlBcastLevel, _ := data["stormctrl_bcast_level"].(int)
	stormctrlBcastRate, _ := data["stormctrl_bcast_rate"].(int)
	stormctrlMcastEnabled, _ := data["stormctrl_mcast_enabled"].(bool)
	stormctrlMcastLevel, _ := data["stormctrl_mcast_level"].(int)
	stormctrlMcastRate, _ := data["stormctrl_mcast_rate"].(int)
	stormctrlType, _ := data["stormctrl_type"].(string)
	stormctrlUcastEnabled, _ := data["stormctrl_ucast_enabled"].(bool)
	stormctrlUcastLevel, _ := data["stormctrl_ucast_level"].(int)
	stormctrlUcastRate, _ := data["stormctrl_ucast_rate"].(int)
	portSecurityEnabled, _ := data["port_security_enabled"].(bool)
	stpPortMode, _ := data["stp_port_mode"].(bool)
	autoneg, _ := data["autoneg"].(bool)
	speed, _ := data["speed"].(int)
	fullDuplex, _ := data["full_duplex"].(bool)
	isolation, _ := data["isolation"].(bool)

	var portSecurityMACs []string
	if set, ok := data["port_security_mac_address"].(*schema.Set); ok {
		var err error
		portSecurityMACs, err = utils.SetToStringSlice(set)
		if err != nil {
			return unifi.DevicePortOverrides{}, fmt.Errorf("unable to process port_security_mac_address: %w", err)
		}
	}

	var excludedNetworkIDs []string
	if set, ok := data["excluded_network_ids"].(*schema.Set); ok {
		var err error
		excludedNetworkIDs, err = utils.SetToStringSlice(set)
		if err != nil {
			return unifi.DevicePortOverrides{}, fmt.Errorf("unable to process excluded_network_ids: %w", err)
		}
	}

	// Per-port VLAN overrides. All of these are `omitempty` on the controller
	// side, so an unset (empty) value is dropped from the PUT. The user declares
	// the whole set of ports, and the device PUT replaces the `port_overrides`
	// array wholesale (no read-modify-write merge). comma-ok reads tolerate a
	// partially-populated data map (e.g. in unit tests).
	nativeNetworkID, _ := data["native_networkconf_id"].(string)
	taggedVLANMgmt, _ := data["tagged_vlan_mgmt"].(string)
	forward, _ := data["forward"].(string)
	voiceNetworkID, _ := data["voice_networkconf_id"].(string)
	settingPreference, _ := data["setting_preference"].(string)

	po := unifi.DevicePortOverrides{
		PortIDX:            idx,
		Name:               name,
		PortProfileID:      profileID,
		OpMode:             opMode,
		PoeMode:            poeMode,
		NATiveNetworkID:    nativeNetworkID,
		TaggedVLANMgmt:     taggedVLANMgmt,
		Forward:            forward,
		ExcludedNetworkIDs: excludedNetworkIDs,
		VoiceNetworkID:     voiceNetworkID,
		SettingPreference:  settingPreference,

		// All bool + omitempty: a false is omitted and the controller keeps
		// its existing value. These can be turned on, not off.
		Dot1XCtrl:                    dot1xCtrl,
		Dot1XIDleTimeout:             dot1xIdleTimeout,
		EgressRateLimitKbps:          egressRateLimitKbps,
		EgressRateLimitKbpsEnabled:   egressRateLimitKbpsEnabled,
		FecMode:                      fecMode,
		FlowControlEnabled:           flowControlEnabled,
		LldpmedEnabled:               lldpmedEnabled,
		LldpmedNotifyEnabled:         lldpmedNotifyEnabled,
		MirrorPortIDX:                mirrorPortIdx,
		MulticastRouterNetworkIDs:    multicastRouterNetworkconfIds,
		PortKeepaliveEnabled:         portKeepaliveEnabled,
		PriorityQueue1Level:          priorityQueue1Level,
		PriorityQueue2Level:          priorityQueue2Level,
		PriorityQueue3Level:          priorityQueue3Level,
		PriorityQueue4Level:          priorityQueue4Level,
		StormctrlBroadcastastEnabled: stormctrlBcastEnabled,
		StormctrlBroadcastastLevel:   stormctrlBcastLevel,
		StormctrlBroadcastastRate:    stormctrlBcastRate,
		StormctrlMcastEnabled:        stormctrlMcastEnabled,
		StormctrlMcastLevel:          stormctrlMcastLevel,
		StormctrlMcastRate:           stormctrlMcastRate,
		StormctrlType:                stormctrlType,
		StormctrlUcastEnabled:        stormctrlUcastEnabled,
		StormctrlUcastLevel:          stormctrlUcastLevel,
		StormctrlUcastRate:           stormctrlUcastRate,
		PortSecurityEnabled:          portSecurityEnabled,
		PortSecurityMACAddress:       portSecurityMACs,
		StpPortMode:                  stpPortMode,
		Autoneg:                      autoneg,
		Speed:                        speed,
		FullDuplex:                   fullDuplex,
		Isolation:                    isolation,
	}

	// go-unifi v1.9 tracks the current controller API, which expresses a LAG
	// as an explicit member list (`aggregate_members`) instead of the legacy
	// starting-port count (`aggregate_num_ports`). Translate the schema's
	// count into the equivalent contiguous member range — N sequential ports
	// starting at this port — which matches the documented schema semantics
	// ("All ports in the LAG must be sequential"), so existing practitioner
	// configs keep working unchanged. When unset (0), leave the slice nil so
	// the field is omitted from the payload entirely.
	if aggregateNumPorts > 0 {
		members := make([]int, aggregateNumPorts)
		for i := range members {
			members[i] = idx + i
		}
		po.AggregateMembers = members
	}

	return po, nil
}

// fromPortOverride flattens a controller port override for state.
//
// `declared` lists the attributes the practitioner wrote for this port. Only
// those are surfaced, so a value the controller auto-populates on a port it was
// never declared on (setting_preference, a native VLAN) never reaches state and
// therefore cannot appear as a diff. A nil `declared` surfaces everything, which
// is what import needs.
//
// This is what lets `port_override` use the SDK's default full-element set
// hash: echoes are filtered here rather than being hidden by a hash that
// ignores them, so an edited attribute changes the element and produces a diff.
func fromPortOverride(po unifi.DevicePortOverrides, declared map[string]bool) map[string]interface{} {
	all := map[string]interface{}{
		"number":          po.PortIDX,
		"name":            po.Name,
		"port_profile_id": po.PortProfileID,
		"op_mode":         po.OpMode,
		"poe_mode":        po.PoeMode,
		// Inverse of the translation in toPortOverride: the member-list
		// length is the LAG port count (0 / unset round-trips as an empty
		// list, preserving the previous zero-value behavior).
		"aggregate_num_ports":   len(po.AggregateMembers),
		"native_networkconf_id": po.NATiveNetworkID,
		"tagged_vlan_mgmt":      po.TaggedVLANMgmt,
		"forward":               po.Forward,
		"excluded_network_ids":  utils.StringSliceToSet(po.ExcludedNetworkIDs),
		"voice_networkconf_id":  po.VoiceNetworkID,
		"setting_preference":    po.SettingPreference,

		"dot1x_ctrl":                       po.Dot1XCtrl,
		"dot1x_idle_timeout":               po.Dot1XIDleTimeout,
		"egress_rate_limit_kbps":           po.EgressRateLimitKbps,
		"egress_rate_limit_kbps_enabled":   po.EgressRateLimitKbpsEnabled,
		"fec_mode":                         po.FecMode,
		"flow_control_enabled":             po.FlowControlEnabled,
		"lldpmed_enabled":                  po.LldpmedEnabled,
		"lldpmed_notify_enabled":           po.LldpmedNotifyEnabled,
		"mirror_port_idx":                  po.MirrorPortIDX,
		"multicast_router_networkconf_ids": utils.StringSliceToSet(po.MulticastRouterNetworkIDs),
		"port_keepalive_enabled":           po.PortKeepaliveEnabled,
		"priority_queue1_level":            po.PriorityQueue1Level,
		"priority_queue2_level":            po.PriorityQueue2Level,
		"priority_queue3_level":            po.PriorityQueue3Level,
		"priority_queue4_level":            po.PriorityQueue4Level,
		"stormctrl_bcast_enabled":          po.StormctrlBroadcastastEnabled,
		"stormctrl_bcast_level":            po.StormctrlBroadcastastLevel,
		"stormctrl_bcast_rate":             po.StormctrlBroadcastastRate,
		"stormctrl_mcast_enabled":          po.StormctrlMcastEnabled,
		"stormctrl_mcast_level":            po.StormctrlMcastLevel,
		"stormctrl_mcast_rate":             po.StormctrlMcastRate,
		"stormctrl_type":                   po.StormctrlType,
		"stormctrl_ucast_enabled":          po.StormctrlUcastEnabled,
		"stormctrl_ucast_level":            po.StormctrlUcastLevel,
		"stormctrl_ucast_rate":             po.StormctrlUcastRate,
		"port_security_enabled":            po.PortSecurityEnabled,
		"port_security_mac_address":        utils.StringSliceToSet(po.PortSecurityMACAddress),
		"stp_port_mode":                    po.StpPortMode,
		"autoneg":                          po.Autoneg,
		"speed":                            po.Speed,
		"full_duplex":                      po.FullDuplex,
		"isolation":                        po.Isolation,
	}

	if declared == nil {
		return all
	}

	// `number` is the identity and is always present.
	filtered := map[string]interface{}{"number": all["number"]}
	for k, v := range all {
		if declared[k] {
			filtered[k] = v
		}
	}
	return filtered
}

func waitForDeviceState(ctx context.Context, d *schema.ResourceData, meta interface{}, targetState unifi.DeviceState, pendingStates []unifi.DeviceState, timeout time.Duration) (*unifi.Device, error) {
	c, ok := meta.(*base.Client)
	if !ok {
		return nil, fmt.Errorf("unexpected meta type: %T", meta)
	}

	site, _ := d.Get("site").(string)
	mac, _ := d.Get("mac").(string)

	if site == "" {
		site = c.Site
	}

	// Always consider unknown to be a pending state.
	pendingStates = append(pendingStates, unifi.DeviceStateUnknown)

	pending := make([]string, 0, len(pendingStates))
	for _, state := range pendingStates {
		pending = append(pending, state.String())
	}

	wait := retry.StateChangeConf{
		Pending: pending,
		Target:  []string{targetState.String()},
		Refresh: func() (interface{}, string, error) {
			device, err := c.GetDeviceByMAC(ctx, site, mac)

			if errors.Is(err, unifi.ErrNotFound) {
				err = nil
			}

			// When a device is forgotten, it will disappear from the UI for a few seconds before reappearing.
			// During this time, `device.GetDeviceByMAC` will return a 400.
			//
			// TODO: Improve handling of this situation in `go-unifi`.
			if err != nil && strings.Contains(err.Error(), "api.err.UnknownDevice") {
				err = nil
			}

			var state string
			if device != nil {
				state = device.State.String()
			}

			// TODO: Why is this needed???
			if device == nil {
				return nil, state, err
			}

			return device, state, err
		},
		Timeout:        timeout,
		NotFoundChecks: 30,
	}

	outputRaw, err := wait.WaitForStateContext(ctx)

	if output, ok := outputRaw.(*unifi.Device); ok {
		return output, err
	}

	return nil, err
}
