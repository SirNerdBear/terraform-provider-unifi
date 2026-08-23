package network

import (
	"context"
	"errors"
	"fmt"

	"github.com/SirNerdBear/terraform-provider-unifi/internal/provider/utils"

	"github.com/filipowm/go-unifi/unifi"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"

	"github.com/SirNerdBear/terraform-provider-unifi/internal/provider/base"
)

var (
	wlanValidMinimumDataRate2g = []int{1000, 2000, 5500, 6000, 9000, 11000, 12000, 18000, 24000, 36000, 48000, 54000}
	wlanValidMinimumDataRate5g = []int{6000, 9000, 12000, 18000, 24000, 36000, 48000, 54000}
)

func ResourceWLAN() *schema.Resource {
	return &schema.Resource{
		Description: "The `unifi_wlan` resource manages wireless networks (SSIDs) on UniFi access points.\n\n" +
			"This resource allows you to create and manage WiFi networks with various security options including WPA2, WPA3, " +
			"and enterprise authentication. You can configure features such as guest policies, minimum data rates, band steering, " +
			"and scheduled availability.\n\n" +
			"Each WLAN can be customized with different security settings, VLAN assignments, and client options to meet specific " +
			"networking requirements.",

		CreateContext: resourceWLANCreate,
		ReadContext:   resourceWLANRead,
		UpdateContext: resourceWLANUpdate,
		DeleteContext: resourceWLANDelete,
		Importer: &schema.ResourceImporter{
			StateContext: base.ImportSiteAndID,
		},

		Schema: map[string]*schema.Schema{
			"id": {
				Description: "The unique identifier of the wireless network in the UniFi controller.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"site": {
				Description: "The name of the UniFi site where the wireless network should be created. If not specified, the default site will be used.",
				Type:        schema.TypeString,
				Computed:    true,
				Optional:    true,
				ForceNew:    true,
			},
			"name": {
				Description:  "The SSID (network name) that will be broadcast by the access points. Must be between 1 and 32 characters long.",
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringLenBetween(1, 32),
			},
			"user_group_id": {
				Description: "The ID of the user group that defines the rate limiting and firewall rules for clients on this network.",
				Type:        schema.TypeString,
				Required:    true,
			},
			"security": {
				Description: "The security protocol for the wireless network. Valid values are:\n" +
					"  * `wpapsk` - WPA Personal (PSK) with WPA2/WPA3 options\n" +
					"  * `wpaeap` - WPA Enterprise (802.1x)\n" +
					"  * `open` - Open network (no encryption)",
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validation.StringInSlice([]string{"wpapsk", "wpaeap", "open"}, false),
			},
			"wpa3_support": {
				Description: "Enable WPA3 security protocol. Requires security to be set to `wpapsk` and PMF mode to be enabled. WPA3 provides enhanced security features over WPA2.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"wpa3_transition": {
				Description: "Enable WPA3 transition mode, which allows both WPA2 and WPA3 clients to connect. This provides backward compatibility while gradually transitioning to WPA3." +
					" Requires security to be set to `wpapsk` and `wpa3_support` to be true.",
				Type:     schema.TypeBool,
				Optional: true,
			},
			"pmf_mode": {
				Description: "Protected Management Frames (PMF) mode. It cannot be disabled if using WPA3. Valid values are:\n" +
					"  * `required` - All clients must support PMF (required for WPA3)\n" +
					"  * `optional` - Clients can optionally use PMF (recommended when transitioning from WPA2 to WPA3)\n" +
					"  * `disabled` - PMF is disabled (not compatible with WPA3)",
				Type:         schema.TypeString,
				Optional:     true,
				ValidateFunc: validation.StringInSlice([]string{"required", "optional", "disabled"}, false),
				Default:      "disabled",
			},
			"passphrase": {
				Description: "The WPA pre-shared key (password) for the network. Required when security is not set to `open`.",
				Type:        schema.TypeString,
				// only required if security != open
				Optional:  true,
				Sensitive: true,
			},
			"hide_ssid": {
				Description: "When enabled, the access points will not broadcast the network name (SSID). Clients will need to manually enter the SSID to connect.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"is_guest": {
				Description: "Mark this as a guest network. Guest networks are isolated from other networks and can have special restrictions like captive portals.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"multicast_enhance": {
				Description: "Enable multicast enhancement to convert multicast traffic to unicast for better reliability and performance, especially for applications like video streaming.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"mac_filter_enabled": {
				Description: "Enable MAC address filtering to control network access based on client MAC addresses. Works in conjunction with `mac_filter_list` and `mac_filter_policy`.",
				Type:        schema.TypeBool,
				Optional:    true,
			},
			"mac_filter_list": {
				Description: "List of MAC addresses to filter in XX:XX:XX:XX:XX:XX format. Only applied when `mac_filter_enabled` is true. MAC addresses are case-insensitive.",
				Type:        schema.TypeSet,
				Optional:    true,
				Elem: &schema.Schema{
					Type:             schema.TypeString,
					ValidateFunc:     validation.StringMatch(utils.MacAddressRegexp, "Mac address is invalid"),
					DiffSuppressFunc: utils.MacDiffSuppressFunc,
				},
			},
			"mac_filter_policy": {
				Description: "MAC address filter policy. Valid values are:\n" +
					"  * `allow` - Only allow listed MAC addresses\n" +
					"  * `deny` - Block listed MAC addresses",
				Type:         schema.TypeString,
				Optional:     true,
				Default:      "deny",
				ValidateFunc: validation.StringInSlice([]string{"allow", "deny"}, false),
			},
			"radius_profile_id": {
				Description: "ID of the RADIUS profile to use for WPA Enterprise authentication (when security is 'wpaeap'). Reference existing profiles using the `unifi_radius_profile` data source.",
				Type:        schema.TypeString,
				Optional:    true,
			},
			"schedule": {
				Description: "Time-based access control configuration for the wireless network. Allows automatic enabling/disabling of the network on specified schedules.",
				Type:        schema.TypeList,
				Optional:    true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"day_of_week": {
							Description:  "Day of week. Valid values: `sun`, `mon`, `tue`, `wed`, `thu`, `fri`, `sat`.",
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: validation.StringInSlice([]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}, false),
						},
						"start_hour": {
							Description:  "Start hour in 24-hour format (0-23).",
							Type:         schema.TypeInt,
							Required:     true,
							ValidateFunc: validation.IntBetween(0, 23),
						},
						"start_minute": {
							Description:  "Start minute (0-59).",
							Type:         schema.TypeInt,
							Optional:     true,
							Default:      0,
							ValidateFunc: validation.IntBetween(0, 59),
						},
						"duration": {
							Description:  "Duration in minutes that the network should remain active.",
							Type:         schema.TypeInt,
							Required:     true,
							ValidateFunc: validation.IntAtLeast(1),
						},
						"name": {
							Description: "Friendly name for this schedule block (e.g., 'Business Hours', 'Weekend Access').",
							Type:        schema.TypeString,
							Optional:    true,
						},
					},
				},
			},
			"no2ghz_oui": {
				Description: "When enabled, devices from specific manufacturers (identified by their OUI - Organizationally Unique Identifier) " +
					"will be prevented from connecting on 2.4GHz and forced to use 5GHz. This improves overall network performance by " +
					"ensuring capable devices use the less congested 5GHz band. Common examples include newer smartphones and laptops.",
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},
			"l2_isolation": {
				Description: "Isolates wireless clients from each other at layer 2 (ethernet) level. When enabled, devices on this WLAN " +
					"cannot communicate directly with each other, improving security especially for guest networks or IoT devices. " +
					"Each client can only communicate with the gateway/router.",
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},
			"proxy_arp": {
				Description: "Enable ARP proxy on this WLAN. When enabled, the UniFi controller will respond to ARP requests on behalf " +
					"of clients, reducing broadcast traffic and potentially improving network performance. This is particularly useful " +
					"in high-density wireless environments.",
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},
			"bss_transition": {
				Description: "Enable BSS Transition Management to help clients roam between APs more efficiently.",
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
			},
			"uapsd": {
				Description: "Enable Unscheduled Automatic Power Save Delivery to improve battery life for mobile devices.",
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
			},
			"fast_roaming_enabled": {
				Description: "Enable 802.11r Fast BSS Transition for seamless roaming between APs. Requires client device support.",
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
			},
			"minimum_data_rate_2g_kbps": {
				Description: "Minimum data rate for 2.4GHz devices in Kbps. Use `0` to disable. Valid values: " +
					utils.MarkdownValueListInt(wlanValidMinimumDataRate2g),
				Type:         schema.TypeInt,
				Optional:     true,
				ValidateFunc: validation.IntInSlice(append([]int{0}, wlanValidMinimumDataRate2g...)),
			},
			"minimum_data_rate_5g_kbps": {
				Description: "Minimum data rate for 5GHz devices in Kbps. Use `0` to disable. Valid values: " +
					utils.MarkdownValueListInt(wlanValidMinimumDataRate5g),
				Type:         schema.TypeInt,
				Optional:     true,
				ValidateFunc: validation.IntInSlice(append([]int{0}, wlanValidMinimumDataRate5g...)),
			},
			"wlan_band": {
				Description: "Radio band selection (legacy single-band field). Valid values:\n" +
					"  * `both` - Both 2.4GHz and 5GHz\n" +
					"  * `2g` - 2.4GHz only\n" +
					"  * `5g` - 5GHz only\n\n" +
					"Cannot express a 6GHz selection — use `wlan_bands` for that. When neither this nor `wlan_bands` is set, " +
					"the controller's default (all supported bands) applies.",
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ValidateFunc:  validation.StringInSlice([]string{"2g", "5g", "both"}, false),
				ConflictsWith: []string{"wlan_bands"},
			},
			"wlan_bands": {
				Description: "Radio bands to broadcast this SSID on (modern multi-band field, supersedes `wlan_band` and supports 6GHz). " +
					"Valid values for each element: `2g`, `5g`, `6g`. Note that 6GHz requires WPA3 (or WPA3 transition mode) and a " +
					"6GHz-capable access point. When set, the legacy `wlan_band` field is derived from it and `setting_preference` " +
					"is forced to `manual`, matching UniFi UI behavior.",
				Type:          schema.TypeSet,
				Optional:      true,
				Computed:      true,
				MinItems:      1,
				ConflictsWith: []string{"wlan_band"},
				Elem: &schema.Schema{
					Type:         schema.TypeString,
					ValidateFunc: validation.StringInSlice([]string{"2g", "5g", "6g"}, false),
				},
			},
			"network_id": {
				Description: "ID of the network (VLAN) for this SSID. Used to assign the WLAN to a specific network segment.",
				Type:        schema.TypeString,
				Optional:    true,
			},
			"ap_group_ids": {
				Description: "IDs of the AP groups that should broadcast this SSID. Used to control which access points broadcast this network.",
				Type:        schema.TypeSet,
				Optional:    true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},

			// ---- fields go-unifi serialises on EVERY write ----
			// No omitempty, so the Go zero value goes on the wire whether or not it
			// is configured. Before these existed the provider overwrote all of them
			// with false on every apply. Optional+Computed so an unset attribute
			// round-trips the controller's value instead of clobbering it.
			"auth_cache": {
				Description: "Cache 802.1x authentication results to speed up roaming.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"b_supported": {
				Description: "Allow legacy 802.11b rates.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"bc_filter_enabled": {
				Description: "Filter broadcast traffic, keeping it off the air unless listed in `bc_filter_list`.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"country_beacon": {
				Description: "Advertise the regulatory country in beacons (802.11d).",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"dpi_enabled": {
				Description: "Apply deep packet inspection to clients on this SSID.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"element_adopt": {
				Description: "UniFi Elements adoption.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"enabled": {
				Description: "Whether the SSID is broadcast at all. A disabled WLAN keeps its configuration.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"enhanced_iot": {
				Description: "Enhanced IoT connectivity, which relaxes rates and steering for constrained devices.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"hotspot2conf_enabled": {
				Description: "Enable the Hotspot 2.0 / Passpoint profile.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"iapp_enabled": {
				Description: "Inter-Access-Point Protocol, which helps clients hand off between APs.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"minrate_na_advertising_rates": {
				Description: "Advertise the 5GHz minimum rate in beacons rather than only enforcing it.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"minrate_ng_advertising_rates": {
				Description: "Advertise the 2.4GHz minimum rate in beacons rather than only enforcing it.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"mlo_enabled": {
				Description: "Multi-Link Operation (Wi-Fi 7).",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"name_combine_enabled": {
				Description: "Broadcast one SSID name across bands. When false the controller appends `name_combine_suffix` to the 5GHz name.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"optimize_iot_wifi_connectivity": {
				Description: "Optimisations for IoT devices that struggle to associate.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"p2p": {
				Description: "Wi-Fi Direct / peer-to-peer.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"p2p_cross_connect": {
				Description: "Allow peer-to-peer clients to reach the wired network.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"private_preshared_keys_enabled": {
				Description: "Per-client PSKs (see `private_preshared_keys`).",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"radius_das_enabled": {
				Description: "RADIUS Dynamic Authorization Extensions (CoA/Disconnect).",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"radius_mac_auth_enabled": {
				Description: "Authenticate clients by MAC against RADIUS.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"radius_macacl_empty_password": {
				Description: "Send an empty password for RADIUS MAC authentication.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"rrm_enabled": {
				Description: "802.11k Radio Resource Management, which helps clients pick a better AP.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"sae_psk_vlan_required": {
				Description: "Require a VLAN on every SAE PSK entry.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"schedule_reversed": {
				Description: "Invert the schedule, so the listed blocks are when the SSID is OFF.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"tdls_prohibit": {
				Description: "Prohibit direct client-to-client tunnelled links.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"vlan_enabled": {
				Description: "Whether the legacy `vlan` field applies. Modern configs use `network_id` instead.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"wpa3_enhanced_192": {
				Description: "WPA3 Enterprise 192-bit mode.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"wpa3_fast_roaming": {
				Description: "802.11r fast roaming for WPA3.",
				Type:        schema.TypeBool,
				Optional:    true,
				Computed:    true,
			},
			"wlangroup_id": {
				Description: "ID of the WLAN group this SSID belongs to.",
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
			},
			"wpa_enc": {
				Description:  "WPA encryption cipher.",
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.StringInSlice([]string{"auto", "ccmp", "gcmp", "ccmp-256", "gcmp-256"}, false),
			},
			"wpa_mode": {
				Description:  "WPA protocol version.",
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.StringInSlice([]string{"auto", "wpa1", "wpa2"}, false),
			},
			"dtim_mode": {
				Description:  "DTIM interval mode. `custom` enables the per-band `dtim_*` values.",
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ValidateFunc: validation.StringInSlice([]string{"default", "custom"}, false),
			},
			"group_rekey": {
				Description: "Group key rotation interval in seconds. 0 disables rotation.",
				Type:        schema.TypeInt,
				Optional:    true,
				Computed:    true,
			},
		},
	}
}

func resourceWLANGetResourceData(d *schema.ResourceData, meta interface{}) (*unifi.WLAN, error) {
	c, ok := meta.(*base.Client)
	if !ok {
		return nil, fmt.Errorf("unexpected meta type: %T", meta)
	}

	security, _ := d.Get("security").(string)
	passphrase, _ := d.Get("passphrase").(string)
	switch security {
	case "open":
		passphrase = ""
	}

	pmf, _ := d.Get("pmf_mode").(string)
	wpa3, _ := d.Get("wpa3_support").(bool)
	wpa3Transition, _ := d.Get("wpa3_transition").(bool)
	switch security {
	case "wpapsk":
		// nothing
	default:
		if wpa3 || wpa3Transition {
			return nil, errors.New("wpa3_support and wpa3_transition are only valid for security type wpapsk")
		}
	}
	if !c.SupportsWPA3() {
		if wpa3 || wpa3Transition {
			return nil, fmt.Errorf("WPA 3 support is not available on controller version %q, you must be on %q or higher", c.Version, base.ControllerVersionWPA3)
		}
	}

	if wpa3Transition && pmf == "disabled" {
		return nil, errors.New("WPA 3 transition mode requires pmf_mode to be turned on")
	} else if wpa3 && !wpa3Transition && pmf != "required" {
		return nil, errors.New("for WPA 3 you must set pmf_mode to required")
	}

	macFilterEnabled, _ := d.Get("mac_filter_enabled").(bool)
	macFilterListSet, _ := d.Get("mac_filter_list").(*schema.Set)
	macFilterList, err := utils.SetToStringSlice(macFilterListSet)
	if err != nil {
		return nil, err
	}
	if !macFilterEnabled {
		macFilterList = nil
	}

	// version specific fields and validation
	networkID, _ := d.Get("network_id").(string)
	apGroupIDsSet, _ := d.Get("ap_group_ids").(*schema.Set)
	apGroupIDs, err := utils.SetToStringSlice(apGroupIDsSet)
	if err != nil {
		return nil, err
	}
	wlanBand, _ := d.Get("wlan_band").(string)

	// Only send wlan_bands when it is explicitly configured. The attribute is
	// Computed, so d.Get also returns controller-derived state for configs
	// that only set the legacy wlan_band — echoing that stale array back
	// would override a wlan_band change.
	wlanBandsSet, _ := d.Get("wlan_bands").(*schema.Set)
	wlanBands, err := utils.SetToStringSlice(wlanBandsSet)
	if err != nil {
		return nil, err
	}
	bandsConfigured := false
	if raw := d.GetRawConfig(); !raw.IsNull() {
		bandsConfigured = !raw.GetAttr("wlan_bands").IsNull()
	}
	settingPreference := ""
	if bandsConfigured {
		// wlan_bands is authoritative — but ONLY if the legacy wlan_band
		// string is absent from the payload. The controller derives
		// wlan_bands FROM a present wlan_band and ignores the array we send:
		// empirically wlan_band="5g" + wlan_bands=["5g","6g"] persisted
		// ["5g"], and wlan_band="both" + the same array persisted ["2g","5g"]
		// (i.e. "both" expands to 2.4+5, NOT a permissive "defer to the
		// array"). The legacy enum (2g/5g/both) can't even express
		// ["5g","6g"]. So we must OMIT wlan_band entirely: it has omitempty,
		// so leaving it "" drops it from the JSON and the controller honors
		// wlan_bands as given. Pin setting_preference to manual the way the
		// UI does when bands are hand-picked.
		wlanBand = ""
		settingPreference = "manual"
	} else {
		wlanBands = nil
	}

	scheduleList, _ := d.Get("schedule").([]interface{})
	schedule, err := listToSchedules(scheduleList)
	if err != nil {
		return nil, fmt.Errorf("unable to process schedule block: %w", err)
	}

	minRate2g, _ := d.Get("minimum_data_rate_2g_kbps").(int)
	minRate5g, _ := d.Get("minimum_data_rate_5g_kbps").(int)

	minrateSettingPreference := "auto"
	if minRate2g != 0 || minRate5g != 0 {
		if minRate2g == 0 || minRate5g == 0 {
			// this is really only true I think in >= 7.2, but easier to just apply this in general
			return nil, errors.New("you must set minimum data rates on both 2g and 5g if setting either")
		}
		minrateSettingPreference = "manual"
	}

	name, _ := d.Get("name").(string)
	hideSSID, _ := d.Get("hide_ssid").(bool)
	isGuest, _ := d.Get("is_guest").(bool)
	userGroupID, _ := d.Get("user_group_id").(string)
	multicastEnhance, _ := d.Get("multicast_enhance").(bool)
	macFilterPolicy, _ := d.Get("mac_filter_policy").(string)
	radiusProfileID, _ := d.Get("radius_profile_id").(string)
	no2ghzOui, _ := d.Get("no2ghz_oui").(bool)
	l2Isolation, _ := d.Get("l2_isolation").(bool)
	proxyArp, _ := d.Get("proxy_arp").(bool)
	bssTransition, _ := d.Get("bss_transition").(bool)
	uapsd, _ := d.Get("uapsd").(bool)
	fastRoaming, _ := d.Get("fast_roaming_enabled").(bool)

	return &unifi.WLAN{
		Name:                    name,
		XPassphrase:             passphrase,
		HideSSID:                hideSSID,
		IsGuest:                 isGuest,
		NetworkID:               networkID,
		ApGroupIDs:              apGroupIDs,
		UserGroupID:             userGroupID,
		Security:                security,
		WPA3Support:             wpa3,
		WPA3Transition:          wpa3Transition,
		MulticastEnhanceEnabled: multicastEnhance,
		MACFilterEnabled:        macFilterEnabled,
		MACFilterList:           macFilterList,
		MACFilterPolicy:         macFilterPolicy,
		RADIUSProfileID:         radiusProfileID,
		ScheduleWithDuration:    schedule,
		ScheduleEnabled:         len(schedule) > 0,
		WLANBand:                wlanBand,
		WLANBands:               wlanBands,
		SettingPreference:       settingPreference,
		PMFMode:                 pmf,

		// Previously hardcoded here, which rewrote whatever the controller held:
		// group_rekey 0 -> 3600 and iapp_enabled true -> false on every live SSID.
		WPAEnc:      wlanStrDef(d, "wpa_enc", "ccmp"),
		WPAMode:     wlanStrDef(d, "wpa_mode", "wpa2"),
		DTIMMode:    wlanStrDef(d, "dtim_mode", "default"),
		WLANGroupID: wlanStr(d, "wlangroup_id"),
		GroupRekey:  wlanIntDef(d, "group_rekey", 3600),

		AuthCache:                   wlanBool(d, "auth_cache"),
		BSupported:                  wlanBool(d, "b_supported"),
		BroadcastFilterEnabled:      wlanBool(d, "bc_filter_enabled"),
		CountryBeacon:               wlanBool(d, "country_beacon"),
		DPIEnabled:                  wlanBool(d, "dpi_enabled"),
		ElementAdopt:                wlanBool(d, "element_adopt"),
		Enabled:                     wlanBoolDef(d, "enabled", true),
		EnhancedIot:                 wlanBool(d, "enhanced_iot"),
		Hotspot2ConfEnabled:         wlanBool(d, "hotspot2conf_enabled"),
		IappEnabled:                 wlanBool(d, "iapp_enabled"),
		MinrateNaAdvertisingRates:   wlanBool(d, "minrate_na_advertising_rates"),
		MinrateNgAdvertisingRates:   wlanBool(d, "minrate_ng_advertising_rates"),
		MloEnabled:                  wlanBool(d, "mlo_enabled"),
		NameCombineEnabled:          wlanBoolDef(d, "name_combine_enabled", true),
		OptimizeIotWifiConnectivity: wlanBool(d, "optimize_iot_wifi_connectivity"),
		P2P:                         wlanBool(d, "p2p"),
		P2PCrossConnect:             wlanBool(d, "p2p_cross_connect"),
		PrivatePresharedKeysEnabled: wlanBool(d, "private_preshared_keys_enabled"),
		RADIUSDasEnabled:            wlanBool(d, "radius_das_enabled"),
		RADIUSMACAuthEnabled:        wlanBool(d, "radius_mac_auth_enabled"),
		RADIUSMACaclEmptyPassword:   wlanBool(d, "radius_macacl_empty_password"),
		RrmEnabled:                  wlanBool(d, "rrm_enabled"),
		SaePskVLANRequired:          wlanBool(d, "sae_psk_vlan_required"),
		ScheduleReversed:            wlanBool(d, "schedule_reversed"),
		TdlsProhibit:                wlanBool(d, "tdls_prohibit"),
		VLANEnabled:                 wlanBool(d, "vlan_enabled"),
		WPA3Enhanced192:             wlanBool(d, "wpa3_enhanced_192"),
		WPA3FastRoaming:             wlanBool(d, "wpa3_fast_roaming"),

		No2GhzOui:          no2ghzOui,
		L2Isolation:        l2Isolation,
		ProxyArp:           proxyArp,
		BssTransition:      bssTransition,
		UapsdEnabled:       uapsd,
		FastRoamingEnabled: fastRoaming,

		MinrateSettingPreference: minrateSettingPreference,

		MinrateNgEnabled:      minRate2g != 0,
		MinrateNgDataRateKbps: minRate2g,

		MinrateNaEnabled:      minRate5g != 0,
		MinrateNaDataRateKbps: minRate5g,
	}, nil
}

func resourceWLANCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, ok := meta.(*base.Client)
	if !ok {
		return diag.Errorf("unexpected meta type: %T", meta)
	}

	req, err := resourceWLANGetResourceData(d, meta)
	if err != nil {
		return diag.FromErr(err)
	}

	site, _ := d.Get("site").(string)
	if site == "" {
		site = c.Site
	}

	resp, err := c.CreateWLAN(ctx, site, req)
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(resp.ID)

	return resourceWLANSetResourceData(resp, d, site)
}

