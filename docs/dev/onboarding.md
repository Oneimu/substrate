# Local Development Onboarding: gVisor & microVM on kind

This guide walks a new contributor from a fresh machine to a working local
Substrate cluster, covering both sandbox runtimes:

- **gVisor** (`ateom-gvisor`) — zero hardware requirements, works anywhere
  Docker runs. Start here.
- **microVM** (`ateom-microvm`, Kata + Cloud Hypervisor) — requires `/dev/kvm`,
  i.e. a KVM-capable Linux host or an Apple Silicon Mac with nested
  virtualization via [Lima](https://lima-vm.io/).

For the architecture and terminology behind everything below, see
[architecture.md](../architecture.md) and [glossary.md](../glossary.md).

## 1. Mental model (60 seconds)

Substrate decouples compute infrastructure from stateful agent execution:

1. **WorkerPool** — pre-warmed pools of worker pods (running `ateom`) ready to
   accept workloads.
2. **ActorTemplate** — reusable definitions of an actor: container images,
   snapshot policy, sandbox class.
3. **Actor** — a stateful instance that can be created, activated (*resume*),
   frozen into object storage (*suspend*), and re-activated on any free worker.
4. **atenet-router** — ingress gateway that routes HTTP to whichever worker pod
   currently hosts the actor, resuming it on demand.

### Runtime comparison

| Dimension | gVisor | microVM |
|---|---|---|
| Isolation boundary | Application kernel / syscall interception (Sentry) | Hardware VM (KVM + Cloud Hypervisor) |
| Hardware requirement | None | `/dev/kvm` (bare metal, nested virt, or Lima) |
| Node label | None (default) | `ate.dev/sandboxClass=microvm` (applied automatically by `hack/create-kind-cluster.sh` when KVM is detected) |
| Runtime assets | `runsc`, fetched per `SandboxConfig` | 5 assets (`cloud-hypervisor`, `virtiofsd`, `vmlinux`, `rootfs.img`, `configuration-clh.toml`) staged into the in-cluster rustfs S3 bucket |
| Demo namespace | `ate-demo-counter` | `ate-demo-counter-microvm` |
| Demo template | `ate-demo-counter/counter` | `ate-demo-counter-microvm/counter-microvm` |

## 2. Prerequisites (all paths)

You need `docker`, `go`, and `kubectl`. (`kind` and `ko` are pinned in
`hack/tools/` and invoked automatically by the hack scripts — no install
needed.)

```sh
# Docker: follow https://docs.docker.com/engine/install/ for your distro, then:
sudo usermod -aG docker "$USER"
newgrp docker          # or log out/in to pick up the group
docker ps              # should succeed without sudo

# Go 1.26+ (see go.mod for the exact version)
go version

# kubectl: https://kubernetes.io/docs/tasks/tools/

# Go-installed binaries land in $(go env GOPATH)/bin — put it on PATH now,
# you'll need it for kubectl-ate later:
export PATH="$(go env GOPATH)/bin:${PATH}"
echo 'export PATH="$(go env GOPATH)/bin:${PATH}"' >> ~/.bashrc

# Sanity check
for tool in docker go kubectl; do
  which "$tool" >/dev/null && echo "OK  $tool" || echo "MISSING  $tool"
done
```

## 3. Path A: gVisor on kind (start here)

Zero-dependency onboarding on any Linux or macOS machine with Docker. No
hardware virtualization needed.

```sh
cd <your substrate checkout>

# 1. Create the kind cluster (also starts a local image registry).
#    The script probes for /dev/kvm; without it the cluster still works
#    for gVisor — microVM support is simply disabled.
./hack/create-kind-cluster.sh

# 2. Deploy the Substrate control plane (CRDs, ate-api-server, atelet,
#    atenet, valkey, rustfs, ...)
./hack/install-ate-kind.sh --deploy-ate-system

# 3. Deploy the gVisor counter demo (worker pool + actor template)
./hack/install-ate-kind.sh --deploy-demo-counter
```

Verify:

```sh
kubectl get pods -n ate-system            # all Running/Completed
kubectl get pods -n ate-demo-counter      # counter worker pod(s) Running
kubectl get workerpools,actortemplates -A # actortemplate READY=True
```

Then jump to [§5 Workload testing](#5-workload-testing--actor-lifecycle).

## 4. Path B: microVM on kind

The microVM runtime needs `/dev/kvm` visible to the Docker environment where
the kind node runs. Two equally supported host setups — pick whichever matches
your hardware:

### 4.1 Option A: Linux host with KVM

Works on bare-metal Linux or any cloud VM with nested virtualization enabled
(e.g. GCE N2/N2D instances with nested virt, or equivalent on other clouds).

**Step 1 — verify KVM:**

```sh
ls -la /dev/kvm
# Expected: crw-rw---- 1 root kvm 10, 232 ... /dev/kvm

grep -cE '(vmx|svm)' /proc/cpuinfo   # >0 means CPU virt support (x86)
```

`hack/create-kind-cluster.sh` probes for KVM by running a root container with
`--device /dev/kvm`, which works out of the box with a standard (rootful)
Docker install. With **rootless Docker** the container's root is remapped to
your user, so the probe fails with `permission denied` — use rootful Docker
instead, or open up the device with `sudo chmod 666 /dev/kvm`.

**Step 2 — create the cluster:**

```sh
./hack/create-kind-cluster.sh
# Look for: "/dev/kvm found: micro-VM (kata + cloud-hypervisor) support will be enabled."

kubectl get nodes --show-labels | grep 'ate.dev/sandboxClass=microvm'
```

**Step 3 — run the microVM demo:**

```sh
./hack/run-microvm-demo-kind.sh
```

This is a one-shot bring-up: it deploys the control plane
(`--deploy-ate-system`), then installs the cluster-wide microVM deps —
assembling the 5 runtime assets for your architecture (skipped if already
present under `bin/microvm-assets/`), staging them into
`s3://ate-snapshots/kata-assets/` (the in-cluster rustfs bucket), and
applying the `microvm` SandboxConfig — and finally deploys the demo worker
pool + template. See
[hack/microvm-assets/README.md](../../hack/microvm-assets/README.md) for the
manual equivalent of each step.

> **Note:** staging currently requires the `aws` CLI on the host
> (`hack/microvm-assets/stage-to-rustfs.sh`). This is a known wart — see
> [Known issues](#7-known-issues--planned-improvements). Until fixed, install it
> (`sudo apt install -y awscli` or the
> [AWS CLI v2 bundle](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)),
> or run it containerized:
>
> ```sh
> alias aws='docker run --rm --network host -v "$PWD:/aws" -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY amazon/aws-cli'
> ```

**Step 4 — verify:**

```sh
kubectl get pods -n ate-demo-counter-microvm
kubectl get workerpools,actortemplates -A
```

Expected:

```
NAMESPACE                  NAME                                 DESIRED  READY  AVAILABLE
ate-demo-counter-microvm   workerpool.ate.dev/counter-microvm   1        1      1

NAMESPACE                  NAME                                      READY  AGE
ate-demo-counter-microvm   actortemplate.ate.dev/counter-microvm     True   1m
```

### 4.2 Option B: Apple Silicon macOS via Lima

Lima can run a Linux VM with **nested virtualization**, exposing `/dev/kvm` to
Docker (and therefore to the kind node) inside the VM. This is a well-trodden
path — much of Substrate's development happens on macOS via limactl.

> **Hardware requirement:** the first-generation M1 lacks hardware support for
> ARM nested virtualization (FEAT_NV2) — Lima will fail with
> `[hostagent] Starting VZ ... FATA exiting`. Use a newer Apple Silicon chip,
> or fall back to Option A on a Linux host.

**Step 1 — install tooling:**

```sh
brew install lima docker go kubectl
```

(As in §2, `kind` and `ko` come from `hack/tools/` — no install needed.)

**Step 2 — launch Lima with nested virtualization:**

The guest image is pinned to Ubuntu 25.10 until the kernel issue in the
current default image is fixed — revisit the pin once a fixed image ships:

```sh
limactl start --name=docker-nested template://docker-rootful --nested-virt --set '.images = [
{"location":"https://cloud-images.ubuntu.com/releases/questing/release/ubuntu-25.10-server-cloudimg-arm64.img","arch":"aarch64"},
{"location":"https://cloud-images.ubuntu.com/releases/questing/release/ubuntu-25.10-server-cloudimg-amd64.img","arch":"x86_64"}
]'
```

When prompted to edit the configuration, set at least:

```yaml
cpus: 8
memory: "16GiB"
nestedVirtualization: true
networks:
  - vzNAT: true
mounts:
  - location: "~"
    writable: true
```

(The writable home mount lets the kind/ko workflows write into your checkout;
8 CPUs / 16 GiB is a comfortable floor for the control plane plus a microVM
worker; `vzNAT` gives the VM outbound networking under the vz VM type.)

**Step 3 — assemble the arm64 assets *inside* the Lima VM:**

`hack/microvm-assets/assemble.sh` must run on a Linux host of the target
architecture (on arm64 it builds `virtiofsd` from source with cargo against
Linux-only libraries), so run it in the Lima guest, not on macOS:

```sh
limactl shell docker-nested

# Inside the VM — install build deps once:
sudo apt-get update && sudo apt-get install -y git pkg-config libcap-ng-dev libseccomp-dev zstd unzip
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh   # rust via rustup
source "$HOME/.cargo/env"

cd <your substrate checkout>    # visible via the writable home mount
./hack/microvm-assets/assemble.sh
exit
```

The assets land in `bin/microvm-assets/arm64/` in your checkout, which is
shared with the host through the home mount — the demo script will find them
there and skip re-assembling.

**Step 4 — point the Docker CLI at Lima and bring everything up (on macOS):**

```sh
export DOCKER_HOST="unix://${HOME}/.lima/docker-nested/sock/docker.sock"
echo 'export DOCKER_HOST="unix://${HOME}/.lima/docker-nested/sock/docker.sock"' >> ~/.zprofile

cd <your substrate checkout>

# Cluster + microVM demo (the demo script deploys the control plane itself)
./hack/create-kind-cluster.sh
./hack/run-microvm-demo-kind.sh
```

Verify as in Option A, Step 4.

## 5. Workload testing & actor lifecycle

This section is identical for both runtimes — only the template name differs.

### 5.1 CLI setup & atespace

```sh
# Build and install the kubectl-ate plugin (lands in $(go env GOPATH)/bin,
# which must be on PATH — see §2)
go install ./cmd/kubectl-ate

kubectl ate create atespace demo
```

### 5.2 Create an actor

```sh
# gVisor:
kubectl ate create actor my-counter-1 -a demo --template ate-demo-counter/counter

# microVM:
kubectl ate create actor my-counter-1 -a demo --template ate-demo-counter-microvm/counter-microvm
```

A new actor starts as `STATUS_SUSPENDED` — it consumes **zero** compute/RAM on
any worker pod until resumed.

### 5.3 Resume (activate)

```sh
kubectl ate resume actor my-counter-1 -a demo
kubectl ate get actors -a demo
# STATUS_RUNNING, with an ATEOM POD and IP assigned
```

### 5.4 Send traffic through the router

```sh
kubectl port-forward -n ate-system svc/atenet-router 8080:80 &

# The Host header encodes the actor and atespace — the router uses it to
# locate (and, if needed, resume) the actor:
curl -s -H "Host: my-counter-1.demo.actors.resources.substrate.ate.dev" http://localhost:8080/
curl -s -H "Host: my-counter-1.demo.actors.resources.substrate.ate.dev" http://localhost:8080/
# -> hello from: <pod ip> | preserved memory count: 2 | preserved file counter: 2
```

### 5.5 State persistence across suspend/resume ("teleport")

```sh
# Checkpoint RAM + filesystem into the S3 bucket and free the worker:
kubectl ate suspend actor my-counter-1 -a demo
kubectl ate get actors -a demo    # STATUS_SUSPENDED, ATEOM POD: <none>

# Rehydrate onto any available worker:
kubectl ate resume actor my-counter-1 -a demo

# In-memory state survived the round trip:
curl -s -H "Host: my-counter-1.demo.actors.resources.substrate.ate.dev" http://localhost:8080/
# -> hello from: <pod ip> | preserved memory count: 3 | preserved file counter: 3
```

The counter continuing from where it left off — potentially on a *different*
worker pod — is the whole point: full working memory and filesystem state
round-tripped through object storage.

### 5.6 Cleanup

```sh
# Remove Substrate and all registered demos from the cluster:
./hack/install-ate-kind.sh --delete-all

# Or tear down the kind cluster entirely:
./hack/delete-kind-cluster.sh
```

## 6. Troubleshooting / friction log

Issues actually hit during onboarding, with root causes and fixes:

| # | Symptom | Root cause | Fix |
|---|---|---|---|
| 1 | `permission denied while trying to connect to the docker API` | User not in the `docker` group (or group not active in this session) | `sudo usermod -aG docker "$USER"` then `newgrp docker` or re-login |
| 2 | `/dev/kvm: permission denied` during the kind KVM probe | Rootless Docker: the probe container's root is remapped to your user, which can't open the device (`660 root:kvm`) | Use rootful Docker, or `sudo chmod 666 /dev/kvm` before `./hack/create-kind-cluster.sh` |
| 3 | `kubectl: command not found` from install scripts | `kubectl` not installed | Install per [kubernetes.io/docs/tasks/tools](https://kubernetes.io/docs/tasks/tools/) |
| 4 | `error: the 'aws' CLI is required but was not found in PATH` | `stage-to-rustfs.sh` uses `aws s3 cp` to stage microVM assets | Install the AWS CLI, or use the dockerized alias in §4.1 (see known issue below) |
| 5 | `error: unknown command "ate" for "kubectl"` | `go install ./cmd/kubectl-ate` put the plugin in `$(go env GOPATH)/bin`, which isn't on `PATH` | `export PATH="$(go env GOPATH)/bin:${PATH}"` (persist it in your shell rc) |
| 6 | Lima: `[hostagent] Starting VZ ... FATA exiting` on M1 | M1 lacks FEAT_NV2 hardware nested virtualization | Use a newer Apple Silicon Mac, or a Linux/KVM host (Option A) |

## 7. Known issues / planned improvements

Rough edges in the current scripts that surfaced during onboarding review;
fixes welcome:

- **microVM asset staging requires the `aws` CLI on the host.**
  `hack/microvm-assets/stage-to-rustfs.sh` should run the AWS CLI via Docker
  (or an equivalent in-cluster job) rather than requiring a host install.

## 8. Next steps

With a working cluster you're set up to develop:

- **Build and test:** `make build`, `make test`, and `make verify` (codegen /
  lint checks). Root-gated tests run via `hack/run-root-tests.sh` — see
  [CONTRIBUTING.md](../../CONTRIBUTING.md).
- **End-to-end tests on your kind cluster:** `hack/run-e2e-kind.sh`.
- **Repo conventions and layout:** [AGENTS.md](../../AGENTS.md) and
  [docs/dev/code-layout.md](code-layout.md).
- **More demos:** [demos/](../../demos/) — multiplexing Claude Code agents,
  request parking, autoscaled worker pools, and more.
- **Getting help:** the [ate-dev](https://groups.google.com/g/ate-dev) Google
  Group, or `#substrate-users` / `#substrate-dev` on the
  [CNCF Slack](https://slack.cncf.io/).
