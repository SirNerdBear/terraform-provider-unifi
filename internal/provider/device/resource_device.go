package device

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	radios, _ := d.Get("radio").(*schema.Set)
	etherLighting, _ := d.Get("ether_lighting").([]interface{})
	if radios.Len() > 0 || len(etherLighting) > 0 {
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
		"port_override":       portOverrides,
		"radio":               radiosFromDevice(resp, d),
		"ether_lighting":      etherLightingFromDevice(resp, d),
	}
	for k, v := range values {
		if err := d.Set(k, v); err != nil {
			return diag.FromErr(err)
		}
	}

	return nil
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

	return &unifi.Device{
		MAC:               mac,
		Name:              name,
		SwitchVLANEnabled: switchVLANEnabled,
		PortOverrides:     pos,
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
		PortSecurityEnabled:    portSecurityEnabled,
		PortSecurityMACAddress: portSecurityMACs,
		StpPortMode:            stpPortMode,
		Autoneg:                autoneg,
		Speed:                  speed,
		FullDuplex:             fullDuplex,
		Isolation:              isolation,
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

		"port_security_enabled":     po.PortSecurityEnabled,
		"port_security_mac_address": utils.StringSliceToSet(po.PortSecurityMACAddress),
		"stp_port_mode":             po.StpPortMode,
		"autoneg":                   po.Autoneg,
		"speed":                     po.Speed,
		"full_duplex":               po.FullDuplex,
		"isolation":                 po.Isolation,
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