func resourceWLANSetResourceData(resp *unifi.WLAN, d *schema.ResourceData, site string) diag.Diagnostics {
	security := resp.Security
	passphrase := resp.XPassphrase
	wpa3 := false
	wpa3Transition := false
	switch security {
	case "open":
		passphrase = ""
	case "wpapsk":
		wpa3 = resp.WPA3Support
		wpa3Transition = resp.WPA3Transition
	}

	macFilterEnabled := resp.MACFilterEnabled
	var macFilterList *schema.Set
	macFilterPolicy := "deny"
	if macFilterEnabled {
		macFilterList = utils.StringSliceToSet(resp.MACFilterList)
		macFilterPolicy = resp.MACFilterPolicy
	}

	apGroupIDs := utils.StringSliceToSet(resp.ApGroupIDs)

	schedule := listFromSchedules(resp.ScheduleWithDuration)

	minRate2g := 0
	if resp.MinrateSettingPreference != "auto" && resp.MinrateNgEnabled {
		minRate2g = resp.MinrateNgDataRateKbps
	}
	minRate5g := 0
	if resp.MinrateSettingPreference != "auto" && resp.MinrateNaEnabled {
		minRate5g = resp.MinrateNaDataRateKbps
	}

	for key, value := range map[string]interface{}{
		"site":                           site,
		"name":                           resp.Name,
		"user_group_id":                  resp.UserGroupID,
		"passphrase":                     passphrase,
		"hide_ssid":                      resp.HideSSID,
		"is_guest":                       resp.IsGuest,
		"security":                       security,
		"wpa3_support":                   wpa3,
		"wpa3_transition":                wpa3Transition,
		"multicast_enhance":              resp.MulticastEnhanceEnabled,
		"mac_filter_enabled":             macFilterEnabled,
		"mac_filter_list":                macFilterList,
		"mac_filter_policy":              macFilterPolicy,
		"radius_profile_id":              resp.RADIUSProfileID,
		"schedule":                       schedule,
		"wlan_band":                      resp.WLANBand,
		"wlan_bands":                     utils.StringSliceToSet(resp.WLANBands),
		"no2ghz_oui":                     resp.No2GhzOui,
		"l2_isolation":                   resp.L2Isolation,
		"proxy_arp":                      resp.ProxyArp,
		"bss_transition":                 resp.BssTransition,
		"uapsd":                          resp.UapsdEnabled,
		"fast_roaming_enabled":           resp.FastRoamingEnabled,
		"ap_group_ids":                   apGroupIDs,
		"network_id":                     resp.NetworkID,
		"pmf_mode":                       resp.PMFMode,
		"minimum_data_rate_2g_kbps":      minRate2g,
		"minimum_data_rate_5g_kbps":      minRate5g,
		"wpa_enc":                        resp.WPAEnc,
		"wpa_mode":                       resp.WPAMode,
		"dtim_mode":                      resp.DTIMMode,
		"wlangroup_id":                   resp.WLANGroupID,
		"group_rekey":                    resp.GroupRekey,
		"auth_cache":                     resp.AuthCache,
		"b_supported":                    resp.BSupported,
		"bc_filter_enabled":              resp.BroadcastFilterEnabled,
		"country_beacon":                 resp.CountryBeacon,
		"dpi_enabled":                    resp.DPIEnabled,
		"element_adopt":                  resp.ElementAdopt,
		"enabled":                        resp.Enabled,
		"enhanced_iot":                   resp.EnhancedIot,
		"hotspot2conf_enabled":           resp.Hotspot2ConfEnabled,
		"iapp_enabled":                   resp.IappEnabled,
		"minrate_na_advertising_rates":   resp.MinrateNaAdvertisingRates,
		"minrate_ng_advertising_rates":   resp.MinrateNgAdvertisingRates,
		"mlo_enabled":                    resp.MloEnabled,
		"name_combine_enabled":           resp.NameCombineEnabled,
		"optimize_iot_wifi_connectivity": resp.OptimizeIotWifiConnectivity,
		"p2p":                            resp.P2P,
		"p2p_cross_connect":              resp.P2PCrossConnect,
		"private_preshared_keys_enabled": resp.PrivatePresharedKeysEnabled,
		"radius_das_enabled":             resp.RADIUSDasEnabled,
		"radius_mac_auth_enabled":        resp.RADIUSMACAuthEnabled,
		"radius_macacl_empty_password":   resp.RADIUSMACaclEmptyPassword,
		"rrm_enabled":                    resp.RrmEnabled,
		"sae_psk_vlan_required":          resp.SaePskVLANRequired,
		"schedule_reversed":              resp.ScheduleReversed,
		"tdls_prohibit":                  resp.TdlsProhibit,
		"vlan_enabled":                   resp.VLANEnabled,
		"wpa3_enhanced_192":              resp.WPA3Enhanced192,
		"wpa3_fast_roaming":              resp.WPA3FastRoaming,
	} {
		if err := d.Set(key, value); err != nil {
			return diag.FromErr(err)
		}
	}

	return nil
}

func resourceWLANRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, ok := meta.(*base.Client)
	if !ok {
		return diag.Errorf("unexpected meta type: %T", meta)
	}

	id := d.Id()
	site, _ := d.Get("site").(string)
	if site == "" {
		site = c.Site
	}

	resp, err := c.GetWLAN(ctx, site, id)
	if errors.Is(err, unifi.ErrNotFound) {
		d.SetId("")
		return nil
	}
	if err != nil {
		return diag.FromErr(err)
	}

	return resourceWLANSetResourceData(resp, d, site)
}

func resourceWLANUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, ok := meta.(*base.Client)
	if !ok {
		return diag.Errorf("unexpected meta type: %T", meta)
	}

	req, err := resourceWLANGetResourceData(d, meta)
	if err != nil {
		return diag.FromErr(err)
	}

	req.ID = d.Id()
	site, _ := d.Get("site").(string)
	if site == "" {
		site = c.Site
	}
	req.SiteID = site

	// go-unifi v1.9.2's updateWLAN converts a successful-but-empty PUT response into
	// unifi.ErrNotFound (see utils.ReReadOnUpdateNotFound / issue #98); re-read to
	// tell a spurious error from a genuine out-of-band deletion.
	resp, err := c.UpdateWLAN(ctx, site, req)
	resp, found, err := utils.ReReadOnUpdateNotFound(resp, err, func() (*unifi.WLAN, error) {
		return c.GetWLAN(ctx, site, req.ID)
	})
	if err != nil {
		return diag.FromErr(err)
	}
	if !found {
		d.SetId("")
		return nil
	}

	return resourceWLANSetResourceData(resp, d, site)
}

func resourceWLANDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c, ok := meta.(*base.Client)
	if !ok {
		return diag.Errorf("unexpected meta type: %T", meta)
	}

	id := d.Id()
	site, _ := d.Get("site").(string)
	if site == "" {
		site = c.Site
	}

	err := c.DeleteWLAN(ctx, site, id)
	if errors.Is(err, unifi.ErrNotFound) {
		return nil
	}
	return diag.FromErr(err)
}

func listToSchedules(list []interface{}) ([]unifi.WLANScheduleWithDuration, error) {
	schedules := make([]unifi.WLANScheduleWithDuration, 0, len(list))
	for _, item := range list {
		data, ok := item.(map[string]interface{})
		if !ok {
			return nil, errors.New("unexpected data in block")
		}
		ss := toSchedule(data)
		schedules = append(schedules, ss)
	}
	return schedules, nil
}

func toSchedule(data map[string]interface{}) unifi.WLANScheduleWithDuration {
	// TODO: error check these?
	dow, _ := data["day_of_week"].(string)
	startHour, _ := data["start_hour"].(int)
	startMinute, _ := data["start_minute"].(int)
	duration, _ := data["duration"].(int)
	name, _ := data["name"].(string)

	return unifi.WLANScheduleWithDuration{
		StartDaysOfWeek: []string{dow},
		StartHour:       startHour,
		StartMinute:     startMinute,
		DurationMinutes: duration,
		Name:            name,
	}
}

