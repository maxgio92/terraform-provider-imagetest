package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/chainguard-dev/terraform-provider-imagetest/internal/features"
	"github.com/chainguard-dev/terraform-provider-imagetest/internal/harness"
	"github.com/chainguard-dev/terraform-provider-imagetest/internal/inventory"
	"github.com/chainguard-dev/terraform-provider-imagetest/internal/log"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// TODO: Make the default feature timeout configurable?
	defaultFeatureCreateTimeout = 15 * time.Minute
)

var _ resource.ResourceWithModifyPlan = &FeatureResource{}

func NewFeatureResource() resource.Resource {
	return &FeatureResource{}
}

// FeatureResource defines the resource implementation.
type FeatureResource struct {
	store *ProviderStore
}

// FeatureResourceModel describes the resource data model.
type FeatureResourceModel struct {
	Id          types.String       `tfsdk:"id"`
	Name        types.String       `tfsdk:"name"`
	Description types.String       `tfsdk:"description"`
	Labels      types.Map          `tfsdk:"labels"`
	Before      []FeatureStepModel `tfsdk:"before"`
	After       []FeatureStepModel `tfsdk:"after"`
	Steps       []FeatureStepModel `tfsdk:"steps"`
	Timeouts    timeouts.Value     `tfsdk:"timeouts"`

	Harness FeatureHarnessResourceModel `tfsdk:"harness"`
}

type FeatureStepModel struct {
	Name    types.String             `tfsdk:"name"`
	Cmd     types.String             `tfsdk:"cmd"`
	Workdir types.String             `tfsdk:"workdir"`
	Retry   *FeatureStepBackoffModel `tfsdk:"retry"`
}

type FeatureStepBackoffModel struct {
	Attempts types.Int64   `tfsdk:"attempts"`
	Delay    types.String  `tfsdk:"delay"`
	Factor   types.Float64 `tfsdk:"factor"`
}

func (r *FeatureResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_feature"
}

func (r *FeatureResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		// This description is used by the documentation generator and the language server.
		MarkdownDescription: "Feature resource, used to evaluate the steps of a given test",
		Attributes: mergeResourceSchemas(
			defaultFeatureHarnessResourceSchemaAttributes(),
			map[string]schema.Attribute{
				"id": schema.StringAttribute{
					Description: "ID is an encoded hash of the feature name and harness ID. It is used as a computed unique identifier of the feature within a given harness.",
					Computed:    true,
				},
				"name": schema.StringAttribute{
					Description: "The name of the feature",
					Required:    true,
				},
				"description": schema.StringAttribute{
					Description: "A descriptor of the feature",
					Optional:    true,
				},
				"before": schema.ListNestedAttribute{
					Description: "Actions to run against the harness before the core feature steps.",
					Optional:    true,
					NestedObject: schema.NestedAttributeObject{
						Attributes: map[string]schema.Attribute{
							"name": schema.StringAttribute{
								Description: "An identifying name for this step",
								Optional:    true,
							},
							"cmd": schema.StringAttribute{
								Description: "The command or set of commands that should be run at this step",
								Required:    true,
							},
							"workdir": schema.StringAttribute{
								Description: "An optional working directory for the step to run in",
								Optional:    true,
							},
							"retry": schema.SingleNestedAttribute{
								Description: "Optional retry configuration for the step",
								Optional:    true,
								Attributes:  addFeatureStepBackoffSchemaAttributes(),
							},
						},
					},
				},
				"after": schema.ListNestedAttribute{
					Description: "Actions to run againast the harness after the core steps have run OR after a step has failed.",
					Optional:    true,
					NestedObject: schema.NestedAttributeObject{
						Attributes: map[string]schema.Attribute{
							"name": schema.StringAttribute{
								Description: "An identifying name for this step",
								Optional:    true,
							},
							"cmd": schema.StringAttribute{
								Description: "The command or set of commands that should be run at this step",
								Required:    true,
							},
							"workdir": schema.StringAttribute{
								Description: "An optional working directory for the step to run in",
								Optional:    true,
							},
							"retry": schema.SingleNestedAttribute{
								Description: "Optional retry configuration for the step",
								Optional:    true,
								Attributes:  addFeatureStepBackoffSchemaAttributes(),
							},
						},
					},
				},
				"steps": schema.ListNestedAttribute{
					Description: "Actions to run against the harness.",
					Optional:    true,
					NestedObject: schema.NestedAttributeObject{
						Attributes: map[string]schema.Attribute{
							"name": schema.StringAttribute{
								Description: "An identifying name for this step",
								Optional:    true,
							},
							"cmd": schema.StringAttribute{
								Description: "The command or set of commands that should be run at this step",
								Required:    true,
							},
							"workdir": schema.StringAttribute{
								Description: "An optional working directory for the step to run in",
								Optional:    true,
							},
							"retry": schema.SingleNestedAttribute{
								Description: "Optional retry configuration for the step",
								Optional:    true,
								Attributes:  addFeatureStepBackoffSchemaAttributes(),
							},
						},
					},
				},
				"labels": schema.MapAttribute{
					Description: "A set of labels used to optionally filter execution of the feature",
					Optional:    true,
					ElementType: basetypes.StringType{},
				},
				"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
					Create: true,
				}),
			},
		),
	}
}

