# microsoft-intune

MCP server that patches one Windows device through the Windows Update deployment service of Microsoft Graph. No vendor server covers this (ADR-0065 as amended). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Decision and risk

Graph v1.0 has no call that patches one device. The owner chose the beta API on 2026-11-21. The server sends each call to `<INTUNE_GRAPH_URL>/beta`. Microsoft can change the beta API with no notice.

The server guards against a change in three ways.

1. The path `beta` is one constant, `graphVersion`. A test fails when it changes.
2. Each answer is checked. The tool stops with the error "the Graph beta answer has a new shape" when `id`, `state.requestedValue` or `@odata.type` is not as the reference states.
3. The files in `testdata/` hold the request bodies and the answers of the reference. `TestShapeChange` feeds a changed answer to each tool. A test also compares each request body to its file.

## Environment

| Variable | Meaning |
|---|---|
| `INTUNE_TENANT_ID` | Required. The Microsoft Entra tenant id. One server serves one tenant. |
| `INTUNE_CLIENT_ID` | Required. Application id of the app registration. |
| `INTUNE_CLIENT_SECRET` | Required. Client secret of the app registration. |
| `INTUNE_GRAPH_URL` | Optional. The default is `https://graph.microsoft.com`. It must have no path. The server adds `/beta`. A URL with a version path, such as `/v1.0`, is refused. |
| `INTUNE_AUTHORITY_URL` | Optional. The default is `https://login.microsoftonline.com`. |
| `INTUNE_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

The server gets a token with the client credentials grant and the scope `https://graph.microsoft.com/.default`. The image holds no credential.

## Tools

Reference: https://learn.microsoft.com/en-us/graph/api/resources/windowsupdates-deployment?view=graph-rest-beta and the source in https://github.com/microsoftgraph/microsoft-graph-docs-contrib (folder `api-reference/beta`).

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `list_catalog_entries` | `GET /admin/windows/updates/catalog/entries` with `$filter=isof(...)`, `$orderby` and `$top` | Not a change. | `readOnlyHint: true` |
| `start_patch_deployment` | `POST /admin/windows/updates/deployments`, then `POST .../deployments/{id}/audience/updateAudienceById` | None. | `destructiveHint: true`, `idempotentHint: false` |
| `get_patch_deployment` | `GET .../deployments/{id}`, `GET .../deployments/{id}/audience`, `GET .../deploymentAudiences/{audienceId}/members` | Not a change. | `readOnlyHint: true` |
| `pause_patch_deployment` | `PATCH .../deployments/{id}` with `state.requestedValue` `paused` | `resume_patch_deployment` | `destructiveHint: false`, `idempotentHint: true` |
| `resume_patch_deployment` | `PATCH .../deployments/{id}` with `state.requestedValue` `none` | `pause_patch_deployment` | `destructiveHint: false`, `idempotentHint: true` |
| `remove_patch_deployment` | `DELETE .../deployments/{id}` | None. | `destructiveHint: true`, `idempotentHint: true` |

One device for each deployment. `start_patch_deployment` creates a deployment, then adds one device to its audience. An empty audience gets no offer. When the second call fails, the tool deletes the deployment. Each tool that changes or reads a deployment first checks that its audience holds exactly one device. It refuses any other deployment, so the tools never act on a group.

`start_patch_deployment` is marked as an action with no undo, the same mark as a session revoke. The reason is that the pause only stops the offer of the update. It does not uninstall an update that the device already installed, and Graph has no call that uninstalls an update. The owner decided on 2026-11-21 that an update install destroys no data, so it does not wait for approval (ADR-0180). The mark tells the caller that the install stays. `pause_patch_deployment` and `resume_patch_deployment` stay reversible, because they only pause and resume the offer.

A deployment that Intune created can fail on the audience calls. Microsoft lists this as a known issue. The tools then refuse the deployment.

`list_catalog_entries` filters on the server with `$filter=isof('microsoft.graph.windowsUpdates.<kind>UpdateCatalogEntry')`, which the Microsoft page https://learn.microsoft.com/en-us/graph/windowsupdates-deploy-expedited-update shows for this call. It sends `$top` with the limit. When Graph answers with an `@odata.nextLink`, the tool follows it for at most 5 pages. It never uses the link as a URL. It takes the paging parameter (`$skiptoken` or `$skip`) and sends it to the fixed base URL. The result has `has_more`, which is true when more entries exist than the limit. The tool drops an entry of another type as a second defense.

A quality update is deployed as an expedited update. The Microsoft page "Deploy an expedited quality update using Windows Autopatch" shows a quality deployment with `settings.expedite` (`isExpedited: true`, `isReadinessTest: false`) and shows no quality deployment without it. So `start_patch_deployment` with `kind: quality` sends that setting. An expedited update installs as fast as possible and overrides the restart settings of the device policy. Before it creates anything, the tool reads the catalog entry and refuses an entry whose type is not the kind, and a quality entry with `isExpeditable` false. A feature update has no settings.

Each deployment tool that changes or reads a deployment first reads the audience, and then changes the deployment. Another person or tool can change the audience between the two calls. The check is a read followed by a change, not a lock. The member must be of the type `#microsoft.graph.windowsUpdates.azureADDevice`. A member of the type `updatableAssetGroup`, or an answer with no type, is refused, because a group counts as one member and can hold many devices. The shape check also needs `@odata.type` in the answer of a deployment, which the examples of the reference show. When Graph leaves it out, the tool stops with the shape error, which is the safe side.

The server did not run against a Microsoft tenant. The tests use a mock that follows the reference.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Give the app registration the application permission `WindowsUpdates.ReadWrite.All` and no other. It is the least privileged permission for the calls that change a deployment. `WindowsUpdates.Read.All` is enough for the read tools. Do not give the app an Intune or a directory role that it does not need. The device must be enrolled in Windows Update for Business deployment service management for the quality or feature update.