func fromSchedule(dow string, s unifi.WLANScheduleWithDuration) map[string]interface{} {
	return map[string]interface{}{
		"day_of_week":  dow,
		"start_hour":   s.StartHour,
		"start_minute": s.StartMinute,
		"duration":     s.DurationMinutes,
		"name":         s.Name,
	}
}

func listFromSchedules(ss []unifi.WLANScheduleWithDuration) []interface{} {
	// this explodes days of week lists in to individual schedules
	list := make([]interface{}, 0, len(ss))
	for _, s := range ss {
		for _, dow := range s.StartDaysOfWeek {
			v := fromSchedule(dow, s)
			list = append(list, v)
		}
	}
	return list
}

// Optional+Computed accessors. d.Get returns the value read back from the
// controller when config leaves the attribute unset, so these round-trip
// instead of writing a Go zero over a live setting.
func wlanBool(d *schema.ResourceData, key string) bool {
	v, _ := d.Get(key).(bool)
	return v
}

func wlanStr(d *schema.ResourceData, key string) string {
	v, _ := d.Get(key).(string)
	return v
}

func wlanInt(d *schema.ResourceData, key string) int {
	v, _ := d.Get(key).(int)
	return v
}

// isNewAndUnset is true only when creating a resource whose config leaves this
// attribute out. On update d.Get already holds the value read back from the
// controller and that must win -- otherwise a default overwrites a live
// setting, which is the bug these attributes exist to fix.
func isNewAndUnset(d *schema.ResourceData, key string) bool {
	if d.Id() != "" {
		return false
	}
	raw := d.GetRawConfig()
	return raw.IsNull() || !utils.IsRawConfigSet(raw, key)
}

func wlanBoolDef(d *schema.ResourceData, key string, def bool) bool {
	if isNewAndUnset(d, key) {
		return def
	}
	return wlanBool(d, key)
}

func wlanStrDef(d *schema.ResourceData, key, def string) string {
	if isNewAndUnset(d, key) {
		return def
	}
	return wlanStr(d, key)
}

func wlanIntDef(d *schema.ResourceData, key string, def int) int {
	if isNewAndUnset(d, key) {
		return def
	}
	return wlanInt(d, key)
}
