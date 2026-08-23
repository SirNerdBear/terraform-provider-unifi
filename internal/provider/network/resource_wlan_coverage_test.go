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
	"schedule_enabled":           true, // len(schedule) > 0
	"minrate_ng_enabled":         true, // minimum_data_rate_2g_kbps != 0
	"minrate_na_enabled":         true, // minimum_data_rate_5g_kbps != 0
	"minrate_setting_preference": true, // auto unless a rate is set
	"setting_preference":         true, // manual when wlan_bands is configured
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