func (r *FeatureResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	// Prevent panic if the provider has not been configured.
	if req.ProviderData == nil {
		return
	}

	store, ok := req.ProviderData.(*ProviderStore)
	if !ok {
		resp.Diagnostics.AddError("invalid provider data", "...")
		return
	}

	r.store = store
}

// ModifyPlan implements resource.ResourceWithModifyPlan.
func (r *FeatureResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// If we have state, and the plan for id is null, we're in a destroy so do nothing
	if !req.State.Raw.IsNull() && req.Plan.Raw.IsNull() {
		return
	}

	var data FeatureResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Create an ID that is a hash of the feature name
	id, err := r.store.Encode(data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("failed to encode feature name", err.Error())
		return
	}

	// Set the "constants" we know during plan
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("harness"), data.Harness)...)
	if resp.Diagnostics.HasError() {
		return
	}

	labels := make(map[string]string)
	if diags := data.Labels.ElementsAs(ctx, &labels, false); diags.HasError() {
		return
	}

	added, err := r.store.Inventory(data.Harness.Inventory).AddFeature(ctx, inventory.Feature{
		Id:      id,
		Labels:  labels,
		Harness: inventory.Harness(data.Harness.Id.ValueString()),
	})
	if err != nil {
		resp.Diagnostics.AddError("failed to add feature to inventory", err.Error())
		return
	}

	if added {
		log.Debug(ctx, fmt.Sprintf("Feature.ModifyPlan() | feature [%s] added to inventory", id), "inventory", data.Harness.Inventory.Seed.ValueString())
	}
}

func (r *FeatureResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data FeatureResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// TODO: Move this around if/when we start storing test output in the state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)

	resp.Diagnostics.Append(r.do(ctx, data)...)
	if resp.Diagnostics.HasError() {
		return
	}
}

