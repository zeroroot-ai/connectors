package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zeroroot-ai/connectors/internal/kit"
)

// The tools follow the orchestrator API of Puppet Enterprise:
//   - POST /orchestrator/v1/command/task
//     https://help.puppet.com/pe/current/topics/orchestrator_api_post_command_task.htm
//     body {"environment","task","params","scope":{"nodes":[...]},"description"}
//     answer 202 {"job": {"id": "<job url>", "name": "<job number>"}}
//   - GET /orchestrator/v1/jobs/{id}
//     https://help.puppet.com/pe/current/topics/orchestrator_api_get_jobs_job_id.htm
//     answer {"id","state","options","node_count","timestamp","status","nodes"}
//     state is new, ready, running, stopping, stopped, finished or failed
//   - GET /orchestrator/v1/jobs/{id}/nodes
//     https://help.puppet.com/pe/current/topics/orchestrator_api_get_jobs_job_id_nodes.htm
//     answer {"items":[{"name","state","result","details","duration",
//     "start_timestamp","finish_timestamp"}], "next-events"}
//   - Header "X-Authentication: <token>". The API listens on port 8143.
//
// Puppet has no patch API of its own. A patch is a task that the operator
// names in the configuration. The model never names a task, so it cannot run
// an arbitrary task. get_job reads options.task and type of the job, and shows
// only a job of those two tasks.

