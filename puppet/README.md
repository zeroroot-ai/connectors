# puppet

MCP server for patch jobs on Puppet Enterprise nodes. The vendor server of Puppet Enterprise only helps with code (`get_puppet_guide`, `list_puppet_entities`, `get_puppet_entity_docs`) and starts no patch job (ADR-0177). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `PUPPET_ORCHESTRATOR_URL` | Required. Orchestrator URL, for example `https://pe.example.com:8143`. It must use https. |
| `PUPPET_TOKEN` | Required. A Puppet Enterprise user authentication token. The server sends it as `X-Authentication`. |
| `PUPPET_PATCH_TASK` | Required. Name of the task that patches a node, for example `pe_patch::patch_server`. |
| `PUPPET_PATCH_PARAMS` | Optional. JSON object of the parameters of the patch task. The default is `{}`. |
| `PUPPET_ROLLBACK_TASK` | Required. Name of the task that rolls a patch back. Puppet has no such task of its own. Write one. |
| `PUPPET_ROLLBACK_PARAMS` | Optional. JSON object of the parameters of the rollback task. The default is `{}`. |
| `PUPPET_ENVIRONMENT` | Optional. The environment that holds the tasks. The default is `production`. |
| `PUPPET_CA_CERT_FILE` | Optional. Path of a PEM file with the CA of Puppet Enterprise, because the certificate of the orchestrator comes from the CA of the Puppet install. |
| `PUPPET_TIMEOUT` | Optional. Time limit of one vendor call. The default is `30s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

References: https://help.puppet.com/pe/current/topics/orchestrator_api_post_command_task.htm, https://help.puppet.com/pe/current/topics/orchestrator_api_get_jobs_job_id.htm and https://help.puppet.com/pe/current/topics/orchestrator_api_get_jobs_job_id_nodes.htm

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `start_patch_job` | `POST /orchestrator/v1/command/task` with the patch task | `start_rollback_job` | `destructiveHint: false`, `idempotentHint: false` |
| `start_rollback_job` | `POST /orchestrator/v1/command/task` with the rollback task | `start_patch_job` | `destructiveHint: false`, `idempotentHint: false` |
| `get_job` | `GET /orchestrator/v1/jobs/{id}` and `GET /orchestrator/v1/jobs/{id}/nodes` | Not needed | `readOnlyHint: true` |

Puppet has no patch API. A patch is a task, and the operator names that task in the environment of the server. The model never names a task, so it cannot run another task. A start tool takes one node (a certname) and nothing else. It never sends a query or a node group. Puppet answers HTTP 202 at once with a job number. Call `get_job` for the state. `get_job` shows only a job whose `type` is `task` and whose `options.task` is the patch task or the rollback task of the server. It refuses any other orchestrator job and does not read its nodes, so the model cannot read the output of another task. The state of a job is `new`, `ready`, `running`, `stopping`, `stopped`, `finished` or `failed`. A `finished` job can hold a node with a failed task, so read the state of each node in `nodes`.

The rollback is a task of the operator. The tool description says that Puppet does not check that the task restores the state before the patch. The gibson#1544 issue asked for a rollback, so the server needs `PUPPET_ROLLBACK_TASK` and does not start without it.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Create a Puppet Enterprise role for the user of the token. Give it only these permissions, and scope task permission to the two tasks and to the node group that the agent may patch.

- Run the patch task and the rollback task (the "Tasks" object type, permission "Run" for those two tasks).
- Run tasks on the nodes of that node group, and view the orchestrator jobs of the user.

The exact permission names are in the console of Puppet Enterprise. The pages cited above do not list them, so check them when you make the role. Use a token with a short lifetime and renew it.
