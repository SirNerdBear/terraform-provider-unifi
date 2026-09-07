package settings

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"

	ut "github.com/SirNerdBear/terraform-provider-unifi/internal/provider/types"

	"github.com/hashicorp/terraform-plugin-framework-validators/int32validator"

	"github.com/filipowm/go-unifi/unifi"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int32planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/SirNerdBear/terraform-provider-unifi/internal/provider/base"
)

// SSHKeyModel represents an SSH key configuration.
type SSHKeyModel struct {
	Name    types.String `tfsdk:"name"`
	Type    types.String `tfsdk:"type"`
	Key     types.String `tfsdk:"key"`
	Comment types.String `tfsdk:"comment"`
}

func (m *SSHKeyModel) AttributeTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":    types.StringType,
		"type":    types.StringType,
		"key":     types.StringType,
		"comment": types.StringType,
	}
}

// mgmtModel represents the data model for management settings.
type mgmtModel struct {
	base.Model
	AdvancedFeatureEnabled types.Bool   `tfsdk:"advanced_feature_enabled"`
	AlertEnabled           types.Bool   `tfsdk:"alert_enabled"`
	AutoUpgrade            types.Bool   `tfsdk:"auto_upgrade"`
	AutoUpgradeHour        types.Int32  `tfsdk:"auto_upgrade_hour"`
	BootSound              types.Bool   `tfsdk:"boot_sound"`
	DebugToolsEnabled      types.Bool   `tfsdk:"debug_tools_enabled"`
	DirectConnectEnabled   types.Bool   `tfsdk:"direct_connect_enabled"`
	LedEnabled             types.Bool   `tfsdk:"led_enabled"`
	OutdoorModeEnabled     types.Bool   `tfsdk:"outdoor_mode_enabled"`
	UnifiIdpEnabled        types.Bool   `tfsdk:"unifi_idp_enabled"`
	WifimanEnabled         types.Bool   `tfsdk:"wifiman_enabled"`
	SSHAuthPasswordEnabled types.Bool   `tfsdk:"ssh_auth_password_enabled"`
	SSHBindWildcard        types.Bool   `tfsdk:"ssh_bind_wildcard"`
	SSHKeys                types.List   `tfsdk:"ssh_key"`
	SSHPassword            types.String `tfsdk:"ssh_password"`
	SSHPasswordWo          types.String `tfsdk:"ssh_password_wo"`
	SSHPasswordWoVersion   types.Int32  `tfsdk:"ssh_password_wo_version"`
	SSHEnabled             types.Bool   `tfsdk:"ssh_enabled"`
	SSHUsername            types.String `tfsdk:"ssh_username"`
}

// ApplyWriteOnly implements base.WriteOnlyAware. ssh_password mirrors the
// configuration here too, so a password the practitioner stopped managing
// drops out of state instead of being carried forever.
func (m *mgmtModel) ApplyWriteOnly(_ context.Context, config interface{}) diag.Diagnostics {
	diags := diag.Diagnostics{}
	c, ok := config.(*mgmtModel)
	if !ok {
		diags.AddError("Invalid model type", fmt.Sprintf("Expected *mgmtModel, got: %T", config))
		return diags
	}
	m.SSHPassword = c.SSHPassword
	m.SSHPasswordWo = c.SSHPasswordWo
	m.SSHPasswordWoVersion = c.SSHPasswordWoVersion
	return diags
}

