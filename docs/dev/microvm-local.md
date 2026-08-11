# Running the microVM runtime locally

The microVM sandbox class (`ateom-microvm`: a Kata guest on Cloud Hypervisor)
needs `/dev/kvm`, which makes local bring-up higher-friction than the default
gVisor path. This guide covers just that delta: getting a KVM-capable Docker
environment — on Linux, or on Apple Silicon macOS via
[Lima](https://lima-vm.io/) — then running the microVM counter demo and
verifying a guest-memory snapshot round-trip.

**Prerequisites:** complete the
[Quickstart (Development)](../../README.md#quickstart-development) in the
README first — it covers the base tooling and the default (gVisor) path this
guide builds on. For background on the runtime, see
[architecture.md](../architecture.md) and
[hack/microvm-assets/README.md](../../hack/microvm-assets/README.md).

## Option A: Linux host with KVM

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

This is a one-shot bring-up: it deploys the control plane, installs the
cluster-wide microVM deps via `hack/install-microvm-deps.sh` — assembling the
guest runtime assets for your architecture (skipped if already present under
`bin/microvm-assets/`), staging them into the in-cluster rustfs bucket, and
applying the `microvm` `SandboxConfig` — then deploys the demo worker pool +
template. Staging currently requires the `aws` CLI on the host
([install guide](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)).

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

## Option B: Apple Silicon macOS via Lima

Lima can run a Linux VM with **nested virtualization**, exposing `/dev/kvm` to
Docker (and therefore to the kind node) inside the VM. This is a well-trodden
path — much of Substrate's development happens on macOS via limactl.

> **Hardware requirement:** the first-generation M1 lacks hardware support for
> ARM nested virtualization (FEAT_NV2) — Lima will fail with
> `[hostagent] Starting VZ ... FATA exiting`. Use a newer Apple Silicon chip,
> or fall back to Option A on a Linux host.

**Step 1 — install Lima and the Docker CLI:**

```sh
brew install lima docker
```

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
sudo apt-get update && sudo apt-get install -y git pkg-config libcap-ng-dev libseccomp-dev zstd
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

./hack/create-kind-cluster.sh
./hack/run-microvm-demo-kind.sh
```

Verify as in Option A, Step 4.

## Smoke test: guest-memory snapshot round-trip

The [counter demo](../../demos/counter/README.md) keeps its count in RAM, so a
count that survives suspend/resume proves the guest-memory snapshot
round-tripped. Same flow as the README Quickstart, using the microVM
template:

```sh
kubectl ate create atespace demo   # skip if it already exists
kubectl ate create actor vm-counter-1 -a demo --template ate-demo-counter-microvm/counter-microvm
kubectl ate resume actor vm-counter-1 -a demo

kubectl port-forward -n ate-system svc/atenet-router 8000:80 &
curl -X POST -H "Host: vm-counter-1.demo.actors.resources.substrate.ate.dev" -i http://localhost:8000/
curl -X POST -H "Host: vm-counter-1.demo.actors.resources.substrate.ate.dev" -i http://localhost:8000/

# Suspend (checkpoints guest RAM to the snapshot bucket, frees the worker),
# resume (possibly onto a different worker), and confirm the count continues:
kubectl ate suspend actor vm-counter-1 -a demo
kubectl ate resume actor vm-counter-1 -a demo
curl -X POST -H "Host: vm-counter-1.demo.actors.resources.substrate.ate.dev" -i http://localhost:8000/
```

## Troubleshooting

| Symptom | Root cause | Fix |
|---|---|---|
| `/dev/kvm: permission denied` during the kind KVM probe | Rootless Docker: the probe container's root is remapped to your user, which can't open the device (`660 root:kvm`) | Use rootful Docker, or `sudo chmod 666 /dev/kvm` before `./hack/create-kind-cluster.sh` |
| `error: the 'aws' CLI is required but was not found in PATH` | `stage-to-rustfs.sh` uses `aws s3 cp` to stage microVM assets | Install the [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) |
| `cargo not found` from `assemble.sh` | On arm64, `virtiofsd` is built from source | Install the build deps listed in Option B, Step 3 |
| Lima: `[hostagent] Starting VZ ... FATA exiting` on M1 | M1 lacks FEAT_NV2 hardware nested virtualization | Use a newer Apple Silicon Mac, or a Linux/KVM host (Option A) |
