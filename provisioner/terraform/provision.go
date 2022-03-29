package terraform

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/awalterschulze/gographviz"
	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/mitchellh/mapstructure"
	"golang.org/x/xerrors"

	"github.com/coder/coder/provisionersdk"
	"github.com/coder/coder/provisionersdk/proto"
)

// Provision executes `terraform apply`.
func (t *terraform) Provision(stream proto.DRPCProvisioner_ProvisionStream) error {
	shutdown, shutdownFunc := context.WithCancel(stream.Context())
	defer shutdownFunc()

	request, err := stream.Recv()
	if err != nil {
		return err
	}
	if request.GetCancel() != nil {
		return nil
	}
	// We expect the first message is start!
	if request.GetStart() == nil {
		return nil
	}
	go func() {
		for {
			request, err := stream.Recv()
			if err != nil {
				return
			}
			if request.GetCancel() == nil {
				// This is only to process cancels!
				continue
			}
			shutdownFunc()
			return
		}
	}()
	start := request.GetStart()
	statefilePath := filepath.Join(start.Directory, "terraform.tfstate")
	if len(start.State) > 0 {
		err := os.WriteFile(statefilePath, start.State, 0600)
		if err != nil {
			return xerrors.Errorf("write statefile %q: %w", statefilePath, err)
		}
	}

	terraform, err := tfexec.NewTerraform(start.Directory, t.binaryPath)
	if err != nil {
		return xerrors.Errorf("create new terraform executor: %w", err)
	}
	version, _, err := terraform.Version(shutdown, false)
	if err != nil {
		return xerrors.Errorf("get terraform version: %w", err)
	}
	if !version.GreaterThanOrEqual(minimumTerraformVersion) {
		return xerrors.Errorf("terraform version %q is too old. required >= %q", version.String(), minimumTerraformVersion.String())
	}

	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			_ = stream.Send(&proto.Provision_Response{
				Type: &proto.Provision_Response_Log{
					Log: &proto.Log{
						Level:  proto.LogLevel_DEBUG,
						Output: scanner.Text(),
					},
				},
			})
		}
	}()
	if t.cachePath != "" {
		err = terraform.SetEnv(map[string]string{
			"TF_PLUGIN_CACHE_DIR": t.cachePath,
		})
		if err != nil {
			return xerrors.Errorf("set terraform plugin cache dir: %w", err)
		}
	}
	terraform.SetStdout(writer)
	t.logger.Debug(shutdown, "running initialization")
	err = terraform.Init(shutdown)
	if err != nil {
		return xerrors.Errorf("initialize terraform: %w", err)
	}
	t.logger.Debug(shutdown, "ran initialization")
	_ = reader.Close()
	terraform.SetStdout(io.Discard)

	env := os.Environ()
	env = append(env,
		"CODER_URL="+start.Metadata.CoderUrl,
		"CODER_WORKSPACE_TRANSITION="+strings.ToLower(start.Metadata.WorkspaceTransition.String()),
		"CODER_WORKSPACE_NAME="+start.Metadata.WorkspaceName,
		"CODER_WORKSPACE_OWNER="+start.Metadata.WorkspaceOwner,
	)
	for key, value := range provisionersdk.AgentScriptEnv() {
		env = append(env, key+"="+value)
	}
	vars := []string{}
	for _, param := range start.ParameterValues {
		switch param.DestinationScheme {
		case proto.ParameterDestination_ENVIRONMENT_VARIABLE:
			env = append(env, fmt.Sprintf("%s=%s", param.Name, param.Value))
		case proto.ParameterDestination_PROVISIONER_VARIABLE:
			vars = append(vars, fmt.Sprintf("%s=%s", param.Name, param.Value))
		default:
			return xerrors.Errorf("unsupported parameter type %q for %q", param.DestinationScheme, param.Name)
		}
	}

	closeChan := make(chan struct{})
	reader, writer = io.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() {
		defer close(closeChan)
		decoder := json.NewDecoder(reader)
		for {
			var log terraformProvisionLog
			err := decoder.Decode(&log)
			if err != nil {
				return
			}
			logLevel, err := convertTerraformLogLevel(log.Level)
			if err != nil {
				// Not a big deal, but we should handle this at some point!
				continue
			}
			_ = stream.Send(&proto.Provision_Response{
				Type: &proto.Provision_Response_Log{
					Log: &proto.Log{
						Level:  logLevel,
						Output: log.Message,
					},
				},
			})

			if log.Diagnostic == nil {
				continue
			}

			// If the diagnostic is provided, let's provide a bit more info!
			logLevel, err = convertTerraformLogLevel(log.Diagnostic.Severity)
			if err != nil {
				continue
			}
			_ = stream.Send(&proto.Provision_Response{
				Type: &proto.Provision_Response_Log{
					Log: &proto.Log{
						Level:  logLevel,
						Output: log.Diagnostic.Detail,
					},
				},
			})
		}
	}()

	planfilePath := filepath.Join(start.Directory, "terraform.tfplan")
	var args []string
	if start.DryRun {
		args = []string{
			"plan",
			"-no-color",
			"-input=false",
			"-json",
			"-refresh=true",
			"-out=" + planfilePath,
		}
	} else {
		args = []string{
			"apply",
			"-no-color",
			"-auto-approve",
			"-input=false",
			"-json",
			"-refresh=true",
		}
	}
	if start.Metadata.WorkspaceTransition == proto.WorkspaceTransition_DESTROY {
		args = append(args, "-destroy")
	}
	for _, variable := range vars {
		args = append(args, "-var", variable)
	}
	// #nosec
	cmd := exec.CommandContext(stream.Context(), t.binaryPath, args...)
	go func() {
		select {
		case <-stream.Context().Done():
			return
		case <-shutdown.Done():
			_ = cmd.Process.Signal(os.Interrupt)
		}
	}()
	cmd.Stdout = writer
	cmd.Env = env
	cmd.Dir = terraform.WorkingDir()
	err = cmd.Run()
	if err != nil {
		if start.DryRun {
			if shutdown.Err() != nil {
				return stream.Send(&proto.Provision_Response{
					Type: &proto.Provision_Response_Complete{
						Complete: &proto.Provision_Complete{
							Error: err.Error(),
						},
					},
				})
			}
			return xerrors.Errorf("plan terraform: %w", err)
		}
		errorMessage := err.Error()
		// Terraform can fail and apply and still need to store it's state.
		// In this case, we return Complete with an explicit error message.
		statefileContent, err := os.ReadFile(statefilePath)
		if err != nil {
			return xerrors.Errorf("read file %q: %w", statefilePath, err)
		}
		return stream.Send(&proto.Provision_Response{
			Type: &proto.Provision_Response_Complete{
				Complete: &proto.Provision_Complete{
					State: statefileContent,
					Error: errorMessage,
				},
			},
		})
	}
	_ = reader.Close()
	<-closeChan

	var resp *proto.Provision_Response
	if start.DryRun {
		resp, err = parseTerraformPlan(stream.Context(), terraform, planfilePath)
	} else {
		resp, err = parseTerraformApply(stream.Context(), terraform, statefilePath)
	}
	if err != nil {
		return err
	}
	return stream.Send(resp)
}