func (r *FeatureResource) do(ctx context.Context, data FeatureResourceModel) diag.Diagnostics {
	timeout, diags := data.Timeouts.Create(ctx, defaultFeatureCreateTimeout)
	if diags.HasError() {
		return diags
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ctx, err := r.store.Logger(ctx, data.Harness.Inventory, "feature_id", data.Id.ValueString(), "feature_name", data.Name.ValueString(), "harness_name", data.Harness.Id.ValueString())
	if err != nil {
		return []diag.Diagnostic{diag.NewErrorDiagnostic("failed to create logger", err.Error())}
	}

	harness, ok := r.store.harnesses.Get(data.Harness.Id.ValueString())
	if !ok {
		return []diag.Diagnostic{diag.NewWarningDiagnostic(fmt.Sprintf("skipping feature [%s] since harness was skipped", data.Id.ValueString()), "given provider runtime labels do not match feature labels")}
	}

	defer func() {
		remaining, err := r.store.Inventory(data.Harness.Inventory).RemoveFeature(ctx, inventory.Feature{
			Id:      data.Id.ValueString(),
			Harness: inventory.Harness(data.Harness.Id.ValueString()),
		})
		if err != nil {
			diags = append(diags, diag.NewErrorDiagnostic("failed to remove feature from inventory", err.Error()))
			return
		}

		if len(remaining) == 0 {
			log.Debug(ctx, "no more features remain in inventory, removing harness")
			if err := r.store.Inventory(data.Harness.Inventory).RemoveHarness(ctx, inventory.Harness(data.Harness.Id.ValueString())); err != nil {
				diags = append(diags, diag.NewErrorDiagnostic("failed to remove harness from inventory", err.Error()))
				return
			}

			// Destroy the harness...
			if r.store.SkipTeardown() {
				diags = append(diags, diag.NewWarningDiagnostic(fmt.Sprintf("skipping harness [%s] teardown because IMAGETEST_SKIP_TEARDOWN is set", data.Harness.Id.ValueString()), "harness must be removed manually"))
				return
			}

			if err := harness.Destroy(ctx); err != nil {
				diags = append(diags, diag.NewWarningDiagnostic("failed to destroy harness", err.Error()))
				return
			}
		}
	}()

	fopts := []features.Option{
		features.WithDescription(data.Description.ValueString()),
	}

	feat := features.New(data.Name.ValueString(), fopts...)

	for _, before := range data.Before {
		if err := r.step(feat, harness, before, features.Before); err != nil {
			return []diag.Diagnostic{diag.NewErrorDiagnostic("failed to create before step", err.Error())}
		}
	}

	for _, after := range data.After {
		if err := r.step(feat, harness, after, features.After); err != nil {
			return []diag.Diagnostic{diag.NewErrorDiagnostic("failed to create after step", err.Error())}
		}
	}

	for _, assess := range data.Steps {
		if err := r.step(feat, harness, assess, features.Assessment); err != nil {
			return []diag.Diagnostic{diag.NewErrorDiagnostic("failed to create assessment step", err.Error())}
		}
	}

	log.Info(ctx, "testing feature against harness")

	if err := feat.Test(ctx); err != nil {
		return []diag.Diagnostic{
			diag.NewErrorDiagnostic(
				fmt.Sprintf("failed to test feature: %s", feat.Name),
				err.Error()),
		}
	}

	return diags
}

func (r *FeatureResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data FeatureResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// TODO: Move this around if/when we start storing test output in the state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)

	resp.Diagnostics.Append(r.do(ctx, data)...)
	if resp.Diagnostics.HasError() {
		return
	}
}

func (r *FeatureResource) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {
}

func (r *FeatureResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *FeatureResource) step(feat *features.Feature, h harness.Harness, data FeatureStepModel, level features.Level) error {
	fn := features.StepFn(func(ctx context.Context) error {
		ctx = log.With(ctx,
			"step_name", data.Name.ValueString(),
			"cmd", data.Cmd.ValueString(),
			"feature", data.Name.ValueString(),
		)

		// capture a combined output buffer and a stderr buffer. the combined
		// output is usually easier to reason that just stdout alone, and lets us
		// return more information on failures.
		var bufall, buferr bytes.Buffer

		err := h.Run(ctx, harness.Command{
			Args:       data.Cmd.ValueString(),
			WorkingDir: data.Workdir.ValueString(),
			Stdout:     &bufall,
			Stderr:     io.MultiWriter(&buferr, &bufall),
		})

		ctx = log.With(ctx,
			"output", bufall.String(),
		)

		if err != nil {
			if rerr, ok := err.(*harness.RunError); ok {
				log.Warn(ctx, "feature step failed with non-zero exit code",
					"exit_code", rerr.ExitCode,
					"stderr", buferr.String())
				return rerr
			}
			return fmt.Errorf("running step: %w", err)
		}

		log.Info(ctx, "ran feature step")
		return nil
	})

	sopts := []features.StepOpt{}

	if data.Retry != nil {
		duration, err := time.ParseDuration(data.Retry.Delay.ValueString())
		if err != nil {
			return fmt.Errorf("failed to parse step retry duration: %w", err)
		}
		sopts = append(sopts, features.StepWithRetry(wait.Backoff{
			Duration: duration,
			Steps:    int(data.Retry.Attempts.ValueInt64()),
			Factor:   data.Retry.Factor.ValueFloat64(),
			// Set a small default value just as a best practice, even though this
			// isn't exposed, in reality it will never be noticed
			Jitter: 0.05,
		}))
	}

	switch level {
	case features.Before:
		feat.WithBefore(data.Name.ValueString(), fn, sopts...)
	case features.After:
		feat.WithAfter(data.Name.ValueString(), fn, sopts...)
	case features.Assessment:
		feat.WithAssessment(data.Name.ValueString(), fn, sopts...)
	}

	return nil
}

func addFeatureStepBackoffSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"attempts": schema.Int64Attribute{
			Description: "The maximum number of attempts to retry the step.",
			Required:    true,
		},
		"delay": schema.StringAttribute{
			Description: "The delay to wait before retrying. Defaults to immediately retrying (0s).",
			Optional:    true,
			Computed:    true,
			Default:     stringdefault.StaticString("0s"),
		},
		"factor": schema.Float64Attribute{
			Description: "The factor to multiply the delay by on each retry. The default value of 1.0 means no delay increase per retry.",
			Optional:    true,
			Computed:    true,
			Default:     float64default.StaticFloat64(1.0),
		},
	}
}

func defaultFeatureHarnessResourceSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"harness": schema.SingleNestedAttribute{
			Required: true,
			Attributes: map[string]schema.Attribute{
				"id": schema.StringAttribute{
					Required: true,
				},
				"name": schema.StringAttribute{
					Required: true,
				},
				"inventory": schema.SingleNestedAttribute{
					Required: true,
					Attributes: map[string]schema.Attribute{
						"seed": schema.StringAttribute{
							Required: true,
						},
					},
				},
			},
		},
	}
}