func (m *mgmtModel) AsUnifiModel(ctx context.Context) (interface{}, diag.Diagnostics) {
	var diags diag.Diagnostics

	sshKeys, d := m.getSSHKeys(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return nil, diags
	}

	password := m.SSHPassword.ValueString()
	if !m.SSHPasswordWo.IsNull() {
		password = m.SSHPasswordWo.ValueString()
	}

	return &unifi.SettingMgmt{
		ID:                      m.ID.ValueString(),
		Key:                     unifi.SettingMgmtKey,
		AutoUpgrade:             m.AutoUpgrade.ValueBool(),
		AutoUpgradeHour:         int(m.AutoUpgradeHour.ValueInt32()),
		AdvancedFeatureEnabled:  m.AdvancedFeatureEnabled.ValueBool(),
		AlertEnabled:            m.AlertEnabled.ValueBool(),
		BootSound:               m.BootSound.ValueBool(),
		DebugToolsEnabled:       m.DebugToolsEnabled.ValueBool(),
		DirectConnectEnabled:    m.DirectConnectEnabled.ValueBool(),
		LedEnabled:              m.LedEnabled.ValueBool(),
		OutdoorModeEnabled:      m.OutdoorModeEnabled.ValueBool(),
		UnifiIDpEnabled:         m.UnifiIdpEnabled.ValueBool(),
		WifimanEnabled:          m.WifimanEnabled.ValueBool(),
		XSshEnabled:             m.SSHEnabled.ValueBool(),
		XSshAuthPasswordEnabled: m.SSHAuthPasswordEnabled.ValueBool(),
		XSshBindWildcard:        m.SSHBindWildcard.ValueBool(),
		XSshUsername:            m.SSHUsername.ValueString(),
		XSshPassword:            password,
		XSshKeys:                sshKeys,
	}, diags
}

func (m *mgmtModel) getSSHKeys(ctx context.Context) ([]unifi.SettingMgmtXSshKeys, diag.Diagnostics) {
	diags := diag.Diagnostics{}
	var sshKeys []unifi.SettingMgmtXSshKeys

	if m.SSHKeys.IsNull() || m.SSHKeys.IsUnknown() {
		return sshKeys, diags
	}

	var sshKeyElements []SSHKeyModel
	diags.Append(m.SSHKeys.ElementsAs(ctx, &sshKeyElements, false)...)
	if diags.HasError() {
		return nil, diags
	}

	for _, sshKey := range sshKeyElements {
		sshKeys = append(sshKeys, unifi.SettingMgmtXSshKeys{
			Name:    sshKey.Name.ValueString(),
			KeyType: sshKey.Type.ValueString(),
			Key:     sshKey.Key.ValueString(),
			Comment: sshKey.Comment.ValueString(),
		})
	}

	return sshKeys, diags
}

func (m *mgmtModel) Merge(ctx context.Context, other interface{}) diag.Diagnostics {
	diags := diag.Diagnostics{}
	resp, ok := other.(*unifi.SettingMgmt)
	if !ok {
		diags.AddError("Invalid model type", fmt.Sprintf("Expected *unifi.SettingMgmt, got: %T", other))
		return diags
	}

	m.ID = types.StringValue(resp.ID)
	m.AutoUpgrade = types.BoolValue(resp.AutoUpgrade)
	m.AutoUpgradeHour = types.Int32Value(int32(resp.AutoUpgradeHour))
	m.AdvancedFeatureEnabled = types.BoolValue(resp.AdvancedFeatureEnabled)
	m.AlertEnabled = types.BoolValue(resp.AlertEnabled)
	m.BootSound = types.BoolValue(resp.BootSound)
	m.DebugToolsEnabled = types.BoolValue(resp.DebugToolsEnabled)
	m.DirectConnectEnabled = types.BoolValue(resp.DirectConnectEnabled)
	m.LedEnabled = types.BoolValue(resp.LedEnabled)
	m.OutdoorModeEnabled = types.BoolValue(resp.OutdoorModeEnabled)
	m.UnifiIdpEnabled = types.BoolValue(resp.UnifiIDpEnabled)
	m.WifimanEnabled = types.BoolValue(resp.WifimanEnabled)
	m.SSHEnabled = types.BoolValue(resp.XSshEnabled)
	m.SSHAuthPasswordEnabled = types.BoolValue(resp.XSshAuthPasswordEnabled)
	m.SSHBindWildcard = types.BoolValue(resp.XSshBindWildcard)
	m.SSHUsername = types.StringValue(resp.XSshUsername)
	// x_ssh_password comes back on read but is never persisted: state keeps
	// whatever the practitioner configured -- null when the credential is fed
	// through write-only ssh_password_wo. x_ssh_password carries omitempty,
	// so an unconfigured password never reaches the wire on write either.
	// Controller-side drift in the password is invisible by design.

	// Convert SSH keys
	if len(resp.XSshKeys) > 0 {
		sshKeyElements := make([]SSHKeyModel, 0, len(resp.XSshKeys))
		for _, sshKey := range resp.XSshKeys {
			sshKeyElements = append(sshKeyElements, SSHKeyModel{
				Name:    types.StringValue(sshKey.Name),
				Type:    types.StringValue(sshKey.KeyType),
				Key:     types.StringValue(sshKey.Key),
				Comment: types.StringValue(sshKey.Comment),
			})
		}
		sshKeys, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: (&SSHKeyModel{}).AttributeTypes()}, sshKeyElements)
		diags.Append(d...)
		if !diags.HasError() {
			m.SSHKeys = sshKeys
		}
	} else {
		m.SSHKeys = types.ListNull(types.ObjectType{AttrTypes: (&SSHKeyModel{}).AttributeTypes()})
	}

	return diags
}

