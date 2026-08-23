package network

import (
	"reflect"
	"strings"
	"testing"

	"github.com/filipowm/go-unifi/unifi"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientWLANFields reflects over unifi.WLAN rather than hardcoding a list, so a
// go-unifi bump cannot silently grow a field the schema does not know about.
// Returns json name -> true if the tag carries omitempty.
func clientWLANFields() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(unifi.WLAN{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name == "" {
			continue
		}
		omitempty := false
		for _, p := range parts[1:] {
			if p == "omitempty" {
				omitempty = true
			}
		}
		out[name] = omitempty
	}
	return out
}

// Schema attribute -> go-unifi json name, where they differ.
var wlanAlias = map[string]string{
	"passphrase":                "x_passphrase",
	"multicast_enhance":         "mcastenhance_enabled",
	"network_id":                "networkconf_id",
	"user_group_id":             "usergroup_id",
	"radius_profile_id":         "radiusprofile_id",
	"uapsd":                     "uapsd_enabled",
	"schedule":                  "schedule_with_duration",
	"minimum_data_rate_2g_kbps": "minrate_ng_data_rate_kbps",
	"minimum_data_rate_5g_kbps": "minrate_na_data_rate_kbps",
}

// Written by the provider from another attribute rather than declared directly.
var wlanDerived = map[string]bool{
	"schedule_enabled":   true, // len(schedule) > 0
	"setting_preference": true, // manual when wlan_bands is configured
}

// Identity and controller bookkeeping, never configuration.
var wlanMeta = map[string]bool{
	"_id": true, "site_id": true, "attr_hidden": true, "attr_hidden_id": true,
	"attr_no_delete": true, "attr_no_edit": true,
}

func wlanCovered() map[string]bool {
	covered := map[string]bool{}
	for k := range ResourceWLAN().Schema {
		if a, ok := wlanAlias[k]; ok {
			covered[a] = true
			continue
		}
		covered[k] = true
	}
	for k := range wlanDerived {
		covered[k] = true
	}
	for k := range wlanMeta {
		covered[k] = true
	}
	return covered
}

// A field with no omitempty is ALWAYS serialised. If the schema cannot express
// it, every apply writes the Go zero value over whatever the controller holds.
// That is silent data loss, so it is a build failure.
func TestWLANSchema_CoversEveryAlwaysSentField(t *testing.T) {
	require.NotNil(t, ResourceWLAN().Schema)
	covered := wlanCovered()

	var missing []string
	for name, omitempty := range clientWLANFields() {
		if omitempty || covered[name] {
			continue
		}
		missing = append(missing, name)
	}
	assert.Empty(t, missing,
		"go-unifi serialises these on every write and unifi_wlan cannot express them, "+
			"so an apply overwrites them with the Go zero value: %v", missing)
}

// omitempty fields leave the payload when unset, so they survive a merging PUT.
// Reported, not failed: this is a coverage gap, not data loss.
func TestWLANSchema_ReportsUnexpressibleOptionalFields(t *testing.T) {
	covered := wlanCovered()
	var gaps []string
	for name, omitempty := range clientWLANFields() {
		if !omitempty || covered[name] {
			continue
		}
		gaps = append(gaps, name)
	}
	t.Logf("%d omitempty field(s) not in the schema; dropped from the payload "+
		"and preserved only if a wlanconf PUT merges: %v", len(gaps), gaps)
}

var _ = schema.Resource{}

// Regression: every SSID at nerdhq holds minrate_setting_preference "auto"
// together with minrate_ng_enabled true and a real rate. The old read path
// zeroed the rate whenever preference was "auto", and the write path then
// derived minrate_ng_enabled false from that zero and sent it -- the field
// carries no omitempty, so it went on the wire every time. terraform plan could
// not show it, because the value was derived rather than declared.
func TestWLANMinrate_RoundTripsWhenPreferenceIsAuto(t *testing.T) {
	live := &unifi.WLAN{
		Name: "IotaWatt", Security: "wpapsk",
		MinrateSettingPreference: "auto",
		MinrateNgEnabled:         true,
		MinrateNgDataRateKbps:    1000,
		MinrateNaEnabled:         false,
		MinrateNaDataRateKbps:    6000,
	}
	d := ResourceWLAN().TestResourceData()
	require.Nil(t, resourceWLANSetResourceData(live, d, "default"))

	assert.Equal(t, 1000, d.Get("minimum_data_rate_2g_kbps"), "2.4GHz rate must survive the read")
	assert.Equal(t, 6000, d.Get("minimum_data_rate_5g_kbps"), "5GHz rate must survive the read")
	assert.Equal(t, true, d.Get("minrate_ng_enabled"), "enabled flag is independent of preference")
	assert.Equal(t, false, d.Get("minrate_na_enabled"))
	assert.Equal(t, "auto", d.Get("minrate_setting_preference"))
}

// Adopting an SSID must not copy its PSK into the state file.
func TestWLANPassphrase_NotStoredWhenUnconfigured(t *testing.T) {
	d := ResourceWLAN().TestResourceData()
	require.Nil(t, resourceWLANSetResourceData(
		&unifi.WLAN{Name: "IotaWatt", Security: "wpapsk", XPassphrase: "hunter2hunter2"},
		d, "default"))
	assert.Empty(t, d.Get("passphrase"),
		"an unconfigured passphrase must not be persisted to state")
}
