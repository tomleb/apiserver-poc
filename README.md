# POC apiserver

This apiserver is only for experimenting and exploring the aggregation layer of
Kubernetes. Highly unstructured document follows (sorry).

It uses code from [agones](https://github.com/googleforgames/agones/) because
it was one of the resource to learn about api extension. (See
[here](https://github.com/googleforgames/agones/pull/682) for an example of them
moving to API extension for a specific resource). Again, this is just
exploratory work.

# How To

## Deploy

Here's how to quickly get this running on your cluster. This depends on `docker
buildx` for my own convenience.

1. Create the k3s cluster by running the following command. This will
   automatically download [k3d](https://github.com/k3d-io/k3d/) used to create
   the cluster.

```sh
make create-cluster
```

2. Run this command to build the image, import it into k3s and deploy the
   `Deployment`.

```sh
make deploy
```

3. You can easily follow the logs with `make log`.
4. When you make changes to the code, simply re-run `make deploy` to re-build
   the image, re-import it and re-deploy the pod.

## Playing around



```sh
# Create a RancherToken (this creates a backing secret without the
# plaintext token)
kubectl create -f ./hack/token.yaml -o yaml

# Look at the underlying secret
kubectl get secret foo -o yaml

# Get the same data but as a RancherToken in different format

# As a table, though we don't support the table stuff so only name+age is shown
kubectl get ranchertokens foo
# As JSON
kubectl get ranchertokens foo -o json
# As YAML
kubectl get ranchertokens foo -o yaml

# Disable the token (supports PATCH with merge-patch strategy)
kubectl apply -f ./hack/token-disabled.yaml

# Delete the token (deletes the underlying secret). This gives an error because
# kubectl delete tries to GET the resource after deleting it and we don't
# respond with the appropriate NotFound error.
kubectl delete ranchertokens foo

# Not yet implemented, but listing tokens should be possible
kubectl get ranchertokens
```



# Findings

[sample-apiserver](https://github.com/kubernetes/sample-apiserver/) is a more
complete implementation of apiserver and reuses the k8s library
(`k8s.io/apiserver`). Some things that it includes:
- auto generated openapi spec from Go types (this we want)
- store data to etcd
- multiple api version with preferred version
- table conversion for `kubectl get`
- supports running webhooks

There are two other projects that are meant to help building api extension
servers:
- https://github.com/kubernetes-sigs/apiserver-runtime
- https://github.com/kubernetes-sigs/apiserver-builder-alpha/

Both of these appear to be unmaintained / experimental.

Watch requests used to be done through their own endpoints (eg:
`/apis/apps/v1/watch/statefulsets`) but that's now deprecated. Instead, it
should use the same List endpoint but with `watch=true` param.

Warning not to depend on authorization being done for incoming requests:
k8s.io/apiserver/pkg/server/options/authentication.go..Humm..

# APIServer

An API server needs to implement at least these APIs:
- Discovery API
- OpenAPI V2 / V3

# Discovery API

It is described here:
https://kubernetes.io/docs/concepts/overview/kubernetes-api/#discovery-api. It
describes the types of resources that are present in the API server. It is then
aggregated into the list of resources in the core apiserver.

Basically, you can get information about resources like so:

```
kubectl get --raw /apis/<group>/<version>
```

eg:

```
$ kubectl get --raw /apis/tomlebreux.com/v1alpha1
{
  "kind": "APIResourceList",
  "apiVersion": "v1",
  "groupVersion": "tomlebreux.com/v1alpha1",
  "resources": [
    {
      "name": "clusterranchertokens",
      "singularName": "clusterranchertoken",
      "namespaced": false,
      "kind": "ClusterRancherToken",
      "verbs": [
        "create",
        "list"
      ]
    },
    {
      "name": "ranchertokens",
      "singularName": "ranchertoken",
      "namespaced": true,
      "kind": "RancherToken",
      "verbs": [
        "create"
      ]
    }
  ]
}
```

Listing the versions and preferred version:

```
$ kubectl get --raw /apis/tomlebreux.com
{
  "kind": "APIGroup",
  "apiVersion": "v1",
  "name": "tomlebreux.com",
  "versions": [
    {
      "groupVersion": "tomlebreux.com/v1alpha1",
      "version": "v1alpha1"
    }
  ],
  "preferredVersion": {
    "groupVersion": "tomlebreux.com/v1alpha1",
    "version": "v1alpha1"
  }
}
```

kubectl and client-go can use this information to map a GVK to a HTTP path. A
table shows this below.

## HTTP Path and Methods

| Command    | Method   | Path  |
| --------   | -------  | ---   |
| `kubectl create -f /path/to/file.yaml`               | POST | `/apis/tomlebreux.com/v1alpha1/clusterranchertokens` |
| `kubectl get clusterranchertoken`             | GET  | `/apis/tomlebreux.com/v1alpha1/clusterranchertokens` |
| `kubectl get clusterranchertoken my-token`    | GET  | `/apis/tomlebreux.com/v1alpha1/clusterranchertokens/my-token` |
| `kubectl get -w clusterranchertoken my-token` | GET  | `/apis/tomlebreux.com/v1alpha1/clusterranchertokens/my-token` followed by `/apis/tomlebreux.com/v1alpha1/clusterranchertokens?fieldSelector=metadata.name=my-token&resourceVersion=0&watch=true` |
| `kubectl get ranchertoken`                    | GET  | `/apis/tomlebreux.com/v1alpha1/namespaces/default/ranchertokens`          |
| `kubectl get ranchertoken my-token`           | GET  | `/apis/tomlebreux.com/v1alpha1/namespaces/default/ranchertokens/my-token` |

## Testing ClusterRancherTokens

`kubectl create -f <clusterranchertokens>` results in the following
request:

```json
{
  "headers": {
    "Accept": ["application/json"],
    "Accept-Encoding": ["gzip"],
    "Audit-Id": ["c6f17c4a-385e-4e97-afd5-e6bd8cf08c34"],
    "Content-Length": ["155"],
    "Content-Type": ["application/json"],
    "Kubectl-Command": ["kubectl create"],
    "Kubectl-Session": ["911f8dc0-8deb-4bf0-85da-94e5e8536b63"],
    "User-Agent": ["kubectl/v1.27.14 (linux/amd64) kubernetes/f678fbc"],
    "X-Forwarded-For": ["172.16.32.3"],
    "X-Forwarded-Host": ["10.42.0.41:9443"],
    "X-Forwarded-Proto": ["https"],
    "X-Forwarded-Uri": ["/apis/tomlebreux.com/v1alpha1/clusterranchertokens"],
    "X-Remote-Group": ["system:masters","system:authenticated"],
    "X-Remote-User": ["system:admin"]
  },
  "host": "10.42.0.41:9443",
  "message": "ClusterRancherTokens",
  "method": "POST",
  "requestURI": "/apis/tomlebreux.com/v1alpha1/clusterranchertokens?fieldManager=kubectl-create&fieldValidation=Strict"
}
```

Observation: Note the `fieldManager=kubectl-create` and `fieldValidation=Strict`
parameters. More on fieldValidation
[here](https://github.com/kubernetes/enhancements/blob/master/keps/sig-api-machinery/2885-server-side-unknown-field-validation/README.md).
More on fieldManager
[here](https://kubernetes.io/docs/reference/using-api/server-side-apply/).



`kubectl get clusterranchertokens` results in the following request:


```json
{
  "headers": {
    "Accept":["application/json;as=Table;v=v1;g=meta.k8s.io,application/json;as=Table;v=v1beta1;g=meta.k8s.io,application/json"],
    "Accept-Encoding":["gzip"],
    "Audit-Id":["81b5ee33-98e8-405e-9770-284a742b4e57"],
    "Kubectl-Command":["kubectl get"],
    "Kubectl-Session":["c0fdc6ac-dcbd-447c-841b-c2f01a82e1b6"],
    "User-Agent":["kubectl/v1.27.14 (linux/amd64) kubernetes/f678fbc"],
    "X-Forwarded-For":["172.16.32.3"],
    "X-Forwarded-Host":["10.42.0.41:9443"],
    "X-Forwarded-Proto":["https"],
    "X-Forwarded-Uri":["/apis/tomlebreux.com/v1alpha1/clusterranchertokens"],
    "X-Remote-Group":["system:masters","system:authenticated"],
    "X-Remote-User":["system:admin"]
  },
  "host":"10.42.0.41:9443",
  "method":"GET",
  "requestURI":"/apis/tomlebreux.com/v1alpha1/clusterranchertokens?limit=500"
}
```

Observation: We'll need to implement a "table conversion" mechanism for when
kubectl asks for a table (such as above). This basically returns lists of
printable columns and their values.

# OpenAPI

The API server should serve both `/openapi/v3` and /`openapi/v2` endpoints.
These endpoints are polled every ~20 seconds. It's then possible to view them
like so:

```
kubectl get --raw /openapi/v2
kubectl get --raw /openapi/v3
```

TODO: Verify that openapi v3 is required for SSA (server-side apply) patches.

sample-apiserver leverages codegen to automatically build the OpenAPI definition
for Go types. Look
[here](https://github.com/kubernetes/sample-apiserver/blob/master/pkg/generated/openapi/zz_generated.openapi.go).

# Q&A

- Does authz happen by the main apiserver? Yes, the main apiserver will do both
  authn and authz. Our apiserver will only receive a request from the main
  apiserver if the user is authorized. (Simple example in
  [hack/sa.yaml](hack/sa.yaml).)
- Are webhooks (mutating/validating) run? It appears that they aren't run. If we
  want them run we might need the API server to watch the validating/mutating
  webhook config dynamically and call them when appropriate. Done
  [here](https://github.com/kubernetes/apiserver/blob/78af3642d28d050e359206248f9bcfb7ebb50926/pkg/server/plugins.go#L29-L34)
  in the `k8s.io/apiserver` library)
- Why kubectl doesn't care about verbs from Discovery API?

# Troubleshooting

1. ServiceNotFound

When the service that the APIService object points to doesn't exist:

```
$ kubectl get apiservice 
NAME                                   SERVICE                        AVAILABLE                 AGE
v1.                                    Local                          True                      4h5m
v1.scheduling.k8s.io                   Local                          True                      4h5m
v1.storage.k8s.io                      Local                          True                      4h5m
v1alpha1.tomlebreux.com                default/rancher-apiextension   False (ServiceNotFound)   167m
```

2. 