// NewMgmtResource creates a new instance of the management settings resource.
func NewMgmtResource() resource.Resource {
	return &mgmtResource{
		GenericResource: NewSettingResource(
			"unifi_setting_mgmt",
			func() *mgmtModel { return &mgmtModel{} },
			func(ctx context.Context, client *base.Client, site string) (interface{}, error) {
				return client.GetSettingMgmt(ctx, site)
			},
			func(ctx context.Context, client *base.Client, site string, body interface{}) (interface{}, error) {
				b, _ := body.(*unifi.SettingMgmt)
				return client.UpdateSettingMgmt(ctx, site, b)
			},
		),
	}
}

var (
	_ base.ResourceModel              = &mgmtModel{}
	_ resource.Resource               = &mgmtResource{}
	_ resource.ResourceWithConfigure  = &mgmtResource{}
	_ resource.ResourceWithModifyPlan = &mgmtResource{}
)

type mgmtResource struct {
	*base.GenericResource[*mgmtModel]
}

func (r *mgmtResource) ModifyPlan(_ context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	resp.Diagnostics.Append(r.RequireMinVersionForPath("7.0", path.Root("auto_upgrade_hour"), req.Config)...)
	resp.Diagnostics.Append(r.RequireMinVersionForPath("7.3", path.Root("debug_tools_enabled"), req.Config)...)
}