func parseTerraformPlan(ctx context.Context, terraform *tfexec.Terraform, planfilePath string) (*proto.Provision_Response, error) {
	plan, err := terraform.ShowPlanFile(ctx, planfilePath)
	if err != nil {
		return nil, xerrors.Errorf("show terraform plan file: %w", err)
	}

	rawGraph, err := terraform.Graph(ctx)
	if err != nil {
		return nil, xerrors.Errorf("graph: %w", err)
	}
	resourceDependencies, err := findDirectDependencies(rawGraph)
	if err != nil {
		return nil, xerrors.Errorf("find dependencies: %w", err)
	}

	resources := make([]*proto.Resource, 0)
	agents := map[string]*proto.Agent{}

	// Store all agents inside the maps!
	for _, resource := range plan.Config.RootModule.Resources {
		if resource.Type != "coder_agent" {
			continue
		}
		agent := &proto.Agent{
			Auth: &proto.Agent_Token{},
		}
		if envRaw, has := resource.Expressions["env"]; has {
			env, ok := envRaw.ConstantValue.(map[string]string)
			if ok {
				agent.Env = env
			}
		}
		if startupScriptRaw, has := resource.Expressions["startup_script"]; has {
			startupScript, ok := startupScriptRaw.ConstantValue.(string)
			if ok {
				agent.StartupScript = startupScript
			}
		}
		if _, has := resource.Expressions["instance_id"]; has {
			// This is a dynamic value. If it's expressed, we know
			// it's at least an instance ID, which is better than nothing.
			agent.Auth = &proto.Agent_InstanceId{
				InstanceId: "",
			}
		}

		agents[resource.Address] = agent
	}
	for _, resource := range plan.PlannedValues.RootModule.Resources {
		if resource.Type == "coder_agent" {
			continue
		}
		resourceKey := strings.Join([]string{resource.Type, resource.Name}, ".")
		resourceNode, exists := resourceDependencies[resourceKey]
		if !exists {
			continue
		}
		// Associate resources that depend on an agent.
		var agent *proto.Agent
		for _, dep := range resourceNode {
			var has bool
			agent, has = agents[dep]
			if has {
				break
			}
		}

		resources = append(resources, &proto.Resource{
			Name:  resource.Name,
			Type:  resource.Type,
			Agent: agent,
		})
	}

	return &proto.Provision_Response{
		Type: &proto.Provision_Response_Complete{
			Complete: &proto.Provision_Complete{
				Resources: resources,
			},
		},
	}, nil
}