var (
	taskFormat = regexp.MustCompile(`^[a-z][a-z0-9_]*(::[a-z][a-z0-9_]*)*$`)
	envFormat  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// certname is the name of a node. Puppet lower-cases it.
	certname = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,250}[a-z0-9])?$`)
	jobID    = regexp.MustCompile(`^\d{1,12}$`)
)

type taskSpec struct {
	Name   string
	Params map[string]any
}

func build(env *kit.Env, log *slog.Logger) (func(*mcp.Server), error) {
	caFile := env.Optional("PUPPET_CA_CERT_FILE", "")
	var hc *http.Client
	if caFile != "" {
		c, err := clientWithCA(caFile)
		if err != nil {
			return nil, err
		}
		hc = c
	}
	return buildWith(env, log, hc)
}

// clientWithCA returns an HTTP client that trusts the CA of Puppet Enterprise.
func clientWithCA(path string) (*http.Client, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // the path is set by the operator in the environment
	if err != nil {
		return nil, fmt.Errorf("read PUPPET_CA_CERT_FILE: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("PUPPET_CA_CERT_FILE holds no PEM certificate")
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}, nil
}

// buildWith is build with a replaceable HTTP client. Tests set it.
func buildWith(env *kit.Env, log *slog.Logger, hc *http.Client) (func(*mcp.Server), error) {
	base := env.URL("PUPPET_ORCHESTRATOR_URL")
	token := env.Secret("PUPPET_TOKEN")
	environment := env.Optional("PUPPET_ENVIRONMENT", "production")
	patch := env.Require("PUPPET_PATCH_TASK")
	rollback := env.Require("PUPPET_ROLLBACK_TASK")
	patchParams := env.Optional("PUPPET_PATCH_PARAMS", "{}")
	rollbackParams := env.Optional("PUPPET_ROLLBACK_PARAMS", "{}")
	timeout := env.Duration("PUPPET_TIMEOUT", 30*time.Second)
	if err := env.Err(); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if base.Scheme != "https" {
		return nil, errors.New("PUPPET_ORCHESTRATOR_URL must use https")
	}
	if !envFormat.MatchString(environment) {
		return nil, errors.New("PUPPET_ENVIRONMENT has the wrong format")
	}
	svc := &service{environment: environment}
	for _, t := range []struct {
		name, task, params string
		into               *taskSpec
	}{
		{"PUPPET_PATCH_TASK", patch, patchParams, &svc.patch},
		{"PUPPET_ROLLBACK_TASK", rollback, rollbackParams, &svc.rollback},
	} {
		if !taskFormat.MatchString(t.task) {
			return nil, fmt.Errorf("%s is not a Puppet task name such as pe_patch::patch_server", t.name)
		}
		t.into.Name = t.task
		if err := json.Unmarshal([]byte(t.params), &t.into.Params); err != nil || t.into.Params == nil {
			return nil, fmt.Errorf("the parameters of %s must be a JSON object", t.name)
		}
	}
	// The API path has a fixed prefix.
	root := *base
	root.Path += "/orchestrator/v1"
	svc.client = kit.NewClient(&root, kit.ClientOptions{
		Auth:   kit.HeaderAuth("X-Authentication", "", token),
		Logger: log, Timeout: timeout, HTTPClient: hc,
	})
	return func(s *mcp.Server) { svc.register(s, log) }, nil
}

type service struct {
	client      *kit.Client
	environment string
	patch       taskSpec
	rollback    taskSpec
}

func (v *service) register(s *mcp.Server, log *slog.Logger) {
	kit.AddTool(s, log, &mcp.Tool{
		Name: "start_patch_job",
		Description: "Change action. Start the patch task of the operator on one Puppet Enterprise node. " +
			"Puppet answers at once with a job id. The patch runs in the job. Read the result with get_job. " +
			"A patch can restart services or the node. " +
			"Undo: call start_rollback_job with the same node. The rollback task is a task of the operator, and " +
			"Puppet does not check that it restores the state before the patch.",
		Annotations: kit.Reversible("Start patch job", false),
	}, v.startPatch)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "start_rollback_job",
		Description: "Change action. Start the rollback task of the operator on one Puppet Enterprise node. " +
			"Puppet answers at once with a job id. Read the result with get_job. " +
			"This is the undo of start_patch_job. To patch again, call start_patch_job.",
		Annotations: kit.Reversible("Start rollback job", false),
	}, v.startRollback)
	kit.AddTool(s, log, &mcp.Tool{
		Name: "get_job",
		Description: "Read the state of one Puppet orchestrator job and the result of each node in it. Read only. " +
			"The job state is new, ready, running, stopping, stopped, finished or failed. " +
			"The tool shows only a job of the patch task or the rollback task of this server, " +
			"as start_patch_job and start_rollback_job start them. It refuses any other job.",
		Annotations: kit.ReadOnly("Get Puppet job"),
	}, v.getJob)
}

type nodeInput struct {
	Node string `json:"node" jsonschema:"certname of one node, for example web01.example.com. A list, a query and a group are not accepted."`
}

type startOutput struct {
	JobID string `json:"job_id"`
	Task  string `json:"task"`
	Node  string `json:"node"`
}

func (v *service) startPatch(ctx context.Context, _ *mcp.CallToolRequest, in nodeInput) (*mcp.CallToolResult, startOutput, error) {
	return v.start(ctx, v.patch, in.Node, "Zero Root: patch")
}

func (v *service) startRollback(ctx context.Context, _ *mcp.CallToolRequest, in nodeInput) (*mcp.CallToolResult, startOutput, error) {
	return v.start(ctx, v.rollback, in.Node, "Zero Root: rollback")
}

func (v *service) start(ctx context.Context, t taskSpec, node, description string) (*mcp.CallToolResult, startOutput, error) {
	out := startOutput{Task: t.Name, Node: node}
	if err := kit.CheckString("node", node, 253, certname); err != nil {
		return nil, out, err
	}
	var resp struct {
		Job struct {
			Name string `json:"name"`
		} `json:"job"`
	}
	err := v.client.Do(ctx, kit.Request{
		Method: http.MethodPost,
		Path:   "/command/task",
		JSON: map[string]any{
			"environment": v.environment,
			"task":        t.Name,
			"params":      t.Params,
			"scope":       map[string]any{"nodes": []string{node}},
			"description": description,
		},
	}, &resp)
	if err != nil {
		return nil, out, puppetError("start job", err)
	}
	if !jobID.MatchString(resp.Job.Name) {
		return nil, out, errors.New("start job: Puppet answered with no job number")
	}
	out.JobID = resp.Job.Name
	return nil, out, nil
}

type jobInput struct {
	JobID string `json:"job_id" jsonschema:"job number that start_patch_job or start_rollback_job returned"`
}

type nodeResult struct {
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Duration float64 `json:"duration,omitempty"`
	Started  string  `json:"start_timestamp,omitempty"`
	Finished string  `json:"finish_timestamp,omitempty"`
	Message  string  `json:"message,omitempty"`
	Result   any     `json:"result,omitempty"`
}

type jobOutput struct {
	JobID     string       `json:"job_id"`
	Task      string       `json:"task"`
	State     string       `json:"state"`
	NodeCount int          `json:"node_count"`
	Updated   string       `json:"timestamp,omitempty"`
	Nodes     []nodeResult `json:"nodes"`
}

func (v *service) getJob(ctx context.Context, _ *mcp.CallToolRequest, in jobInput) (*mcp.CallToolResult, jobOutput, error) {
	out := jobOutput{JobID: in.JobID, Nodes: []nodeResult{}}
	if err := kit.CheckString("job_id", in.JobID, 12, jobID); err != nil {
		return nil, out, err
	}
	var job struct {
		Type      string `json:"type"`
		State     string `json:"state"`
		NodeCount int    `json:"node_count"`
		Timestamp string `json:"timestamp"`
		Options   struct {
			Task string `json:"task"`
		} `json:"options"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("jobs", in.JobID)}, &job); err != nil {
		return nil, out, puppetError("get job", err)
	}
	// The tool shows only a job of the two tasks of this server. Another job
	// can hold the output of any task on any node.
	if job.Type != "task" || (job.Options.Task != v.patch.Name && job.Options.Task != v.rollback.Name) {
		return nil, out, fmt.Errorf("%w: job %s is not a job of the patch task or the rollback task of this server",
			kit.ErrInvalidInput, in.JobID)
	}
	out.Task = job.Options.Task
	var nodes struct {
		Items []struct {
			Name     string  `json:"name"`
			State    string  `json:"state"`
			Duration float64 `json:"duration"`
			Started  string  `json:"start_timestamp"`
			Finished string  `json:"finish_timestamp"`
			Details  struct {
				Message string `json:"message"`
			} `json:"details"`
			Result any `json:"result"`
		} `json:"items"`
	}
	if err := v.client.Do(ctx, kit.Request{Method: http.MethodGet, Path: kit.Path("jobs", in.JobID, "nodes")}, &nodes); err != nil {
		return nil, out, puppetError("get job nodes", err)
	}
	out.State, out.NodeCount, out.Updated = job.State, job.NodeCount, job.Timestamp
	for _, n := range nodes.Items {
		out.Nodes = append(out.Nodes, nodeResult{Name: n.Name, State: n.State, Duration: n.Duration,
			Started: n.Started, Finished: n.Finished, Message: n.Details.Message, Result: n.Result})
	}
	return nil, out, nil
}

// puppetError shows the kind and the message of an orchestrator error, which
// has the field "kind" (the kinds are listed in the page of command/task) and
// a message.
func puppetError(op string, err error) error {
	var ae *kit.APIError
	if errors.As(err, &ae) {
		var body struct {
			Kind    string `json:"kind"`
			Msg     string `json:"msg"`
			Message string `json:"message"`
		}
		if kit.DecodeErrorBody(ae, &body) == nil && body.Kind != "" {
			return fmt.Errorf("%s: Puppet answered HTTP %d, %s: %s%s", op, ae.Status, body.Kind, body.Msg, body.Message)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