func (r *mgmtResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The `unifi_setting_mgmt` resource manages site-wide management settings in the UniFi controller.\n\n" +
			"This resource allows you to configure important management features including:\n" +
			"  * Automatic firmware upgrades for UniFi devices\n" +
			"  * SSH access for advanced configuration and troubleshooting\n" +
			"  * SSH key management for secure remote access\n\n" +
			"These settings affect how the UniFi controller manages devices at the site level. " +
			"They are particularly important for:\n" +
			"  * Maintaining device security through automatic updates\n" +
			"  * Enabling secure remote administration\n" +
			"  * Implementing SSH key-based authentication",
		Attributes: map[string]schema.Attribute{
			"id":   ut.ID(),
			"site": ut.SiteAttribute(),
			"auto_upgrade": schema.BoolAttribute{
				MarkdownDescription: "Enable automatic firmware upgrades for all UniFi devices at this site. When enabled, devices will automatically " +
					"update to the latest stable firmware version approved for your controller version.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"auto_upgrade_hour": schema.Int32Attribute{
				MarkdownDescription: "The hour of the day (0-23) when automatic firmware upgrades will occur.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Int32{
					int32planmodifier.UseStateForUnknown(),
				},
				Validators: []validator.Int32{
					int32validator.Between(0, 23),
				},
			},
			"advanced_feature_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable advanced features for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"alert_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable alerts for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"boot_sound": schema.BoolAttribute{
				MarkdownDescription: "Enable the boot sound for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"debug_tools_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable debug tools for UniFi devices at this site. Requires controller version 7.3 or later.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"direct_connect_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable direct connect for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"led_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable the LED light for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"outdoor_mode_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable outdoor mode for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"unifi_idp_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable UniFi IDP for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"wifiman_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable WiFiman for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"ssh_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable SSH access to UniFi devices at this site. When enabled, you can connect to devices using SSH for advanced " +
					"configuration and troubleshooting. It's recommended to only enable this temporarily when needed.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"ssh_auth_password_enabled": schema.BoolAttribute{
				MarkdownDescription: "Enable SSH password authentication for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"ssh_bind_wildcard": schema.BoolAttribute{
				MarkdownDescription: "Enable SSH bind wildcard for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"ssh_username": schema.StringAttribute{
				MarkdownDescription: "The SSH username for UniFi devices at this site.",
				Optional:            true,
				Computed:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"ssh_password": schema.StringAttribute{
				MarkdownDescription: "The SSH password for UniFi devices at this site. Never read back from the controller: state carries " +
					"it only while it is set here in configuration. Prefer `ssh_password_wo` for a credential sourced from a secret store.",
				Optional:  true,
				Computed:  true,
				Sensitive: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"ssh_password_wo": schema.StringAttribute{
				MarkdownDescription: "The SSH password for UniFi devices at this site, as a **write-only** attribute. Terraform never stores " +
					"it in plan or state, so it can be fed from an `ephemeral` block -- a Vault/OpenBao KV secret, for instance -- " +
					"without the credential landing in the state file. Requires Terraform 1.11 or later.\n\n" +
					"Prefer this over `ssh_password` for anything sourced from a secret store. Because Terraform cannot see a " +
					"write-only value, it cannot detect that the credential changed: it is sent on create, and on any update the " +
					"resource is already making for another reason.",
				Optional:  true,
				WriteOnly: true,
				Sensitive: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
					stringvalidator.ConflictsWith(path.MatchRoot("ssh_password")),
				},
			},
			"ssh_password_wo_version": schema.Int32Attribute{
				MarkdownDescription: "Companion to `ssh_password_wo`. Terraform cannot see a write-only value change, so rotating the " +
					"credential in the secret store alone never reaches the controller -- bump this number alongside the rotation " +
					"to force an update that sends the new value.",
				Optional: true,
				Validators: []validator.Int32{
					int32validator.AlsoRequires(path.MatchRoot("ssh_password_wo")),
				},
			},
		},
		Blocks: map[string]schema.Block{
			"ssh_key": schema.ListNestedBlock{
				MarkdownDescription: "List of SSH public keys that are allowed to connect to UniFi devices when SSH is enabled. Using SSH keys is more " +
					"secure than password authentication.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							MarkdownDescription: "A friendly name for the SSH key to help identify its owner or purpose (e.g., 'admin-laptop' or 'backup-server').",
							Required:            true,
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
							},
						},
						"type": schema.StringAttribute{
							MarkdownDescription: "The type of SSH key. Common values include:\n" +
								"  * `ssh-rsa` - RSA key (most common)\n" +
								"  * `ssh-ed25519` - Ed25519 key (more secure)\n" +
								"  * `ecdsa-sha2-nistp256` - ECDSA key",
							Required: true,
							Validators: []validator.String{
								stringvalidator.LengthAtLeast(1),
							},
						},
						"key": schema.StringAttribute{
							MarkdownDescription: "The public key string. This is the content that would normally go in an authorized_keys file, " +
								"excluding the type and comment (e.g., 'AAAAB3NzaC1yc2EA...').",
							Optional: true,
						},
						"comment": schema.StringAttribute{
							MarkdownDescription: "An optional comment to provide additional context about the key (e.g., 'generated on 2024-01-01' or 'expires 2025-12-31').",
							Optional:            true,
						},
					},
				},
			},
		},
	}
}