func parseTerraformApply(ctx context.Context, terraform *tfexec.Terraform, statefilePath string) (*proto.Provision_Response, error) {
	statefileContent, err := os.ReadFile(statefilePath)
	if err != nil {
		return nil, xerrors.Errorf("read file %q: %w", statefilePath, err)
	}
	state, err := terraform.ShowStateFile(ctx, statefilePath)
	if err != nil {
		return nil, xerrors.Errorf("show state file %q: %w", statefilePath, err)
	}
	resources := make([]*proto.Resource, 0)
	if state.Values != nil {
		rawGraph, err := terraform.Graph(ctx)
		if err != nil {
			return nil, xerrors.Errorf("graph: %w", err)
		}
		resourceDependencies, err := findDirectDependencies(rawGraph)
		if err != nil {
			return nil, xerrors.Errorf("find dependencies: %w", err)
		}
		type agentAttributes struct {
			ID            string            `mapstructure:"id"`
			Token         string            `mapstructure:"token"`
			InstanceID    string            `mapstructure:"instance_id"`
			Env           map[string]string `mapstructure:"env"`
			StartupScript string            `mapstructure:"startup_script"`
		}
		agents := map[string]*proto.Agent{}

		// Store all agents inside the maps!
		for _, resource := range state.Values.RootModule.Resources {
			if resource.Type != "coder_agent" {
				continue
			}
			var attrs agentAttributes
			err = mapstructure.Decode(resource.AttributeValues, &attrs)
			if err != nil {
				return nil, xerrors.Errorf("decode agent attributes: %w", err)
			}
			agent := &proto.Agent{
				Id:            attrs.ID,
				Env:           attrs.Env,
				StartupScript: attrs.StartupScript,
				Auth: &proto.Agent_Token{
					Token: attrs.Token,
				},
			}
			if attrs.InstanceID != "" {
				agent.Auth = &proto.Agent_InstanceId{
					InstanceId: attrs.InstanceID,
				}
			}
			resourceKey := strings.Join([]string{resource.Type, resource.Name}, ".")
			agents[resourceKey] = agent
		}

		for _, resource := range state.Values.RootModule.Resources {
			if resource.Type == "coder_agent" {
				continue
			}
			resourceKey := strings.Join([]string{resource.Type, resource.Name}, ".")
			resourceNode, exists := resourceDependencies[resourceKey]
			if !exists {
				continue
			}
			// Associate resources that depend on an agent.
			var agent *proto.Agent
			for _, dep := range resourceNode {
				var has bool
				agent, has = agents[dep]
				if has {
					break
				}
			}

			resources = append(resources, &proto.Resource{
				Name:  resource.Name,
				Type:  resource.Type,
				Agent: agent,
			})
		}
	}

	return &proto.Provision_Response{
		Type: &proto.Provision_Response_Complete{
			Complete: &proto.Provision_Complete{
				State:     statefileContent,
				Resources: resources,
			},
		},
	}, nil
}

type terraformProvisionLog struct {
	Level   string `json:"@level"`
	Message string `json:"@message"`

	Diagnostic *terraformProvisionLogDiagnostic `json:"diagnostic"`
}

type terraformProvisionLogDiagnostic struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail"`
}

func convertTerraformLogLevel(logLevel string) (proto.LogLevel, error) {
	switch strings.ToLower(logLevel) {
	case "trace":
		return proto.LogLevel_TRACE, nil
	case "debug":
		return proto.LogLevel_DEBUG, nil
	case "info":
		return proto.LogLevel_INFO, nil
	case "warn":
		return proto.LogLevel_WARN, nil
	case "error":
		return proto.LogLevel_ERROR, nil
	default:
		return proto.LogLevel(0), xerrors.Errorf("invalid log level %q", logLevel)
	}
}

// findDirectDependencies maps Terraform resources to their parent and
// children nodes. This parses GraphViz output from Terraform which
// certainly is not ideal, but seems reliable.
func findDirectDependencies(rawGraph string) (map[string][]string, error) {
	parsedGraph, err := gographviz.ParseString(rawGraph)
	if err != nil {
		return nil, xerrors.Errorf("parse graph: %w", err)
	}
	graph, err := gographviz.NewAnalysedGraph(parsedGraph)
	if err != nil {
		return nil, xerrors.Errorf("analyze graph: %w", err)
	}
	direct := map[string][]string{}
	for _, node := range graph.Nodes.Nodes {
		label, exists := node.Attrs["label"]
		if !exists {
			continue
		}
		label = strings.Trim(label, `"`)

		dependencies := make([]string, 0)
		for _, edges := range []map[string][]*gographviz.Edge{
			graph.Edges.SrcToDsts[node.Name],
			graph.Edges.DstToSrcs[node.Name],
		} {
			for destination := range edges {
				dependencyNode, exists := graph.Nodes.Lookup[destination]
				if !exists {
					continue
				}
				label, exists := dependencyNode.Attrs["label"]
				if !exists {
					continue
				}
				label = strings.Trim(label, `"`)
				dependencies = append(dependencies, label)
			}
		}
		direct[label] = dependencies
	}
	return direct, nil
}
