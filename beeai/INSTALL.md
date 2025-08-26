# Kagenti Operator Documentation

This document provides information on how to install and use the `kagenti-operator`. The operator supports two primary workflows:
1. Building new agent images from source (`AgentBuild` CR)
2. Deploying existing agent images (`Agent` CR)

## Overview

The `kagenti-operator` automates the management of BeeAI Agent resources within your Kubernetes cluster. It simplifies:
- Building agent images from source code
- Deploying pre-built agent images
- Managing agent lifecycle

## Prerequisites

Before installing the `kagenti-operator`, ensure you have:

* **Kubernetes Cluster:** A working Kubernetes cluster (Kind recommended for development)
* **kubectl:** The Kubernetes command-line tool
* **For building agents:**
  * Agent source code in a GitHub repository with a working Dockerfile
  * GitHub token for accessing repositories and pushing images to ghcr.io
* **For deploying agents:**
  * Existing agent image in a container registry (ghcr.io, Docker Hub, etc.)
  * (Optional) Registry credentials if using private images
* **Install [ollama]:**(https://ollama.com/download)
* **Clone [beeai examples]:** (https://github.com/i-am-bee/beeai-platform.git)

## Installation

### 1. Start Ollama

In a new terminal, run:

```shell
ollama run "your-llm-model" --keepalive 60m
```
### 2. Start Kagenti operator
In a new terminal, run:

```shell
curl -sSL https://raw.githubusercontent.com/kagenti/kagenti-operator/main/beeai/scripts/install.sh | bash
```

## Usage

### Option 1: Building and Deploying New Agents From Source
By default the operator install script creates Local Registry within Kind and a token is not needed to
push and pull images. If you want to use an external registry like ghcr.io, follow instructions below. 
In a new terminal, run:
```
export PASSWORD=<your github token>
kubectl create secret generic github-token-secret --from-literal=token="$PASSWORD"
```

Use the AgentBuild CR to build new agent images from source and optionally deploy them.

Example AgentBuild Configuration
Create research-agent-build.yaml:

```yaml
apiVersion: beeai.beeai.dev/v1
kind: AgentBuild
metadata:
  name: research-agent-build
spec:
  # Example agent source code repository
  repoUrl: "github.com/kagenti/agent-examples.git"
   # subfolder in the above repo containing ollama-deep-researcher agent source code
  sourceSubfolder: beeai/ollama-deep-researcher
  # Github user name
  repoUser: "your-github-username"
  # branch name in your source repository
  revision: "main"
  # image name to build
  image: "research-agent"
  imageTag: "v0.0.1"
  # repo url to receive the image built. The Tekton build step will push the docker image
  # to this repo. Uncomment below if using external image repository like ghcr.io
  #  imageRegistry: "ghcr.io/your-github-username"
  # Use local registry 
  imageRegistry: "registry.cr-system.svc.cluster.local:5000"
  # reference the secret created in step 3 above
  env:
    # Uncomment below code to enable pushing docker image to external repository like ghcr.io
    # your github repo token to push the docker image
    #- name: "SOURCE_REPO_SECRET"
    #  valueFrom:
    #    secretKeyRef:
    #      name: "github-token-secret"
    #      key: "token"

  # auto deploy the agent when pipeline succeeds
  deployAfterBuild: true
  
  # auto cleanup Tekton pods when pipeline succeeds
  cleanupAfterBuild: true

  # define agent configuration parameters
  agent:
    name: "research-agent"
    description: "research-agent"
    env:
    # Make sure the port is set. Otherwise, BeeAI will use random port in your agent
      - name: PORT
        value: 8000
      - name: LLM_API_BASE
        value: "http://host.docker.internal:11434/v1"
      - name: LLM_API_KEY
        value: "dummy"  
      # Replace "your-llm-model" below with the actual LLM model  
      - name: LLM_MODEL
        value: "your-llm-model"
    resources:
      limits:
        cpu: "500m"
        memory: "1Gi"
      requests:
        cpu: "100m"
        memory: "256Mi"

```
In a new terminal, deploy the AgentBuild CR:

```
kubectl apply -f research-agent-build.yaml
```

In a new terminal, watch the pods using command:

```shell
watch kubectl get pods
```

When a Tekton pipeline succeeds you should see the following pods:

```
NAME                                         READY   STATUS      RESTARTS   AGE
kagenti-operator-controller-manager-585c587f7f-nxctl   1/1     Running     0          85s
research-agent-c7cc5f568-82chd                         1/1     Running     0          9s
```

`Important:` Take note of the agent pod name research-agent-c7cc5f568-82chd. 

**Finding the BeeAI Agent's Internal Name:**

The name you'll use with the BeeAI CLI to interact with your agent may be different from the Kubernetes pod name. This internal agent name is typically defined within the agent's code itself. To discover the specific name your deployed agent is using, you need to inspect its logs. `Note:` need a better way to discover agent's name.

1.  **Identify the Agent Pod Name:**

    As described in the previous step, use the `watch kubectl get pods` command and identify the pod that starts with the `name` you specified in the `agent` section of your `AgentBuild` resource (e.g., `research-agent-c7cc5f568-82chd`).

2.  **View the Pod's Logs:**

    Once you have the agent pod's name, use the `kubectl logs` command followed by the pod's name to view its output:

    ```bash
    kubectl logs <your-agent-pod-name>
    ```

    Replace `<your-agent-pod-name>` with the actual name of your agent pod (e.g., `research-agent-c7cc5f568-82chd`).

3.  **Locate the Agent Name in the Logs:**

    Examine the logs for a message indicating the agent's internal name. BeeAI agents typically log this information during startup. Look for a line similar to the following:

    ```
    Agent with name 'ollama-deep-researcher' created.
    ```

    In this example, the internal name of the BeeAI agent is `'ollama-deep-researcher'`.

4. **Port Forward to k8s Service:**

    If you configured agent pod to expose port 8000 (env var PORT value), port-forward to the agent's k8s service. The operator creates the service using the same name as the agent pod name defined in AgentBuild CR. 

```shell
kubectl port-forward svc/research-agent 8000:8000
```
5. **Clone BeeAI Repository:** 
To run the beeai client code below, you must first clone beeai source code and cd to beeai directory. `This is an inconvenience
that we are working on to resolve in a near future.`

```
git clone https://github.com/i-am-bee/beeai-platform.git
cd beeai
```

6. **Use the Internal Name with the BeeAI CLI:**

    Use the agent's name identified in step 3 above to the agent as follows:

    
    ```bash
     env BEEAI__HOST=http://localhost:8000 \
        BEEAI__MCP_SSE_PATH='/sse' \
        uv run beeai agent run ollama-deep-researcher "llamas vs alpacas main differences"
    ```

    **Important:** Ensure you replace `"ollama-deep-researcher"` in the command above with the actual agent name you found in the pod's logs.


### Option 2: Deploying Agent From an Existing Image

Use the Agent CR to deploy pre-built agent images.

```Example Agent Configuration```
The following example uses a pre-built agent image from `kagenti-operator` repository.

Create research-agent.yaml:

```yaml
apiVersion: beeai.beeai.dev/v1
kind: Agent
metadata:
  name: existing-research-agent
spec:
  description: "Deploying an existing agent image"
  image: "ghcr.io/kagenti/research-agent:v0.0.2"
  env:
    - name: PORT
      value: "8000"
    # Replace "your-llm-model" below with the actual LLM model  
    - name: LLM_MODEL
      value: "your-llm-model"
    - name: LLM_API_BASE
      value: "http://host.docker.internal:11434/v1"
    - name: LLM_API_KEY
      value: "dummy"  
    - name: PRODUCTION_MODE
      value: "false"
# When using image from a private repo, uncomment below code and create k8s secret
# containing the token      
#    - name: IMAGE_REPO_SECRET
#      value: github-token

  resources:
    limits:
      cpu: "1"
      memory: "2Gi"
    requests:
      cpu: "500m"
      memory: "1Gi"
```

In a new terminal, deploy the research-agent.yaml CR:

```
kubectl apply -f research-agent.yaml
```

In a new terminal, watch the pods using command:

```shell
watch kubectl get pods
```

You should see a pod in a Running state:

```
existing-research-agent-c7cc5f568-73ff21                         1/1     Running     0          5s
```

``Verifying Deployment``

Follow the same steps as outlined in section `Finding the BeeAI Agent's Internal Name` above to discover internal agent's name.

If your configured agent pod to expose port 8000 (env var PORT value), port-forward to the agent's k8s service as follows:

```shell
kubectl port-forward svc/existing-research-agent 8000:8000
```

Run BeeAI client, making sure to use correct agent name as noted above. In our case, the agent name is `ollama-deep-researcher`:

`Important:` To run the beeai client code below, you must first clone beeai source code and cd to beeai directory. `This is an inconvenience
that we are working on to resolve in a near future.`

```
git clone https://github.com/i-am-bee/beeai-platform.git
cd beeai
```

```shell
 env BEEAI__HOST=http://localhost:8000 \
    BEEAI__MCP_SSE_PATH='/sse' \
    uv run beeai agent run ollama-deep-researcher "llamas vs alpacas main differences"
```



