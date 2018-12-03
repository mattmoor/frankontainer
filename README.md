## Frankontainers

Frankontainers is an experiment to explore dynamically assembling containers
from the layers of other containers.  The name is a play on Frankenstein's
monster, which was a humanoid monster assembled from the parts of humans
(what we're doing with the layers from containers!).

The net effect of this technique is NOT to produce a particularly useful
container image, but to create a benign container image.

> But why?

By including the layers from other images into our benign image, we enable
those layers to become cached on the local machine.  This lets us avoid
pulling those layers when images containing them are fetched later.

> But Matt, this sounds a lot like WarmImage...

That's because it is!  You can think of this an the next evolution of
WarmImage, as it is squarely aimed at addressing (some of) its short-comings:

1. **WarmImage uses a Pod-per-Image.** This is a problem because while the CPU
  and Memory requirements of the Pod are trivial, there are practical limits
  to how many of these it is possible to schedule on a node (due to things
  like networking).

1. **WarmImage exercised no discretion.** It cached everything, even if that would
  mean filling the disks of every node on the cluster.  Practically speaking
  it should have picked which images would give the biggest bang for our buck.

1. **WarmImage couldn't subdivide images.** If the above weren't limitations, then
  this wouldn't really matter, but as we start to practically consider how
  we might solve the above, our ability to "pack" things is pretty limited,
  since we must deal with whole images, not layers.

> So how can we do better?

The approach we take here is to essentially look across the set of containers
that the system is holistically asking us to [cache](https://github.com/knative/caching),
e.g.

```
- gcr.io/foo-bar/baz@sha256:aaaaaaaa
  - sha256:11111111  100 MiB
  - sha256:22222222  20 KiB
  - sha256:33333333  4 MiB

- docker.io/istio/pilot@sha256:bbbbbbbb
  - sha256:11111111  100 MiB
  - sha256:44444444  75 MiB
  - sha256:55555555  20 KiB
  - sha256:66666666  4 MiB

- docker.io/envoyproxy/envoy@sha256:cccccccc
  - sha256:11111111  100 MiB
  - sha256:44444444  75 MiB
  - sha256:77777777  140 MiB
  - sha256:88888888  32 KiB
  - sha256:99999999  5 MiB
```

### The "Recipe"

> TODO(mattmoor): Define a controller process for producing the Recipe instead of
> just documenting the Recipe as the hand-off.

We can now give the process a quota: "Don't use more than 200 MiB of disk for cache"
and pick the optimal set of layers to cache to maximize our usage of this quota. We
call this our frankontainer "Recipe".

Suppose we decide to cache the following layers (given our quota):

```
  sha256:11111111
  sha256:44444444
  sha256:33333333
  sha256:99999999
```

We would update the "Recipe" ConfigMap with enough information to find each of these
layers:

```yaml
data:
  # TODO(mattmoor): incorporate the rest of the Image spec.
  11111111: gcr.io/foo-bar/baz@sha256:aaaaaaaa
  44444444: docker.io/envoyproxy/envoy@sha256:cccccccc
  33333333: gcr.io/foo-bar/baz@sha256:aaaaaaaa
  99999999: docker.io/envoyproxy/envoy@sha256:cccccccc
```

### The "Doctor"

The "Recipe" ConfigMap is then read by the "Doctor" component; a Deployment
exposed via a NodePort service, which serves the Docker/OCI distribution
protocol.

The "Doctor" takes the "Recipe" of layers, and when an image is requested it
responds with a container image that is dynamically assembled as follows:

|       Image       |
|-------------------|
| `sha256:11111111` |
| `sha256:44444444` |
| `sha256:33333333` |
| `sha256:99999999` |
|  `./cmd/monster`  |

The `./cmd/monster` container is built via `ko` and rebased on the collection
of layers described in our "Recipe", so that it is the topmost layer (and cannot
conflict with any layers chosen above).

> If needed, we could also "white out" the `/` directory, but that would be
> largely cosmetic for our statically linked Go entrypoint.

This container image is our "monster".

### The "Monster" Army

We stand up our Frankontainer "Monster" army via a DaemonSet that pulls images
from the "Doctor" via its NodePort service:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: monster-army
spec:
  ...
  template:
    ...
    spec:
      containers:
      - name: the-monster
        image: 127.0.0.1:32100/monster:latest
        ...
```

This will start a container on every Node that simply sleeps, taking no memory
or CPU, but fetches (and pins) all of the selected layers so they will remain
resident and reduce the pull latency of subsequent containers containing these
layers.

