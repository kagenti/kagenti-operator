# Token Broker — Kubernetes deployment

Manifests that accompany the `token-broker` image produced by the build script.
All objects live in the `kagenti-system` namespace.

## Manifests (apply in order)

| File | Object | Notes |
|------|--------|-------|
| `00-serviceaccount.yaml` | ServiceAccount | No RBAC — the broker does not call the Kubernetes API. |
| `01-secret.example.yaml` | Secret (template) | OAuth client credentials. **Do not commit real values** — see below. |
| `02-deployment.yaml` | Deployment | Set `image:` and `OAUTH_CALLBACK_URL`. `replicas: 1` (in-memory state). |
| `03-service.yaml` | Service (ClusterIP) | `token-broker.kagenti-system.svc.cluster.local:8190`. |
| `04-httproute.yaml` | HTTPRoute | Exposes `GET /oauth/callback` via the shared `http` gateway. |

## Credentials

Do not apply `01-secret.example.yaml` as-is. Create the secret out of band:

```sh
kubectl create secret generic github-oauth-credentials -n kagenti-system \
  --from-literal=client-id=<CLIENT_ID> \
  --from-literal=client-secret=<CLIENT_SECRET>
```

## Apply

```sh
kubectl apply -f 00-serviceaccount.yaml
# create the secret (see above) instead of applying the example
kubectl apply -f 02-deployment.yaml -f 03-service.yaml -f 04-httproute.yaml
```

## The callback route must match

`OAUTH_CALLBACK_URL` in the Deployment and the `hostnames:` in the HTTPRoute
must use the **same host**. The OAuth provider redirects the browser to that
URL; the gateway routes the host + `/oauth/callback` to the broker. A mismatch
(or a missing/unattached route) makes the post-consent redirect 404, and the
broker then blocks waiting for a callback that never arrives.

The route attaches to the gateway `http` in `kagenti-system`. That listener
admits routes from namespaces labelled `shared-gateway-access: "true"`, which
`kagenti-system` already carries.

## Migrating from the old ad-hoc objects

Earlier deployments placed the HTTPRoute in `kagenti-demo` with a parentRef to a
non-existent `kagenti-gateway`/`istio-system`, which never attached (404 on the
callback). The token broker does not deploy into `kagenti-demo`. Remove the
stale objects:

```sh
kubectl delete httproute token-broker-oauth-callback -n kagenti-demo --ignore-not-found
kubectl delete referencegrant allow-demo-httproute-to-token-broker -n kagenti-system --ignore-not-found
```
