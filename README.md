![ci](https://github.com/doitintl/kubeip/workflows/build/badge.svg) [![Go Report Card](https://goreportcard.com/badge/github.com/doitintl/kubeip)](https://goreportcard.com/report/github.com/doitintl/kubeip) ![Docker Pulls](https://img.shields.io/docker/pulls/doitintl/kubeip)

# What is kubeIP?

Many applications need to be whitelisted by users based on a Source IP Address. As of today, Google Kubernetes Engine doesn't support assigning a static pool of IP addresses to the GKE cluster. Using kubeIP, this problem is solved by assigning GKE nodes external IP addresses from a predefined list. kubeIP monitors the Kubernetes API for new/removed nodes and applies the changes accordingly.

# Deploy kubeIP (without building from source)

If you just want to use kubeIP (instead of building it yourself from source), please follow the instructions in this section. You’ll need Kubernetes version 1.10 or newer, Helm 3 or newer, and the Google Cloud SDK. You can install the [Google Cloud SDK](https://cloud.google.com/sdk) (which also installs kubectl).

To configure your Google Cloud SDK, set default project as:

```
gcloud config set project {your project_id}
```

Set the environment variables and make sure to configure before continuing:

```
export GCP_REGION=<gcp-region>
export GCP_ZONE=<gcp-zone>
export GKE_CLUSTER_NAME=<cluster-name>
export PROJECT_ID=$(gcloud config list --format 'value(core.project)')
export KUBEIP_NODEPOOL=<nodepool-with-static-ips>
export KUBEIP_SELF_NODEPOOL=<nodepool-for-kubeip-to-run-in>
```

**Get credentials for yourself**

If you haven't alrady, set up your GKE cluster credentials with
(replace `$GKE_CLUSTER_NAME` with your real GKE cluster name):

```
gcloud container clusters get-credentials $GKE_CLUSTER_NAME \
    --region $GCP_ZONE \
    --project $PROJECT_ID
```

You will need admin access to the cluster to deploy to the `kube-system` namespace.
Either set the relevant IAM rolebinding for your user, or get RBAC permissions with:

```
kubectl create clusterrolebinding cluster-admin-binding \
    --clusterrole cluster-admin --user `gcloud config list --format 'value(core.account)'`
```

**Creating an IAM Service Account**

Create a Service Account with this command:

```
gcloud iam service-accounts create kubeip-service-account --display-name "kubeIP"
```

Create and attach a custom kubeIP role to the service account by running the following commands:

```
gcloud iam roles create kubeip --project $PROJECT_ID --file roles.yaml

gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member=serviceAccount:kubeip-service-account@$PROJECT_ID.iam.gserviceaccount.com \
    --role=projects/$PROJECT_ID/roles/kubeip \
    --condition=None
```

**Allow the Kubernetes service account to use the Google service account**

kubeIP uses Workload Identity. The chart annotates the `kubeip-sa` service account in `kube-system` and does not mount a key file.

```
gcloud iam service-accounts add-iam-policy-binding \
    kubeip-service-account@$PROJECT_ID.iam.gserviceaccount.com \
    --role=roles/iam.workloadIdentityUser \
    --member="serviceAccount:${PROJECT_ID}.svc.id.goog[kube-system/kubeip-sa]"
```

**Create Static, Reserved IP Addresses:**

Create as many static IP addresses for the number of nodes in your GKE cluster (this example creates 10 addresses) so you will have enough addresses when your cluster scales up (manually or automatically):

```
for i in {1..10}; do gcloud compute addresses create kubeip-ip$i --project=$PROJECT_ID --region=$GCP_REGION; done
```

Add labels to reserved IP addresses. A common practice is to assign a unique value per cluster (for example cluster name):

```
for i in {1..10}; do gcloud beta compute addresses update kubeip-ip$i --update-labels kubeip=$GKE_CLUSTER_NAME --region $GCP_REGION; done
```

Install kubeIP with Helm. `KUBEIP_LABELVALUE` should be your GKE cluster name, and `KUBEIP_NODEPOOL` should be the node pool kubeIP assigns addresses to. `KUBEIP_FORCEASSIGNMENT` defaults to true and assigns reserved addresses to existing nodes in that pool. Run the kubeIP pod on a different pool (`KUBEIP_SELF_NODEPOOL`).

```
helm upgrade --install kubeip helm/kubeip \
    --namespace kube-system \
    --set gsa_name=kubeip-service-account@$PROJECT_ID.iam.gserviceaccount.com \
    --set env.KUBEIP_LABELVALUE=$GKE_CLUSTER_NAME \
    --set env.KUBEIP_NODEPOOL=$KUBEIP_NODEPOOL \
    --set-json "nodeSelector={\"cloud.google.com/gke-nodepool\":\"$KUBEIP_SELF_NODEPOOL\"}"
```

To assign addresses to other node pools, set `env.KUBEIP_ADDITIONALNODEPOOLS` to a comma-separated list. Tag those addresses with the `KUBEIP_LABELKEY` value + `-node-pool` and the node pool name, for example `kubeip-node-pool=my-node-pool`.

```
helm upgrade --install kubeip helm/kubeip \
    --namespace kube-system \
    --reuse-values \
    --set env.KUBEIP_ADDITIONALNODEPOOLS=my-node-pool
```

**Move an existing deploy/ installation**

Helm 4 can adopt the Deployment, ServiceAccount, ClusterRole, and ClusterRoleBinding that `kubectl apply -f deploy/` created. Copy the live settings first, because the chart does not read the `kubeip-config` ConfigMap:
To assign addresses to other node pools, set `KUBEIP_ADDITIONALNODEPOOLS` to a comma-separated list. A pool name is an exact match and uses that name as the address tag: `my-node-pool,other-pool` looks for `kubeip-node-pool=my-node-pool` and `kubeip-node-pool=other-pool`.

To use a different tag, write `node_pool|ip_tag`. Tag those addresses with the `KUBEIP_LABELKEY` value + `-node-pool` and the `ip_tag`, for example `kubeip-node-pool=batch-ips` for an entry `batch-pool|batch-ips`.

In the `node_pool|ip_tag` form, a selector that ends with `*` matches every node pool with that prefix, and those pools share one tag. `workers-*|worker-ips` assigns addresses labeled `kubeip-node-pool=worker-ips` to pools such as `workers-a` and `workers-b`. An exact pool name is not a prefix, including a name that ends with `*`.

`KUBEIP_NODEPOOL` still uses reserved addresses that have no `{KUBEIP_LABELKEY}-node-pool` label.

```
kubectl -n kube-system get configmap kubeip-config -o jsonpath='{.data}{"\n"}'
kubectl -n kube-system get deploy kubeip -o jsonpath='{.spec.template.spec.nodeSelector}{"\n"}'
kubectl -n kube-system get sa kubeip-sa -o jsonpath='{.metadata.annotations.iam\.gke\.io/gcp-service-account}{"\n"}'
```

Pass those values to the same `helm upgrade --install` command and add `--take-ownership`. The Workload Identity binding above must already exist: the upgraded pod no longer mounts `kubeip-key`. After the new pod is assigning addresses, remove the objects the chart does not manage:

```
kubectl -n kube-system delete configmap kubeip-config secret kubeip-key
```

Once you’ve assigned an IP address to a node kubeIP, a label will be created for that node `kubip_assigned` with the value of the IP address (`.` are replaced with `-`):

 `172.31.255.255 ==> 172-31-255-255`

**Ordering IPs**

KubeIP can order IPs based on the numeric value identified by `KUBEIP_ORDERBYLABELKEY`.  

IPs are ordered in descending order if `KUBEIP_ORDERBYDESC` is set to true, ascending order otherwise. 

Missing `KUBEIP_ORDERBYLABELKEY` or invalid values present on `KUBEIP_ORDERBYLABELKEY` will be assigned the lowest priority.  

When nodes are added, deleted or on tick, kubeIP will check whether the nodes have the most optimal IP assignment.  What does this mean? 

E.g. Let's assume Node1 has IP_A, Node2 has IP_B and IP_A > IP_B, when we scale the cluster down the cluster two things might happen  
1. Node 1 is deleted which results in a sub-optimal IP assignment since Node2 has IP_B and IP_A > IP_B
2. Node 2 is deleted maintaining optimal order.  

In the first case Node 2 is re-assigned IP_A.  

To order the IPs reserved above in asc order use 

```
for i in {1..10}; do gcloud beta compute addresses update kubeip-ip$i --update-labels priority=$i --region=$GCP_REGION; done
```

and upgrade the release:

```
helm upgrade kubeip helm/kubeip \
    --namespace kube-system \
    --reuse-values \
    --set env.KUBEIP_ORDERBYLABELKEY=priority \
    --set env.KUBEIP_ORDERBYDESC=false
```

**Copy Labels**

KubeIP will also copy all labels from the IP being assigned over to the node if `KUBEIP_COPYLABELS` is set to true.  

This is typically helpful when we want to have node selection not based on IP but more semantic label keys and values.  

As an example let's label `kubeip-ip1` with `platform_whitelisted=true`, to do this we execute the following command 

```
gcloud beta compute addresses update kubeip-ip1 --update-labels "platform_whitelisted=true" --region=$GCP_REGION;
```

Now, when a node is assigned the IP address of `kubeip-ip1` it will also be labelled with `platform_whitelisted=true` as well as the default `kubip_assigned`.  

An IP can have multiple labels, all will be copied over.

**Clear Labels**

When IPs get assigned or re-assigned to achieve optimal IP assignment we can configure the system to clear any previous labels. Set `KUBEIP_CLEARLABELS` flag to `true` if you want this behaviour. 

This feature is required when labels are not overlapping.  E.g. let's assume we have the following tagged IPs; IP_A and IP_B, order by priority

```
IP_A test_a=value_a,test_b=value_b,priority=1
IP_B test_c=value_c,priority=2
```
Let's assume that the assignment was as follows 

```
IP_A => NodeA
IP_B => NodeB
```

At this point `NodeA` has labels `test_a=value_a,test_b=value_b` and `NodeB` has labels `test_c=value_c`.  Note priority is not copied over.  

If `NodeA` is deleted a re-assignment needs to happen (due to the fact that IP_A > IP_B) and `NodeB` would have
- `test_a=value_a,test_b=value_b,test_c=value_c` if `KUBEIP_CLEARLABELS="false"` and 
- `test_a=value_a,test_b=value_b` if `KUBEIP_CLEARLABELS="true"`

Note that `test_c` is not an overlapping label and hence might cause problems if `KUBEIP_CLEARLABELS` is not set to `true`.  

**Dry Run Mode**

Dry run mode allows debugging the operations performed by KubeIP without actually performing the operations.  

ONLY use this mode during development of new features on KubeIP.  


# Deploy & Build From Source

You need Kubernetes version 1.10 or newer. You also need Docker version and kubectl 1.10.x or newer installed on your machine, as well as the Google Cloud SDK. You can install the [Google Cloud SDK](https://cloud.google.com/sdk) (which also installs kubectl).


**Clone Git Repository**

Make sure your $GOPATH is [configured](https://github.com/golang/go/wiki/SettingGOPATH). You'll need to clone this repository to your `$GOPATH/src` folder. 

```
mkdir -p $GOPATH/src/doitintl/kubeip
git clone https://github.com/doitintl/kubeip.git $GOPATH/src/doitintl/kubeip
cd $GOPATH/src/doitintl/kubeip
```

**Set Environment Variables**

Replace **us-central1** with the region where your GKE cluster resides and **kubeip-cluster** with your real GKE cluster name

```
export GCP_REGION=us-central1
export GCP_ZONE=us-central1-b
export GKE_CLUSTER_NAME=kubeip-cluster
export PROJECT_ID=$(gcloud config list --format 'value(core.project)')
```

**Develop kubeIP Locally**

Compile the kubeIP binary and run tests

```
make
```

**Build kubeIP's Container Image**


Compile the kubeIP binary and build the Docker image as following:

```
make image
```

Tag the image using:

```
docker tag kubeip gcr.io/$PROJECT_ID/kubeip
```

Finally, push the image to Google Container Registry with:

```
docker push gcr.io/$PROJECT_ID/kubeip
```

Alternatively, you can export `REGISTRY` to `gcr.io/$PROJECT_ID` and run the script `build-all-and-push.sh` which builds and publishes the docker image.

**Create an IAM service account**

Create a Service Account with this command:

```
gcloud iam service-accounts create kubeip-service-account --display-name "kubeIP"
```

Create and attach the custom kubeIP role to the service account by running the following commands:

```
gcloud iam roles create kubeip --project $PROJECT_ID --file roles.yaml

gcloud projects add-iam-policy-binding $PROJECT_ID --member serviceAccount:kubeip-service-account@$PROJECT_ID.iam.gserviceaccount.com --role projects/$PROJECT_ID/roles/kubeip
```

Allow the Kubernetes service account to use it through Workload Identity:

```
gcloud iam service-accounts add-iam-policy-binding \
    kubeip-service-account@$PROJECT_ID.iam.gserviceaccount.com \
    --role=roles/iam.workloadIdentityUser \
    --member="serviceAccount:${PROJECT_ID}.svc.id.goog[kube-system/kubeip-sa]"
```

**Get cluster credentials and RBAC permissions**

```
gcloud container clusters get-credentials $GKE_CLUSTER_NAME \
    --region $GCP_ZONE \
    --project $PROJECT_ID
```

```
kubectl create clusterrolebinding cluster-admin-binding \
    --clusterrole cluster-admin --user `gcloud config list --format 'value(core.account)'`
```

**Create static reserved IP addresses:**

Create as many static IP addresses for the number of nodes in your GKE cluster (this example creates 10 addresses) so you will have enough addresses when your cluster scales up (automatically or manually):

```
for i in {1..10}; do gcloud compute addresses create kubeip-ip$i --project=$PROJECT_ID --region=$GCP_REGION; done
```

Add labels to reserved IP addresses. A common practice is to assign a unique value per cluster. You can use your cluster name for example:

```
for i in {1..10}; do gcloud beta compute addresses update kubeip-ip$i --update-labels kubeip=$GKE_CLUSTER_NAME --region $GCP_REGION; done
```

By default, kubeIP manages nodes in the `default-pool` node pool and looks for addresses labeled `kubeip=reserved`. Set `env.KUBEIP_NODEPOOL`, `env.KUBEIP_LABELKEY`, and `env.KUBEIP_LABELVALUE` to change that. `KUBEIP_FORCEASSIGNMENT` defaults to true and, on startup and every five minutes, assigns a reserved address to nodes in the pool that do not have one.

Install the image you pushed:

```
helm upgrade --install kubeip helm/kubeip \
    --namespace kube-system \
    --set image=gcr.io/$PROJECT_ID/kubeip \
    --set gsa_name=kubeip-service-account@$PROJECT_ID.iam.gserviceaccount.com \
    --set env.KUBEIP_LABELVALUE=$GKE_CLUSTER_NAME
```

References:

 - Event listening code was take from [kubewatch](https://github.com/bitnami-labs/kubewatch/)
